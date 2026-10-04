package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gymbadminton/internal/auth"
	"gymbadminton/internal/service"
)

type Server struct {
	Svc        *service.Service
	Secret     string
	AllowClock bool
	Mux        *http.ServeMux
}

func NewServer(svc *service.Service, secret string, allowClock bool) *Server {
	s := &Server{Svc: svc, Secret: secret, AllowClock: allowClock, Mux: http.NewServeMux()}
	s.routes()
	return s
}

// ---------- 响应辅助 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOK(w http.ResponseWriter, v any) { writeJSON(w, http.StatusOK, v) }

func errStatus(err error) (int, string, string) {
	var ce *service.CodeError
	if errors.As(err, &ce) {
		switch {
		case errors.Is(ce.Err, service.ErrNotFound):
			return http.StatusNotFound, ce.Code, ce.Message
		case errors.Is(ce.Err, service.ErrConflict):
			return http.StatusConflict, ce.Code, ce.Message
		case errors.Is(ce.Err, service.ErrValidation):
			return http.StatusBadRequest, ce.Code, ce.Message
		case errors.Is(ce.Err, service.ErrIllegalState):
			return http.StatusConflict, ce.Code, ce.Message
		case errors.Is(ce.Err, service.ErrMaintenance), errors.Is(ce.Err, service.ErrMemberOnly),
			errors.Is(ce.Err, service.ErrOutsideHours), errors.Is(ce.Err, service.ErrAdvanceLimit),
			errors.Is(ce.Err, service.ErrUnpaid), errors.Is(ce.Err, service.ErrTooEarly):
			return http.StatusUnprocessableEntity, ce.Code, ce.Message
		}
		return http.StatusBadRequest, ce.Code, ce.Message
	}
	if errors.Is(err, service.ErrNotFound) {
		return http.StatusNotFound, "NOT_FOUND", "资源不存在"
	}
	return http.StatusInternalServerError, "INTERNAL", err.Error()
}

func writeErr(w http.ResponseWriter, err error) {
	status, code, msg := errStatus(err)
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return &service.CodeError{Code: "BAD_JSON", Message: "请求体 JSON 解析失败: " + err.Error()}
	}
	return nil
}

func parseID64(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("非法 ID")
	}
	return id, nil
}

func parseDay(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, errors.New("日期格式需为 YYYY-MM-DD")
	}
	return t, nil
}

func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02T15:04"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, errors.New("时间格式需为 RFC3339 或 2006-01-02 15:04")
}

// ---------- 认证 ----------

func (s *Server) claims(r *http.Request) (auth.Claims, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return auth.Claims{}, false
	}
	c, err := auth.Parse(s.Secret, strings.TrimPrefix(h, "Bearer "))
	if err != nil {
		return auth.Claims{}, false
	}
	return c, true
}

func (s *Server) requireMember(w http.ResponseWriter, r *http.Request) (auth.Claims, bool) {
	c, ok := s.claims(r)
	if !ok || c.Kind != "member" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "UNAUTHORIZED", "message": "请先以会员身份登录"}})
		return c, false
	}
	return c, true
}

func (s *Server) requireStaff(w http.ResponseWriter, r *http.Request, roles ...string) (auth.Claims, bool) {
	c, ok := s.claims(r)
	if !ok || c.Kind != "staff" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "UNAUTHORIZED", "message": "请先以员工身份登录"}})
		return c, false
	}
	if len(roles) > 0 {
		allowed := false
		for _, role := range roles {
			if c.Role == role {
				allowed = true
			}
		}
		if !allowed {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{"code": "FORBIDDEN", "message": "当前角色无权操作：" + c.Role}})
			return c, false
		}
	}
	return c, true
}

// segs 返回 path 去掉 prefix 后的非空段
func segs(prefix, path string) []string {
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return nil
	}
	return strings.Split(rest, "/")
}

func (s *Server) routes() {
	m := s.Mux
	m.HandleFunc("/api/v1/auth/register", s.handleRegister)
	m.HandleFunc("/api/v1/auth/login/member", s.handleMemberLogin)
	m.HandleFunc("/api/v1/auth/login/staff", s.handleStaffLogin)

	m.HandleFunc("/api/v1/venues", s.handleVenues)
	m.HandleFunc("/api/v1/venues/", s.dispatchVenue)
	m.HandleFunc("/api/v1/bookings", s.handleBookings)
	m.HandleFunc("/api/v1/bookings/", s.dispatchBooking)
	m.HandleFunc("/api/v1/me/bookings", s.handleMyBookings)
	m.HandleFunc("/api/v1/me/relocation-offers", s.handleMyRelocationOffers)
	m.HandleFunc("/api/v1/quotes", s.handleQuotes)

	// 设备端
	m.HandleFunc("/api/v1/devices/gate/scan", s.handleGateScan)
	m.HandleFunc("/api/v1/devices/light/", s.dispatchDeviceLight)

	// 运营端
	m.HandleFunc("/api/v1/operator/maintenances", s.handleMaintenances)
	m.HandleFunc("/api/v1/operator/maintenances/", s.dispatchMaintenance)
	m.HandleFunc("/api/v1/operator/extra-slots", s.handleExtraSlots)
	m.HandleFunc("/api/v1/operator/rules/cancellation", s.handleCancelRules)
	m.HandleFunc("/api/v1/operator/relocation-offers", s.handleRelocationOffers)

	// 前台端（清场提醒等待办）
	m.HandleFunc("/api/v1/front/notifications", s.handleFrontNotifications)
	m.HandleFunc("/api/v1/front/notifications/", s.dispatchFrontNotification)

	// 客服端
	m.HandleFunc("/api/v1/cs/disputes", s.handleDisputes)
	m.HandleFunc("/api/v1/cs/disputes/", s.dispatchDispute)

	// 后台调度（演示用，可带 staff token 或无鉴权仅内网）
	m.HandleFunc("/internal/jobs/expire-unpaid", s.handleJobExpire)
	m.HandleFunc("/internal/jobs/auto-light", s.handleJobAutoLight)
	m.HandleFunc("/internal/jobs/judge-noshow", s.handleJobJudge)

	// 测试时钟
	m.HandleFunc("/internal/clock", s.handleClock)

	m.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, map[string]string{"status": "ok"})
	})

	// Swagger UI（静态资源已内置，无需外网）
	m.Handle("/swagger/", s.swaggerHandler())
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/swagger/", http.StatusFound)
	})
}
