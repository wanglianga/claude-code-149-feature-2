package api

import (
	"encoding/json"
	"net/http"
)

// 后台任务触发端点（生产环境由进程内 ticker 周期调用；这里额外暴露便于验证）

func (s *Server) handleJobExpire(w http.ResponseWriter, r *http.Request) {
	n, err := s.Svc.ExpireUnpaid(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, map[string]any{"expired": n})
}

func (s *Server) handleJobAutoLight(w http.ResponseWriter, r *http.Request) {
	n, err := s.Svc.AutoLightControl(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, map[string]any{"processed": n})
}

func (s *Server) handleJobJudge(w http.ResponseWriter, r *http.Request) {
	n, err := s.Svc.JudgeNoShows(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, map[string]any{"judged": n})
}

func (s *Server) handleJobRelocationTimeout(w http.ResponseWriter, r *http.Request) {
	n, err := s.Svc.AutoResolvePendingRelocations(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, map[string]any{"auto_resolved": n})
}

// 虚拟时钟：仅 ALLOW_TEST_CLOCK=1 时生效
func (s *Server) handleClock(w http.ResponseWriter, r *http.Request) {
	if !s.AllowClock {
		writeJSON(w, 403, map[string]any{"error": map[string]string{"code": "CLOCK_DISABLED", "message": "测试时钟未启用（需 ALLOW_TEST_CLOCK=1）"}})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeOK(w, map[string]any{"now": s.Svc.Now(r.Context())})
	case http.MethodPost:
		var req struct {
			OffsetSeconds *int64 `json:"offset_seconds"`
			Reset         bool   `json:"reset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BAD_JSON", "message": err.Error()}})
			return
		}
		if req.Reset {
			_ = s.Svc.Clock.Reset(r.Context())
		} else if req.OffsetSeconds != nil {
			_ = s.Svc.Clock.SetOffset(r.Context(), *req.OffsetSeconds)
		}
		writeOK(w, map[string]any{"now": s.Svc.Now(r.Context())})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}
