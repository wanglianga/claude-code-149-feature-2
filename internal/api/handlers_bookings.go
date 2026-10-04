package api

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"gymbadminton/internal/auth"
	"gymbadminton/internal/service"
)

//go:embed web
var webFS embed.FS

func (s *Server) swaggerHandler() http.Handler {
	sub, _ := fs.Sub(webFS, "web")
	files := http.StripPrefix("/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".yaml") || strings.HasSuffix(r.URL.Path, ".yml") {
			w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		}
		files.ServeHTTP(w, r)
	})
}

// ---------- 认证 ----------

type registerReq struct {
	Phone      string `json:"phone"`
	Name       string `json:"name"`
	Password   string `json:"password"`
	MemberType string `json:"member_type"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	var req registerReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.Svc.RegisterMember(r.Context(), req.Phone, req.Name, req.Password, req.MemberType)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "message": "注册成功，请登录"})
}

type loginReq struct {
	Phone    string `json:"phone"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleMemberLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	var req loginReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.Svc.LoginMember(r.Context(), req.Phone, req.Password)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, res)
}

func (s *Server) handleStaffLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	var req loginReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.Svc.LoginStaff(r.Context(), req.Username, req.Password)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, res)
}

// ---------- 场馆 / 排期 ----------

func (s *Server) handleVenues(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	vs, err := s.Svc.ListVenues(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, map[string]any{"venues": vs})
}

// /api/v1/venues/{id}/courts | /api/v1/venues/{id}/schedule
func (s *Server) dispatchVenue(w http.ResponseWriter, r *http.Request) {
	parts := segs("/api/v1/venues", r.URL.Path)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	venueID, err := parseID64(parts[0])
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_ID", "message": "非法场馆 ID"}})
		return
	}
	switch parts[1] {
	case "courts":
		cs, err := s.Svc.ListCourts(r.Context(), venueID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, map[string]any{"courts": cs})
	case "schedule":
		courtID, err := parseID64(r.URL.Query().Get("court_id"))
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_ID", "message": "court_id 必填"}})
			return
		}
		day, err := parseDay(r.URL.Query().Get("day"))
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_DAY", "message": err.Error()}})
			return
		}
		sch, err := s.Svc.CourtDaySchedule(r.Context(), venueID, courtID, day)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, sch)
	default:
		http.NotFound(w, r)
	}
}

// ---------- 报价 ----------

type quoteReq struct {
	VenueID      int64  `json:"venue_id"`
	MemberID     int64  `json:"member_id"`
	StartTime    string `json:"start_time"`
	EndTime      string `json:"end_time"`
	RacketCount  int    `json:"racket_count"`
	ShuttleCount int    `json:"shuttle_count"`
}

func (s *Server) handleQuotes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	var req quoteReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
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
	out, err := s.Svc.Quote(r.Context(), service.QuoteInput{
		VenueID: req.VenueID, MemberID: req.MemberID,
		StartTime: st, EndTime: en,
		RacketCount: req.RacketCount, ShuttleCount: req.ShuttleCount,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, out)
}

// ---------- 预约 ----------

type createBookingReq struct {
	IDempotencyKey string `json:"idempotency_key"`
	VenueID        int64  `json:"venue_id"`
	CourtID        int64  `json:"court_id"`
	StartTime      string `json:"start_time"`
	EndTime        string `json:"end_time"`
	PartySize      int    `json:"party_size"`
	RentRacket     bool   `json:"rent_racket"`
	RacketCount    int    `json:"racket_count"`
	BuyShuttle     bool   `json:"buy_shuttle"`
	ShuttleCount   int    `json:"shuttle_count"`
	UseBenefit     *bool  `json:"use_benefit"`
	Note           string `json:"note"`
}

func (s *Server) handleBookings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	c, ok := s.requireMember(w, r)
	if !ok {
		return
	}
	var req createBookingReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
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
	res, err := s.Svc.CreateBooking(r.Context(), service.CreateBookingInput{
		IDempotencyKey: req.IDempotencyKey,
		VenueID:        req.VenueID,
		CourtID:        req.CourtID,
		MemberID:       c.Sub,
		StartTime:      st,
		EndTime:        en,
		PartySize:      req.PartySize,
		RentRacket:     req.RentRacket,
		RacketCount:    req.RacketCount,
		BuyShuttle:     req.BuyShuttle,
		ShuttleCount:   req.ShuttleCount,
		UseBenefit:     req.UseBenefit == nil || *req.UseBenefit,
		Note:           req.Note,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, res)
}

