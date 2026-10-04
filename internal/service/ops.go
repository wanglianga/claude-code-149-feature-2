package service

import (
	"context"
	"fmt"
	"time"

	"gymbadminton/internal/store"

	"github.com/jackc/pgx/v5"
)

// ---------- 场地维护锁定 / 临时换场 ----------

type MaintenanceInput struct {
	VenueID   int64
	CourtID   int64
	StartTime time.Time
	EndTime   time.Time
	Reason    string
	StaffID   int64
}

// CreateMaintenance 运营锁定场地维护（如预约开始前发现地胶起翘）。若覆盖已预约订单：
// held 订单直接过期释放；paid/warming/in_use 订单进入 relocation_pending（临时换场流程），
// 由用户选择换场/顺延/退款；逾期未选择由后台任务按场馆原因全额退款。
func (s *Service) CreateMaintenance(ctx context.Context, in MaintenanceInput) (*store.Maintenance, error) {
	in.StartTime = in.StartTime.UTC().Truncate(time.Minute)
	in.EndTime = in.EndTime.UTC().Truncate(time.Minute)
	if !in.EndTime.After(in.StartTime) {
		return nil, ce("BAD_TIME", "维护结束时间必须晚于开始时间", ErrValidation)
	}
	now := s.now(ctx)
	var m store.Maintenance
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`insert into maintenance_windows(venue_id,court_id,start_time,end_time,reason,created_by)
			 values($1,$2,$3,$4,$5,$6)
			 returning `+maintenanceColumns,
			in.VenueID, in.CourtID, in.StartTime, in.EndTime, in.Reason, in.StaffID)
		var err error
		m, err = scanMaintenance(row)
		if err != nil {
			return err
		}
		// 场地锁定以 maintenance_windows 为准（active/repair_done 均阻止预约与闸机），
		// 不改 courts.status，避免多条维护/验收交错时场地状态难以恢复。
		// 处理受影响订单
		rows, err := tx.Query(ctx,
			`select `+bookingColumns+` from bookings
			 where court_id=$1 and status in ('held','paid','warming','in_use')
			   and tstzrange(start_time,end_time,'[)') && tstzrange($2,$3,'[)')
			 for update`, in.CourtID, in.StartTime, in.EndTime)
		if err != nil {
			return err
		}
		var affected []*store.Booking
		for rows.Next() {
			b, e := scanBooking(rows)
			if e != nil {
				rows.Close()
				return e
			}
			affected = append(affected, b)
		}
		rows.Close()
		for _, b := range affected {
			if b.Status == "held" {
				if _, err := tx.Exec(ctx, `update bookings set status='expired',version=version+1,updated_at=now() where id=$1`, b.ID); err != nil {
					return err
				}
				if err := releaseReservedBenefit(ctx, tx, b); err != nil {
					return err
				}
				if err := emitEvent(ctx, tx, "booking.affected_by_maintenance", "operator", &b.ID, &b.VenueID, &b.CourtID, &in.StaffID,
					map[string]any{"maintenance_id": m.ID, "old_status": b.Status, "handling": "expired"}); err != nil {
					return err
				}
				continue
			}
			// paid / warming / in_use -> relocation_pending：等待用户选择换场/顺延/退款
			// 换场方案确认截止：开场前 10 分钟；若锁定时距开场不足 10 分钟，则给 5 分钟处理宽限
			due := b.StartTime.Add(-10 * time.Minute)
			if !due.After(now) {
				due = now.Add(5 * time.Minute)
			}
			if _, err := tx.Exec(ctx,
				`update bookings set status='relocation_pending',
				 relocation_status='pending',relocation_maintenance_id=$2,original_court_id=court_id,
				 relocation_due_at=$3,
				 note=case when note='' then $4 else note||E'\n'||$4 end,
				 version=version+1,updated_at=now()
				 where id=$1`,
				b.ID, m.ID, due, fmt.Sprintf("场地临时维护（维护单 #%d）：%s。请选择换场/顺延/退款。", m.ID, in.Reason)); err != nil {
				return err
			}
			// 锁定时旧场地若已预热/开灯，立即关灯留痕（安全；换场后在新场地重新授权开灯）
			if b.LightOffAt == nil && (b.WarmupAt != nil || b.LightOnAt != nil) {
				if _, err := tx.Exec(ctx,
					`insert into light_records(booking_id,court_id,action,command_source,success,detail,occurred_at)
					 values($1,$2,'off','operator',true,'maintenance lock, pending relocation',$3)`,
					b.ID, b.CourtID, now); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx,
					`update bookings set light_off_at=coalesce(light_off_at,$2) where id=$1`, b.ID, now); err != nil {
					return err
				}
			}
			if err := emitEvent(ctx, tx, "booking.relocation_pending", "operator", &b.ID, &b.VenueID, &b.CourtID, &in.StaffID,
				map[string]any{"maintenance_id": m.ID, "old_status": b.Status, "due_at": due,
					"reason": in.Reason, "entered_old_court": b.FirstGateAt != nil}); err != nil {
				return err
			}
		}
		return emitEvent(ctx, tx, "maintenance.created", "operator", nil, &in.VenueID, &in.CourtID, &in.StaffID,
			map[string]any{"maintenance_id": m.ID, "affected": len(affected),
				"start": in.StartTime, "end": in.EndTime, "reason": in.Reason})
	})
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// CompleteRepair 维修完工上报：active -> repair_done。
// 此时场地仍不开放预约/闸机，必须等待前台验收（verified）。
func (s *Service) CompleteRepair(ctx context.Context, id, staffID int64) (*store.Maintenance, error) {
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`update maintenance_windows set status='repair_done',repair_finished_at=now()
			 where id=$1 and status='active'`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ce("NOT_REPAIRABLE", "维护记录不存在或当前状态不可上报完工（需 active）", ErrIllegalState)
		}
		var venueID, courtID int64
		if err := tx.QueryRow(ctx, `select venue_id,court_id from maintenance_windows where id=$1`, id).Scan(&venueID, &courtID); err != nil {
			return err
		}
		return emitEvent(ctx, tx, "maintenance.repair_completed", "operator", nil, &venueID, &courtID, &staffID,
			map[string]any{"maintenance_id": id})
	})
	if err != nil {
		return nil, err
	}
	return s.GetMaintenance(ctx, id)
}

