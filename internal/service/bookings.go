package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"gymbadminton/internal/store"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------- DTO ----------

type CreateBookingInput struct {
	IDempotencyKey string
	VenueID        int64
	CourtID        int64
	MemberID       int64
	StartTime      time.Time
	EndTime        time.Time
	PartySize      int
	RentRacket     bool
	RacketCount    int
	BuyShuttle     bool
	ShuttleCount   int
	UseBenefit     bool   // 是否使用会员免费时长/补偿券（默认 true）
	Source         string // member / operator
	Note           string
}

type CreateBookingResult struct {
	Booking         *store.Booking `json:"booking"`
	PriceText       string         `json:"price_text"`
	PaymentDeadline time.Time      `json:"payment_deadline"`
}

func genEntryCode(ctx context.Context, q pgx.Tx) (string, error) {
	for i := 0; i < 20; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(1000000))
		if err != nil {
			return "", err
		}
		code := fmt.Sprintf("%06d", n.Int64())
		var exists bool
		if err := q.QueryRow(ctx, `select exists(select 1 from bookings where entry_code=$1)`, code).Scan(&exists); err != nil {
			return "", err
		}
		if !exists {
			return code, nil
		}
	}
	return "", errors.New("cannot allocate entry code")
}

func genBookingNo(now time.Time) string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	return "BK" + now.Format("20060102150405") + fmt.Sprintf("%06d", n.Int64())
}

// availableBenefitMinutes 月度剩余可用（含预占扣减） + 未被占用的补偿券
func availableBenefitMinutes(ctx context.Context, q pgx.Tx, memberID, venueID int64, month string, freeHours float64) (int, int, int, error) {
	var used, reserved int
	err := q.QueryRow(ctx,
		`select coalesce(free_minutes_used,0), coalesce(free_minutes_reserved,0)
		 from member_benefit_usage
		 where member_id=$1 and venue_id=$2 and year_month=$3`,
		memberID, venueID, month).Scan(&used, &reserved)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, 0, err
	}
	monthlyLeft := int(freeHours*60) - used - reserved
	if monthlyLeft < 0 {
		monthlyLeft = 0
	}
	var grants int
	if err := q.QueryRow(ctx,
		`select coalesce(sum(minutes),0) from benefit_grants
		 where member_id=$1 and venue_id=$2 and used_at is null and reserved_booking_id is null`,
		memberID, venueID).Scan(&grants); err != nil {
		return 0, 0, 0, err
	}
	return monthlyLeft + grants, monthlyLeft, grants, nil
}

// reserveBenefit 下单预占权益分钟（先月度后券）。返回（月度占用，券占用）。
func reserveBenefit(ctx context.Context, tx pgx.Tx, b *store.Booking, monthlyAvail, need int) (int, int, error) {
	if need <= 0 {
		return 0, 0, nil
	}
	fromMonthly := need
	if fromMonthly > monthlyAvail {
		fromMonthly = monthlyAvail
	}
	fromGrants := need - fromMonthly
	month := b.StartTime.Format("2006-01")

	if fromMonthly > 0 {
		if _, err := tx.Exec(ctx,
			`insert into member_benefit_usage(member_id,venue_id,year_month,free_minutes_reserved)
			 values($1,$2,$3,$4)
			 on conflict (member_id,venue_id,year_month)
			 do update set free_minutes_reserved = member_benefit_usage.free_minutes_reserved + $4`,
			b.MemberID, b.VenueID, month, fromMonthly); err != nil {
			return 0, 0, err
		}
		// 超额校验（并发预占时可能突破）
		var total int
		if err := tx.QueryRow(ctx,
			`select free_minutes_used + free_minutes_reserved from member_benefit_usage
			 where member_id=$1 and venue_id=$2 and year_month=$3`,
			b.MemberID, b.VenueID, month).Scan(&total); err != nil {
			return 0, 0, err
		}
		if total > int(policyFreeHours(ctx, tx, b.VenueID, b.MemberTypeSnap)*60) {
			return 0, 0, ce("BENEFIT_EXHAUSTED", "会员免费时长已被其他待支付订单占用", ErrConflict)
		}
	}
	if fromGrants > 0 {
		if err := reserveGrants(ctx, tx, b.MemberID, b.VenueID, b.ID, fromGrants); err != nil {
			return 0, 0, err
		}
	}
	return fromMonthly, fromGrants, nil
}

