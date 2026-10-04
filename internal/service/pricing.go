package service

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"gymbadminton/internal/store"
)

// 计费单位：分。所有费率均已分为基础。

type priceInput struct {
	venue             *store.Venue
	courtPriceCents   int64 // 场地专属价（分/小时），0 时用场馆基础价（如 VIP 场地）
	policy            store.MemberPolicy
	peakWindows       []store.PeakWindow
	start, end        time.Time
	racketCount       int
	shuttleCount      int
	availableBenefitM int // 当前可用的权益分钟数（月度剩余 + 赠送券）
}

type priceBreakdown struct {
	IsPeak             bool  `json:"is_peak"`
	DurationMinutes    int   `json:"duration_minutes"`
	NormalMinutes      int   `json:"normal_minutes"`
	PeakMinutes        int   `json:"peak_minutes"`
	CourtFeeCents      int64 `json:"court_fee_cents"`
	AddonCents         int64 `json:"addon_cents"`
	DiscountCents      int64 `json:"discount_cents"`
	BenefitMinutesUsed int   `json:"benefit_minutes_used"`
	BenefitCents       int64 `json:"benefit_cents"`
	PayableCents       int64 `json:"payable_cents"`
}

// isPeakAt 判断某一刻是否落在高峰窗口（按场馆本地时间的星期/钟点）
func isPeakAt(windows []store.PeakWindow, t time.Time) bool {
	wd := int(t.Weekday())
	hm := t.Hour()*60 + t.Minute()
	for _, w := range windows {
		if int(w.Weekday) != wd {
			continue
		}
		sh := clockHM(w.StartTime)
		eh := clockHM(w.EndTime)
		if hm >= sh && hm < eh {
			return true
		}
	}
	return false
}

func parseHHMM(hhmm string, h, m *int) (int, error) {
	parts := strings.Split(strings.TrimSpace(hhmm), ":")
	if len(parts) < 2 {
		return 0, fmt.Errorf("bad time %q", hhmm)
	}
	a, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}
	b, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, err
	}
	*h, *m = a, b
	return 0, nil
}

func clockHM(hhmm string) int {
	var h, m int
	if _, err := parseHHMM(hhmm, &h, &m); err != nil {
		return 0
	}
	return h*60 + m
}

// quote 预约下单时的报价（按分钟分段，平峰/高峰分别计价，再叠加会员折扣与权益）
func quote(in priceInput) priceBreakdown {
	out := priceBreakdown{DurationMinutes: int(in.end.Sub(in.start).Minutes())}
	rateNormal := in.venue.PriceCentsPerHour
	if in.courtPriceCents > 0 {
		rateNormal = in.courtPriceCents // 场地差异化定价（换场差价计算的关键）
	}
	ratePeak := int64(math.Round(float64(rateNormal) * in.venue.PeakMultiplier))

	for m := 0; m < out.DurationMinutes; m++ {
		at := in.start.Add(time.Duration(m) * time.Minute)
		if isPeakAt(in.peakWindows, at) {
			out.PeakMinutes++
		} else {
			out.NormalMinutes++
		}
	}
	// 按平/高峰分钟数统一折算（整小时精确，避免逐分钟取整累积误差）
	courtFee := int64(math.Round(float64(out.NormalMinutes)*float64(rateNormal)/60)) +
		int64(math.Round(float64(out.PeakMinutes)*float64(ratePeak)/60))
	out.IsPeak = out.PeakMinutes > 0
	out.CourtFeeCents = courtFee

	out.AddonCents = int64(in.racketCount)*in.venue.RacketFeeCents +
		int64(in.shuttleCount)*in.venue.ShuttleFeeCents

	// 会员折扣（仅场地费）
	if in.policy.DiscountPercent < 100 {
		out.DiscountCents = courtFee - courtFee*int64(in.policy.DiscountPercent)/100
	}
	discountedCourt := courtFee - out.DiscountCents

	// 权益分钟抵扣：按折后每分钟均价抵扣，封顶为折后场地费
	if in.availableBenefitM > 0 && discountedCourt > 0 {
		useM := in.availableBenefitM
		if useM > out.DurationMinutes {
			useM = out.DurationMinutes
		}
		perMinute := float64(discountedCourt) / float64(out.DurationMinutes)
		benefit := int64(math.Round(perMinute * float64(useM)))
		if benefit > discountedCourt {
			benefit = discountedCourt
			// 反推实际用满的分钟数
			for useM > 0 && int64(math.Round(perMinute*float64(useM-1))) >= discountedCourt {
				useM--
			}
		}
		out.BenefitMinutesUsed = useM
		out.BenefitCents = benefit
	}
	out.PayableCents = discountedCourt + out.AddonCents - out.BenefitCents
	if out.PayableCents < 0 {
		out.PayableCents = 0
	}
	return out
}

// overtimeFee 延时费用：超出预约结束（已含补偿时长）的部分，按 15 分钟向上取整，
// 用延时时刻的费率（可能命中高峰）乘延时系数。rateNormal 为场地平时费率（0 时取场馆基础价）。
func overtimeFee(v *store.Venue, windows []store.PeakWindow, baseEnd, actualEnd time.Time, rateNormal int64) (int, int64) {
	if !actualEnd.After(baseEnd) {
		return 0, 0
	}
	if rateNormal <= 0 {
		rateNormal = v.PriceCentsPerHour
	}
	rawMin := int(actualEnd.Sub(baseEnd).Minutes())
	units := (rawMin + 14) / 15
	billMin := units * 15
	rate := rateNormal
	if isPeakAt(windows, actualEnd.Add(-time.Minute)) {
		rate = int64(math.Round(float64(rate) * v.PeakMultiplier))
	}
	fee := int64(math.Round(float64(rate)*float64(billMin)/60*v.OvertimeMultiplier + 0.5))
	return billMin, fee
}

// overlimitFee 拼场超员费用
func overlimitFee(v *store.Venue, partySize, capacity int) (int, int64) {
	extra := partySize - capacity
	if extra <= 0 {
		return 0, 0
	}
	return extra, int64(extra) * v.OverlimitFeeCents
}