// VerifyMaintenance 前台验收：pass=true 时维护单 verified 且场地重新开放预约；
// pass=false 验收不通过，退回 active 继续维修。
func (s *Service) VerifyMaintenance(ctx context.Context, id, staffID int64, pass bool, note string) (*store.Maintenance, error) {
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		var courtID, venueID int64
		var status string
		if err := tx.QueryRow(ctx,
			`select court_id,venue_id,status from maintenance_windows where id=$1 for update`, id).
			Scan(&courtID, &venueID, &status); err != nil {
			if err == pgx.ErrNoRows {
				return ce("NOT_FOUND", "维护记录不存在", ErrNotFound)
			}
			return err
		}
		if !pass {
			if status != "repair_done" && status != "active" {
				return ce("NOT_VERIFIABLE", "仅维修中/待验收的维护可退回重修", ErrIllegalState)
			}
			if _, err := tx.Exec(ctx,
				`update maintenance_windows set status='active',verify_note=$2,verified_by=$3,verified_at=now() where id=$1`,
				id, "验收不通过，退回维修："+note, staffID); err != nil {
				return err
			}
			return emitEvent(ctx, tx, "maintenance.verify_rejected", "front", nil, &venueID, &courtID, &staffID,
				map[string]any{"maintenance_id": id, "note": note})
		}
		if status != "repair_done" && status != "active" {
			return ce("ALREADY_VERIFIED", "维护单已验收或已释放（"+status+"）", ErrIllegalState)
		}
		if _, err := tx.Exec(ctx,
			`update maintenance_windows set status='verified',verified_by=$2,verified_at=now(),verify_note=$3,
			 repair_finished_at=coalesce(repair_finished_at,now())
			 where id=$1`, id, staffID, note); err != nil {
			return err
		}
		return emitEvent(ctx, tx, "maintenance.verified", "front", nil, &venueID, &courtID, &staffID,
			map[string]any{"maintenance_id": id, "note": note})
	})
	if err != nil {
		return nil, err
	}
	return s.GetMaintenance(ctx, id)
}

