package service

import (
	"context"
	"fmt"
	"time"

	"gymbadminton/internal/store"

	"github.com/jackc/pgx/v5"
)

// ---------- 退订 ----------

type CancellationQuote struct {
	RefundableCents int64   `json:"refundable_cents"`
	RefundPercent   int     `json:"refund_percent"`
	IsPeak          bool    `json:"is_peak"`
	HoursBefore     float64 `json:"hours_before"`
}

// QuoteCancel 按场馆退订规则（平峰/高峰分档）计算退款比例
func (s *Service) QuoteCancel(ctx context.Context, bookingID int64) (*CancellationQuote, error) {
	q := &CancellationQuote{}
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1`, bookingID))
		if err != nil {
			return err
		}
		windows, err := s.peakWindows(ctx, tx, b.VenueID)
		if err != nil {
			return err
		}
		now := s.now(ctx)
		q.IsPeak = isPeakAt(windows, b.StartTime)
		q.HoursBefore = b.StartTime.Sub(now).Hours()
		if q.HoursBefore < 0 {
			q.HoursBefore = 0
		}
		if err := tx.QueryRow(ctx,
			`select coalesce(max(refund_percent),0) from cancellation_rules
			 where venue_id=$1 and is_peak=$2 and min_hours_before <= $3`,
			b.VenueID, q.IsPeak, q.HoursBefore).Scan(&q.RefundPercent); err != nil {
			return err
		}
		q.RefundableCents = b.PaidCents * int64(q.RefundPercent) / 100
		return nil
	})
	if err != nil {
		return nil, err
	}
	return q, nil
}

// CancelBooking 用户/运营退订。held 直接取消并释放权益；
// paid/warming 按退订规则退款（warming 已预热也可退，单独记录灯控关闭）。
func (s *Service) CancelBooking(ctx context.Context, bookingID int64, source string, actorID *int64, reason string) (*store.Booking, *CancellationQuote, error) {
	if source == "" {
		source = "member"
	}
	now := s.now(ctx)
	var quote CancellationQuote
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, bookingID))
		if err != nil {
			return err
		}
		if b.Status != "held" && b.Status != "paid" && b.Status != "warming" {
			return ce("NOT_CANCELLABLE", "当前状态不可退订（"+b.Status+"）", ErrIllegalState)
		}
		windows, err := s.peakWindows(ctx, tx, b.VenueID)
		if err != nil {
			return err
		}
		peak := isPeakAt(windows, b.StartTime)
		hoursBefore := b.StartTime.Sub(now).Hours()
		if hoursBefore < 0 {
			hoursBefore = 0
		}
		var percent int
		if err := tx.QueryRow(ctx,
			`select coalesce(max(refund_percent),0) from cancellation_rules
			 where venue_id=$1 and is_peak=$2 and min_hours_before <= $3`,
			b.VenueID, peak, hoursBefore).Scan(&percent); err != nil {
			return err
		}
		refund := int64(0)
		if b.Status != "held" {
			refund = b.PaidCents * int64(percent) / 100
		}
		quote = CancellationQuote{RefundableCents: refund, RefundPercent: percent, IsPeak: peak, HoursBefore: hoursBefore}

		if _, err := tx.Exec(ctx,
			`update bookings set status='cancelled',version=version+1,updated_at=now(),
			 refunded_cents=$2 where id=$1`, b.ID, refund); err != nil {
			return err
		}
		if err := releaseReservedBenefit(ctx, tx, b); err != nil {
			return err
		}
		if refund > 0 {
			if _, err := tx.Exec(ctx,
				`insert into payments(booking_id,kind,amount_cents,method,status,transaction_no,reason,created_source,created_by)
				 values($1,'refund',$2,'mock','succeeded',$3,$4,$5,$6)`,
				b.ID, refund, fmt.Sprintf("RF%d-%06d", b.ID, random6()),
				fmt.Sprintf("退订退款（%s，%d%%）", peakText(peak), percent), source, actorID); err != nil {
				return err
			}
		}
		if b.WarmupAt != nil && b.LightOffAt == nil {
			if _, err := tx.Exec(ctx,
				`insert into light_records(booking_id,court_id,action,command_source,success,detail,occurred_at)
				 values($1,$2,'off',$3,true,'cancelled',$4)`, b.ID, b.CourtID, source, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `update bookings set light_off_at=$2 where id=$1`, b.ID, now); err != nil {
				return err
			}
		}
		return emitEvent(ctx, tx, "booking.cancelled", source, &b.ID, &b.VenueID, &b.CourtID, actorID, map[string]any{
			"refund_cents": refund, "refund_percent": percent, "is_peak": peak, "reason": reason,
		})
	})
	if err != nil {
		return nil, nil, err
	}
	b, err := s.GetBooking(ctx, bookingID)
	return b, &quote, err
}

func peakText(peak bool) string {
	if peak {
		return "高峰时段"
	}
	return "平峰时段"
}

// ---------- 支付超时释放 ----------

// ExpireUnpaid 扫描 held 且已过支付时限的订单并释放场地与权益预占。
// 已进入 warming 的订单不处理（灯控预热是独立状态）。
func (s *Service) ExpireUnpaid(ctx context.Context) (int, error) {
	now := s.now(ctx)
	rows, err := s.pool().Query(ctx,
		`select id from bookings where status='held' and expires_at is not null and expires_at < $1
		 order by id limit 200`, now)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	expired := 0
	for _, id := range ids {
		if err := s.expireOne(ctx, id, "system"); err == nil {
			expired++
		}
	}
	return expired, nil
}

func (s *Service) expireOne(ctx context.Context, id int64, source string) error {
	return inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, id))
		if err != nil {
			return err
		}
		if b.Status != "held" {
			return nil
		}
		now := s.now(ctx)
		if b.ExpiresAt == nil || !now.After(*b.ExpiresAt) {
			return nil
		}
		if _, err := tx.Exec(ctx,
			`update bookings set status='expired',version=version+1,updated_at=now() where id=$1`, b.ID); err != nil {
			return err
		}
		if err := releaseReservedBenefit(ctx, tx, b); err != nil {
			return err
		}
		return emitEvent(ctx, tx, "booking.expired", source, &b.ID, &b.VenueID, &b.CourtID, nil,
			map[string]any{"reason": "payment_timeout"})
	})
}

// ---------- 自动灯控 ----------

// AutoLightControl 周期任务：
//  1. paid 到达预热点 -> warming（独立状态，超时任务不回收）；
//  2. warming 到开场时刻 -> 自动开灯 in_use；
//  3. 开灯后闲置超过 idle_light_off_minutes 仍无闸机/前台到场证据 -> 自动关灯省电，
//     灯已开但无人上场会留 light_records，爽约裁决仍以闸机/前台为准。
func (s *Service) AutoLightControl(ctx context.Context) (int, error) {
	now := s.now(ctx)
	processed := 0

	// 1) 预热（只对尚未开场的订单）
	rows, err := s.pool().Query(ctx, `
		select b.id from bookings b join venues v on v.id=b.venue_id
		where b.status='paid'
		  and b.start_time - (v.light_warmup_minutes::int * interval '1 minute') <= $1
		  and b.start_time > $1`, now)
	if err != nil {
		return 0, err
	}
	var warmIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		warmIDs = append(warmIDs, id)
	}
	rows.Close()
	for _, id := range warmIDs {
		if _, lerr := s.LightCommand(ctx, id, "warmup", "scheduler", nil, "auto warmup"); lerr == nil {
			processed++
		}
	}

	// 2) 到点自动开灯（仅开场后宽限窗口内，避免给已严重迟到的订单自动开灯）
	rows, err = s.pool().Query(ctx, `
		select b.id from bookings b join venues v on v.id=b.venue_id
		where b.status='warming' and b.start_time <= $1
		  and b.start_time + (v.late_grace_minutes::int * interval '1 minute') >= $1`, now)
	if err != nil {
		return processed, err
	}
	var onIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return processed, err
		}
		onIDs = append(onIDs, id)
	}
	rows.Close()
	for _, id := range onIDs {
		if _, lerr := s.LightCommand(ctx, id, "on", "scheduler", nil, "auto on at start"); lerr == nil {
			processed++
		}
	}

	// 3) 开灯闲置自动关灯
	rows, err = s.pool().Query(ctx, `
		select b.id from bookings b join venues v on v.id=b.venue_id
		where b.status='in_use' and b.light_on_at is not null and b.light_off_at is null
		  and b.first_presence_at is null
		  and b.light_on_at + (v.idle_light_off_minutes::int * interval '1 minute') < $1`, now)
	if err != nil {
		return processed, err
	}
	var offIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return processed, err
		}
		offIDs = append(offIDs, id)
	}
	rows.Close()
	for _, id := range offIDs {
		if _, lerr := s.LightCommand(ctx, id, "off", "scheduler", nil, "idle auto off"); lerr == nil {
			processed++
		}
	}
	return processed, nil
}

// ---------- 爽约 / 迟到 / 场馆原因 裁决 ----------

// presenceEvidence 到场证据：
//   - 闸机 allowed（正常放行）或 waiting（人已到场边，因维护/前一场延时等待）均算到场；
//   - 前台签到算到场；
//   - 灯控开启只有人工来源（device/front/operator）才作为“灯开了没人上场”的争议证据，
//     调度器自动开灯不构成用户到场，也不会掩盖爽约。
func presenceEvidence(ctx context.Context, tx pgx.Tx, bookingID int64) (gateOK, frontOK, lightOn bool, err error) {
	if err = tx.QueryRow(ctx,
		`select exists(select 1 from gate_records where booking_id=$1 and result in ('allowed','waiting'))`, bookingID).Scan(&gateOK); err != nil {
		return
	}
	if err = tx.QueryRow(ctx,
		`select exists(select 1 from front_records where booking_id=$1 and action='checkin')`, bookingID).Scan(&frontOK); err != nil {
		return
	}
	err = tx.QueryRow(ctx,
		`select coalesce(bool_or(action='on' and success and command_source in ('device','front','operator')),false)
		 from light_records where booking_id=$1`, bookingID).Scan(&lightOn)
	return
}

func venueSideEvidence(ctx context.Context, tx pgx.Tx, b *store.Booking) (maint, prevRun, hasRefund bool, err error) {
	if err = tx.QueryRow(ctx,
		`select exists(select 1 from maintenance_windows where court_id=$1 and status in ('active','repair_done')
		  and tstzrange(start_time,end_time,'[)') && tstzrange($2,$3,'[)'))`,
		b.CourtID, b.StartTime, b.EndTime).Scan(&maint); err != nil {
		return
	}
	if err = tx.QueryRow(ctx,
		`select exists(select 1 from bookings where court_id=$1 and id<>$2 and status='in_use'
		  and actual_end is null and end_time < $3)`,
		b.CourtID, b.ID, b.StartTime).Scan(&prevRun); err != nil {
		return
	}
	err = tx.QueryRow(ctx,
		`select exists(select 1 from refund_requests where booking_id=$1 and status='pending')`, b.ID).Scan(&hasRefund)
	return
}

// JudgeNoShows 开场后 noshow_judge_minutes 仍无闸机/前台到场证据的订单自动裁决
func (s *Service) JudgeNoShows(ctx context.Context) (int, error) {
	now := s.now(ctx)
	rows, err := s.pool().Query(ctx, `
		select b.id from bookings b join venues v on v.id=b.venue_id
		where b.verdict is null
		  and b.status not in ('relocation_pending','moved','venue_fault')
		  and (
		    (b.status in ('paid','warming')
		      and b.start_time + (v.noshow_judge_minutes::int * interval '1 minute') <= $1
		      and b.start_time > $1 - interval '6 hour')
		    or
		    (b.status='in_use' and b.first_presence_at is null
		      and b.start_time + (v.noshow_judge_minutes::int * interval '1 minute') <= $1)
		  )`, now)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	judged := 0
	for _, id := range ids {
		if err := s.judgeOne(ctx, id, "scheduler"); err == nil {
			judged++
		}
	}
	return judged, nil
}

type VerdictEvidence struct {
	Verdict      string `json:"verdict"`
	Source       string `json:"source"`
	HasGate      bool   `json:"has_gate_allowed"`
	HasFront     bool   `json:"has_front_checkin"`
	HasLightOn   bool   `json:"has_light_on"`
	HasRefundReq bool   `json:"has_refund_request"`
	Maintenance  bool   `json:"maintenance_blocked"`
	PrevOverrun  bool   `json:"prev_overrun"`
	Detail       string `json:"detail"`
}

// EvaluateVerdict 只计算判定不落库，供客服页面与自动裁决共用
func (s *Service) EvaluateVerdict(ctx context.Context, bookingID int64) (*VerdictEvidence, error) {
	ev := &VerdictEvidence{}
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1`, bookingID))
		if err != nil {
			return err
		}
		var e error
		ev.HasGate, ev.HasFront, ev.HasLightOn, e = presenceEvidence(ctx, tx, b.ID)
		if e != nil {
			return e
		}
		var e2 error
		ev.Maintenance, ev.PrevOverrun, ev.HasRefundReq, e2 = venueSideEvidence(ctx, tx, b)
		if e2 != nil {
			return e2
		}
		ev.decide(b, s, ctx, tx)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ev, nil
}

