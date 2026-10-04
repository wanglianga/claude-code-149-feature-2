package service

import (
	"context"
	"errors"
	"time"

	"gymbadminton/internal/auth"
	"gymbadminton/internal/store"

	"github.com/jackc/pgx/v5"
)

// ---------- 注册 / 登录 ----------

func (s *Service) RegisterMember(ctx context.Context, phone, name, password, memberType string) (int64, error) {
	if len(phone) < 6 {
		return 0, ce("BAD_PHONE", "手机号格式不正确", ErrValidation)
	}
	if password == "" {
		password = "123456" // 演示环境兜底
	}
	if memberType == "" {
		memberType = "normal"
	}
	if memberType != "normal" && memberType != "silver" && memberType != "gold" {
		return 0, ce("BAD_TYPE", "会员类型非法", ErrValidation)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.pool().QueryRow(ctx,
		`insert into members(phone,name,password_hash,member_type) values($1,$2,$3,$4)
		 on conflict (phone) do nothing returning id`, phone, name, hash, memberType).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ce("PHONE_EXISTS", "手机号已注册", ErrConflict)
	}
	return id, err
}

type LoginResult struct {
	Token  string        `json:"token"`
	Member *store.Member `json:"member,omitempty"`
	Staff  *store.Staff  `json:"staff,omitempty"`
}