func (s *Service) GetMaintenance(ctx context.Context, id int64) (*store.Maintenance, error) {
	m, err := scanMaintenance(s.pool().QueryRow(ctx,
		`select `+maintenanceColumns+` from maintenance_windows where id=$1`, id))
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func scanMaintenance(row pgx.Row) (store.Maintenance, error) {
	var m store.Maintenance
	err := row.Scan(&m.ID, &m.VenueID, &m.CourtID, &m.StartTime, &m.EndTime, &m.Reason, &m.Status,
		&m.CreatedBy, &m.ReleasedBy, &m.ReleasedAt,
		&m.RepairFinishedAt, &m.VerifiedBy, &m.VerifiedAt, &m.VerifyNote, &m.CreatedAt)
	if err == pgx.ErrNoRows {
		return m, ErrNotFound
	}
	return m, err
}

const maintenanceColumns = `id,venue_id,court_id,start_time,end_time,reason,status,created_by,released_by,released_at,
	repair_finished_at,verified_by,verified_at,verify_note,created_at`

// ReleaseMaintenance 运营直接撤销维护锁定（仅 active/repair_done 可撤销，如误登记）。
// 与前台验收等效：场地立即恢复开放。
func (s *Service) ReleaseMaintenance(ctx context.Context, id, staffID int64) error {
	return inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`update maintenance_windows set status='released',released_by=$2,released_at=now(),
			 verified_by=coalesce(verified_by,$2),verified_at=coalesce(verified_at,now())
			 where id=$1 and status in ('active','repair_done')`, id, staffID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ce("NOT_FOUND", "维护记录不存在或已结束", ErrNotFound)
		}
		var venueID, courtID int64
		if err := tx.QueryRow(ctx, `select venue_id,court_id from maintenance_windows where id=$1`, id).Scan(&venueID, &courtID); err != nil {
			return err
		}
		return emitEvent(ctx, tx, "maintenance.released", "operator", nil, &venueID, &courtID, &staffID,
			map[string]any{"maintenance_id": id})
	})
}

// ---------- 临时加场 ----------

func (s *Service) CreateExtraSlot(ctx context.Context, in store.ExtraSlot, staffID int64) (*store.ExtraSlot, error) {
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`insert into extra_slots(venue_id,court_id,slot_date,opens_at,closes_at,note,created_by)
			 values($1,$2,$3::date,$4,$5,$6,$7)
			 returning id,venue_id,court_id,slot_date::text,opens_at::text,closes_at::text,note`,
			in.VenueID, in.CourtID, in.SlotDate, in.OpensAt, in.ClosesAt, in.Note, staffID)
		if err := row.Scan(&in.ID, &in.VenueID, &in.CourtID, &in.SlotDate, &in.OpensAt, &in.ClosesAt, &in.Note); err != nil {
			return err
		}
		return emitEvent(ctx, tx, "extraslot.created", "operator", nil, &in.VenueID, in.CourtID, &staffID,
			map[string]any{"date": in.SlotDate, "opens": in.OpensAt, "closes": in.ClosesAt})
	})
	if err != nil {
		return nil, err
	}
	return &in, nil
}

// ---------- 规则配置 ----------

func (s *Service) UpsertCancelRule(ctx context.Context, venueID int64, isPeak bool, minHoursBefore float64, refundPercent, staffID int) error {
	return inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`insert into cancellation_rules(venue_id,is_peak,min_hours_before,refund_percent)
			 values($1,$2,$3,$4)
			 on conflict (venue_id,is_peak,min_hours_before)
			 do update set refund_percent=$4`, venueID, isPeak, minHoursBefore, refundPercent); err != nil {
			return err
		}
		return emitEvent(ctx, tx, "rule.cancel_updated", "operator", nil, &venueID, nil, int64Ptr(int64(staffID)),
			map[string]any{"is_peak": isPeak, "min_hours_before": minHoursBefore, "refund_percent": refundPercent})
	})
}

// ---------- 退款申请（用户侧） ----------

