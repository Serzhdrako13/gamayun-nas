package api

import (
	"errors"
	"net/http"
	"time"

	"gamayun-nas/internal/apps"
	"gamayun-nas/internal/helper"
)

// Подсистема приложений — состояние, включение с выбором пула,
// выключение, возобновление после паузы.
func (s *server) appsRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/apps", s.appsH(s.appsState))
	m.HandleFunc("POST /api/v1/apps/enable", s.appsH(s.appsEnable))
	m.HandleFunc("POST /api/v1/apps/disable", s.appsH(s.appsDisable))
	m.HandleFunc("POST /api/v1/apps/resume", s.appsH(s.appsResume))
	s.appsProjectRoutes(m)
	s.catalogRoutes(m)
}

func (s *server) appsH(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Apps == nil {
			writeError(w, http.StatusServiceUnavailable, "apps_unavailable", "подсистема приложений не настроена")
			return
		}
		h(w, r)
	}
}

func (s *server) appsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, helper.ErrUnavailable):
		s.helperError(w, r, err)
	case errors.Is(err, apps.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, apps.ErrConflictSvc):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, apps.ErrInvalidSvc):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, apps.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "apps_unavailable", err.Error())
	default:
		s.internalError(w, r, err)
	}
}

func (s *server) appsState(w http.ResponseWriter, r *http.Request) {
	v := s.Apps.View()
	if time.Since(v.CheckedAt) > 10*time.Second || r.URL.Query().Get("refresh") == "1" {
		v = s.Apps.RefreshIfIdle(r.Context())
	}
	writeJSON(w, http.StatusOK, v)
}

// AppsEnableRequest — тело POST /api/v1/apps/enable.
type AppsEnableRequest struct {
	Pool string `json:"pool"`
}

func (s *server) appsEnable(w http.ResponseWriter, r *http.Request) {
	var req AppsEnableRequest
	if !s.body(w, r, &req) {
		return
	}
	t, err := s.Apps.Enable(r.Context(), req.Pool)
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	s.Logger.Info("приложения: включение", "pool", req.Pool, "task", t.ID, "actor", actor(r))
	writeJSON(w, http.StatusAccepted, t)
}

func (s *server) appsDisable(w http.ResponseWriter, r *http.Request) {
	t, err := s.Apps.Disable(r.Context())
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	s.Logger.Info("приложения: выключение", "task", t.ID, "actor", actor(r))
	writeJSON(w, http.StatusAccepted, t)
}

func (s *server) appsResume(w http.ResponseWriter, r *http.Request) {
	t, err := s.Apps.Resume(r.Context())
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	s.Logger.Info("приложения: возобновление", "task", t.ID, "actor", actor(r))
	writeJSON(w, http.StatusAccepted, t)
}