func (s *Service) LoginMember(ctx context.Context, phone, password string) (*LoginResult, error) {
	var m store.Member
	err := s.pool().QueryRow(ctx,
		`select id,phone,name,coalesce(password_hash,''),member_type,no_show_count,status
		 from members where phone=$1`, phone).
		Scan(&m.ID, &m.Phone, &m.Name, &m.PasswordHash, &m.MemberType, &m.NoShowCount, &m.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ce("BAD_CREDENTIALS", "手机号或密码错误", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if m.PasswordHash == "" {
		// 种子账号可能无密码，允许演示密码
		if password != "123456" {
			return nil, ce("BAD_CREDENTIALS", "手机号或密码错误", ErrNotFound)
		}
	} else if !auth.CheckPassword(m.PasswordHash, password) {
		return nil, ce("BAD_CREDENTIALS", "手机号或密码错误", ErrNotFound)
	}
	token := auth.Issue(s.secret(), auth.Claims{Sub: m.ID, Kind: "member", Name: m.Name})
	return &LoginResult{Token: token, Member: &m}, nil
}

func (s *Service) LoginStaff(ctx context.Context, username, password string) (*LoginResult, error) {
	var st store.Staff
	err := s.pool().QueryRow(ctx,
		`select id,username,password_hash,name,role,venue_id from staff where username=$1`, username).
		Scan(&st.ID, &st.Username, &st.PasswordHash, &st.Name, &st.Role, &st.VenueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ce("BAD_CREDENTIALS", "用户名或密码错误", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if !auth.CheckPassword(st.PasswordHash, password) {
		return nil, ce("BAD_CREDENTIALS", "用户名或密码错误", ErrNotFound)
	}
	token := auth.Issue(s.secret(), auth.Claims{Sub: st.ID, Kind: "staff", Role: st.Role, VenueID: deref(st.VenueID), Name: st.Name})
	return &LoginResult{Token: token, Staff: &st}, nil
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// secret 由 Server 注入；用字段保存避免配置环依赖
func (s *Service) secret() string { return s.TokenSecret }

// ---------- 场馆 / 场地 / 排期 ----------

func (s *Service) ListVenues(ctx context.Context) ([]store.Venue, error) {
	rows, err := s.pool().Query(ctx, `select
		id,name,address,opens_at::text,closes_at::text,
		price_cents_per_hour,peak_multiplier,overtime_multiplier,
		racket_fee_cents,shuttle_fee_cents,default_court_capacity,overlimit_fee_cents,
		payment_timeout_seconds,light_warmup_minutes,checkin_window_minutes,
		late_grace_minutes,noshow_judge_minutes,idle_light_off_minutes,
		max_auto_overtime_minutes,member_only_peak_release_hours,noshow_penalty_count
		from venues order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Venue
	for rows.Next() {
		var v store.Venue
		if err := rows.Scan(&v.ID, &v.Name, &v.Address, &v.OpensAt, &v.ClosesAt,
			&v.PriceCentsPerHour, &v.PeakMultiplier, &v.OvertimeMultiplier,
			&v.RacketFeeCents, &v.ShuttleFeeCents, &v.DefaultCourtCapacity, &v.OverlimitFeeCents,
			&v.PaymentTimeoutSeconds, &v.LightWarmupMinutes, &v.CheckinWindowMinutes,
			&v.LateGraceMinutes, &v.NoShowJudgeMinutes, &v.IdleLightOffMinutes,
			&v.MaxAutoOvertimeMinutes, &v.MemberOnlyPeakReleaseHours, &v.NoShowPenaltyCount); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Service) ListCourts(ctx context.Context, venueID int64) ([]store.Court, error) {
	rows, err := s.pool().Query(ctx,
		`select id,venue_id,court_no,coalesce(capacity,0),status,coalesce(price_cents_per_hour,0) from courts where venue_id=$1 order by court_no`, venueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Court
	for rows.Next() {
		var c store.Court
		if err := rows.Scan(&c.ID, &c.VenueID, &c.CourtNo, &c.Capacity, &c.Status, &c.PriceCentsPerHour); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type SlotAvailability struct {
	StartTime    time.Time `json:"start_time"`
	EndTime      time.Time `json:"end_time"`
	Status       string    `json:"status"` // free / booked / maintenance / closed
	IsPeak       bool      `json:"is_peak"`
	BookingID    *int64    `json:"booking_id,omitempty"`
	BookingNo    string    `json:"booking_no,omitempty"`
	BookingState string    `json:"booking_state,omitempty"`
}

type CourtSchedule struct {
	Court store.Court        `json:"court"`
	Slots []SlotAvailability `json:"slots"`
}

// CourtDaySchedule 返回某场地某天 30 分钟粒度的占用情况（预约/维护/闭馆/高峰）
func (s *Service) CourtDaySchedule(ctx context.Context, venueID, courtID int64, day time.Time) (*CourtSchedule, error) {
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	res := &CourtSchedule{}
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		venue, err := s.loadVenue(ctx, tx, venueID)
		if err != nil {
			return err
		}
		court, err := s.loadCourt(ctx, tx, courtID)
		if err != nil {
			return err
		}
		res.Court = *court
		windows, err := s.peakWindows(ctx, tx, venueID)
		if err != nil {
			return err
		}
		extra, err := loadExtraSlots(ctx, tx, venueID, courtID, day.Format("2006-01-02"))
		if err != nil {
			return err
		}

		rows, err := tx.Query(ctx,
			`select id,booking_no,start_time,end_time,status from bookings
			 where court_id=$1 and start_time >= $2::timestamptz and start_time < $2::timestamptz + interval '1 day'
			 order by start_time`, courtID, day)
		if err != nil {
			return err
		}
		type bk struct {
			id         int64
			no         string
			start, end time.Time
			status     string
		}
		var bks []bk
		for rows.Next() {
			var b bk
			if err := rows.Scan(&b.id, &b.no, &b.start, &b.end, &b.status); err != nil {
				rows.Close()
				return err
			}
			bks = append(bks, b)
		}
		rows.Close()

		type mw struct {
			start, end time.Time
			status     string
		}
		mrows, err := tx.Query(ctx,
			`select start_time,end_time,status from maintenance_windows
			 where court_id=$1 and status in ('active','repair_done')
			   and tstzrange(start_time,end_time,'[)') && tstzrange($2::timestamptz,($2::timestamptz + interval '1 day'),'[)')`,
			courtID, day)
		if err != nil {
			return err
		}
		var mws []mw
		for mrows.Next() {
			var m mw
			if err := mrows.Scan(&m.start, &m.end, &m.status); err != nil {
				mrows.Close()
				return err
			}
			mws = append(mws, m)
		}
		mrows.Close()

		open := clockHM(venue.OpensAt)
		closeMin := clockHM(venue.ClosesAt)
		startMinutes := map[int]bool{}
		var ordered []int
		addRange := func(a, b int) {
			for hm := a; hm < b; hm += 30 {
				if !startMinutes[hm] {
					startMinutes[hm] = true
					ordered = append(ordered, hm)
				}
			}
		}
		addRange(open, closeMin)
		for _, e := range extra {
			addRange(clockHM(e.OpensAt), clockHM(e.ClosesAt))
		}
		sortInts(ordered)

		for _, hm := range ordered {
			st := day.Add(time.Duration(hm) * time.Minute)
			en := st.Add(30 * time.Minute)
			slot := SlotAvailability{StartTime: st, EndTime: en, Status: "free", IsPeak: isPeakAt(windows, st)}
			if !courtOpenAt(venue, extra, st) {
				slot.Status = "closed"
			}
			for _, m := range mws {
				if st.Before(m.end) && en.After(m.start) {
					slot.Status = "maintenance"
				}
			}
			for _, b := range bks {
				if st.Before(b.end) && en.After(b.start) {
					// 换场旧单/待换场单不占位（待换场场地由维护格体现）
					if b.status == "cancelled" || b.status == "expired" ||
						b.status == "moved" || b.status == "relocation_pending" {
						continue
					}
					slot.Status = "booked"
					id := b.id
					slot.BookingID = &id
					slot.BookingNo = b.no
					slot.BookingState = b.status
				}
			}
			res.Slots = append(res.Slots, slot)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ---------- 预约查询 ----------

func (s *Service) ListMemberBookings(ctx context.Context, memberID int64, limit int) ([]*store.Booking, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool().Query(ctx,
		`select `+bookingColumns+` from bookings where member_id=$1 order by start_time desc limit $2`, memberID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Booking
	for rows.Next() {
		b, err := scanBooking(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Service) ListVenueBookings(ctx context.Context, venueID int64, status string, day *time.Time, limit int) ([]*store.Booking, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `select ` + bookingColumns + ` from bookings where venue_id=$1`
	args := []any{venueID}
	idx := 2
	if status != "" {
		q += ` and status=$` + itoaIdx(idx)
		args = append(args, status)
		idx++
	}
	if day != nil {
		q += ` and start_time >= $` + itoaIdx(idx) + ` and start_time < $` + itoaIdx(idx+1)
		d := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
		args = append(args, d, d.Add(24*time.Hour))
		idx += 2
	}
	q += ` order by start_time asc limit $` + itoaIdx(idx)
	args = append(args, limit)
	rows, err := s.pool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Booking
	for rows.Next() {
		b, err := scanBooking(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func itoaIdx(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

// ---------- 报价（下单前预览） ----------

type QuoteInput struct {
	VenueID      int64
	CourtID      int64 // 可选：场地差异化定价
	MemberID     int64
	StartTime    time.Time
	EndTime      time.Time
	RacketCount  int
	ShuttleCount int
}

type QuoteOutput struct {
	priceBreakdown
	MemberType string `json:"member_type"`
}

func (s *Service) Quote(ctx context.Context, in QuoteInput) (*QuoteOutput, error) {
	in.StartTime = in.StartTime.UTC().Truncate(time.Minute)
	in.EndTime = in.EndTime.UTC().Truncate(time.Minute)
	out := &QuoteOutput{}
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		venue, err := s.loadVenue(ctx, tx, in.VenueID)
		if err != nil {
			return err
		}
		var courtPrice int64
		if in.CourtID > 0 {
			court, cerr := s.loadCourt(ctx, tx, in.CourtID)
			if cerr != nil {
				return cerr
			}
			courtPrice = court.PriceCentsPerHour
		}
		var m store.Member
		err = tx.QueryRow(ctx,
			`select id,phone,name,coalesce(password_hash,''),member_type,no_show_count,status
			 from members where id=$1`, in.MemberID).
			Scan(&m.ID, &m.Phone, &m.Name, &m.PasswordHash, &m.MemberType, &m.NoShowCount, &m.Status)
		if err != nil {
			return err
		}
		out.MemberType = m.MemberType
		policy, err := s.getMemberPolicy(ctx, tx, venue.ID, m.MemberType)
		if err != nil {
			return err
		}
		windows, err := s.peakWindows(ctx, tx, venue.ID)
		if err != nil {
			return err
		}
		avail, _, _, err := availableBenefitMinutes(ctx, tx, m.ID, venue.ID,
			in.StartTime.Format("2006-01"), policy.FreeHoursPerMonth)
		if err != nil {
			return err
		}
		out.priceBreakdown = quote(priceInput{
			venue: venue, courtPriceCents: courtPrice, policy: policy, peakWindows: windows,
			start: in.StartTime, end: in.EndTime,
			racketCount: in.RacketCount, shuttleCount: in.ShuttleCount,
			availableBenefitM: avail,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