func (s *Service) CreateRefundRequest(ctx context.Context, bookingID, memberID int64, amountCents int64, reason string) (*store.RefundRequest, error) {
	var rr store.RefundRequest
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, bookingID))
		if err != nil {
			return err
		}
		if b.MemberID != memberID {
			return ce("FORBIDDEN", "只能为自己的订单申请退款", ErrIllegalState)
		}
		maxAmt := b.PaidCents - b.RefundedCents
		if amountCents <= 0 || amountCents > maxAmt {
			return ce("BAD_AMOUNT", fmt.Sprintf("退款金额需在 1~%d 分之间", maxAmt), ErrValidation)
		}
		var pend int
		if err := tx.QueryRow(ctx,
			`select count(*) from refund_requests where booking_id=$1 and status='pending'`, b.ID).Scan(&pend); err != nil {
			return err
		}
		if pend > 0 {
			return ce("ALREADY_PENDING", "已有待审核退款申请", ErrConflict)
		}
		row := tx.QueryRow(ctx,
			`insert into refund_requests(booking_id,member_id,amount_cents,reason)
			 values($1,$2,$3,$4)
			 returning id,booking_id,member_id,amount_cents,reason,status,reviewed_by,reviewed_at,review_note,created_at`,
			b.ID, memberID, amountCents, reason)
		if err := row.Scan(&rr.ID, &rr.BookingID, &rr.MemberID, &rr.AmountCents, &rr.Reason,
			&rr.Status, &rr.ReviewedBy, &rr.ReviewedAt, &rr.ReviewNote, &rr.CreatedAt); err != nil {
			return err
		}
		return emitEvent(ctx, tx, "refund.requested", "member", &b.ID, &b.VenueID, &b.CourtID, &memberID,
			map[string]any{"amount_cents": amountCents, "reason": reason})
	})
	if err != nil {
		return nil, err
	}
	return &rr, nil
}

// ---------- 客服争议处理 ----------

type DisputeResolution struct {
	Action      string // refund / partial_refund / comp_minutes / rescind_noshow / reject / mixed
	RefundCents int64
	CompMinutes int
	Note        string
	StaffID     int64
}

// ResolveDispute 客服综合闸机/灯控/前台/维护记录后的处理：
// 退费、部分退费、补偿时长（未来券）、撤销爽约（返还次数）、驳回。
func (s *Service) ResolveDispute(ctx context.Context, disputeID int64, in DisputeResolution) (*store.Dispute, error) {
	var d store.Dispute
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `select id,booking_id,opened_by,auto_opened,status,issue,
			resolution_type,refund_cents,comp_minutes,resolution_note,resolved_by,resolved_at,created_at
			from disputes where id=$1 for update`, disputeID).
			Scan(&d.ID, &d.BookingID, &d.OpenedBy, &d.AutoOpened, &d.Status, &d.Issue,
				&d.ResolutionType, &d.RefundCents, &d.CompMinutes, &d.ResolutionNote, &d.ResolvedBy, &d.ResolvedAt, &d.CreatedAt); err != nil {
			if err == pgx.ErrNoRows {
				return ce("NOT_FOUND", "争议单不存在", ErrNotFound)
			}
			return err
		}
		if d.Status != "open" {
			return ce("ALREADY_RESOLVED", "争议单已处理", ErrIllegalState)
		}
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, d.BookingID))
		if err != nil {
			return err
		}

		resType := in.Action
		switch in.Action {
		case "refund":
			amt := b.PaidCents - b.RefundedCents
			if amt <= 0 {
				return ce("NO_REFUNDABLE", "没有可退金额", ErrValidation)
			}
			if err := doRefund(ctx, tx, b, amt, "客服全额退费："+in.Note, "cs", in.StaffID); err != nil {
				return err
			}
			in.RefundCents = amt
		case "partial_refund":
			maxAmt := b.PaidCents - b.RefundedCents
			if in.RefundCents <= 0 || in.RefundCents > maxAmt {
				return ce("BAD_AMOUNT", fmt.Sprintf("退款金额需在 1~%d 分之间", maxAmt), ErrValidation)
			}
			if err := doRefund(ctx, tx, b, in.RefundCents, "客服部分退费："+in.Note, "cs", in.StaffID); err != nil {
				return err
			}
		case "comp_minutes":
			if in.CompMinutes <= 0 {
				return ce("BAD_COMP", "补偿分钟数必须大于 0", ErrValidation)
			}
			if _, err := tx.Exec(ctx,
				`insert into benefit_grants(member_id,venue_id,minutes,reason,granted_source,granted_by)
				 values($1,$2,$3,$4,'cs',$5)`,
				b.MemberID, b.VenueID, in.CompMinutes, "客服补偿："+in.Note, in.StaffID); err != nil {
				return err
			}
		case "rescind_noshow":
			if b.Status == "no_show" || b.Verdict != nil && *b.Verdict == "no_show" {
				if _, err := tx.Exec(ctx,
					`update members set no_show_count=greatest(no_show_count-1,0) where id=$1`, b.MemberID); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx,
					`update bookings set status=case when status='no_show' then 'completed' else status end,
					 verdict='excused',judge_source='cs_override',version=version+1,updated_at=now() where id=$1`, b.ID); err != nil {
					return err
				}
			}
		case "mixed":
			// 同时部分退费 + 补偿券
			if in.RefundCents > 0 {
				maxAmt := b.PaidCents - b.RefundedCents
				if in.RefundCents > maxAmt {
					in.RefundCents = maxAmt
				}
				if in.RefundCents > 0 {
					if err := doRefund(ctx, tx, b, in.RefundCents, "客服部分退费："+in.Note, "cs", in.StaffID); err != nil {
						return err
					}
				}
			}
			if in.CompMinutes > 0 {
				if _, err := tx.Exec(ctx,
					`insert into benefit_grants(member_id,venue_id,minutes,reason,granted_source,granted_by)
					 values($1,$2,$3,$4,'cs',$5)`,
					b.MemberID, b.VenueID, in.CompMinutes, "客服补偿："+in.Note, in.StaffID); err != nil {
					return err
				}
			}
		case "reject":
			// 驳回不改账
		default:
			return ce("BAD_ACTION", "未知处理动作", ErrValidation)
		}

		// 同步处理待审退款申请
		if in.Action == "refund" || in.Action == "partial_refund" || in.Action == "mixed" {
			if _, err := tx.Exec(ctx,
				`update refund_requests set status='approved',reviewed_by=$2,reviewed_at=now(),review_note=$3
				 where booking_id=$1 and status='pending'`, b.ID, in.StaffID, in.Note); err != nil {
				return err
			}
		} else if in.Action == "reject" {
			if _, err := tx.Exec(ctx,
				`update refund_requests set status='rejected',reviewed_by=$2,reviewed_at=now(),review_note=$3
				 where booking_id=$1 and status='pending'`, b.ID, in.StaffID, in.Note); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx,
			`update disputes set status='resolved',resolution_type=$2,refund_cents=$3,comp_minutes=$4,
			 resolution_note=$5,resolved_by=$6,resolved_at=now() where id=$1`,
			d.ID, resType, in.RefundCents, in.CompMinutes, in.Note, in.StaffID); err != nil {
			return err
		}
		return emitEvent(ctx, tx, "dispute.resolved", "cs", &b.ID, &b.VenueID, &b.CourtID, &in.StaffID, map[string]any{
			"action": in.Action, "refund_cents": in.RefundCents, "comp_minutes": in.CompMinutes, "note": in.Note,
		})
	})
	if err != nil {
		return nil, err
	}
	return s.GetDispute(ctx, disputeID)
}

