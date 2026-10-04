package service

import (
	"context"

	"gymbadminton/internal/store"
)

// EndInput 映射 API 的结算方式
func EndInput(method string) EndSessionInput { return EndSessionInput{Method: method} }

// ResolutionInput 映射 API 的客服处理动作
func ResolutionInput(action string, refund int64, comp int, note string, staffID int64) DisputeResolution {
	return DisputeResolution{
		Action: action, RefundCents: refund, CompMinutes: comp,
		Note: note, StaffID: staffID,
	}
}

// ListMaintenance 列出（可按场馆过滤）维护记录
func (s *Service) ListMaintenance(ctx context.Context, venueID int64) ([]store.Maintenance, error) {
	q := `select ` + maintenanceColumns + ` from maintenance_windows`
	args := []any{}
	if venueID > 0 {
		q += ` where venue_id=$1 order by start_time desc`
		args = append(args, venueID)
	} else {
		q += ` order by start_time desc limit 200`
	}
	rows, err := s.pool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Maintenance
	for rows.Next() {
		m, err := scanMaintenance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