func (ev *VerdictEvidence) decide(b *store.Booking, s *Service, ctx context.Context, tx pgx.Tx) {
	arrived := ev.HasGate || ev.HasFront
	venueBlocking := ev.Maintenance || ev.PrevOverrun

	switch {
	case venueBlocking && !ev.HasLightOn:
		// 场地侧原因导致未开打（无论用户是否到场边等待）
		if ev.Maintenance {
			ev.Verdict, ev.Source, ev.Detail = "venue_fault", "maintenance", "预约时段场地处于维护锁定"
		} else {
			ev.Verdict, ev.Source, ev.Detail = "venue_fault", "prev_overrun", "前一场延时未结束导致无法入场"
		}
	case ev.HasLightOn && arrived:
		// 人工开灯且有到场记录 -> 正常/迟到
		first := b.FirstPresenceAt
		if first != nil {
			venue, err := s.loadVenue(ctx, tx, b.VenueID)
			if err == nil && first.After(b.StartTime.Add(time.Duration(venue.LateGraceMinutes)*time.Minute)) {
				ev.Verdict, ev.Source = "late", "presence"
				ev.Detail = fmt.Sprintf("开场后 %d 分钟才到场", int(first.Sub(b.StartTime).Minutes()))
				return
			}
		}
		ev.Verdict, ev.Source, ev.Detail = "on_time", "presence", "有闸机/前台到场记录且已开灯开打"
	case ev.HasLightOn && !arrived:
		ev.Verdict, ev.Source, ev.Detail = "ambiguous", "light_only", "人工开灯但无闸机/前台到场记录（灯开了未上场），需客服人工裁决"
	case arrived:
		// 到了场但没开打成（又无场地侧原因记录）：先按到场处理，客服可结合 timeline 复核
		ev.Verdict, ev.Source, ev.Detail = "on_time", "presence", "有闸机/前台到场记录（未见开灯开打，建议客服复核）"
	case ev.HasRefundReq:
		ev.Verdict, ev.Source, ev.Detail = "ambiguous", "refund_request", "用户有退款申请但无到场证据，需客服人工裁决"
	default:
		ev.Verdict, ev.Source, ev.Detail = "no_show", "absence", "闸机/灯控/前台均无到场记录"
	}
}

