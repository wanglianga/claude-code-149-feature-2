package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gymbadminton/internal/store"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 场地维护临时换场：
//   维护锁定后受影响订单进入 relocation_pending，用户可选择
//   1) move     换至同时间可用场地（更贵差价场馆承担，更便宜差额退还）
//   2) postpone 顺延至其他时间/场地
//   3) refund   场馆原因全额退款
// 换场确认后同步闸机/灯控权限到新场地；已扫码进入旧场地则生成清场提醒，
// 维护原因写入旧订单，防止爽约裁决把用户误判为占场。

// ---------- 候选场地 ----------

type RelocationOption struct {
	Court               store.Court `json:"court"`
	StartTime           time.Time   `json:"start_time"`
	EndTime             time.Time   `json:"end_time"`
	IsPeak              bool        `json:"is_peak"`
	CourtFeeCents       int64       `json:"court_fee_cents"`        // 新场地原价场地费
	MemberCourtCents    int64       `json:"member_court_cents"`     // 折后且按原权益分钟抵扣后的场地费
	OldMemberCourtCents int64       `json:"old_member_court_cents"` // 旧场地折后场地费（对照）
	// 差价：>0 由场馆承担（会员不加价）；<0 退还会员
	PriceDiffCents    int64 `json:"price_diff_cents"`
	VenueBorneCents   int64 `json:"venue_borne_cents"`
	MemberRefundCents int64 `json:"member_refund_cents"`
}

type RelocationOptions struct {
	BookingID int64              `json:"booking_id"`
	Reason    string             `json:"reason"`
	DueAt     *time.Time         `json:"due_at"`
	Options   []RelocationOption `json:"options"`
}

