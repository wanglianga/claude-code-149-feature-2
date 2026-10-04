package seed

import (
	"context"
	"fmt"

	"gymbadminton/internal/auth"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Seed 幂等写入演示数据。所有账号密码见 README。
func Seed(ctx context.Context, pool *pgxpool.Pool) error {
	members := []struct{ phone, name, pass, mtype string }{
		{"13800000001", "张伟（普通会员）", "123456", "normal"},
		{"13800000002", "李娜（银卡会员）", "123456", "silver"},
		{"13800000003", "王强（金卡会员）", "123456", "gold"},
	}
	for _, m := range members {
		hash, err := auth.HashPassword(m.pass)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx,
			`insert into members(phone,name,password_hash,member_type)
			 values($1,$2,$3,$4) on conflict (phone) do nothing`,
			m.phone, m.name, hash, m.mtype); err != nil {
			return err
		}
	}

	var venueID int64
	err := pool.QueryRow(ctx,
		`insert into venues(name,address,price_cents_per_hour,peak_multiplier,overtime_multiplier)
		 values('城市体育馆 · 羽毛球馆','市中区体育路 1 号',6000,1.6667,1.5)
		 on conflict do nothing returning id`).Scan(&venueID)
	if err != nil {
		// 已存在则查出 id
		if err = pool.QueryRow(ctx, `select id from venues where name='城市体育馆 · 羽毛球馆' limit 1`).Scan(&venueID); err != nil {
			return err
		}
	}

	staff := []struct{ user, pass, name, role string }{
		{"operator", "operator123", "场馆运营-赵经理", "operator"},
		{"front", "front123", "前台-孙小梅", "front"},
		{"cs", "cs123", "客服-周明", "cs"},
	}
	for _, st := range staff {
		hash, err := auth.HashPassword(st.pass)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx,
			`insert into staff(username,password_hash,name,role,venue_id)
			 values($1,$2,$3,$4,$5) on conflict (username) do nothing`,
			st.user, hash, st.name, st.role, venueID); err != nil {
			return err
		}
	}

	for i := 1; i <= 8; i++ {
		no := fmt.Sprintf("%02d", i)
		if _, err := pool.Exec(ctx,
			`insert into courts(venue_id,court_no,capacity) values($1,$2,6)
			 on conflict (venue_id,court_no) do nothing`, venueID, no); err != nil {
			return err
		}
	}
	// VIP 场地（单场地价格覆盖：90 元/小时）：用于「换到更高价场地，差价场馆承担」演示
	for _, no := range []string{"09", "10"} {
		if _, err := pool.Exec(ctx,
			`insert into courts(venue_id,court_no,capacity,price_cents_per_hour) values($1,$2,6,9000)
			 on conflict (venue_id,court_no) do update set price_cents_per_hour=excluded.price_cents_per_hour`,
			venueID, no); err != nil {
			return err
		}
	}

	policies := []struct {
		typ             string
		discount        int
		freeHours       float64
		maxAdvanceHours int
	}{
		{"normal", 100, 0, 24},
		{"silver", 90, 2, 72},
		{"gold", 80, 5, 168},
	}
	for _, p := range policies {
		if _, err := pool.Exec(ctx,
			`insert into member_type_policies(venue_id,member_type,discount_percent,free_hours_per_month,max_advance_hours)
			 values($1,$2,$3,$4,$5)
			 on conflict (venue_id,member_type) do update
			 set discount_percent=$3,free_hours_per_month=$4,max_advance_hours=$5`,
			venueID, p.typ, p.discount, p.freeHours, p.maxAdvanceHours); err != nil {
			return err
		}
	}

	// 高峰：工作日 18-21；周末 9-12、14-17
	peak := []struct {
		wd     int
		st, en string
	}{}
	for wd := 1; wd <= 5; wd++ {
		peak = append(peak, struct {
			wd     int
			st, en string
		}{wd, "18:00", "21:00"})
	}
	for _, wd := range []int{0, 6} {
		peak = append(peak, struct {
			wd     int
			st, en string
		}{wd, "09:00", "12:00"})
		peak = append(peak, struct {
			wd     int
			st, en string
		}{wd, "14:00", "17:00"})
	}
	for _, p := range peak {
		if _, err := pool.Exec(ctx,
			`insert into peak_windows(venue_id,weekday,start_time,end_time)
			 select $1,$2,$3::time,$4::time
			 where not exists(select 1 from peak_windows where venue_id=$1 and weekday=$2 and start_time=$3::time)`,
			venueID, p.wd, p.st, p.en); err != nil {
			return err
		}
	}

	// 退订规则
	rules := []struct {
		peak    bool
		hours   float64
		percent int
	}{
		{false, 24, 100}, {false, 6, 80}, {false, 2, 50}, {false, 0, 0},
		{true, 24, 80}, {true, 4, 50}, {true, 1, 20}, {true, 0, 0},
	}
	for _, r := range rules {
		if _, err := pool.Exec(ctx,
			`insert into cancellation_rules(venue_id,is_peak,min_hours_before,refund_percent)
			 values($1,$2,$3,$4)
			 on conflict (venue_id,is_peak,min_hours_before) do update set refund_percent=$4`,
			venueID, r.peak, r.hours, r.percent); err != nil {
			return err
		}
	}
	return nil
}
