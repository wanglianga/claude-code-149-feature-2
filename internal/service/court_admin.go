package service

import (
	"context"

	"gymbadminton/internal/store"

	"github.com/jackc/pgx/v5"
)

// SetCourtPrice 设置场地专属平时价（分/小时），0 表示恢复场馆基础价。
// 用于 VIP 场地差异化定价，也是换场差价计算的基础。
func (s *Service) SetCourtPrice(ctx context.Context, courtID int64, priceCents int64, staffID int64) (*store.Court, error) {
	err := inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		var venueID int64
		if err := tx.QueryRow(ctx, `select venue_id from courts where id=$1`, courtID).Scan(&venueID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`update courts set price_cents_per_hour=nullif($2,0) where id=$1`, courtID, priceCents); err != nil {
			return err
		}
		return emitEvent(ctx, tx, "court.price_updated", "operator", nil, &venueID, &courtID, &staffID,
			map[string]any{"price_cents_per_hour": priceCents})
	})
	if err != nil {
		return nil, err
	}
	var c *store.Court
	err = inTx(ctx, s.pool(), func(tx pgx.Tx) error {
		var e error
		c, e = s.loadCourt(ctx, tx, courtID)
		return e
	})
	return c, err
}