func (s *Service) judgeOne(ctx context.Context, id int64, source string) error {
	return inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, id))
		if err != nil {
			return err
		}
		if b.Status != "paid" && b.Status != "warming" && !(b.Status == "in_use" && b.FirstPresenceAt == nil) {
			return nil
		}
		venue, err := s.loadVenue(ctx, tx, b.VenueID)
		if err != nil {
			return err
		}
		now := s.now(ctx)
		if now.Before(b.StartTime.Add(time.Duration(venue.NoShowJudgeMinutes) * time.Minute)) {
			return nil
		}

		gateOK, frontOK, lightOn, err := presenceEvidence(ctx, tx, b.ID)
		if err != nil {
			return err
		}
		maint, prevRun, hasRefund, err := venueSideEvidence(ctx, tx, b)
		if err != nil {
			return err
		}
		ev := &VerdictEvidence{
			HasGate: gateOK, HasFront: frontOK, HasLightOn: lightOn,
			HasRefundReq: hasRefund, Maintenance: maint, PrevOverrun: prevRun,
		}
		ev.decide(b, s, ctx, tx)

		newStatus := b.Status
		switch ev.Verdict {
		case "no_show":
			newStatus = "no_show"
		case "venue_fault":
			newStatus = "venue_fault"
		case "ambiguous":
			newStatus = "disputed"
		default:
			// late / on_time：仅记录判定，不改变业务状态
		}

		if _, err := tx.Exec(ctx,
			`update bookings set status=$2,verdict=$3,judge_source=$4,judged_at=$5,
			 version=version+1,updated_at=now() where id=$1`,
			b.ID, newStatus, ev.Verdict, ev.Source, now); err != nil {
			return err
		}

		switch ev.Verdict {
		case "no_show":
			if _, err := tx.Exec(ctx,
				`update members set no_show_count=no_show_count+$2 where id=$1`,
				b.MemberID, venue.NoShowPenaltyCount); err != nil {
				return err
			}
			if err := releaseReservedBenefit(ctx, tx, b); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`insert into disputes(booking_id,auto_opened,status,issue,created_at)
				 values($1,true,'open',$2,now()) on conflict (booking_id) do nothing`,
				b.ID, "系统自动判定爽约：闸机/灯控/前台无到场记录"); err != nil {
				return err
			}
		case "venue_fault":
			if b.PaidCents > 0 {
				if _, err := tx.Exec(ctx,
					`insert into payments(booking_id,kind,amount_cents,method,status,transaction_no,reason,created_source)
					 values($1,'refund',$2,'mock','succeeded',$3,$4,'system')`,
					b.ID, b.PaidCents, fmt.Sprintf("RF%d-%06d", b.ID, random6()),
					"场馆原因自动退款："+ev.Detail); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx,
					`update bookings set refunded_cents=paid_cents,version=version+1 where id=$1`, b.ID); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx,
				`insert into disputes(booking_id,auto_opened,status,issue,created_at)
				 values($1,true,'open',$2,now()) on conflict (booking_id) do nothing`,
				b.ID, "场馆原因自动裁决："+ev.Detail); err != nil {
				return err
			}
		case "ambiguous":
			if _, err := tx.Exec(ctx,
				`insert into disputes(booking_id,auto_opened,status,issue,created_at)
				 values($1,true,'open',$2,now()) on conflict (booking_id) do nothing`,
				b.ID, "证据不足需人工裁决："+ev.Detail); err != nil {
				return err
			}
		}

		if err := emitEvent(ctx, tx, "verdict."+ev.Verdict, source, &b.ID, &b.VenueID, &b.CourtID, nil, map[string]any{
			"gate": gateOK, "front": frontOK, "light_on": lightOn,
			"refund_request": hasRefund, "maintenance": maint, "prev_overrun": prevRun,
			"detail": ev.Detail,
		}); err != nil {
			return err
		}
		// 裁决收尾关灯
		if (lightOn || b.WarmupAt != nil) && b.LightOffAt == nil {
			if _, err := tx.Exec(ctx,
				`insert into light_records(booking_id,court_id,action,command_source,success,detail,occurred_at)
				 values($1,$2,'off','system',true,'verdict cleanup',$3)`, b.ID, b.CourtID, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`update bookings set light_off_at=coalesce(light_off_at,$2) where id=$1`, b.ID, now); err != nil {
				return err
			}
		}
		return nil
	})
}