func doRefund(ctx context.Context, tx pgx.Tx, b *store.Booking, amount int64, reason, source string, staffID int64) error {
	if amount <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`insert into payments(booking_id,kind,amount_cents,method,status,transaction_no,reason,created_source,created_by)
		 values($1,'refund',$2,'mock','succeeded',$3,$4,$5,$6)`,
		b.ID, amount, fmt.Sprintf("RF%d-%06d", b.ID, random6()), reason, source, staffID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`update bookings set refunded_cents=refunded_cents+$2,version=version+1,updated_at=now() where id=$1`,
		b.ID, amount); err != nil {
		return err
	}
	return nil
}

func int64Ptr(i int64) *int64 { return &i }

// ---------- 查询：争议全景 ----------

type DisputeDetail struct {
	Dispute         *store.Dispute               `json:"dispute"`
	Booking         *store.Booking               `json:"booking"`
	Payments        []store.Payment              `json:"payments"`
	GateRecords     []store.GateRecord           `json:"gate_records"`
	LightRecords    []store.LightRecord          `json:"light_records"`
	FrontRecords    []store.FrontRecord          `json:"front_records"`
	Events          []store.Event                `json:"events"`
	Maintenance     []store.Maintenance          `json:"maintenance"`
	DeviceSync      []store.DevicePermissionSync `json:"device_permission_sync"`
	VacateReminders []store.VacateReminder       `json:"vacate_reminders"`
	RelocatedFrom   *store.Booking               `json:"relocated_from,omitempty"`
	RelocatedTo     *store.Booking               `json:"relocated_to,omitempty"`
	Verdict         *VerdictEvidence             `json:"verdict"`
}

