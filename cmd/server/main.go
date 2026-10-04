package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"gymbadminton/internal/api"
	"gymbadminton/internal/config"
	"gymbadminton/internal/seed"
	"gymbadminton/internal/service"
	"gymbadminton/internal/store"
)

func main() {
	ctx := context.Background()
	cfg := config.Load()

	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("连接 PostgreSQL 失败: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		log.Fatalf("数据库迁移失败: %v", err)
	}
	locker := store.NewLocker(cfg.RedisAddr)
	{
		deadline := time.Now().Add(60 * time.Second)
		var lastErr error
		for time.Now().Before(deadline) {
			if lastErr = locker.Ping(ctx); lastErr == nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
		if lastErr != nil {
			log.Fatalf("连接 Redis 失败: %v", lastErr)
		}
	}
	if err := seed.Seed(ctx, db.Pool); err != nil {
		log.Fatalf("种子数据写入失败: %v", err)
	}

	clock := &service.Clock{RDB: locker.RDB, Allow: cfg.AllowTestClock}
	svc := service.New(db, locker, clock, cfg.TokenSecret)
	srv := api.NewServer(svc, cfg.TokenSecret, cfg.AllowTestClock)

	// 后台周期任务：支付超时释放 / 灯控预热与自动开关 / 爽约裁决
	if cfg.EnableBgJobs {
		go runJobs(ctx, svc, 20*time.Second)
	} else {
		log.Printf("后台周期任务已关闭（DISABLE_BG_JOBS=1），请通过 /internal/jobs/* 手动触发")
	}

	log.Printf("服务启动：http://0.0.0.0%s  Swagger: http://localhost%s/swagger/", cfg.HTTPAddr, cfg.HTTPAddr)
	if err := http.ListenAndServe(cfg.HTTPAddr, srv.Mux); err != nil {
		log.Fatal(err)
	}
}

func runJobs(ctx context.Context, svc *service.Service, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	tick := func() {
		if n, err := svc.ExpireUnpaid(ctx); err != nil {
			log.Printf("job expire-unpaid error: %v", err)
		} else if n > 0 {
			log.Printf("释放支付超时订单 %d 个", n)
		}
		if n, err := svc.AutoLightControl(ctx); err != nil {
			log.Printf("job auto-light error: %v", err)
		} else if n > 0 {
			log.Printf("灯控自动处理 %d 次", n)
		}
		if n, err := svc.JudgeNoShows(ctx); err != nil {
			log.Printf("job judge-noshow error: %v", err)
		} else if n > 0 {
			log.Printf("爽约自动裁决 %d 单", n)
		}
		if n, err := svc.AutoResolvePendingRelocations(ctx); err != nil {
			log.Printf("job relocation-timeout error: %v", err)
		} else if n > 0 {
			log.Printf("换场逾期未决自动退款 %d 单", n)
		}
	}
	tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