// RelocationCandidates 筛选与订单同时段（或指定时段）可换的场地并给出差价。
func (s *Service) RelocationCandidates(ctx context.Context, bookingID int64, altStart, altEnd *time.Time) (*RelocationOptions, error) {
	out := &RelocationOptions{BookingID: bookingID}
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, bookingID))
		if err != nil {
			return err
		}
		if b.Status != "relocation_pending" {
			return ce("NOT_RELOCATABLE", "当前订单状态不处于换场处理中（"+b.Status+"）", ErrIllegalState)
		}
		st, en := b.StartTime, b.EndTime
		if altStart != nil && altEnd != nil {
			st, en = altStart.UTC().Truncate(time.Minute), altEnd.UTC().Truncate(time.Minute)
			if !en.After(st) {
				return ce("BAD_TIME", "结束时间必须晚于开始时间", ErrValidation)
			}
			if en.Sub(st) < 30*time.Minute || en.Sub(st) > 4*time.Hour {
				return ce("BAD_DURATION", "预约时长需在 30~240 分钟之间", ErrValidation)
			}
		}
		out.DueAt = b.RelocationDueAt
		if b.RelocationMaintenanceID != nil {
			if m, e := s.GetMaintenance(ctx, *b.RelocationMaintenanceID); e == nil {
				out.Reason = m.Reason
			}
		}
		venue, err := s.loadVenue(ctx, tx, b.VenueID)
		if err != nil {
			return err
		}
		windows, err := s.peakWindows(ctx, tx, venue.ID)
		if err != nil {
			return err
		}
		policy, err := s.getMemberPolicy(ctx, tx, venue.ID, b.MemberTypeSnap)
		if err != nil {
			return err
		}
		// 旧场地折后场地费（对照基准；附加费不随场地变化）
		oldBD := quote(priceInput{venue: venue, policy: policy, peakWindows: windows,
			start: st, end: en, availableBenefitM: b.BenefitMinutes})
		oldNet := oldBD.CourtFeeCents - oldBD.DiscountCents - oldBD.BenefitCents

		rows, err := tx.Query(ctx,
			`select id, venue_id, court_no, coalesce(capacity,0), status, coalesce(price_cents_per_hour,0)
			 from courts where venue_id=$1 order by court_no`, venue.ID)
		if err != nil {
			return err
		}
		var courts []store.Court
		for rows.Next() {
			var c store.Court
			if err := rows.Scan(&c.ID, &c.VenueID, &c.CourtNo, &c.Capacity, &c.Status, &c.PriceCentsPerHour); err != nil {
				rows.Close()
				return err
			}
			courts = append(courts, c)
		}
		rows.Close()

		for _, c := range courts {
			if c.ID == b.CourtID || c.Status != "active" {
				continue
			}
			capacity := c.Capacity
			if capacity == 0 {
				capacity = venue.DefaultCourtCapacity
			}
			if b.PartySize > capacity {
				continue // 容量不够（如原 6 人换到 4 人小场）
			}
			ok, err := courtFreeAt(ctx, tx, venue, c.ID, st, en)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			cExtra, err := loadExtraSlots(ctx, tx, venue.ID, c.ID, st.Format("2006-01-02"))
			if err != nil {
				return err
			}
			durMin := int(en.Sub(st).Minutes())
			openAll := true
			for mm := 0; mm < durMin; mm++ {
				if !courtOpenAt(venue, cExtra, st.Add(time.Duration(mm)*time.Minute)) {
					openAll = false
					break
				}
			}
			if !openAll {
				continue
			}
			bd := quote(priceInput{venue: venue, courtPriceCents: c.PriceCentsPerHour, policy: policy,
				peakWindows: windows, start: st, end: en, availableBenefitM: b.BenefitMinutes})
			newNet := bd.CourtFeeCents - bd.DiscountCents - bd.BenefitCents
			opt := RelocationOption{
				Court: c, StartTime: st, EndTime: en, IsPeak: bd.IsPeak,
				CourtFeeCents: bd.CourtFeeCents, MemberCourtCents: newNet,
				OldMemberCourtCents: oldNet, PriceDiffCents: newNet - oldNet,
			}
			if opt.PriceDiffCents > 0 {
				opt.VenueBorneCents = opt.PriceDiffCents // 更贵：场馆承担
			} else if opt.PriceDiffCents < 0 {
				opt.MemberRefundCents = -opt.PriceDiffCents // 更便宜：差额退还
			}
			out.Options = append(out.Options, opt)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// courtFreeAt 判断场地在 [st,en) 是否无活跃预约、无维护锁定
func courtFreeAt(ctx context.Context, q pgx.Tx, venue *store.Venue, courtID int64, st, en time.Time) (bool, error) {
	var booked, blocked bool
	if err := q.QueryRow(ctx,
		`select exists(select 1 from bookings
		  where court_id=$1 and status in ('held','paid','warming','in_use')
		  and tstzrange(start_time,end_time,'[)') && tstzrange($2,$3,'[)'))`,
		courtID, st, en).Scan(&booked); err != nil {
		return false, err
	}
	if booked {
		return false, nil
	}
	if err := q.QueryRow(ctx,
		`select exists(select 1 from maintenance_windows
		  where court_id=$1 and status in ('active','repair_done')
		  and tstzrange(start_time,end_time,'[)') && tstzrange($2,$3,'[)'))`,
		courtID, st, en).Scan(&blocked); err != nil {
		return false, err
	}
	return !blocked, nil
}

// ---------- 用户决策 ----------

type RelocationDecision struct {
	Choice    string // move / postpone / refund
	CourtID   int64
	StartTime *time.Time
	EndTime   *time.Time
	Note      string
	// Source/Actor：member 本人或 staff 协助
	Source string
	Actor  *int64
}

type RelocationResult struct {
	OldBooking      *store.Booking               `json:"old_booking"`
	NewBooking      *store.Booking               `json:"new_booking,omitempty"`
	Choice          string                       `json:"choice"`
	RefundCents     int64                        `json:"refund_cents"`
	VenueBorneCents int64                        `json:"venue_borne_cents"`
	DeviceSync      []store.DevicePermissionSync `json:"device_sync"`
	VacateReminder  *store.VacateReminder        `json:"vacate_reminder,omitempty"`
}

// ResolveRelocation 处理用户对换场方案的确认。
func (s *Service) ResolveRelocation(ctx context.Context, bookingID int64, in RelocationDecision) (*RelocationResult, error) {
	if in.Source == "" {
		in.Source = "member"
	}
	var res RelocationResult
	res.Choice = in.Choice
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		b, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1 for update`, bookingID))
		if err != nil {
			return err
		}
		if b.Status != "relocation_pending" {
			return ce("NOT_RELOCATABLE", "当前订单状态不处于换场处理中（"+b.Status+"）", ErrIllegalState)
		}
		var maint store.Maintenance
		if b.RelocationMaintenanceID != nil {
			m, e := scanMaintenance(tx.QueryRow(ctx, `select `+maintenanceColumns+` from maintenance_windows where id=$1`,
				*b.RelocationMaintenanceID))
			if e != nil && !errors.Is(e, ErrNotFound) {
				return e
			}
			maint = m
		}

		switch in.Choice {
		case "refund":
			amt, err := s.applyRelocationRefund(ctx, tx, b, maint, in.Source, in.Actor, in.Note)
			if err != nil {
				return err
			}
			res.RefundCents = amt
		case "move", "postpone":
			newB, venueBorne, refundCents, syncs, vr, err := s.applyCourtMove(ctx, tx, b, maint, in)
			if err != nil {
				return err
			}
			res.NewBooking = newB
			res.VenueBorneCents = venueBorne
			res.RefundCents = refundCents
			res.DeviceSync = syncs
			res.VacateReminder = vr
		default:
			return ce("BAD_CHOICE", "choice 必须为 move / postpone / refund", ErrValidation)
		}
		var nb *int64
		if res.NewBooking != nil {
			nb = &res.NewBooking.ID
		}
		if err := emitEvent(ctx, tx, "booking.relocation_resolved", in.Source, &b.ID, &b.VenueID, &b.CourtID, in.Actor,
			map[string]any{"choice": in.Choice, "new_booking_id": nb,
				"refund_cents": res.RefundCents, "venue_borne_cents": res.VenueBorneCents,
				"vacate_reminded": res.VacateReminder != nil, "note": in.Note}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23P01" {
			return nil, ce("SLOT_TAKEN", "目标场地时段刚被预约，请重新获取换场候选", ErrConflict)
		}
		return nil, err
	}
	res.OldBooking, _ = s.GetBooking(ctx, bookingID)
	if res.NewBooking != nil {
		res.NewBooking, _ = s.GetBooking(ctx, res.NewBooking.ID)
	}
	return &res, nil
}

// applyRelocationRefund 用户选择退款 / 逾期系统退款：场馆原因全额退款 + 争议单，
// 维护原因写入订单，旧订单置 venue_fault。返回退款金额。
func (s *Service) applyRelocationRefund(ctx context.Context, tx pgx.Tx, b *store.Booking, maint store.Maintenance,
	source string, actor *int64, note string) (int64, error) {
	reasonText := "场地临时维护"
	if maint.ID != 0 {
		reasonText = fmt.Sprintf("场地临时维护（维护单 #%d）：%s", maint.ID, maint.Reason)
	}
	if _, err := tx.Exec(ctx,
		`update bookings set status='venue_fault',verdict='venue_fault',judge_source='maintenance',
		 judged_at=now(),relocation_status='resolved',relocation_due_at=null,
		 note=case when note='' then $2 else note||E'\n'||$2 end,
		 version=version+1,updated_at=now()
		 where id=$1`, b.ID, reasonText+"，用户选择退款。"+coalesceNote(note)); err != nil {
		return 0, err
	}
	var amt int64
	if b.PaidCents-b.RefundedCents > 0 {
		amt = b.PaidCents - b.RefundedCents
		if _, err := tx.Exec(ctx,
			`insert into payments(booking_id,kind,amount_cents,method,status,transaction_no,reason,created_source,created_by)
			 values($1,'refund',$2,'mock','succeeded',$3,$4,$5,$6)`,
			b.ID, amt, fmt.Sprintf("RF%d-%06d", b.ID, random6()),
			reasonText+"，场馆原因全额退款", source, actor); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx,
			`update bookings set refunded_cents=paid_cents,version=version+1 where id=$1`, b.ID); err != nil {
			return 0, err
		}
	}
	if err := releaseReservedBenefit(ctx, tx, b); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`insert into disputes(booking_id,opened_by,auto_opened,status,issue,created_at)
		 values($1,$2::bigint,$2::bigint is null,'open',$3,now()) on conflict (booking_id) do nothing`,
		b.ID, actor, reasonText+"，换场流程用户选择退款"); err != nil {
		return 0, err
	}
	// 旧场地灯控权限收回（闸机随订单终止状态自动拒绝）
	if err := syncDevicePermission(ctx, tx, b, b.CourtID, b.EntryCode, "revoke",
		"用户选择退款，维护原因收回权限", source, actor); err != nil {
		return 0, err
	}
	if err := emitEvent(ctx, tx, "booking.relocation_refunded", source, &b.ID, &b.VenueID, &b.CourtID, actor,
		map[string]any{"reason": reasonText, "note": note, "refund_cents": amt}); err != nil {
		return 0, err
	}
	return amt, nil
}

func coalesceNote(n string) string {
	if n == "" {
		return ""
	}
	return "（备注：" + n + "）"
}

// applyCourtMove 创建新订单承接预约，旧订单置 moved 并双向关联。
func (s *Service) applyCourtMove(ctx context.Context, tx pgx.Tx, b *store.Booking, maint store.Maintenance,
	in RelocationDecision) (*store.Booking, int64, int64, []store.DevicePermissionSync, *store.VacateReminder, error) {
	st, en := b.StartTime, b.EndTime
	if in.Choice == "postpone" {
		if in.StartTime == nil || in.EndTime == nil {
			return nil, 0, 0, nil, nil, ce("BAD_TIME", "顺延必须提供新的开始/结束时间", ErrValidation)
		}
		st, en = in.StartTime.UTC().Truncate(time.Minute), in.EndTime.UTC().Truncate(time.Minute)
		now := s.now(ctx)
		if !st.After(now) {
			return nil, 0, 0, nil, nil, ce("PAST_SLOT", "顺延时间必须晚于当前时间", ErrValidation)
		}
	}
	if in.CourtID <= 0 {
		return nil, 0, 0, nil, nil, ce("BAD_COURT", "请选择目标场地", ErrValidation)
	}
	if in.CourtID == b.CourtID {
		return nil, 0, 0, nil, nil, ce("BAD_COURT", "目标场地与旧场地相同", ErrValidation)
	}
	venue, err := s.loadVenue(ctx, tx, b.VenueID)
	if err != nil {
		return nil, 0, 0, nil, nil, err
	}
	court, err := s.loadCourt(ctx, tx, in.CourtID)
	if err != nil {
		return nil, 0, 0, nil, nil, err
	}
	if court.VenueID != venue.ID {
		return nil, 0, 0, nil, nil, ce("COURT_VENUE_MISMATCH", "目标场地不属于该场馆", ErrValidation)
	}
	if court.Status != "active" {
		return nil, 0, 0, nil, nil, ce("COURT_INACTIVE", "目标场地当前不可用", ErrMaintenance)
	}
	durMin := int(en.Sub(st).Minutes())
	if durMin < 30 || durMin > 240 {
		return nil, 0, 0, nil, nil, ce("BAD_DURATION", "预约时长需在 30~240 分钟之间", ErrValidation)
	}
	capacity := court.Capacity
	if capacity == 0 {
		capacity = venue.DefaultCourtCapacity
	}
	if b.PartySize > capacity {
		return nil, 0, 0, nil, nil, ce("CAPACITY", "目标场地容量不足", ErrValidation)
	}
	cExtra, err := loadExtraSlots(ctx, tx, venue.ID, court.ID, st.Format("2006-01-02"))
	if err != nil {
		return nil, 0, 0, nil, nil, err
	}
	for mm := 0; mm < durMin; mm++ {
		if !courtOpenAt(venue, cExtra, st.Add(time.Duration(mm)*time.Minute)) {
			return nil, 0, 0, nil, nil, ce("OUTSIDE_HOURS", "目标时段不在营业或临时加场范围内", ErrOutsideHours)
		}
	}
	free, err := courtFreeAt(ctx, tx, venue, court.ID, st, en)
	if err != nil {
		return nil, 0, 0, nil, nil, err
	}
	if !free {
		return nil, 0, 0, nil, nil, ce("SLOT_TAKEN", "目标场地时段不可用（已被预约或维护）", ErrConflict)
	}

	windows, err := s.peakWindows(ctx, tx, venue.ID)
	if err != nil {
		return nil, 0, 0, nil, nil, err
	}
	policy, err := s.getMemberPolicy(ctx, tx, venue.ID, b.MemberTypeSnap)
	if err != nil {
		return nil, 0, 0, nil, nil, err
	}
	bd := quote(priceInput{venue: venue, courtPriceCents: court.PriceCentsPerHour, policy: policy,
		peakWindows: windows, start: st, end: en,
		racketCount: b.RacketCount, shuttleCount: b.ShuttleCount,
		availableBenefitM: b.BenefitMinutes})
	oldNet := b.CourtFeeCents - b.DiscountCents - b.BenefitCents
	newNet := bd.CourtFeeCents - bd.DiscountCents - bd.BenefitCents
	diff := newNet - oldNet
	venueBorne, memberRefund := int64(0), int64(0)
	if diff > 0 {
		venueBorne = diff // 更贵差价场馆承担，会员不加价
	} else if diff < 0 {
		memberRefund = -diff // 更便宜差额退还
	}
	newPayable := bd.AddonCents + newNet

	entryCode, err := genEntryCode(ctx, tx)
	if err != nil {
		return nil, 0, 0, nil, nil, err
	}
	now := s.now(ctx)
	// 新订单初始状态：旧 paid→paid、旧 warming/in_use→warming（旧场地灯已强制关闭，
	// 到新场地后重新开灯；到场证据一并迁移，不会被判爽约）；顺延→paid（新时段未到场）
	newStatus := "paid"
	if (b.Status == "warming" || b.Status == "in_use") && in.Choice != "postpone" {
		newStatus = "warming"
	}
	capSnap := capacity
	noteTxt := fmt.Sprintf("由订单 %s 因场地维护换场（维护单 #%d，%s）",
		b.BookingNo, derefInt64(b.RelocationMaintenanceID), maint.Reason)
	if in.Choice == "postpone" {
		noteTxt = fmt.Sprintf("由订单 %s 因场地维护顺延（维护单 #%d，%s）",
			b.BookingNo, derefInt64(b.RelocationMaintenanceID), maint.Reason)
	}
	var firstGate, gateWaiting, frontCheckin, firstPresence, warmCopy, effStart any
	if newStatus == "warming" || newStatus == "in_use" {
		// 已在旧场地到场的证据随换场迁移，避免被误判爽约
		firstGate = b.FirstGateAt
		gateWaiting = b.GateWaitingAt
		frontCheckin = b.FrontCheckinAt
		firstPresence = b.FirstPresenceAt
		warmCopy = now
	}
	if newStatus == "in_use" {
		effStart = now
	}
	row := tx.QueryRow(ctx, `insert into bookings(
		booking_no,venue_id,court_id,member_id,member_type_snap,member_name_snap,
		party_size,capacity_snapshot,start_time,end_time,
		rent_racket,racket_count,buy_shuttle,shuttle_count,
		court_fee_cents,addon_cents,discount_cents,benefit_cents,benefit_minutes,
		benefit_monthly_minutes,benefit_grant_minutes,
		payable_cents,paid_cents,refunded_cents,status,entry_code,paid_at,
		first_gate_at,gate_waiting_at,front_checkin_at,first_presence_at,
		warmup_at,effective_start,comp_bonus_minutes,
		relocation_status,relocation_maintenance_id,original_court_id,relocated_from_booking_id,
		venue_borne_cents,note,created_source,updated_at)
		values(
		 $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,
		 $15,$16,$17,$18,$19,$20,$21,
		 $22,$23,$24,$25,$26,now(),
		 $27,$28,$29,$30,
		 $31,$32,$33,
		 'resolved',$34,$35,$36,
		 $37,$38,$39,now())
		returning id`,
		genBookingNo(now), venue.ID, court.ID, b.MemberID, b.MemberTypeSnap, b.MemberNameSnap,
		b.PartySize, capSnap, st, en,
		b.RentRacket, b.RacketCount, b.BuyShuttle, b.ShuttleCount,
		bd.CourtFeeCents, bd.AddonCents, bd.DiscountCents, bd.BenefitCents, bd.BenefitMinutesUsed,
		b.BenefitMonthlyMinutes, b.BenefitGrantMinutes,
		newPayable, newPayable, memberRefund, newStatus, entryCode,
		firstGate, gateWaiting, frontCheckin, firstPresence,
		warmCopy, effStart, b.CompBonusMinutes,
		b.RelocationMaintenanceID, b.OriginalCourtID, b.ID,
		venueBorne, noteTxt, in.Source)
	var id int64
	if err := row.Scan(&id); err != nil {
		return nil, 0, 0, nil, nil, err
	}
	newB, err := scanBooking(tx.QueryRow(ctx, `select `+bookingColumns+` from bookings where id=$1`, id))
	if err != nil {
		return nil, 0, 0, nil, nil, err
	}
	// 会员原实付金额随换场结转（场馆承担的差价另记 compensation，两者合计=新单应付）
	memberPaid := b.PaidCents - b.RefundedCents
	if memberPaid > 0 {
		if _, err := tx.Exec(ctx,
			`insert into payments(booking_id,kind,amount_cents,method,status,transaction_no,reason,created_source,created_by)
			 values($1,'payment',$2,'transferred','succeeded',$3,$4,'operator',$5)`,
			id, memberPaid, fmt.Sprintf("TR%d-%06d", id, random6()),
			"由订单 "+b.BookingNo+" 因场地维护换场结转（会员原付款）", in.Actor); err != nil {
			return nil, 0, 0, nil, nil, err
		}
	}
	// 到场证据随换场迁移：复制闸机放行/等待与前台签到记录，防止新订单被误判爽约
	if _, err := tx.Exec(ctx,
		`insert into gate_records(booking_id,entry_code,device_id,result,reason,scanned_at)
		 select $2,$3,device_id,result,
		        reason||case when reason='' then '' else '；' end||'换场自订单 '||$4,
		        scanned_at
		 from gate_records where booking_id=$1 and result in ('allowed','waiting')`,
		b.ID, id, entryCode, b.BookingNo); err != nil {
		return nil, 0, 0, nil, nil, err
	}
	if _, err := tx.Exec(ctx,
		`insert into front_records(booking_id,staff_id,action,detail,created_at)
		 select $2,staff_id,action,detail,created_at
		 from front_records where booking_id=$1 and action='checkin'`,
		b.ID, id); err != nil {
		return nil, 0, 0, nil, nil, err
	}

	// 旧订单收尾：moved + 维护原因留痕 + 关联新订单
	maintText := "场地临时维护"
	if maint.ID != 0 {
		maintText = fmt.Sprintf("场地临时维护（维护单 #%d）：%s", maint.ID, maint.Reason)
	}
	if _, err := tx.Exec(ctx,
		`update bookings set status='moved',verdict='moved',judge_source='relocation',judged_at=now(),
		 relocation_status='resolved',relocated_to_booking_id=$2,relocation_due_at=null,
		 note=case when note='' then $3 else note||E'\n'||$3 end,
		 version=version+1,updated_at=now()
		 where id=$1`, b.ID, id, maintText+"，已换场至 "+court.CourtNo+" 号场地（新订单 #"+fmt.Sprintf("%d", id)+"）。"+coalesceNote(in.Note)); err != nil {
		return nil, 0, 0, nil, nil, err
	}
	// 旧场地若灯还亮着（in_use 被打断），关灯留痕
	if b.LightOffAt == nil && (b.WarmupAt != nil || b.LightOnAt != nil) {
		if _, err := tx.Exec(ctx,
			`insert into light_records(booking_id,court_id,action,command_source,success,detail,occurred_at)
			 values($1,$2,'off','operator',true,'court relocated, lights off old court',$3)`,
			b.ID, b.CourtID, now); err != nil {
			return nil, 0, 0, nil, nil, err
		}
		if _, err := tx.Exec(ctx, `update bookings set light_off_at=$2 where id=$1`, b.ID, now); err != nil {
			return nil, 0, 0, nil, nil, err
		}
	}

	// 差价财务
	if venueBorne > 0 {
		if _, err := tx.Exec(ctx,
			`insert into payments(booking_id,kind,amount_cents,method,status,transaction_no,reason,created_source,created_by)
			 values($1,'compensation',$2,'venue_borne','succeeded',$3,$4,'operator',$5)`,
			id, venueBorne, fmt.Sprintf("VB%d-%06d", id, random6()),
			"场馆承担换至高价场地差价（"+maintText+"）", in.Actor); err != nil {
			return nil, 0, 0, nil, nil, err
		}
	}
	if memberRefund > 0 {
		if _, err := tx.Exec(ctx,
			`insert into payments(booking_id,kind,amount_cents,method,status,transaction_no,reason,created_source,created_by)
			 values($1,'refund',$2,'mock','succeeded',$3,$4,$5,$6)`,
			id, memberRefund, fmt.Sprintf("RF%d-%06d", id, random6()),
			"换至低价场地差价退回（"+maintText+"）", in.Source, in.Actor); err != nil {
			return nil, 0, 0, nil, nil, err
		}
	}

	// 闸机/灯控权限同步：旧场地 revoke，新场地 grant
	var syncs []store.DevicePermissionSync
	oldReason := "换场确认，收回旧场地权限（" + maintText + "）"
	if err := syncDevicePermission(ctx, tx, b, b.CourtID, b.EntryCode, "revoke", oldReason, in.Source, in.Actor); err != nil {
		return nil, 0, 0, nil, nil, err
	}
	newReason := "换场确认，授权新场地 " + court.CourtNo
	if err := syncDevicePermission(ctx, tx, newB, court.ID, entryCode, "grant", newReason, in.Source, in.Actor); err != nil {
		return nil, 0, 0, nil, nil, err
	}
	for _, dt := range []string{"gate", "light"} {
		if rows, e := tx.Query(ctx,
			`select id,booking_id,court_id,entry_code,device_type,permission,reason,sync_status,created_at
			 from device_permission_sync where booking_id in ($1,$2) and device_type=$3 order by id`,
			b.ID, id, dt); e == nil {
			for rows.Next() {
				var sy store.DevicePermissionSync
				if e2 := rows.Scan(&sy.ID, &sy.BookingID, &sy.CourtID, &sy.EntryCode, &sy.DeviceType,
					&sy.Permission, &sy.Reason, &sy.SyncStatus, &sy.CreatedAt); e2 == nil {
					syncs = append(syncs, sy)
				}
			}
			rows.Close()
		}
	}

	// 已扫码/签到进入旧场地（存在到场证据）→ 生成清场提醒（防止被误判为用户占场）
	var vr *store.VacateReminder
	if b.FirstGateAt != nil || b.FirstPresenceAt != nil {
		v, e := s.ensureVacateReminder(ctx, tx, b, &maint, in.Source, in.Actor)
		if e != nil {
			return nil, 0, 0, nil, nil, e
		}
		vr = v
	}
	return newB, venueBorne, memberRefund, syncs, vr, nil
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// syncDevicePermission 写入一条闸机/灯控权限同步记录（旧场地 revoke / 新场地 grant），
// 每个场地两类设备各一条。
func syncDevicePermission(ctx context.Context, tx pgx.Tx, b *store.Booking, courtID int64, entryCode, permission, reason, source string, actor *int64) error {
	for _, dt := range []string{"gate", "light"} {
		if _, err := tx.Exec(ctx,
			`insert into device_permission_sync(booking_id,court_id,entry_code,device_type,permission,reason,sync_status,created_by)
			 values($1,$2,$3,$4,$5,$6,'synced',$7)`,
			b.ID, courtID, entryCode, dt, permission, reason, actor); err != nil {
			return err
		}
		if err := emitEvent(ctx, tx, "device.permission_synced", source, &b.ID, &b.VenueID, &courtID, actor,
			map[string]any{"device_type": dt, "permission": permission, "entry_code": entryCode, "reason": reason}); err != nil {
			return err
		}
	}
	return nil
}

// ensureVacateReminder 生成旧场地清场提醒（幂等：一单一条 pending）。
func (s *Service) ensureVacateReminder(ctx context.Context, tx pgx.Tx, b *store.Booking, maint *store.Maintenance, source string, actor *int64) (*store.VacateReminder, error) {
	var existing int
	if err := tx.QueryRow(ctx,
		`select count(*) from vacate_reminders where booking_id=$1 and status='pending'`, b.ID).Scan(&existing); err != nil {
		return nil, err
	}
	if existing > 0 {
		var vr store.VacateReminder
		err := tx.QueryRow(ctx,
			`select id,booking_id,venue_id,court_id,maintenance_id,message,status,cleared_by,reminded_at,cleared_at,created_at
			 from vacate_reminders where booking_id=$1 and status='pending' limit 1`, b.ID).
			Scan(&vr.ID, &vr.BookingID, &vr.VenueID, &vr.CourtID, &vr.MaintenanceID, &vr.Message,
				&vr.Status, &vr.ClearedBy, &vr.RemindedAt, &vr.ClearedAt, &vr.CreatedAt)
		return &vr, err
	}
	reason := "场地临时维护"
	var mid *int64
	if maint != nil && maint.ID != 0 {
		reason = fmt.Sprintf("场地临时维护（维护单 #%d）：%s", maint.ID, maint.Reason)
		mid = &maint.ID
	}
	msg := fmt.Sprintf("请立即离开 %d 号旧场地（%s），前往新场地或联系前台；本条为场馆维护清场，不计占场/爽约。",
		b.CourtID, reason)
	var vr store.VacateReminder
	err := tx.QueryRow(ctx,
		`insert into vacate_reminders(booking_id,venue_id,court_id,maintenance_id,message)
		 values($1,$2,$3,$4,$5)
		 returning id,booking_id,venue_id,court_id,maintenance_id,message,status,cleared_by,reminded_at,cleared_at,created_at`,
		b.ID, b.VenueID, b.CourtID, mid, msg).
		Scan(&vr.ID, &vr.BookingID, &vr.VenueID, &vr.CourtID, &vr.MaintenanceID, &vr.Message,
			&vr.Status, &vr.ClearedBy, &vr.RemindedAt, &vr.ClearedAt, &vr.CreatedAt)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`update bookings set vacate_reminded_at=coalesce(vacate_reminded_at,now()),version=version+1 where id=$1`, b.ID); err != nil {
		return nil, err
	}
	if err := emitEvent(ctx, tx, "court.vacate_reminded", source, &b.ID, &b.VenueID, &b.CourtID, actor,
		map[string]any{"message": msg, "maintenance_id": mid}); err != nil {
		return nil, err
	}
	return &vr, nil
}

// ---------- 逾期未选择：自动场馆原因退款 ----------

// AutoResolvePendingRelocations 超过 relocation_due_at 仍未确认方案的订单，
// 按场馆原因全额退款，避免悬挂占场。
func (s *Service) AutoResolvePendingRelocations(ctx context.Context) (int, error) {
	now := s.now(ctx)
	rows, err := s.pool().Query(ctx,
		`select id from bookings
		 where status='relocation_pending' and relocation_due_at is not null and relocation_due_at < $1
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
	done := 0
	for _, id := range ids {
		_, err := s.ResolveRelocation(ctx, id, RelocationDecision{Choice: "refund",
			Source: "system", Note: "逾换场方案确认截止时间未选择，系统按场馆原因全额退款"})
		if err == nil {
			done++
		}
	}
	return done, nil
}

// ---------- 前台清场提醒处理 ----------

func (s *Service) ListVacateReminders(ctx context.Context, status string, limit int) ([]store.VacateReminder, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `select id,booking_id,venue_id,court_id,maintenance_id,message,status,cleared_by,reminded_at,cleared_at,created_at
		from vacate_reminders`
	args := []any{}
	if status != "" {
		q += ` where status=$1 order by reminded_at desc limit $2`
		args = append(args, status, limit)
	} else {
		q += ` order by reminded_at desc limit $1`
		args = append(args, limit)
	}
	rows, err := s.pool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.VacateReminder
	for rows.Next() {
		var vr store.VacateReminder
		if err := rows.Scan(&vr.ID, &vr.BookingID, &vr.VenueID, &vr.CourtID, &vr.MaintenanceID, &vr.Message,
			&vr.Status, &vr.ClearedBy, &vr.RemindedAt, &vr.ClearedAt, &vr.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, vr)
	}
	return out, rows.Err()
}

// ClearVacateReminder 前台确认用户已离开旧场地，关闭清场提醒。
func (s *Service) ClearVacateReminder(ctx context.Context, id, staffID int64) (*store.VacateReminder, error) {
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`update vacate_reminders set status='cleared',cleared_by=$2,cleared_at=now()
			 where id=$1 and status='pending'`, id, staffID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ce("NOT_FOUND", "清场提醒不存在或已关闭", ErrNotFound)
		}
		var bookingID, venueID, courtID int64
		if err := tx.QueryRow(ctx,
			`select booking_id,venue_id,court_id from vacate_reminders where id=$1`, id).
			Scan(&bookingID, &venueID, &courtID); err != nil {
			return err
		}
		return emitEvent(ctx, tx, "court.vacate_cleared", "front", &bookingID, &venueID, &courtID, &staffID,
			map[string]any{"reminder_id": id})
	})
	if err != nil {
		return nil, err
	}
	var vr store.VacateReminder
	err = s.pool().QueryRow(ctx,
		`select id,booking_id,venue_id,court_id,maintenance_id,message,status,cleared_by,reminded_at,cleared_at,created_at
		 from vacate_reminders where id=$1`, id).
		Scan(&vr.ID, &vr.BookingID, &vr.VenueID, &vr.CourtID, &vr.MaintenanceID, &vr.Message,
			&vr.Status, &vr.ClearedBy, &vr.RemindedAt, &vr.ClearedAt, &vr.CreatedAt)
	return &vr, err
}