// reserveGrants 占用补偿券（按最早顺序），不足时拆券
func reserveGrants(ctx context.Context, tx pgx.Tx, memberID, venueID, bookingID int64, need int) error {
	rows, err := tx.Query(ctx,
		`select id, minutes from benefit_grants
		 where member_id=$1 and venue_id=$2 and used_at is null and reserved_booking_id is null
		 order by created_at, id for update`,
		memberID, venueID)
	if err != nil {
		return err
	}
	type gr struct {
		id      int64
		minutes int
	}
	var list []gr
	for rows.Next() {
		var g gr
		if err := rows.Scan(&g.id, &g.minutes); err != nil {
			rows.Close()
			return err
		}
		list = append(list, g)
	}
	rows.Close()
	remaining := need
	for _, g := range list {
		if remaining <= 0 {
			break
		}
		if g.minutes <= remaining {
			if _, err := tx.Exec(ctx,
				`update benefit_grants set reserved_booking_id=$1 where id=$2 and reserved_booking_id is null`,
				bookingID, g.id); err != nil {
				return err
			}
			remaining -= g.minutes
		} else {
			// 部分占用：整券预占给本单，再拆分一张剩余分钟的新券
			if _, err := tx.Exec(ctx,
				`update benefit_grants set reserved_booking_id=$1 where id=$2`,
				bookingID, g.id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`insert into benefit_grants(member_id,venue_id,minutes,reason,granted_source,granted_by)
				 select member_id,venue_id,$3,reason,granted_source,granted_by
				 from benefit_grants where id=$2`,
				bookingID, g.id, g.minutes-remaining); err != nil {
				return err
			}
			remaining = 0
		}
	}
	if remaining > 0 {
		return ce("BENEFIT_EXHAUSTED", "补偿券分钟不足", ErrConflict)
	}
	return nil
}

// settleReservedBenefit 支付成功：预占 -> 已核销
func settleReservedBenefit(ctx context.Context, tx pgx.Tx, b *store.Booking) error {
	if b.BenefitMonthlyMinutes > 0 {
		if _, err := tx.Exec(ctx,
			`update member_benefit_usage
			 set free_minutes_used = free_minutes_used + $2,
			     free_minutes_reserved = greatest(free_minutes_reserved - $2, 0)
			 where member_id=$1 and venue_id=$3 and year_month=$4`,
			b.MemberID, b.BenefitMonthlyMinutes, b.VenueID, b.StartTime.Format("2006-01")); err != nil {
			return err
		}
	}
	if b.BenefitGrantMinutes > 0 {
		if _, err := tx.Exec(ctx,
			`update benefit_grants set used_at=now(), used_booking_id=reserved_booking_id
			 where reserved_booking_id=$1`, b.ID); err != nil {
			return err
		}
	}
	return nil
}

// releaseReservedBenefit 支付超时/退订未支付：释放预占
func releaseReservedBenefit(ctx context.Context, tx pgx.Tx, b *store.Booking) error {
	if b.BenefitMonthlyMinutes > 0 {
		if _, err := tx.Exec(ctx,
			`update member_benefit_usage
			 set free_minutes_reserved = greatest(free_minutes_reserved - $2, 0)
			 where member_id=$1 and venue_id=$3 and year_month=$4`,
			b.MemberID, b.BenefitMonthlyMinutes, b.VenueID, b.StartTime.Format("2006-01")); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx,
		`update benefit_grants set reserved_booking_id=null where reserved_booking_id=$1 and used_at is null`,
		b.ID); err != nil {
		return err
	}
	return nil
}