func (s *Service) GetDispute(ctx context.Context, id int64) (*store.Dispute, error) {
	var d store.Dispute
	err := s.pool().QueryRow(ctx, `select id,booking_id,opened_by,auto_opened,status,issue,
		resolution_type,refund_cents,comp_minutes,resolution_note,resolved_by,resolved_at,created_at
		from disputes where id=$1`, id).
		Scan(&d.ID, &d.BookingID, &d.OpenedBy, &d.AutoOpened, &d.Status, &d.Issue,
			&d.ResolutionType, &d.RefundCents, &d.CompMinutes, &d.ResolutionNote, &d.ResolvedBy, &d.ResolvedAt, &d.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	return &d, err
}

func (s *Service) OpenDispute(ctx context.Context, bookingID, staffID int64, issue string) (*store.Dispute, error) {
	var d store.Dispute
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1`, bookingID))
		if err != nil {
			return err
		}
		row := tx.QueryRow(ctx,
			`insert into disputes(booking_id,opened_by,auto_opened,status,issue)
			 values($1,$2,false,'open',$3)
			 on conflict (booking_id) do update set status='open'
			 returning id,booking_id,opened_by,auto_opened,status,issue,resolution_type,refund_cents,comp_minutes,resolution_note,resolved_by,resolved_at,created_at`,
			b.ID, staffID, issue)
		if err := row.Scan(&d.ID, &d.BookingID, &d.OpenedBy, &d.AutoOpened, &d.Status, &d.Issue,
			&d.ResolutionType, &d.RefundCents, &d.CompMinutes, &d.ResolutionNote, &d.ResolvedBy, &d.ResolvedAt, &d.CreatedAt); err != nil {
			return err
		}
		return emitEvent(ctx, tx, "dispute.opened", "cs", &b.ID, &b.VenueID, &b.CourtID, &staffID,
			map[string]any{"issue": issue})
	})
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Service) ListDisputes(ctx context.Context, status string, limit int) ([]store.Dispute, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `select id,booking_id,opened_by,auto_opened,status,issue,resolution_type,refund_cents,comp_minutes,resolution_note,resolved_by,resolved_at,created_at
		from disputes`
	args := []any{}
	if status != "" {
		q += ` where status=$1 order by id desc limit $2`
		args = append(args, status, limit)
	} else {
		q += ` order by id desc limit $1`
		args = append(args, limit)
	}
	rows, err := s.pool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Dispute
	for rows.Next() {
		var d store.Dispute
		if err := rows.Scan(&d.ID, &d.BookingID, &d.OpenedBy, &d.AutoOpened, &d.Status, &d.Issue,
			&d.ResolutionType, &d.RefundCents, &d.CompMinutes, &d.ResolutionNote, &d.ResolvedBy, &d.ResolvedAt, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// BookingTimeline 客服查看预约关联的全部证据（不只看订单表）
func (s *Service) BookingTimeline(ctx context.Context, bookingID int64) (*DisputeDetail, error) {
	d := &DisputeDetail{}
	b, err := s.GetBooking(ctx, bookingID)
	if err != nil {
		return nil, err
	}
	d.Booking = b
	pool := s.pool()

	rows, err := pool.Query(ctx,
		`select id,booking_id,kind,amount_cents,method,status,transaction_no,reason,created_source,created_at
		 from payments where booking_id=$1 order by id`, bookingID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p store.Payment
		var by *int64
		if err := rows.Scan(&p.ID, &p.BookingID, &p.Kind, &p.AmountCents, &p.Method, &p.Status,
			&p.TransactionNo, &p.Reason, &p.CreatedSource, &p.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		_ = by
		d.Payments = append(d.Payments, p)
	}
	rows.Close()

	rows, err = pool.Query(ctx,
		`select id,booking_id,entry_code,device_id,result,reason,scanned_at
		 from gate_records where booking_id=$1 order by id`, bookingID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var g store.GateRecord
		if err := rows.Scan(&g.ID, &g.BookingID, &g.EntryCode, &g.DeviceID, &g.Result, &g.Reason, &g.ScannedAt); err != nil {
			rows.Close()
			return nil, err
		}
		d.GateRecords = append(d.GateRecords, g)
	}
	rows.Close()

	rows, err = pool.Query(ctx,
		`select id,booking_id,court_id,action,command_source,success,detail,occurred_at
		 from light_records where booking_id=$1 order by id`, bookingID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l store.LightRecord
		if err := rows.Scan(&l.ID, &l.BookingID, &l.CourtID, &l.Action, &l.CommandSource, &l.Success, &l.Detail, &l.OccurredAt); err != nil {
			rows.Close()
			return nil, err
		}
		d.LightRecords = append(d.LightRecords, l)
	}
	rows.Close()

	rows, err = pool.Query(ctx,
		`select id,booking_id,staff_id,action,detail,created_at
		 from front_records where booking_id=$1 order by id`, bookingID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f store.FrontRecord
		if err := rows.Scan(&f.ID, &f.BookingID, &f.StaffID, &f.Action, &f.Detail, &f.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		d.FrontRecords = append(d.FrontRecords, f)
	}
	rows.Close()

	rows, err = pool.Query(ctx,
		`select id,booking_id,venue_id,court_id,event_type,source,actor_id,payload,created_at
		 from event_log where booking_id=$1 order by id`, bookingID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e store.Event
		if err := rows.Scan(&e.ID, &e.BookingID, &e.VenueID, &e.CourtID, &e.EventType, &e.Source,
			&e.ActorID, &e.Payload, &e.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		d.Events = append(d.Events, e)
	}
	rows.Close()

	// 维护记录：覆盖订单当前场地与原场地（换场后旧场地才有维护单）
	maintCourt := b.CourtID
	if b.OriginalCourtID != nil {
		maintCourt = *b.OriginalCourtID
	}
	rows, err = pool.Query(ctx,
		`select `+maintenanceColumns+`
		 from maintenance_windows
		 where court_id=$1 or id=coalesce($2,0) order by id`, maintCourt, b.RelocationMaintenanceID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		m, e := scanMaintenance(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		d.Maintenance = append(d.Maintenance, m)
	}
	rows.Close()

	// 闸机/灯控权限同步记录
	rows, err = pool.Query(ctx,
		`select id,booking_id,court_id,entry_code,device_type,permission,reason,sync_status,coalesce(created_by,0),created_at
		 from device_permission_sync where booking_id=$1 order by id`, bookingID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sy store.DevicePermissionSync
		var by int64
		if err := rows.Scan(&sy.ID, &sy.BookingID, &sy.CourtID, &sy.EntryCode, &sy.DeviceType,
			&sy.Permission, &sy.Reason, &sy.SyncStatus, &by, &sy.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if by != 0 {
			sy.CreatedBy = &by
		}
		d.DeviceSync = append(d.DeviceSync, sy)
	}
	rows.Close()

	// 旧场地清场提醒
	rows, err = pool.Query(ctx,
		`select id,booking_id,venue_id,court_id,maintenance_id,message,status,cleared_by,reminded_at,cleared_at,created_at
		 from vacate_reminders where booking_id=$1 order by id`, bookingID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var vr store.VacateReminder
		if err := rows.Scan(&vr.ID, &vr.BookingID, &vr.VenueID, &vr.CourtID, &vr.MaintenanceID, &vr.Message,
			&vr.Status, &vr.ClearedBy, &vr.RemindedAt, &vr.ClearedAt, &vr.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		d.VacateReminders = append(d.VacateReminders, vr)
	}
	rows.Close()

	// 换场链路的对端订单
	if b.RelocatedFromBookingID != nil {
		d.RelocatedFrom, _ = s.GetBooking(ctx, *b.RelocatedFromBookingID)
	}
	if b.RelocatedToBookingID != nil {
		d.RelocatedTo, _ = s.GetBooking(ctx, *b.RelocatedToBookingID)
	}

	var disp store.Dispute
	err = pool.QueryRow(ctx, `select id,booking_id,opened_by,auto_opened,status,issue,
		resolution_type,refund_cents,comp_minutes,resolution_note,resolved_by,resolved_at,created_at
		from disputes where booking_id=$1 order by id desc limit 1`, bookingID).
		Scan(&disp.ID, &disp.BookingID, &disp.OpenedBy, &disp.AutoOpened, &disp.Status, &disp.Issue,
			&disp.ResolutionType, &disp.RefundCents, &disp.CompMinutes, &disp.ResolutionNote, &disp.ResolvedBy, &disp.ResolvedAt, &disp.CreatedAt)
	if err == nil {
		d.Dispute = &disp
	}

	d.Verdict, _ = s.EvaluateVerdict(ctx, bookingID)
	return d, nil
}