func (s *Server) handleMyBookings(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireMember(w, r)
	if !ok {
		return
	}
	bs, err := s.Svc.ListMemberBookings(r.Context(), c.Sub, 50)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, map[string]any{"bookings": bs})
}

// GET /api/v1/me/relocation-offers?status=pending 会员自己的换场方案
func (s *Server) handleMyRelocationOffers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	c, ok := s.requireMember(w, r)
	if !ok {
		return
	}
	list, err := s.Svc.ListMemberRelocationOffers(r.Context(), c.Sub, r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, map[string]any{"offers": list})
}

// 校验会员是否为订单所有者；员工直接放行
func (s *Server) loadOwnedBooking(w http.ResponseWriter, r *http.Request, id int64) (*auth.Claims, bool) {
	c, ok := s.claims(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": map[string]string{"code": "UNAUTHORIZED", "message": "请登录"}})
		return nil, false
	}
	b, err := s.Svc.GetBooking(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return nil, false
	}
	if c.Kind == "member" && b.MemberID != c.Sub {
		writeJSON(w, 403, map[string]any{"error": map[string]string{"code": "FORBIDDEN", "message": "只能操作本人的预约"}})
		return nil, false
	}
	_ = b
	return &c, true
}

func (s *Server) dispatchBooking(w http.ResponseWriter, r *http.Request) {
	parts := segs("/api/v1/bookings", r.URL.Path)
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	id, err := parseID64(parts[0])
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_ID", "message": "非法预约 ID"}})
		return
	}
	action := ""
	if len(parts) >= 2 {
		action = strings.Join(parts[1:], "/")
	}

	switch r.Method {
	case http.MethodGet:
		switch action {
		case "":
			if _, ok := s.loadOwnedBooking(w, r, id); !ok {
				return
			}
			b, err := s.Svc.GetBooking(r.Context(), id)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeOK(w, b)
		case "timeline":
			if _, ok := s.requireStaff(w, r); !ok {
				return
			}
			tl, err := s.Svc.BookingTimeline(r.Context(), id)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeOK(w, tl)
		case "verdict":
			if _, ok := s.requireStaff(w, r); !ok {
				return
			}
			v, err := s.Svc.EvaluateVerdict(r.Context(), id)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeOK(w, v)
		case "relocation-offer":
			if _, ok := s.loadOwnedBooking(w, r, id); !ok {
				return
			}
			o, err := s.Svc.GetRelocationOfferForBooking(r.Context(), id)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeOK(w, o)
		default:
			http.NotFound(w, r)
		}
		return
	case http.MethodPost:
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}

	switch action {
	case "pay":
		c, ok := s.loadOwnedBooking(w, r, id)
		if !ok {
			return
		}
		var req struct {
			Method string `json:"method"`
		}
		_ = decode(r, &req)
		src := "member"
		var actor *int64
		if c.Kind == "staff" {
			src, actor = "front", &c.Sub
		}
		b, err := s.Svc.PayBooking(r.Context(), id, req.Method, src, actor)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, b)

	case "cancel":
		c, ok := s.loadOwnedBooking(w, r, id)
		if !ok {
			return
		}
		var req struct {
			Reason string `json:"reason"`
		}
		_ = decode(r, &req)
		src := "member"
		var actor *int64
		if c.Kind == "staff" {
			src, actor = "operator", &c.Sub
		}
		b, q, err := s.Svc.CancelBooking(r.Context(), id, src, actor, req.Reason)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, map[string]any{"booking": b, "cancellation": q})

	case "quote-cancel":
		if _, ok := s.loadOwnedBooking(w, r, id); !ok {
			return
		}
		q, err := s.Svc.QuoteCancel(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, q)

	case "checkin":
		c, ok := s.requireStaff(w, r, "front", "operator")
		if !ok {
			return
		}
		b, err := s.Svc.FrontCheckin(r.Context(), id, c.Sub)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, b)

	case "light":
		c, ok := s.claims(r)
		if !ok {
			writeJSON(w, 401, map[string]any{"error": map[string]string{"code": "UNAUTHORIZED", "message": "请登录"}})
			return
		}
		var req struct {
			Action string `json:"action"`
			Source string `json:"source"`
			Detail string `json:"detail"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		src := req.Source
		if src == "" {
			if c.Kind == "staff" {
				if c.Role == "front" {
					src = "front"
				} else {
					src = "operator"
				}
			} else {
				src = "device"
			}
		}
		b, err := s.Svc.LightCommand(r.Context(), id, req.Action, src, &c.Sub, req.Detail)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, b)

	case "session/start":
		c, ok := s.claims(r)
		if !ok {
			writeJSON(w, 401, map[string]any{"error": map[string]string{"code": "UNAUTHORIZED", "message": "请登录"}})
			return
		}
		src := "front"
		if c.Kind == "member" {
			src = "device"
		}
		b, err := s.Svc.StartSession(r.Context(), id, src, &c.Sub)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, b)

	case "session/end":
		c, ok := s.claims(r)
		if !ok {
			writeJSON(w, 401, map[string]any{"error": map[string]string{"code": "UNAUTHORIZED", "message": "请登录"}})
			return
		}
		var req struct {
			Method string `json:"method"`
		}
		_ = decode(r, &req)
		src := "front"
		if c.Kind == "member" {
			src = "system"
		}
		b, err := s.Svc.EndSession(r.Context(), id, src, &c.Sub, service.EndInput(req.Method))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, b)

	case "participants":
		if _, ok := s.loadOwnedBooking(w, r, id); !ok {
			return
		}
		var req struct {
			Name     string `json:"name"`
			Phone    string `json:"phone"`
			MemberID *int64 `json:"member_id"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		p, err := s.Svc.AddParticipant(r.Context(), id, req.Name, req.Phone, req.MemberID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 201, p)

	case "headcount":
		c, ok := s.requireStaff(w, r, "front", "operator")
		if !ok {
			return
		}
		var req struct {
			ActualCount int `json:"actual_count"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		b, err := s.Svc.ReconcileHeadcount(r.Context(), id, c.Sub, req.ActualCount)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, b)

	case "waive-overlimit":
		c, ok := s.requireStaff(w, r, "front", "operator")
		if !ok {
			return
		}
		b, err := s.Svc.WaiveOverlimit(r.Context(), id, c.Sub)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, b)

	case "compensation":
		c, ok := s.requireStaff(w, r, "front", "operator", "cs")
		if !ok {
			return
		}
		var req struct {
			Minutes int    `json:"minutes"`
			Scope   string `json:"scope"`
			Reason  string `json:"reason"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		b, err := s.Svc.GrantCompensation(r.Context(), id, c.Sub, req.Minutes, req.Scope, req.Reason)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, b)

	case "refund-requests":
		c, ok := s.requireMember(w, r)
		if !ok {
			return
		}
		var req struct {
			AmountCents int64  `json:"amount_cents"`
			Reason      string `json:"reason"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		b, err := s.Svc.GetBooking(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		if b.MemberID != c.Sub {
			writeJSON(w, 403, map[string]any{"error": map[string]string{"code": "FORBIDDEN", "message": "只能为本人订单申请退款"}})
			return
		}
		rr, err := s.Svc.CreateRefundRequest(r.Context(), id, c.Sub, req.AmountCents, req.Reason)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 201, rr)

	case "relocation-offer/accept":
		// 用户确认换场方案：换场（差价场馆承担）/ 顺延 / 退款
		c, ok := s.requireMember(w, r)
		if !ok {
			return
		}
		var req struct {
			Action    string `json:"action"`
			CourtID   int64  `json:"court_id"`
			StartTime string `json:"start_time"`
			EndTime   string `json:"end_time"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		dec := service.RelocationDecision{Action: req.Action, CourtID: req.CourtID, MemberID: c.Sub}
		if req.StartTime != "" {
			st, err := parseTime(req.StartTime)
			if err != nil {
				writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_TIME", "message": err.Error()}})
				return
			}
			dec.StartTime = st
		}
		if req.EndTime != "" {
			en, err := parseTime(req.EndTime)
			if err != nil {
				writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_TIME", "message": err.Error()}})
				return
			}
			dec.EndTime = en
		}
		res, err := s.Svc.AcceptRelocation(r.Context(), id, dec)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, res)

	case "timeline":
		if _, ok := s.requireStaff(w, r); !ok {
			return
		}
		tl, err := s.Svc.BookingTimeline(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, tl)

	case "verdict":
		if _, ok := s.requireStaff(w, r); !ok {
			return
		}
		v, err := s.Svc.EvaluateVerdict(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, v)

	default:
		http.NotFound(w, r)
	}
}
