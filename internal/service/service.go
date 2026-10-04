package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gymbadminton/internal/store"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrValidation   = errors.New("validation failed")
	ErrIllegalState = errors.New("illegal booking state")
	ErrMaintenance  = errors.New("court under maintenance")
	ErrOutsideHours = errors.New("outside booking hours")
	ErrMemberOnly   = errors.New("peak slot is member-only during priority window")
	ErrAdvanceLimit = errors.New("exceeds member advance-booking limit")
	ErrUnpaid       = errors.New("booking not paid")
	ErrTooEarly     = errors.New("outside check-in window")
)

// CodeError 携带稳定错误码，便于 API 层映射 HTTP 状态与中文提示
type CodeError struct {
	Code    string
	Message string
	Err     error
}

func (e *CodeError) Error() string { return e.Message }
func (e *CodeError) Unwrap() error { return e.Err }

func ce(code, msg string, err error) *CodeError {
	return &CodeError{Code: code, Message: msg, Err: err}
}

type Service struct {
	DB          *store.DB
	Locker      *store.Locker
	Clock       *Clock
	TokenSecret string
}

func New(db *store.DB, locker *store.Locker, clock *Clock, secret string) *Service {
	return &Service{DB: db, Locker: locker, Clock: clock, TokenSecret: secret}
}

func (s *Service) pool() *pgxpool.Pool { return s.DB.Pool }

func (s *Service) now(ctx context.Context) time.Time { return s.Clock.Now(ctx) }

// Now 暴露统一时钟（API 层虚拟时钟查询用）
func (s *Service) Now(ctx context.Context) time.Time { return s.Clock.Now(ctx) }

// ---------- 基础读取 ----------

