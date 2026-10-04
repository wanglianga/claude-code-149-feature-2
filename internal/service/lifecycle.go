package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gymbadminton/internal/store"

	"github.com/jackc/pgx/v5"
)

// ---------- 闸机 ----------

type GateScanResult struct {
	Booking *store.Booking `json:"booking"`
	Result  string         `json:"result"` // allowed / waiting / denied
	Reason  string         `json:"reason"`
}

// GateScan 闸机扫码：结合支付状态、入场时间窗口、场地占用/维护决定放行。
// 每一次扫码都写 gate_records 与 event_log，是爽约判定的关键证据来源。
func (s *Service) GateScan(ctx context.Context, entryCode, deviceID string) (*GateScanResult, error) {
	now := s.now(ctx)
	res := &GateScanResult{}
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where entry_code=$1 for update`, entryCode))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				_, _ = tx.Exec(ctx,
					`insert into gate_records(entry_code,device_id,result,reason,scanned_at) values($1,$2,'denied','无效入场码',$3)`,
					entryCode, deviceID, now)
				res.Result, res.Reason = "denied", "无效入场码"
				return nil
			}
			return err
		}
		res.Booking = b
		venue, err := s.loadVenue(ctx, tx, b.VenueID)
		if err != nil {
			return err
		}

		record := func(result, reason string) error {
			_, e1 := tx.Exec(ctx,
				`insert into gate_records(booking_id,entry_code,device_id,result,reason,scanned_at)
				 values($1,$2,$3,$4,$5,$6)`, b.ID, entryCode, deviceID, result, reason, now)
			return e1
		}

		switch b.Status {
		case "held":
			res.Result, res.Reason = "denied", "订单未支付"
			return record("denied", res.Reason)
		case "cancelled":
			res.Result, res.Reason = "denied", "订单已取消"
			return record("denied", res.Reason)
		case "expired":
			res.Result, res.Reason = "denied", "订单支付超时已释放"
			return record("denied", res.Reason)
		case "no_show":
			res.Result, res.Reason = "denied", "订单已判定爽约"
			return record("denied", res.Reason)
		case "completed":
			res.Result, res.Reason = "denied", "订单已完成"
			return record("denied", res.Reason)
		case "venue_fault":
			res.Result, res.Reason = "denied", "订单因场馆原因已退款关闭"
			return record("denied", res.Reason)
		case "disputed":
			res.Result, res.Reason = "denied", "订单争议处理中，请联系前台"
			return record("denied", res.Reason)
		case "relocated":
			res.Result, res.Reason = "denied", "订单已因场地维护换场/顺延，请使用新订单入场码"
			return record("denied", res.Reason)
		}

		windowStart := b.StartTime.Add(-time.Duration(venue.CheckinWindowMinutes) * time.Minute)
		windowEnd := b.EndTime.Add(time.Duration(venue.LateGraceMinutes) * time.Minute)
		if now.Before(windowStart) {
			res.Result, res.Reason = "denied",
				fmt.Sprintf("未到入场时间（开场前 %d 分钟可入场）", venue.CheckinWindowMinutes)
			if err := record("denied", res.Reason); err != nil {
				return err
			}
			return emitEvent(ctx, tx, "gate.denied", "gate", &b.ID, &b.VenueID, &b.CourtID, nil,
				map[string]any{"reason": res.Reason, "device": deviceID})
		}
		if now.After(windowEnd) {
			res.Result, res.Reason = "denied", "已超过入场时限"
			if err := record("denied", res.Reason); err != nil {
				return err
			}
			return emitEvent(ctx, tx, "gate.denied", "gate", &b.ID, &b.VenueID, &b.CourtID, nil,
				map[string]any{"reason": res.Reason, "device": deviceID})
		}

		// 维护中：可入场馆但场地不可用 → waiting（场边等待/改期）
		mw, err := activeMaintenanceAt(ctx, tx, b.CourtID, now)
		if err != nil {
			return err
		}
		if mw != nil {
			res.Result, res.Reason = "waiting", "场地临时维护，暂不可用"
			if err := record("waiting", res.Reason); err != nil {
				return err
			}
			_, _ = tx.Exec(ctx,
				`update bookings set first_gate_at=coalesce(first_gate_at,$2),
				 gate_waiting_at=coalesce(gate_waiting_at,$2),
				 first_presence_at=coalesce(first_presence_at,$2), version=version+1, updated_at=now()
				 where id=$1`, b.ID, now)
			return emitEvent(ctx, tx, "gate.waiting", "gate", &b.ID, &b.VenueID, &b.CourtID, nil,
				map[string]any{"reason": res.Reason, "maintenance_id": mw.ID, "device": deviceID})
		}

		// 前一场延时未结束？
		var prevOverrun bool
		if err := tx.QueryRow(ctx,
			`select exists(select 1 from bookings
			  where court_id=$1 and id<>$2 and status='in_use'
			  and end_time <= $3 and actual_end is null and light_on_at is not null)`,
			b.CourtID, b.ID, b.StartTime).Scan(&prevOverrun); err != nil {
			return err
		}
		if prevOverrun {
			res.Result, res.Reason = "waiting", "前一场延时未结束，请在场边等候"
			if err := record("waiting", res.Reason); err != nil {
				return err
			}
			_, err = tx.Exec(ctx,
				`update bookings set first_gate_at=coalesce(first_gate_at,$2),
				 gate_waiting_at=coalesce(gate_waiting_at,$2),
				 first_presence_at=coalesce(first_presence_at,$2), version=version+1, updated_at=now()
				 where id=$1`, b.ID, now)
			if err != nil {
				return err
			}
			return emitEvent(ctx, tx, "gate.waiting", "gate", &b.ID, &b.VenueID, &b.CourtID, nil,
				map[string]any{"reason": res.Reason, "device": deviceID})
		}

		// 正常放行
		res.Result, res.Reason = "allowed", "放行"
		if err := record("allowed", ""); err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`update bookings set first_gate_at=coalesce(first_gate_at,$2),
			 first_presence_at=coalesce(first_presence_at,$2), version=version+1, updated_at=now()
			 where id=$1`, b.ID, now)
		if err != nil {
			return err
		}
		return emitEvent(ctx, tx, "gate.allowed", "gate", &b.ID, &b.VenueID, &b.CourtID, nil,
			map[string]any{"device": deviceID})
	})
	if err != nil {
		return nil, err
	}
	if res.Booking != nil {
		res.Booking, _ = s.GetBooking(ctx, res.Booking.ID)
	}
	return res, nil
}

// ---------- 前台签到 ----------

// FrontCheckin 前台人工签到（会员到场的另一证据来源）
func (s *Service) FrontCheckin(ctx context.Context, bookingID, staffID int64) (*store.Booking, error) {
	now := s.now(ctx)
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, bookingID))
		if err != nil {
			return err
		}
		if b.Status != "paid" && b.Status != "warming" && b.Status != "in_use" {
			return ce("NOT_CHECKINABLE", "当前状态不可签到（"+b.Status+"）", ErrIllegalState)
		}
		if _, err := tx.Exec(ctx,
			`insert into front_records(booking_id,staff_id,action,detail) values($1,$2,'checkin','{}'::jsonb)`,
			b.ID, staffID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`update bookings set front_checkin_at=coalesce(front_checkin_at,$2),
			 first_presence_at=coalesce(first_presence_at,$2), version=version+1, updated_at=now()
			 where id=$1`, b.ID, now); err != nil {
			return err
		}
		return emitEvent(ctx, tx, "front.checkin", "front", &b.ID, &b.VenueID, &b.CourtID, &staffID, nil)
	})
	if err != nil {
		return nil, err
	}
	return s.GetBooking(ctx, bookingID)
}

// ---------- 灯控 ----------

// LightCommand 灯控指令。action: warmup / on / off
// source: scheduler / device / front / operator / system
func (s *Service) LightCommand(ctx context.Context, bookingID int64, action, source string, actorID *int64, detail string) (*store.Booking, error) {
	if source == "" {
		source = "device"
	}
	// 设备来源在统一事件日志中归入 light 域（light_records 保留 device 原值）
	eventSource := source
	if source == "device" {
		eventSource = "light"
	}
	now := s.now(ctx)
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, bookingID))
		if err != nil {
			return err
		}
		venue, err := s.loadVenue(ctx, tx, b.VenueID)
		if err != nil {
			return err
		}

		canWarm := b.Status == "paid"
		canOn := b.Status == "paid" || b.Status == "warming"
		canOff := b.Status == "warming" || b.Status == "in_use"

		switch action {
		case "warmup":
			if !canWarm {
				return ce("NOT_WARMABLE", "仅已支付订单可进入灯控预热（当前 "+b.Status+"）", ErrIllegalState)
			}
			// 预热单独成状态：支付超时任务绝不会回收 warming 订单
			if _, err := tx.Exec(ctx,
				`update bookings set status='warming', warmup_at=coalesce(warmup_at,$2),
				 version=version+1, updated_at=now() where id=$1`, b.ID, now); err != nil {
				return err
			}
		case "on":
			if !canOn {
				return ce("NOT_TURNABLE_ON", "当前状态不能开灯（"+b.Status+"）", ErrIllegalState)
			}
			// 调度器自动开灯前，若前一场仍在延时占用，跳过（保留 warming，等前台人工处理）
			if source == "scheduler" {
				var blocked bool
				if err := tx.QueryRow(ctx,
					`select exists(select 1 from bookings
					  where court_id=$1 and id<>$2 and status='in_use'
					  and end_time <= $3 and actual_end is null and light_on_at is not null)`,
					b.CourtID, b.ID, b.StartTime).Scan(&blocked); err != nil {
					return err
				}
				if blocked {
					return ce("PREV_OVERRUN", "前一场延时未结束，暂不自动开灯", ErrIllegalState)
				}
			}
			if err := s.applyLightOn(ctx, tx, b, venue, now, source); err != nil {
				return err
			}
		case "off":
			if !canOff {
				return ce("NOT_TURNABLE_OFF", "当前状态不能关灯（"+b.Status+"）", ErrIllegalState)
			}
			if _, err := tx.Exec(ctx,
				`update bookings set light_off_at=$2, version=version+1, updated_at=now() where id=$1`,
				b.ID, now); err != nil {
				return err
			}
		default:
			return ce("BAD_ACTION", "未知灯控指令", ErrValidation)
		}

		if _, err := tx.Exec(ctx,
			`insert into light_records(booking_id,court_id,action,command_source,success,detail,occurred_at)
			 values($1,$2,$3,$4,true,$5,$6)`,
			b.ID, b.CourtID, action, source, detail, now); err != nil {
			return err
		}
		return emitEvent(ctx, tx, "light."+action, eventSource, &b.ID, &b.VenueID, &b.CourtID, actorID,
			map[string]any{"detail": detail, "command_source": source})
	})
	if err != nil {
		return nil, err
	}
	return s.GetBooking(ctx, bookingID)
}

// applyLightOn 开灯即开场；因场边等待导致晚开时，自动补偿等额时长（免延时费）
func (s *Service) applyLightOn(ctx context.Context, tx pgx.Tx, b *store.Booking, venue *store.Venue, now time.Time, source string) error {
	lateMin := 0
	if b.GateWaitingAt != nil && now.After(b.StartTime) {
		lateMin = int(now.Sub(b.StartTime).Minutes())
		if lateMin < 0 {
			lateMin = 0
		}
	}
	_, err := tx.Exec(ctx,
		`update bookings set status='in_use',
		 light_on_at=coalesce(light_on_at,$2),
		 effective_start=coalesce(effective_start,$2),
		 late_start_minutes=case when $3 > late_start_minutes then $3 else late_start_minutes end,
		 comp_bonus_minutes=comp_bonus_minutes + case when gate_waiting_at is not null and $2 > start_time and $3 > 0 then $3 else 0 end,
		 version=version+1, updated_at=now()
		 where id=$1`, b.ID, now, lateMin)
	if err != nil {
		return err
	}
	if lateMin > 0 {
		detail := map[string]any{"late_minutes": lateMin, "reason": "前一场延时/维护，自动补偿等额可打时长"}
		raw, _ := jsonMarshal(detail)
		if _, err := tx.Exec(ctx,
			`insert into front_records(booking_id,staff_id,action,detail)
			 values($1,$2,'grant_comp',$3::jsonb)`, b.ID, nil, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 前台开始/结束场次 ----------

// StartSession 前台/设备确认开打
func (s *Service) StartSession(ctx context.Context, bookingID int64, source string, actorID *int64) (*store.Booking, error) {
	if source == "" {
		source = "front"
	}
	return s.LightCommand(ctx, bookingID, "on", source, actorID, "session start")
}

type EndSessionInput struct {
	Method string // onsite / member_pay / system_pending
}

// EndSession 结束场次并结算：延时费（补偿时长先抵扣）、超员费，写灯控关灯指令。
func (s *Service) EndSession(ctx context.Context, bookingID int64, source string, actorID *int64, in EndSessionInput) (*store.Booking, error) {
	now := s.now(ctx)
	if source == "" {
		source = "front"
	}
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, bookingID))
		if err != nil {
			return err
		}
		if b.Status != "in_use" {
			return ce("NOT_IN_USE", "场次未在进行中（"+b.Status+"）", ErrIllegalState)
		}
		venue, err := s.loadVenue(ctx, tx, b.VenueID)
		if err != nil {
			return err
		}
		court, err := s.loadCourt(ctx, tx, b.CourtID)
		if err != nil {
			return err
		}
		windows, err := s.peakWindows(ctx, tx, b.VenueID)
		if err != nil {
			return err
		}
		allowedEnd := b.EndTime.Add(time.Duration(b.CompBonusMinutes) * time.Minute)
		otMin, otFee := overtimeFee(venueWithRate(venue, effectiveRate(venue, court)), windows, allowedEnd, now)
		if otFee < 0 {
			otFee = 0
		}

		if _, err := tx.Exec(ctx,
			`update bookings set status='completed',actual_end=$2,light_off_at=coalesce(light_off_at,$2),
			 overtime_minutes=overtime_minutes+$3,overtime_cents=overtime_cents+$4,
			 version=version+1,updated_at=now()
			 where id=$1`, b.ID, now, otMin, otFee); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`insert into light_records(booking_id,court_id,action,command_source,success,detail,occurred_at)
			 values($1,$2,'off',$3,true,$4,$5)`,
			b.ID, b.CourtID, source, fmt.Sprintf("session end, overtime %dmin", otMin), now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`insert into front_records(booking_id,staff_id,action,detail) values($1,$2,'checkout',$3::jsonb)`,
			b.ID, actorID, jsonString(map[string]any{
				"overtime_minutes": otMin, "overtime_cents": otFee,
				"comp_bonus_minutes": b.CompBonusMinutes,
			})); err != nil {
			return err
		}
		// 延时收费：前台现收成功 / 系统挂账待收
		if otFee > 0 {
			payStatus := "succeeded"
			method := in.Method
			if method == "" {
				method = "onsite"
			}
			if source == "system" || method == "system_pending" {
				payStatus = "pending"
				method = "onsite"
			}
			if _, err := tx.Exec(ctx,
				`insert into payments(booking_id,kind,amount_cents,method,status,transaction_no,reason,created_source,created_by)
				 values($1,'payment',$2,$3,$4,$5,'延时费用',$6,$7)`,
				b.ID, otFee, method, payStatus, fmt.Sprintf("OT%d-%06d", b.ID, random6()), source, actorID); err != nil {
				return err
			}
		}
		return emitEvent(ctx, tx, "session.completed", source, &b.ID, &b.VenueID, &b.CourtID, actorID, map[string]any{
			"actual_end": now, "overtime_minutes": otMin, "overtime_cents": otFee,
			"comp_bonus_minutes": b.CompBonusMinutes,
		})
	})
	if err != nil {
		return nil, err
	}
	return s.GetBooking(ctx, bookingID)
}

// ---------- 拼场 / 超员 ----------

func (s *Service) AddParticipant(ctx context.Context, bookingID int64, name, phone string, memberID *int64) (*store.Participant, error) {
	var p store.Participant
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, bookingID))
		if err != nil {
			return err
		}
		row := tx.QueryRow(ctx,
			`insert into booking_participants(booking_id,member_id,name,phone)
			 values($1,$2,$3,$4)
			 on conflict (booking_id,phone) do update set name=excluded.name
			 returning id,booking_id,member_id,name,phone,checked_in,checked_in_at`,
			b.ID, memberID, name, phone)
		if err := row.Scan(&p.ID, &p.BookingID, &p.MemberID, &p.Name, &p.Phone, &p.CheckedIn, &p.CheckedInAt); err != nil {
			return err
		}
		// 预约人数随行更新（上限在下单时按容量 2 倍控制）
		var cnt int
		if err := tx.QueryRow(ctx,
			`select count(*) from booking_participants where booking_id=$1`, b.ID).Scan(&cnt); err != nil {
			return err
		}
		if cnt > b.PartySize {
			if _, err := tx.Exec(ctx, `update bookings set party_size=$2,version=version+1,updated_at=now() where id=$1`, b.ID, cnt); err != nil {
				return err
			}
		}
		return emitEvent(ctx, tx, "participant.added", "member", &b.ID, &b.VenueID, &b.CourtID, memberID,
			map[string]any{"name": name, "phone": phone})
	})
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ReconcileHeadcount 前台核对实际人数；超员则计费（可事后由前台减免）
func (s *Service) ReconcileHeadcount(ctx context.Context, bookingID, staffID int64, actualCount int) (*store.Booking, error) {
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, bookingID))
		if err != nil {
			return err
		}
		venue, err := s.loadVenue(ctx, tx, b.VenueID)
		if err != nil {
			return err
		}
		extraPersons, fee := overlimitFee(venue, actualCount, b.CapacitySnapshot)
		if _, err := tx.Exec(ctx,
			`update bookings set party_size=$2,overlimit_persons=$3,overlimit_cents=$4,
			 version=version+1,updated_at=now() where id=$1`,
			b.ID, actualCount, extraPersons, fee); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`insert into front_records(booking_id,staff_id,action,detail) values($1,$2,'adjust',$3::jsonb)`,
			b.ID, staffID, jsonString(map[string]any{
				"actual_count": actualCount, "overlimit_persons": extraPersons, "overlimit_cents": fee,
			})); err != nil {
			return err
		}
		if fee > 0 {
			if _, err := tx.Exec(ctx,
				`insert into payments(booking_id,kind,amount_cents,method,status,transaction_no,reason,created_source,created_by)
				 values($1,'payment',$2,'onsite','pending',$3,'拼场超员费用','front',$4)`,
				b.ID, fee, fmt.Sprintf("OL%d-%06d", b.ID, random6()), staffID); err != nil {
				return err
			}
		}
		return emitEvent(ctx, tx, "headcount.reconciled", "front", &b.ID, &b.VenueID, &b.CourtID, &staffID,
			map[string]any{"actual_count": actualCount, "overlimit_persons": extraPersons, "overlimit_cents": fee})
	})
	if err != nil {
		return nil, err
	}
	return s.GetBooking(ctx, bookingID)
}

// WaiveOverlimit 前台减免超员费
func (s *Service) WaiveOverlimit(ctx context.Context, bookingID, staffID int64) (*store.Booking, error) {
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, bookingID))
		if err != nil {
			return err
		}
		waived := b.OverlimitCents
		if _, err := tx.Exec(ctx,
			`update bookings set overlimit_cents=0,overlimit_persons=0,version=version+1,updated_at=now() where id=$1`,
			b.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`update payments set status='cancelled' where booking_id=$1 and reason='拼场超员费用' and status='pending'`,
			b.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`insert into front_records(booking_id,staff_id,action,detail) values($1,$2,'waive_overlimit',$3::jsonb)`,
			b.ID, staffID, jsonString(map[string]any{"waived_cents": waived})); err != nil {
			return err
		}
		return emitEvent(ctx, tx, "overlimit.waived", "front", &b.ID, &b.VenueID, &b.CourtID, &staffID,
			map[string]any{"waived_cents": waived})
	})
	if err != nil {
		return nil, err
	}
	return s.GetBooking(ctx, bookingID)
}

// ---------- 前台补偿时长 ----------

// GrantCompensation 前台临时补偿：
// scope=current 直接延长本场可打时长（comp_bonus_minutes，抵扣延时费）；
// scope=future 发放未来可用的补偿券（benefit_grants）。
func (s *Service) GrantCompensation(ctx context.Context, bookingID, staffID int64, minutes int, scope, reason string) (*store.Booking, error) {
	if minutes <= 0 {
		return nil, ce("BAD_COMP", "补偿分钟数必须大于 0", ErrValidation)
	}
	if scope == "" {
		scope = "current"
	}
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, bookingID))
		if err != nil {
			return err
		}
		switch scope {
		case "current":
			if b.Status != "paid" && b.Status != "warming" && b.Status != "in_use" {
				return ce("BAD_STATE", "本场补偿只能在开场前/进行中发放", ErrIllegalState)
			}
			if _, err := tx.Exec(ctx,
				`update bookings set comp_bonus_minutes=comp_bonus_minutes+$2,version=version+1,updated_at=now() where id=$1`,
				b.ID, minutes); err != nil {
				return err
			}
		case "future":
			if _, err := tx.Exec(ctx,
				`insert into benefit_grants(member_id,venue_id,minutes,reason,granted_source,granted_by)
				 values($1,$2,$3,$4,'front',$5)`,
				b.MemberID, b.VenueID, minutes, reason, staffID); err != nil {
				return err
			}
		default:
			return ce("BAD_SCOPE", "scope 必须为 current 或 future", ErrValidation)
		}
		if _, err := tx.Exec(ctx,
			`insert into front_records(booking_id,staff_id,action,detail) values($1,$2,'grant_comp',$3::jsonb)`,
			b.ID, staffID, jsonString(map[string]any{"minutes": minutes, "scope": scope, "reason": reason})); err != nil {
			return err
		}
		return emitEvent(ctx, tx, "comp.granted", "front", &b.ID, &b.VenueID, &b.CourtID, &staffID,
			map[string]any{"minutes": minutes, "scope": scope, "reason": reason})
	})
	if err != nil {
		return nil, err
	}
	return s.GetBooking(ctx, bookingID)
}

func jsonMarshal(v any) ([]byte, error) { return jsonMarshalImpl(v) }

func jsonString(v any) string {
	b, _ := jsonMarshalImpl(v)
	return string(b)
}
