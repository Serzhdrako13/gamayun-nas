package api

import (
	"errors"
	"net/http"
	"time"

	"gamayun-nas/internal/events"
)

// Сроки хранения журнала событий и центра уведомлений.
func (s *server) retentionRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/system/retention", s.retentionH(s.getRetention))
	m.HandleFunc("PUT /api/v1/system/retention", s.retentionH(s.putRetention))
}

func (s *server) retentionH(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.DB == nil || s.Events == nil {
			writeError(w, http.StatusServiceUnavailable, "retention_unavailable", "журнал событий не настроен")
			return
		}
		h(w, r)
	}
}

func (s *server) getRetention(w http.ResponseWriter, r *http.Request) {
	ret, err := events.LoadRetention(r.Context(), s.DB)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ret)
}

// putRetention сохраняет сроки и сразу удаляет записи старше новых сроков
// (предел по числу записей применяет gamayund при старте и раз в сутки).
func (s *server) putRetention(w http.ResponseWriter, r *http.Request) {
	var req events.Retention
	if !s.body(w, r, &req) {
		return
	}
	ctx := r.Context()
	if err := events.SaveRetention(ctx, s.DB, req); err != nil {
		if errors.Is(err, events.ErrInvalidRetention) {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		s.internalError(w, r, err)
		return
	}
	s.Logger.Info("сроки хранения журнала изменены", "actor", actor(r), "retention", req)
	now := time.Now()
	removed := map[string]int64{}
	if n, err := s.Events.PruneAge(ctx, req, now); err != nil {
		s.Logger.Warn("журнал событий: обрезка", "err", err)
	} else {
		removed["events"] = n
	}
	if s.Notify != nil {
		if n, err := s.Notify.PruneAge(ctx, req, now); err != nil {
			s.Logger.Warn("центр уведомлений: обрезка", "err", err)
		} else {
			removed["notifications"] = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"retention": req, "removed": removed})
}