func (s *Service) loadVenue(ctx context.Context, q pgx.Tx, id int64) (*store.Venue, error) {
	row := q.QueryRow(ctx, `select
		id,name,address,opens_at::text,closes_at::text,
		price_cents_per_hour,peak_multiplier,overtime_multiplier,
		racket_fee_cents,shuttle_fee_cents,default_court_capacity,overlimit_fee_cents,
		payment_timeout_seconds,light_warmup_minutes,checkin_window_minutes,
		late_grace_minutes,noshow_judge_minutes,idle_light_off_minutes,
		max_auto_overtime_minutes,member_only_peak_release_hours,noshow_penalty_count
		from venues where id=$1`, id)
	var v store.Venue
	err := row.Scan(&v.ID, &v.Name, &v.Address, &v.OpensAt, &v.ClosesAt,
		&v.PriceCentsPerHour, &v.PeakMultiplier, &v.OvertimeMultiplier,
		&v.RacketFeeCents, &v.ShuttleFeeCents, &v.DefaultCourtCapacity, &v.OverlimitFeeCents,
		&v.PaymentTimeoutSeconds, &v.LightWarmupMinutes, &v.CheckinWindowMinutes,
		&v.LateGraceMinutes, &v.NoShowJudgeMinutes, &v.IdleLightOffMinutes,
		&v.MaxAutoOvertimeMinutes, &v.MemberOnlyPeakReleaseHours, &v.NoShowPenaltyCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &v, err
}

func (s *Service) loadCourt(ctx context.Context, q pgx.Tx, id int64) (*store.Court, error) {
	row := q.QueryRow(ctx,
		`select id, venue_id, court_no, coalesce(capacity,0), status, coalesce(price_cents_per_hour,0) from courts where id=$1`, id)
	var c store.Court
	err := row.Scan(&c.ID, &c.VenueID, &c.CourtNo, &c.Capacity, &c.Status, &c.PriceCentsPerHour)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (s *Service) getMemberPolicy(ctx context.Context, q pgx.Tx, venueID int64, memberType string) (store.MemberPolicy, error) {
	p := store.MemberPolicy{VenueID: venueID, MemberType: memberType, DiscountPercent: 100}
	err := q.QueryRow(ctx,
		`select discount_percent, free_hours_per_month, max_advance_hours
		 from member_type_policies where venue_id=$1 and member_type=$2`,
		venueID, memberType).Scan(&p.DiscountPercent, &p.FreeHoursPerMonth, &p.MaxAdvanceHours)
	if errors.Is(err, pgx.ErrNoRows) {
		// 默认策略：普通会员全价、无免费时长、可提前 24 小时
		return store.MemberPolicy{VenueID: venueID, MemberType: memberType,
			DiscountPercent: 100, MaxAdvanceHours: 24}, nil
	}
	return p, err
}

func (s *Service) peakWindows(ctx context.Context, q pgx.Tx, venueID int64) ([]store.PeakWindow, error) {
	rows, err := q.Query(ctx,
		`select id, venue_id, weekday, start_time::text, end_time::text
		 from peak_windows where venue_id=$1 order by weekday, start_time`, venueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.PeakWindow
	for rows.Next() {
		var w store.PeakWindow
		if err := rows.Scan(&w.ID, &w.VenueID, &w.Weekday, &w.StartTime, &w.EndTime); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// bookingColumns 全字段读取顺序，须与 scanBooking 一致
const bookingColumns = `id,booking_no,venue_id,court_id,member_id,member_type_snap,member_name_snap,
	party_size,capacity_snapshot,start_time,end_time,
	rent_racket,racket_count,buy_shuttle,shuttle_count,
	court_fee_cents,addon_cents,discount_cents,benefit_cents,benefit_minutes,benefit_monthly_minutes,benefit_grant_minutes,overlimit_cents,
	payable_cents,paid_cents,refunded_cents,overtime_cents,late_adjust_cents,
	status,entry_code,expires_at,paid_at,
	first_gate_at,gate_waiting_at,front_checkin_at,first_presence_at,
	warmup_at,light_on_at,light_off_at,effective_start,actual_end,
	overtime_minutes,late_start_minutes,comp_bonus_minutes,overlimit_persons,
	verdict,judged_at,judge_source,note,created_source,
	relocation_status,relocation_maintenance_id,original_court_id,
	relocated_from_booking_id,relocated_to_booking_id,relocation_due_at,venue_borne_cents,vacate_reminded_at,
	version,created_at,updated_at`

func scanBooking(row pgx.Row) (*store.Booking, error) {
	var b store.Booking
	err := row.Scan(
		&b.ID, &b.BookingNo, &b.VenueID, &b.CourtID, &b.MemberID, &b.MemberTypeSnap, &b.MemberNameSnap,
		&b.PartySize, &b.CapacitySnapshot, &b.StartTime, &b.EndTime,
		&b.RentRacket, &b.RacketCount, &b.BuyShuttle, &b.ShuttleCount,
		&b.CourtFeeCents, &b.AddonCents, &b.DiscountCents, &b.BenefitCents, &b.BenefitMinutes,
		&b.BenefitMonthlyMinutes, &b.BenefitGrantMinutes, &b.OverlimitCents,
		&b.PayableCents, &b.PaidCents, &b.RefundedCents, &b.OvertimeCents, &b.LateAdjustCents,
		&b.Status, &b.EntryCode, &b.ExpiresAt, &b.PaidAt,
		&b.FirstGateAt, &b.GateWaitingAt, &b.FrontCheckinAt, &b.FirstPresenceAt,
		&b.WarmupAt, &b.LightOnAt, &b.LightOffAt, &b.EffectiveStart, &b.ActualEnd,
		&b.OvertimeMinutes, &b.LateStartMinutes, &b.CompBonusMinutes, &b.OverlimitPersons,
		&b.Verdict, &b.JudgedAt, &b.JudgeSource, &b.Note, &b.CreatedSource,
		&b.RelocationStatus, &b.RelocationMaintenanceID, &b.OriginalCourtID,
		&b.RelocatedFromBookingID, &b.RelocatedToBookingID, &b.RelocationDueAt, &b.VenueBorneCents, &b.VacateRemindedAt,
		&b.Version, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Service) GetBooking(ctx context.Context, id int64) (*store.Booking, error) {
	return scanBooking(s.pool().QueryRow(ctx,
		`select `+bookingColumns+` from bookings where id=$1`, id))
}

func (s *Service) GetBookingByNo(ctx context.Context, no string) (*store.Booking, error) {
	return scanBooking(s.pool().QueryRow(ctx,
		`select `+bookingColumns+` from bookings where booking_no=$1`, no))
}

// ---------- 事件留痕 ----------

func emitEvent(ctx context.Context, q pgx.Tx, eventType, source string, bookingID, venueID, courtID, actorID *int64, payload any) error {
	raw, _ := json.Marshal(payload)
	_, err := q.Exec(ctx,
		`insert into event_log(booking_id,venue_id,court_id,event_type,source,actor_id,payload)
		 values($1,$2,$3,$4,$5,$6,coalesce(nullif($7,'')::jsonb,'{}'::jsonb))`,
		bookingID, venueID, courtID, eventType, source, actorID, string(raw))
	return err
}

func emitEventDB(ctx context.Context, pool *pgxpool.Pool, eventType, source string, bookingID, venueID, courtID, actorID *int64, payload any) {
	raw, _ := json.Marshal(payload)
	_, _ = pool.Exec(ctx,
		`insert into event_log(booking_id,venue_id,court_id,event_type,source,actor_id,payload)
		 values($1,$2,$3,$4,$5,$6,coalesce(nullif($7,'')::jsonb,'{}'::jsonb))`,
		bookingID, venueID, courtID, eventType, source, actorID, string(raw))
}

func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// ---------- 时间 / 营业窗口 ----------

// courtOpenAt 判断 court 在 t 时刻是否允许预约（营业时段或临时加场；维护另算）
func courtOpenAt(venue *store.Venue, extra []store.ExtraSlot, t time.Time) bool {
	hm := t.Hour()*60 + t.Minute()
	open := clockHM(venue.OpensAt)
	close := clockHM(venue.ClosesAt)
	inRegular := hm >= open && hm < close
	if inRegular {
		return true
	}
	day := t.Format("2006-01-02")
	for _, e := range extra {
		if e.SlotDate == day && hm >= clockHM(e.OpensAt) && hm < clockHM(e.ClosesAt) {
			return true
		}
	}
	return false
}

func loadExtraSlots(ctx context.Context, q pgx.Tx, venueID, courtID int64, day string) ([]store.ExtraSlot, error) {
	rows, err := q.Query(ctx,
		`select id,venue_id,court_id,slot_date::text,opens_at::text,closes_at::text,note
		 from extra_slots where venue_id=$1 and slot_date=$2::date
		 and (court_id is null or court_id=$3)`,
		venueID, day, courtID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ExtraSlot
	for rows.Next() {
		var e store.ExtraSlot
		if err := rows.Scan(&e.ID, &e.VenueID, &e.CourtID, &e.SlotDate, &e.OpensAt, &e.ClosesAt, &e.Note); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// activeMaintenanceNow 返回 t 时刻生效中的维护（维修完工待验收仍视为锁定）
func activeMaintenanceAt(ctx context.Context, q pgx.Tx, courtID int64, t time.Time) (*store.Maintenance, error) {
	row := q.QueryRow(ctx,
		`select `+maintenanceColumns+`
		 from maintenance_windows
		 where court_id=$1 and status in ('active','repair_done')
		   and tstzrange(start_time,end_time,'[)') @> $2::timestamptz
		 limit 1`, courtID, t)
	var m store.Maintenance
	err := row.Scan(&m.ID, &m.VenueID, &m.CourtID, &m.StartTime, &m.EndTime, &m.Reason, &m.Status,
		&m.CreatedBy, &m.ReleasedBy, &m.ReleasedAt, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func fmtCents(c int64) string { return fmt.Sprintf("%.2f 元", float64(c)/100) }
