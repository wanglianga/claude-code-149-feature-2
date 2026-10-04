package api

import (
	"net/http"
	"time"

	"gymbadminton/internal/service"
	"gymbadminton/internal/store"
)

// ---------- 维护锁定 ----------

func (s *Server) handleMaintenances(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// 员工查询维护列表（可按场馆/日期过滤）
		if _, ok := s.requireStaff(w, r); !ok {
			return
		}
		venueID, _ := parseID64(r.URL.Query().Get("venue_id"))
		list, err := s.Svc.ListMaintenance(r.Context(), venueID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, map[string]any{"maintenances": list})
	case http.MethodPost:
		c, ok := s.requireStaff(w, r, "operator")
		if !ok {
			return
		}
		var req struct {
			VenueID int64  `json:"venue_id"`
			CourtID int64  `json:"court_id"`
			Start   string `json:"start_time"`
			End     string `json:"end_time"`
			Reason  string `json:"reason"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		st, err := parseTime(req.Start)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_TIME", "message": err.Error()}})
			return
		}
		en, err := parseTime(req.End)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_TIME", "message": err.Error()}})
			return
		}
		m, err := s.Svc.CreateMaintenance(r.Context(), service.MaintenanceInput{
			VenueID: req.VenueID, CourtID: req.CourtID,
			StartTime: st, EndTime: en, Reason: req.Reason, StaffID: c.Sub,
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 201, m)
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// /api/v1/operator/maintenances/{id}/finish | /accept
// finish：运营上报维护完工（operator）；accept：前台验收（front），通过才重新开放预约。
func (s *Server) dispatchMaintenance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	parts := segs("/api/v1/operator/maintenances", r.URL.Path)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	id, err := parseID64(parts[0])
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_ID", "message": "非法维护 ID"}})
		return
	}
	switch parts[1] {
	case "finish":
		c, ok := s.requireStaff(w, r, "operator")
		if !ok {
			return
		}
		m, err := s.Svc.FinishMaintenance(r.Context(), id, c.Sub)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, m)
	case "accept":
		c, ok := s.requireStaff(w, r, "front")
		if !ok {
			return
		}
		var req struct {
			Pass *bool  `json:"pass"`
			Note string `json:"note"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		pass := req.Pass == nil || *req.Pass
		m, err := s.Svc.AcceptMaintenance(r.Context(), id, c.Sub, pass, req.Note)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, m)
	default:
		http.NotFound(w, r)
	}
}

// GET /api/v1/operator/relocation-offers?status=&venue_id= 员工查看换场方案
func (s *Server) handleRelocationOffers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := s.requireStaff(w, r); !ok {
		return
	}
	venueID, _ := parseID64(r.URL.Query().Get("venue_id"))
	list, err := s.Svc.ListRelocationOffers(r.Context(), venueID, r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, map[string]any{"offers": list})
}

// GET /api/v1/front/notifications?status=pending 前台/运营待办（清场提醒等）
func (s *Server) handleFrontNotifications(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	c, ok := s.requireStaff(w, r, "front", "operator")
	if !ok {
		return
	}
	venueID, _ := parseID64(r.URL.Query().Get("venue_id"))
	if venueID == 0 {
		venueID = c.VenueID
	}
	list, err := s.Svc.ListNotifications(r.Context(), venueID, r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, map[string]any{"notifications": list})
}

// POST /api/v1/front/notifications/{id}/ack 前台确认处理（如旧场地清场完成）
func (s *Server) dispatchFrontNotification(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	c, ok := s.requireStaff(w, r, "front", "operator")
	if !ok {
		return
	}
	parts := segs("/api/v1/front/notifications", r.URL.Path)
	if len(parts) != 2 || parts[1] != "ack" {
		http.NotFound(w, r)
		return
	}
	id, err := parseID64(parts[0])
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_ID", "message": "非法通知 ID"}})
		return
	}
	n, err := s.Svc.AckNotification(r.Context(), id, c.Sub)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, n)
}

// ---------- 临时加场 ----------

func (s *Server) handleExtraSlots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	c, ok := s.requireStaff(w, r, "operator")
	if !ok {
		return
	}
	var req struct {
		VenueID  int64  `json:"venue_id"`
		CourtID  *int64 `json:"court_id"`
		Date     string `json:"slot_date"`
		OpensAt  string `json:"opens_at"`
		ClosesAt string `json:"closes_at"`
		Note     string `json:"note"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := time.Parse("2006-01-02", req.Date); err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_DATE", "message": "日期格式 YYYY-MM-DD"}})
		return
	}
	for _, v := range []string{req.OpensAt, req.ClosesAt} {
		if _, err := time.Parse("15:04", v); err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_TIME", "message": "时间格式 HH:MM"}})
			return
		}
	}
	slot, err := s.Svc.CreateExtraSlot(r.Context(), store.ExtraSlot{
		VenueID: req.VenueID, CourtID: req.CourtID, SlotDate: req.Date,
		OpensAt: req.OpensAt, ClosesAt: req.ClosesAt, Note: req.Note,
	}, c.Sub)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, slot)
}

// ---------- 退订规则 ----------

func (s *Server) handleCancelRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	c, ok := s.requireStaff(w, r, "operator")
	if !ok {
		return
	}
	var req struct {
		VenueID        int64   `json:"venue_id"`
		IsPeak         bool    `json:"is_peak"`
		MinHoursBefore float64 `json:"min_hours_before"`
		RefundPercent  int     `json:"refund_percent"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.Svc.UpsertCancelRule(r.Context(), req.VenueID, req.IsPeak, req.MinHoursBefore, req.RefundPercent, int(c.Sub)); err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, map[string]string{"status": "ok"})
}
