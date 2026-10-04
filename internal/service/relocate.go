package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gymbadminton/internal/store"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------- 换场方案（场地维护临时换场） ----------

const offerColumns = `id,booking_id,maintenance_id,venue_id,old_court_id,status,candidates,clearance_needed,
	resolution,new_booking_id,venue_subsidy_cents,reason,created_by,decided_by,decided_at,created_at`

func scanOffer(row pgx.Row) (*store.RelocationOffer, error) {
	var o store.RelocationOffer
	err := row.Scan(&o.ID, &o.BookingID, &o.MaintenanceID, &o.VenueID, &o.OldCourtID, &o.Status,
		&o.Candidates, &o.ClearanceNeeded, &o.Resolution, &o.NewBookingID, &o.VenueSubsidyCents,
		&o.Reason, &o.CreatedBy, &o.DecidedBy, &o.DecidedAt, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// createRelocationOffer 为维护覆盖到的未开场订单（paid/warming）生成换场方案：
// 筛选同时间可用场地快照；已进场的订单生成旧场地清场提醒；维护原因写入订单防误判占场。
// 调用方须已持有 booking 行锁（CreateMaintenance 事务内）。
func (s *Service) createRelocationOffer(ctx context.Context, tx pgx.Tx, m *store.Maintenance, b *store.Booking, staffID int64) (*store.RelocationOffer, error) {
	now := s.now(ctx)

	// 幂等：同一订单已有待处理方案时不重复生成
	var pending int
	if err := tx.QueryRow(ctx,
		`select count(*) from relocation_offers where booking_id=$1 and status='pending'`, b.ID).Scan(&pending); err != nil {
		return nil, err
	}
	if pending > 0 {
		return scanOffer(tx.QueryRow(ctx,
			`select `+offerColumns+` from relocation_offers where booking_id=$1 and status='pending' order by id desc limit 1`, b.ID))
	}

	// 预热中的订单退回 paid 并关闭旧场地灯光（灯控权限随方案确认再同步到新场地）
	if b.Status == "warming" {
		if _, err := tx.Exec(ctx,
			`update bookings set status='paid', light_off_at=coalesce(light_off_at,$2),
			 version=version+1, updated_at=now() where id=$1`, b.ID, now); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx,
			`insert into light_records(booking_id,court_id,action,command_source,success,detail,occurred_at)
			 values($1,$2,'off','operator',true,'maintenance lock: 换场前关闭旧场地预热灯光',$3)`,
			b.ID, b.CourtID, now); err != nil {
			return nil, err
		}
		b.Status = "paid"
	}

	// 维护原因写入旧订单：闸机/灯控/裁决链路都以此识别“非用户占场”
	if _, err := tx.Exec(ctx,
		`update bookings set maintenance_reason=$2, version=version+1, updated_at=now() where id=$1`,
		b.ID, m.Reason); err != nil {
		return nil, err
	}

	// 用户已扫码进入旧场地 -> 生成旧场地清场提醒（前台待办）
	clearance := b.FirstPresenceAt != nil
	if clearance {
		var courtNo string
		_ = tx.QueryRow(ctx, `select court_no from courts where id=$1`, b.CourtID).Scan(&courtNo)
		msg := fmt.Sprintf("订单 %s（%s）用户已扫码进入 %s 号场地，场地维护（%s）触发临时换场，请引导用户离场并确认清场",
			b.BookingNo, b.MemberNameSnap, courtNo, m.Reason)
		if _, err := tx.Exec(ctx,
			`insert into ops_notifications(venue_id,court_id,booking_id,type,message)
			 values($1,$2,$3,'clearance_reminder',$4)`,
			b.VenueID, b.CourtID, b.ID, msg); err != nil {
			return nil, err
		}
		if err := emitEvent(ctx, tx, "relocate.clearance_reminder", "system", &b.ID, &b.VenueID, &b.CourtID, nil,
			map[string]any{"maintenance_id": m.ID, "message": msg}); err != nil {
			return nil, err
		}
	}

	// 筛选同时间可用场地
	venue, err := s.loadVenue(ctx, tx, b.VenueID)
	if err != nil {
		return nil, err
	}
	cands, err := s.findRelocationCandidates(ctx, tx, venue, b)
	if err != nil {
		return nil, err
	}
	candRaw, _ := json.Marshal(cands)

	var offerID int64
	if err := tx.QueryRow(ctx,
		`insert into relocation_offers(booking_id,maintenance_id,venue_id,old_court_id,candidates,clearance_needed,reason,created_by)
		 values($1,$2,$3,$4,$5::jsonb,$6,$7,$8) returning id`,
		b.ID, m.ID, b.VenueID, b.CourtID, string(candRaw), clearance, m.Reason, staffID).Scan(&offerID); err != nil {
		return nil, err
	}
	if err := emitEvent(ctx, tx, "booking.relocation_offered", "operator", &b.ID, &b.VenueID, &b.CourtID, &staffID,
		map[string]any{"offer_id": offerID, "maintenance_id": m.ID, "candidates": len(cands),
			"clearance_needed": clearance, "reason": m.Reason}); err != nil {
		return nil, err
	}
	return scanOffer(tx.QueryRow(ctx, `select `+offerColumns+` from relocation_offers where id=$1`, offerID))
}

// findRelocationCandidates 筛选同时间可用场地：同场馆、场地启用、容量够、
// 时段无活跃预约、无维护/待验收阻断、营业（或加场）时间内，并计算相对原场地费的差价。
func (s *Service) findRelocationCandidates(ctx context.Context, tx pgx.Tx, venue *store.Venue, b *store.Booking) ([]store.RelocationCandidate, error) {
	windows, err := s.peakWindows(ctx, tx, venue.ID)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx,
		`select id, venue_id, court_no, coalesce(capacity,0), status, price_cents_per_hour
		 from courts where venue_id=$1 and status='active' and id<>$2 order by court_no`,
		venue.ID, b.CourtID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var courts []store.Court
	for rows.Next() {
		var c store.Court
		if err := rows.Scan(&c.ID, &c.VenueID, &c.CourtNo, &c.Capacity, &c.Status, &c.PriceCentsPerHour); err != nil {
			return nil, err
		}
		courts = append(courts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := []store.RelocationCandidate{}
	durMin := int(b.EndTime.Sub(b.StartTime).Minutes())
	day := b.StartTime.Format("2006-01-02")
	for i := range courts {
		c := courts[i]
		cap := c.Capacity
		if cap == 0 {
			cap = venue.DefaultCourtCapacity
		}
		if b.PartySize > cap*2 {
			continue
		}
		// 时段冲突（活跃预约）
		var taken bool
		if err := tx.QueryRow(ctx,
			`select exists(select 1 from bookings where court_id=$1
			  and status in ('held','paid','warming','in_use')
			  and tstzrange(start_time,end_time,'[)') && tstzrange($2,$3,'[)'))`,
			c.ID, b.StartTime, b.EndTime).Scan(&taken); err != nil {
			return nil, err
		}
		if taken {
			continue
		}
		// 维护 / 待验收阻断
		blocked, err := courtBlockedByMaintenance(ctx, tx, c.ID, b.StartTime, b.EndTime)
		if err != nil {
			return nil, err
		}
		if blocked {
			continue
		}
		// 营业 / 加场时间
		extra, err := loadExtraSlots(ctx, tx, venue.ID, c.ID, day)
		if err != nil {
			return nil, err
		}
		open := true
		for mm := 0; mm < durMin; mm++ {
			if !courtOpenAt(venue, extra, b.StartTime.Add(time.Duration(mm)*time.Minute)) {
				open = false
				break
			}
		}
		if !open {
			continue
		}
		rate := effectiveRate(venue, &c)
		_, _, fee := courtFeeForSlot(rate, venue.PeakMultiplier, windows, b.StartTime, b.EndTime)
		out = append(out, store.RelocationCandidate{
			CourtID:           c.ID,
			CourtNo:           c.CourtNo,
			Capacity:          cap,
			PriceCentsPerHour: rate,
			PriceDiffCents:    fee - b.CourtFeeCents,
		})
	}
	return out, nil
}

// ---------- 方案查询 ----------

// GetRelocationOfferForBooking 返回订单最新的换场方案（会员本人或员工）
func (s *Service) GetRelocationOfferForBooking(ctx context.Context, bookingID int64) (*store.RelocationOffer, error) {
	return scanOffer(s.pool().QueryRow(ctx,
		`select `+offerColumns+` from relocation_offers where booking_id=$1 order by id desc limit 1`, bookingID))
}

// ListMemberRelocationOffers 会员自己的换场方案列表
func (s *Service) ListMemberRelocationOffers(ctx context.Context, memberID int64, status string) ([]*store.RelocationOffer, error) {
	q := `select ` + offerColumns + ` from relocation_offers o
		join bookings b on b.id = o.booking_id where b.member_id=$1`
	args := []any{memberID}
	if status != "" {
		q += ` and o.status=$2 order by o.id desc limit 100`
		args = append(args, status)
	} else {
		q += ` order by o.id desc limit 100`
	}
	rows, err := s.pool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.RelocationOffer
	for rows.Next() {
		o, err := scanOffer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ListRelocationOffers 员工查看换场方案（按场馆/状态过滤）
func (s *Service) ListRelocationOffers(ctx context.Context, venueID int64, status string) ([]*store.RelocationOffer, error) {
	q := `select ` + offerColumns + ` from relocation_offers o where 1=1`
	args := []any{}
	idx := 1
	if venueID > 0 {
		idx++
		q += fmt.Sprintf(` and o.venue_id=$%d`, idx-1)
		args = append(args, venueID)
	}
	if status != "" {
		idx++
		q += fmt.Sprintf(` and o.status=$%d`, idx-1)
		args = append(args, status)
	}
	q += ` order by o.id desc limit 200`
	rows, err := s.pool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.RelocationOffer
	for rows.Next() {
		o, err := scanOffer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// expireOffersForBooking 订单离开可换场状态后，待处理方案自动失效
func expireOffersForBooking(ctx context.Context, tx pgx.Tx, bookingID int64) error {
	_, err := tx.Exec(ctx,
		`update relocation_offers set status='expired' where booking_id=$1 and status='pending'`, bookingID)
	return err
}

// ---------- 用户确认方案：换场 / 顺延 / 退款 ----------

type RelocationDecision struct {
	Action    string // relocate / postpone / refund
	CourtID   int64  // relocate 必选（须在候选中）
	StartTime time.Time
	EndTime   time.Time
	MemberID  int64
}

// RelocationAcceptResult 确认结果
type RelocationAcceptResult struct {
	Offer      *store.RelocationOffer `json:"offer"`
	OldBooking *store.Booking         `json:"old_booking"`
	NewBooking *store.Booking         `json:"new_booking,omitempty"`
}

// AcceptRelocation 用户确认换场方案。
// relocate：换到同时间可用场地，差价（新场地更贵时）由场馆承担，闸机/灯控权限同步到新场地；
// postpone：顺延到同时长的新时段（同场地，需已过验收或无维护阻断）；
// refund：放弃场地，场馆原因全额退款并开争议单。
func (s *Service) AcceptRelocation(ctx context.Context, bookingID int64, in RelocationDecision) (*RelocationAcceptResult, error) {
	now := s.now(ctx)
	in.StartTime = in.StartTime.UTC().Truncate(time.Minute)
	in.EndTime = in.EndTime.UTC().Truncate(time.Minute)
	res := &RelocationAcceptResult{}
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, bookingID))
		if err != nil {
			return err
		}
		if b.MemberID != in.MemberID {
			return ce("FORBIDDEN", "只能处理本人的换场方案", ErrIllegalState)
		}
		offer, err := scanOffer(tx.QueryRow(ctx,
			`select `+offerColumns+` from relocation_offers
			 where booking_id=$1 and status='pending' order by id desc limit 1 for update`, bookingID))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return ce("NO_PENDING_OFFER", "该订单没有待确认的换场方案", ErrNotFound)
			}
			return err
		}
		if b.Status != "paid" && b.Status != "warming" {
			return ce("NOT_RELOCATABLE", "订单当前状态不可换场（"+b.Status+"）", ErrIllegalState)
		}
		venue, err := s.loadVenue(ctx, tx, b.VenueID)
		if err != nil {
			return err
		}

		switch in.Action {
		case "refund":
			if err := s.applyRelocationRefund(ctx, tx, b, offer, now); err != nil {
				return err
			}
		case "relocate":
			nb, subsidy, err := s.applyRelocationMove(ctx, tx, venue, b, offer, in.CourtID, b.StartTime, b.EndTime, now, "accepted_relocate")
			if err != nil {
				return err
			}
			offer.VenueSubsidyCents = subsidy
			res.NewBooking = nb
		case "postpone":
			if !in.EndTime.After(in.StartTime) {
				return ce("BAD_TIME", "顺延结束时间必须晚于开始时间", ErrValidation)
			}
			origDur := b.EndTime.Sub(b.StartTime)
			if in.EndTime.Sub(in.StartTime) != origDur {
				return ce("BAD_DURATION", fmt.Sprintf("顺延时长需与原订单一致（%d 分钟）", int(origDur.Minutes())), ErrValidation)
			}
			if !in.StartTime.After(now) {
				return ce("PAST_SLOT", "顺延时段必须晚于当前时间", ErrValidation)
			}
			nb, subsidy, err := s.applyRelocationMove(ctx, tx, venue, b, offer, b.CourtID, in.StartTime, in.EndTime, now, "accepted_postpone")
			if err != nil {
				return err
			}
			offer.VenueSubsidyCents = subsidy
			res.NewBooking = nb
		default:
			return ce("BAD_ACTION", "action 必须为 relocate / postpone / refund", ErrValidation)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 事务外重读，拿到最新状态
	res.Offer, _ = s.GetRelocationOfferForBooking(ctx, bookingID)
	res.OldBooking, _ = s.GetBooking(ctx, bookingID)
	if res.NewBooking != nil {
		res.NewBooking, _ = s.GetBooking(ctx, res.NewBooking.ID)
	}
	return res, nil
}

// applyRelocationRefund 用户选择退款：场馆原因全额退款 + 争议单（与既有 venue_fault 语义一致）
func (s *Service) applyRelocationRefund(ctx context.Context, tx pgx.Tx, b *store.Booking, offer *store.RelocationOffer, now time.Time) error {
	if _, err := tx.Exec(ctx,
		`update bookings set status='venue_fault',verdict='venue_fault',judge_source='maintenance_relocate',
		 judged_at=$2,version=version+1,updated_at=now() where id=$1`, b.ID, now); err != nil {
		return err
	}
	if amt := b.PaidCents - b.RefundedCents; amt > 0 {
		if _, err := tx.Exec(ctx,
			`insert into payments(booking_id,kind,amount_cents,method,status,transaction_no,reason,created_source)
			 values($1,'refund',$2,'mock','succeeded',$3,$4,'system')`,
			b.ID, amt, fmt.Sprintf("RF%d-%06d", b.ID, random6()),
			"场地维护换场：用户选择退款，场馆原因全额退款"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update bookings set refunded_cents=paid_cents where id=$1`, b.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx,
		`insert into disputes(booking_id,auto_opened,status,issue,created_at)
		 values($1,true,'open',$2,now()) on conflict (booking_id) do nothing`,
		b.ID, "场地维护换场，用户选择退款："+offer.Reason); err != nil {
		return err
	}
	resolution, _ := json.Marshal(map[string]any{"action": "refund", "refund_cents": b.PaidCents - b.RefundedCents})
	if _, err := tx.Exec(ctx,
		`update relocation_offers set status='refunded',resolution=$2::jsonb,decided_by=$3,decided_at=$4 where id=$1`,
		offer.ID, string(resolution), b.MemberID, now); err != nil {
		return err
	}
	return emitEvent(ctx, tx, "booking.relocation_refunded", "member", &b.ID, &b.VenueID, &b.CourtID, &b.MemberID,
		map[string]any{"offer_id": offer.ID, "reason": offer.Reason})
}

// applyRelocationMove 换场/顺延共用：旧订单 -> relocated（维护原因已写入），
// 新订单继承价格与已支付金额（更高价差价由场馆承担），闸机/灯控权限同步到新场地。
func (s *Service) applyRelocationMove(ctx context.Context, tx pgx.Tx, venue *store.Venue, b *store.Booking,
	offer *store.RelocationOffer, newCourtID int64, start, end time.Time, now time.Time, resolveStatus string) (*store.Booking, int64, error) {

	isPostpone := resolveStatus == "accepted_postpone"
	if !isPostpone {
		// 换场：目标场地必须在方案候选中
		var cands []store.RelocationCandidate
		if err := json.Unmarshal(offer.Candidates, &cands); err != nil {
			return nil, 0, err
		}
		ok := false
		for _, c := range cands {
			if c.CourtID == newCourtID {
				ok = true
				break
			}
		}
		if !ok {
			return nil, 0, ce("NOT_A_CANDIDATE", "目标场地不在换场候选中，请从方案候选场地中选择", ErrValidation)
		}
	}
	court, err := s.loadCourt(ctx, tx, newCourtID)
	if err != nil {
		return nil, 0, err
	}
	if court.VenueID != b.VenueID {
		return nil, 0, ce("COURT_VENUE_MISMATCH", "目标场地不属于该场馆", ErrValidation)
	}
	if court.Status != "active" {
		return nil, 0, ce("COURT_INACTIVE", "目标场地当前不可用", ErrMaintenance)
	}
	// 重新校验目标时段可用（方案生成后可能被抢占/新增维护）
	var taken bool
	if err := tx.QueryRow(ctx,
		`select exists(select 1 from bookings where court_id=$1 and id<>$2
		  and status in ('held','paid','warming','in_use')
		  and tstzrange(start_time,end_time,'[)') && tstzrange($3,$4,'[)'))`,
		newCourtID, b.ID, start, end).Scan(&taken); err != nil {
		return nil, 0, err
	}
	if taken {
		return nil, 0, ce("SLOT_TAKEN", "目标场地时段刚被预约，请重新选择候选场地或退款", ErrConflict)
	}
	blocked, err := courtBlockedByMaintenance(ctx, tx, newCourtID, start, end)
	if err != nil {
		return nil, 0, err
	}
	if blocked {
		return nil, 0, ce("MAINTENANCE", "目标场地该时段处于维护锁定/待验收状态，请选择其他方案", ErrMaintenance)
	}
	// 营业 / 加场时间
	extra, err := loadExtraSlots(ctx, tx, venue.ID, newCourtID, start.Format("2006-01-02"))
	if err != nil {
		return nil, 0, err
	}
	for mm, durMin := 0, int(end.Sub(start).Minutes()); mm < durMin; mm++ {
		if !courtOpenAt(venue, extra, start.Add(time.Duration(mm)*time.Minute)) {
			return nil, 0, ce("OUTSIDE_HOURS", "目标时段不在营业或临时加场范围内", ErrOutsideHours)
		}
	}

	// 差价：目标场地同时段场地费 - 原场地费；为正由场馆承担，用户不补差价
	windows, err := s.peakWindows(ctx, tx, venue.ID)
	if err != nil {
		return nil, 0, err
	}
	_, _, newFee := courtFeeForSlot(effectiveRate(venue, court), venue.PeakMultiplier, windows, start, end)
	subsidy := newFee - b.CourtFeeCents
	if subsidy < 0 {
		subsidy = 0
	}

	// 旧订单 -> relocated（终态，释放 GiST 占位，防止被误判用户占场）
	if _, err := tx.Exec(ctx,
		`update bookings set status='relocated',light_off_at=coalesce(light_off_at,$2),
		 version=version+1,updated_at=now() where id=$1`, b.ID, now); err != nil {
		return nil, 0, err
	}
	// 灯控权限同步：旧场地灯控关闭留痕
	if b.LightOffAt == nil {
		if _, err := tx.Exec(ctx,
			`insert into light_records(booking_id,court_id,action,command_source,success,detail,occurred_at)
			 values($1,$2,'off','system',true,'换场同步：旧场地灯控关闭',$3)`,
			b.ID, b.CourtID, now); err != nil {
			return nil, 0, err
		}
	}

	// 新订单：继承原价与已支付金额（用户不补差价）
	capacity := court.Capacity
	if capacity == 0 {
		capacity = venue.DefaultCourtCapacity
	}
	entryCode, err := genEntryCode(ctx, tx)
	if err != nil {
		return nil, 0, err
	}
	note := fmt.Sprintf("场馆维护换场（%s），由订单 #%d 迁移；差价 %d 分由场馆承担", offer.Reason, b.ID, subsidy)
	if isPostpone {
		note = fmt.Sprintf("场馆维护顺延（%s），由订单 #%d 迁移；差价 %d 分由场馆承担", offer.Reason, b.ID, subsidy)
	}
	var newID int64
	err = tx.QueryRow(ctx, `insert into bookings(
		booking_no,venue_id,court_id,member_id,member_type_snap,member_name_snap,
		party_size,capacity_snapshot,start_time,end_time,
		rent_racket,racket_count,buy_shuttle,shuttle_count,
		court_fee_cents,addon_cents,discount_cents,benefit_cents,benefit_minutes,
		benefit_monthly_minutes,benefit_grant_minutes,
		payable_cents,paid_cents,status,entry_code,paid_at,note,created_source,updated_at)
		values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,'paid',$24,$25,$26,'system',now())
		returning id`,
		genBookingNo(now), b.VenueID, newCourtID, b.MemberID, b.MemberTypeSnap, b.MemberNameSnap,
		b.PartySize, capacity, start, end,
		b.RentRacket, b.RacketCount, b.BuyShuttle, b.ShuttleCount,
		b.CourtFeeCents, b.AddonCents, b.DiscountCents, b.BenefitCents, b.BenefitMinutes,
		b.BenefitMonthlyMinutes, b.BenefitGrantMinutes,
		b.PayableCents, b.PaidCents, entryCode, b.PaidAt, note).Scan(&newID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23P01" {
			return nil, 0, ce("SLOT_TAKEN", "目标场地时段刚被预约，请重新选择候选场地或退款", ErrConflict)
		}
		return nil, 0, err
	}
	// 款项迁移留痕（不二次收费）
	if b.PaidCents > 0 {
		if _, err := tx.Exec(ctx,
			`insert into payments(booking_id,kind,amount_cents,method,status,transaction_no,reason,created_source)
			 values($1,'payment',$2,'transfer','succeeded',$3,$4,'system')`,
			newID, b.PaidCents, fmt.Sprintf("TR%d-%06d", newID, random6()),
			fmt.Sprintf("场馆维护换场迁移：原订单 #%d 已支付金额转入；差价 %d 分由场馆承担", b.ID, subsidy)); err != nil {
			return nil, 0, err
		}
	}
	// 已核销补偿券的归属订单改指新订单
	if _, err := tx.Exec(ctx,
		`update benefit_grants set used_booking_id=$2 where used_booking_id=$1`, b.ID, newID); err != nil {
		return nil, 0, err
	}
	// 旧订单指向新订单
	if _, err := tx.Exec(ctx, `update bookings set superseded_by=$2 where id=$1`, b.ID, newID); err != nil {
		return nil, 0, err
	}

	// 闸机/灯控权限同步到新场地：新入场码生效；若已进预热窗口，新场地直接预热
	syncDetail := map[string]any{
		"old_court_id": b.CourtID, "new_court_id": newCourtID,
		"entry_code": entryCode, "gate": "新入场码已生效，旧码已作废",
		"light_old": "旧场地灯控已关闭", "venue_subsidy_cents": subsidy,
	}
	if warmFrom := start.Add(-time.Duration(venue.LightWarmupMinutes) * time.Minute); !now.Before(warmFrom) && now.Before(start) {
		if _, err := tx.Exec(ctx,
			`update bookings set status='warming',warmup_at=$2,version=version+1,updated_at=now() where id=$1`,
			newID, now); err != nil {
			return nil, 0, err
		}
		if _, err := tx.Exec(ctx,
			`insert into light_records(booking_id,court_id,action,command_source,success,detail,occurred_at)
			 values($1,$2,'warmup','system',true,'换场同步：新场地灯控预热',$3)`,
			newID, newCourtID, now); err != nil {
			return nil, 0, err
		}
		syncDetail["light_new"] = "新场地已同步预热"
	} else {
		syncDetail["light_new"] = "到预热点由调度器自动预热"
	}
	if err := emitEvent(ctx, tx, "relocate.permission_synced", "system", &newID, &b.VenueID, &newCourtID, nil, syncDetail); err != nil {
		return nil, 0, err
	}
	if err := emitEvent(ctx, tx, "booking.relocated", "member", &b.ID, &b.VenueID, &b.CourtID, &b.MemberID, map[string]any{
		"offer_id": offer.ID, "new_booking_id": newID, "new_court_id": newCourtID,
		"new_start": start, "new_end": end, "venue_subsidy_cents": subsidy, "kind": resolveStatus,
	}); err != nil {
		return nil, 0, err
	}
	if err := emitEvent(ctx, tx, "booking.created_from_relocation", "system", &newID, &b.VenueID, &newCourtID, nil, map[string]any{
		"old_booking_id": b.ID, "offer_id": offer.ID, "venue_subsidy_cents": subsidy,
	}); err != nil {
		return nil, 0, err
	}

	// 方案闭环
	resolution, _ := json.Marshal(map[string]any{
		"action": resolveStatus, "new_booking_id": newID, "new_court_id": newCourtID,
		"new_start": start, "new_end": end, "venue_subsidy_cents": subsidy,
	})
	if _, err := tx.Exec(ctx,
		`update relocation_offers set status=$2,resolution=$3::jsonb,new_booking_id=$4,
		 venue_subsidy_cents=$5,decided_by=$6,decided_at=$7 where id=$1`,
		offer.ID, resolveStatus, string(resolution), newID, subsidy, b.MemberID, now); err != nil {
		return nil, 0, err
	}
	nb, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1`, newID))
	if err != nil {
		return nil, 0, err
	}
	return nb, subsidy, nil
}

// ---------- 前台通知（清场提醒等） ----------

func (s *Service) ListNotifications(ctx context.Context, venueID int64, status string) ([]store.OpsNotification, error) {
	q := `select id,venue_id,court_id,booking_id,type,message,status,created_at,acked_by,acked_at
		from ops_notifications`
	args := []any{}
	idx := 0
	cond := ""
	if venueID > 0 {
		idx++
		cond += fmt.Sprintf(` and venue_id=$%d`, idx)
		args = append(args, venueID)
	}
	if status != "" {
		idx++
		cond += fmt.Sprintf(` and status=$%d`, idx)
		args = append(args, status)
	}
	if cond != "" {
		q += ` where ` + cond[4:]
	}
	q += ` order by id desc limit 200`
	rows, err := s.pool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.OpsNotification{}
	for rows.Next() {
		var n store.OpsNotification
		if err := rows.Scan(&n.ID, &n.VenueID, &n.CourtID, &n.BookingID, &n.Type, &n.Message,
			&n.Status, &n.CreatedAt, &n.AckedBy, &n.AckedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// AckNotification 前台确认处理（如旧场地清场完成）
func (s *Service) AckNotification(ctx context.Context, id, staffID int64) (*store.OpsNotification, error) {
	var n store.OpsNotification
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`update ops_notifications set status='done',acked_by=$2,acked_at=now()
			 where id=$1 and status='pending'
			 returning id,venue_id,court_id,booking_id,type,message,status,created_at,acked_by,acked_at`,
			id, staffID).
			Scan(&n.ID, &n.VenueID, &n.CourtID, &n.BookingID, &n.Type, &n.Message,
				&n.Status, &n.CreatedAt, &n.AckedBy, &n.AckedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ce("NOT_FOUND", "通知不存在或已处理", ErrNotFound)
		}
		if err != nil {
			return err
		}
		return emitEvent(ctx, tx, "notification.acked", "front", n.BookingID, &n.VenueID, n.CourtID, &staffID,
			map[string]any{"notification_id": id, "type": n.Type})
	})
	if err != nil {
		return nil, err
	}
	return &n, nil
}
