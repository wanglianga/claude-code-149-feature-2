package api

import (
	"net/http"
	"time"

	"gymbadminton/internal/service"
)

// ---------- 维护：场地差异化定价 ----------

// /api/v1/operator/courts/{id}/price —— 设置场地差异化定价（用于 VIP 场地 / 换场差价场景）
func (s *Server) dispatchCourtAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	c, ok := s.requireStaff(w, r, "operator")
	if !ok {
		return
	}
	parts := segs("/api/v1/operator/courts", r.URL.Path)
	if len(parts) != 2 || parts[1] != "price" {
		http.NotFound(w, r)
		return
	}
	id, err := parseID64(parts[0])
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_ID", "message": "非法场地 ID"}})
		return
	}
	var req struct {
		PriceCentsPerHour int64 `json:"price_cents_per_hour"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.PriceCentsPerHour < 0 {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_PRICE", "message": "价格不能为负（0 表示恢复场馆基础价）"}})
		return
	}
	court, err := s.Svc.SetCourtPrice(r.Context(), id, req.PriceCentsPerHour, c.Sub)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, court)
}

// ---------- 临时换场：候选方案 / 用户确认 ----------

func (s *Server) handleRelocationOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	parts := segs("/api/v1/bookings", r.URL.Path)
	// /bookings/{id}/relocation/options
	id, err := parseID64(parts[0])
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_ID", "message": "非法预约 ID"}})
		return
	}
	if _, ok := s.loadOwnedBooking(w, r, id); !ok {
		return
	}
	var st, en *time.Time
	if qs := r.URL.Query().Get("start_time"); qs != "" {
		t, err := parseTime(qs)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_TIME", "message": err.Error()}})
			return
		}
		st = &t
	}
	if qe := r.URL.Query().Get("end_time"); qe != "" {
		t, err := parseTime(qe)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_TIME", "message": err.Error()}})
			return
		}
		en = &t
	}
	if (st == nil) != (en == nil) {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_TIME", "message": "顺延候选需同时提供 start_time 和 end_time"}})
		return
	}
	out, err := s.Svc.RelocationCandidates(r.Context(), id, st, en)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, out)
}

func (s *Server) handleRelocationResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	parts := segs("/api/v1/bookings", r.URL.Path)
	id, err := parseID64(parts[0])
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_ID", "message": "非法预约 ID"}})
		return
	}
	c, ok := s.loadOwnedBooking(w, r, id)
	if !ok {
		return
	}
	var req struct {
		Choice    string `json:"choice"`
		CourtID   int64  `json:"court_id"`
		StartTime string `json:"start_time"`
		EndTime   string `json:"end_time"`
		Note      string `json:"note"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	in := service.RelocationDecision{Choice: req.Choice, CourtID: req.CourtID, Note: req.Note}
	if req.StartTime != "" || req.EndTime != "" {
		st, err := parseTime(req.StartTime)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_TIME", "message": err.Error()}})
			return
		}
		en, err := parseTime(req.EndTime)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_TIME", "message": err.Error()}})
			return
		}
		in.StartTime, in.EndTime = &st, &en
	}
	if c.Kind == "staff" {
		in.Source = "operator"
		actor := c.Sub
		in.Actor = &actor
	} else {
		in.Source = "member"
	}
	res, err := s.Svc.ResolveRelocation(r.Context(), id, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, res)
}

// ---------- 前台：旧场地清场提醒 ----------

func (s *Server) handleVacateReminders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := s.requireStaff(w, r, "front", "operator", "cs"); !ok {
		return
	}
	list, err := s.Svc.ListVacateReminders(r.Context(), r.URL.Query().Get("status"), 100)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, map[string]any{"vacate_reminders": list})
}

func (s *Server) handleVacateClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	c, ok := s.requireStaff(w, r, "front", "operator")
	if !ok {
		return
	}
	parts := segs("/api/v1/front/vacate-reminders", r.URL.Path)
	if len(parts) != 2 || parts[1] != "clear" {
		http.NotFound(w, r)
		return
	}
	id, err := parseID64(parts[0])
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_ID", "message": "非法提醒 ID"}})
		return
	}
	vr, err := s.Svc.ClearVacateReminder(r.Context(), id, c.Sub)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, vr)
}