func policyFreeHours(ctx context.Context, tx pgx.Tx, venueID int64, memberType string) float64 {
	var h float64
	err := tx.QueryRow(ctx,
		`select free_hours_per_month from member_type_policies where venue_id=$1 and member_type=$2`,
		venueID, memberType).Scan(&h)
	if err != nil {
		return 0
	}
	return h
}

// CreateBooking 创建预约：Redis 临界锁 + PG GiST 排他约束双重防重。
func (s *Service) CreateBooking(ctx context.Context, in CreateBookingInput) (*CreateBookingResult, error) {
	now := s.now(ctx)
	in.StartTime = in.StartTime.UTC().Truncate(time.Minute)
	in.EndTime = in.EndTime.UTC().Truncate(time.Minute)
	if in.Source == "" {
		in.Source = "member"
	}
	if in.PartySize < 1 {
		in.PartySize = 1
	}

	// 幂等：同一 key 重试直接返回已创建订单
	if in.IDempotencyKey != "" {
		var existing int64
		err := s.pool().QueryRow(ctx,
			`select booking_id from idempotency_keys where idempotency_key=$1`, in.IDempotencyKey).Scan(&existing)
		if err == nil {
			b, gerr := s.GetBooking(ctx, existing)
			if gerr == nil {
				return &CreateBookingResult{Booking: b, PriceText: fmtCents(b.PayableCents),
					PaymentDeadline: derefT(b.ExpiresAt)}, nil
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}

	// 第一层：Redis 非阻塞锁（精确时段），快速拒绝并发重复请求
	lk, err := s.Locker.AcquireCourtLock(ctx, in.CourtID, in.StartTime, in.EndTime, 20*time.Second)
	if err != nil {
		if errors.Is(err, store.ErrLockBusy) {
			return nil, ce("SLOT_LOCKED", "该场地时段正在被其他用户预约，请稍后重试", err)
		}
		return nil, err
	}
	defer func() { _ = s.Locker.Release(ctx, lk) }()

	var b *store.Booking
	txErr := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		venue, err := s.loadVenue(ctx, tx, in.VenueID)
		if err != nil {
			return err
		}
		court, err := s.loadCourt(ctx, tx, in.CourtID)
		if err != nil {
			return err
		}
		if court.VenueID != venue.ID {
			return ce("COURT_VENUE_MISMATCH", "场地不属于该场馆", ErrValidation)
		}
		if court.Status != "active" {
			return ce("COURT_INACTIVE", "场地当前不可预约", ErrMaintenance)
		}

		// 基本时间校验
		if !in.EndTime.After(in.StartTime) {
			return ce("BAD_TIME", "结束时间必须晚于开始时间", ErrValidation)
		}
		durMin := int(in.EndTime.Sub(in.StartTime).Minutes())
		if durMin < 30 || durMin > 240 {
			return ce("BAD_DURATION", "预约时长需在 30~240 分钟之间", ErrValidation)
		}
		if in.StartTime.Before(now) {
			return ce("PAST_SLOT", "不能预约过去的时段", ErrValidation)
		}

		// 会员
		var m store.Member
		err = tx.QueryRow(ctx,
			`select id,phone,name,coalesce(password_hash,''),member_type,no_show_count,status
			 from members where id=$1`, in.MemberID).
			Scan(&m.ID, &m.Phone, &m.Name, &m.PasswordHash, &m.MemberType, &m.NoShowCount, &m.Status)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ce("MEMBER_NOT_FOUND", "会员不存在", ErrNotFound)
			}
			return err
		}
		if m.Status != "active" {
			return ce("MEMBER_BLOCKED", "会员账号已被限制", ErrIllegalState)
		}
		policy, err := s.getMemberPolicy(ctx, tx, venue.ID, m.MemberType)
		if err != nil {
			return err
		}

		// 会员优先预约：高峰时段在释放窗口之前仅会员可订
		windows, err := s.peakWindows(ctx, tx, venue.ID)
		if err != nil {
			return err
		}
		hoursAhead := in.StartTime.Sub(now).Hours()
		if isPeakAt(windows, in.StartTime) {
			if m.MemberType == "normal" && hoursAhead > float64(venue.MemberOnlyPeakReleaseHours) {
				return ce("MEMBER_ONLY_PEAK",
					fmt.Sprintf("高峰时段开场前 %d 小时内仅对银卡/金卡开放", venue.MemberOnlyPeakReleaseHours),
					ErrMemberOnly)
			}
		}
		// 提前预约上限
		if policy.MaxAdvanceHours > 0 && hoursAhead > float64(policy.MaxAdvanceHours) && in.Source == "member" {
			return ce("ADVANCE_LIMIT", "超出该会员类型可提前预约的时间范围", ErrAdvanceLimit)
		}

		// 营业时段 / 临时加场（逐分钟校验）
		day := in.StartTime.Format("2006-01-02")
		extra, err := loadExtraSlots(ctx, tx, venue.ID, court.ID, day)
		if err != nil {
			return err
		}
		for mm := 0; mm < durMin; mm++ {
			if !courtOpenAt(venue, extra, in.StartTime.Add(time.Duration(mm)*time.Minute)) {
				return ce("OUTSIDE_HOURS", "预约时段不在营业或临时加场范围内", ErrOutsideHours)
			}
		}

		// 维护锁定（含边界）；已报完工待验收的场地同样不开放
		blocked, err := courtBlockedByMaintenance(ctx, tx, court.ID, in.StartTime, in.EndTime)
		if err != nil {
			return err
		}
		if blocked {
			return ce("MAINTENANCE", "场地在该时段处于维护锁定/待验收状态", ErrMaintenance)
		}

		// 人数与容量
		capacity := court.Capacity
		if capacity == 0 {
			capacity = venue.DefaultCourtCapacity
		}
		if in.PartySize > capacity*2 {
			return ce("TOO_MANY", fmt.Sprintf("人数超过场地上限（%d 人）的 2 倍", capacity), ErrValidation)
		}

		// 计费
		month := in.StartTime.Format("2006-01")
		var avail, monthlyAvail int
		if in.UseBenefit {
			avail, monthlyAvail, _, err = availableBenefitMinutes(ctx, tx, m.ID, venue.ID, month, policy.FreeHoursPerMonth)
			if err != nil {
				return err
			}
		}
		if !in.RentRacket {
			in.RacketCount = 0
		}
		if !in.BuyShuttle {
			in.ShuttleCount = 0
		}
		bd := quote(priceInput{
			venue: venueWithRate(venue, effectiveRate(venue, court)), policy: policy, peakWindows: windows,
			start: in.StartTime, end: in.EndTime,
			racketCount: in.RacketCount, shuttleCount: in.ShuttleCount,
			availableBenefitM: avail,
		})

		entryCode, err := genEntryCode(ctx, tx)
		if err != nil {
			return err
		}
		expires := now.Add(time.Duration(venue.PaymentTimeoutSeconds) * time.Second)

		var id int64
		row := tx.QueryRow(ctx, `insert into bookings(
			booking_no,venue_id,court_id,member_id,member_type_snap,member_name_snap,
			party_size,capacity_snapshot,start_time,end_time,
			rent_racket,racket_count,buy_shuttle,shuttle_count,
			court_fee_cents,addon_cents,discount_cents,benefit_cents,benefit_minutes,
			payable_cents,status,entry_code,expires_at,note,created_source,updated_at)
			values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,'held',$21,$22,$23,$24,now())
			returning id`,
			genBookingNo(now), venue.ID, court.ID, m.ID, m.MemberType, m.Name,
			in.PartySize, capacity, in.StartTime, in.EndTime,
			in.RentRacket, in.RacketCount, in.BuyShuttle, in.ShuttleCount,
			bd.CourtFeeCents, bd.AddonCents, bd.DiscountCents, bd.BenefitCents, bd.BenefitMinutesUsed,
			bd.PayableCents, entryCode, expires, in.Note, in.Source)
		if err := row.Scan(&id); err != nil {
			return err
		}
		// 权益预占（月度 + 券），支付时核销、超时/退订时释放
		if bd.BenefitMinutesUsed > 0 {
			stub := store.Booking{ID: id, MemberID: m.ID, VenueID: venue.ID,
				StartTime: in.StartTime, MemberTypeSnap: m.MemberType}
			fromM, fromG, err := reserveBenefit(ctx, tx, &stub, monthlyAvail, bd.BenefitMinutesUsed)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`update bookings set benefit_monthly_minutes=$2,benefit_grant_minutes=$3 where id=$1`,
				id, fromM, fromG); err != nil {
				return err
			}
		}
		if in.IDempotencyKey != "" {
			if _, err := tx.Exec(ctx,
				`insert into idempotency_keys(idempotency_key,booking_id) values($1,$2)
				 on conflict (idempotency_key) do nothing`, in.IDempotencyKey, id); err != nil {
				return err
			}
		}
		if err := emitEvent(ctx, tx, "booking.created", in.Source, &id, &venue.ID, &court.ID, &m.ID, map[string]any{
			"payable_cents": bd.PayableCents, "entry_code": entryCode,
			"party_size": in.PartySize, "capacity": capacity,
		}); err != nil {
			return err
		}
		var err2 error
		b, err2 = scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1`, id))
		return err2
	})

	if txErr != nil {
		var pgErr *pgconn.PgError
		if errors.As(txErr, &pgErr) && pgErr.Code == "23P01" { // exclusion_violation
			return nil, ce("SLOT_TAKEN", "该场地时段已被预约（时间冲突）", ErrConflict)
		}
		if errors.Is(txErr, pgx.ErrNoRows) {
			return nil, ce("NOT_FOUND", "资源不存在", ErrNotFound)
		}
		return nil, txErr
	}
	return &CreateBookingResult{
		Booking:         b,
		PriceText:       fmtCents(b.PayableCents),
		PaymentDeadline: derefT(b.ExpiresAt),
	}, nil
}

func derefT(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// ---------- 支付 ----------

// PayBooking 完成支付（mock 支付网关）。held -> paid；零元单也必须显式“支付”确认。
// 支付成功后核销权益分钟（月度用量 / 补偿券）。
func (s *Service) PayBooking(ctx context.Context, bookingID int64, method string, source string, actorID *int64) (*store.Booking, error) {
	now := s.now(ctx)
	if method == "" {
		method = "mock"
	}
	if source == "" {
		source = "member"
	}
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, bookingID))
		if err != nil {
			return err
		}
		if b.Status != "held" {
			return ce("NOT_PAYABLE", "当前状态不可支付（"+b.Status+"）", ErrIllegalState)
		}
		if b.ExpiresAt != nil && now.After(*b.ExpiresAt) {
			// 超时锁在，应由过期任务处理；直接拒绝并提示
			return ce("PAYMENT_EXPIRED", "支付已超时，场地已/将被释放", ErrIllegalState)
		}
		txnNo := fmt.Sprintf("PAY%d-%06d", b.ID, random6())
		if _, err := tx.Exec(ctx, `insert into payments(booking_id,kind,amount_cents,method,status,transaction_no,reason,created_source,created_by)
			values($1,'payment',$2,$3,'succeeded',$4,'',$5,$6)`,
			b.ID, b.PayableCents, method, txnNo, source, actorID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`update bookings set status='paid',paid_cents=payable_cents,paid_at=$2,version=version+1,updated_at=now()
			 where id=$1`, b.ID, now); err != nil {
			return err
		}
		// 权益：预占 -> 核销
		if b.BenefitMinutes > 0 {
			if err := settleReservedBenefit(ctx, tx, b); err != nil {
				return err
			}
		}
		return emitEvent(ctx, tx, "payment.succeeded", source, &b.ID, &b.VenueID, &b.CourtID, actorID, map[string]any{
			"amount_cents": b.PayableCents, "method": method, "transaction_no": txnNo,
		})
	})
	if err != nil {
		return nil, err
	}
	return s.GetBooking(ctx, bookingID)
}
