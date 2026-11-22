package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"gamayun-nas/internal/maintenance"
	"gamayun-nas/internal/smart"
	"gamayun-nas/internal/storage/home"
)

// Проверка пулов (scrub) и SMART по расписанию.
func (s *server) maintenanceRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/maintenance", s.maintH(s.getMaintenance))
	m.HandleFunc("PUT /api/v1/maintenance/schedule", s.maintH(s.putSchedule))
	m.HandleFunc("GET /api/v1/disks/{id}/smart", s.maintH(s.diskSmart))
	m.HandleFunc("POST /api/v1/disks/{id}/smart/test", s.maintH(s.diskSmartTest))
	m.HandleFunc("PUT /api/v1/disks/{id}/smart/enabled", s.maintH(s.diskSmartEnabled))
	m.HandleFunc("GET /api/v1/disks/{id}/smart/history", s.maintH(s.diskSmartHistory))
	m.HandleFunc("PUT /api/v1/maintenance/temperature", s.maintH(s.putTemperature))
	m.HandleFunc("POST /api/v1/storage/pools/{name}/scrub", s.startScrub)
	m.HandleFunc("DELETE /api/v1/storage/pools/{name}/scrub", s.cancelScrub)
}

func (s *server) maintH(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Maintenance == nil {
			writeError(w, http.StatusServiceUnavailable, "maintenance_unavailable", "обслуживание не настроено")
			return
		}
		h(w, r)
	}
}

// getMaintenance — расписание, последние плановые запуски и SMART дисков
// (последний опрос).
func (s *server) getMaintenance(w http.ResponseWriter, r *http.Request) {
	sc, err := s.Maintenance.Schedule(r.Context())
	if err != nil {
		s.Logger.Warn("расписание обслуживания", "err", err)
	}
	last, err := s.Maintenance.Last(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	sm, err := s.Maintenance.Smart(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	th, err := s.Maintenance.Thresholds(r.Context())
	if err != nil {
		s.Logger.Warn("пороги температуры", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedule": sc, "last": last, "smart": sm, "temperature": th})
}

// putTemperature — пороги температуры дисков.
func (s *server) putTemperature(w http.ResponseWriter, r *http.Request) {
	var th maintenance.Thresholds
	if !s.body(w, r, &th) {
		return
	}
	got, err := s.Maintenance.SetThresholds(r.Context(), th)
	if errors.Is(err, maintenance.ErrInvalid) {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Logger.Info("пороги температуры дисков изменены", "actor", actor(r), "temperature", got)
	writeJSON(w, http.StatusOK, got)
}

// diskSmartHistory — история SMART диска за days дней (1–90, по умолчанию 7).
func (s *server) diskSmartHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if (smart.DiskParams{DiskID: id}).Validate() != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "недопустимый идентификатор диска")
		return
	}
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 90 {
			writeError(w, http.StatusBadRequest, "invalid_request", "days — от 1 до 90")
			return
		}
		days = n
	}
	pts, err := s.Maintenance.History(r.Context(), id, time.Now().Add(-time.Duration(days)*24*time.Hour))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disk_id": id, "days": days, "points": pts})
}

// diskSmartSummary — итог SMART последнего опроса в каждом диске
// снимка (без нового опроса); ошибка — только в журнал службы.
func (s *server) diskSmartSummary(r *http.Request, snap map[string]any) {
	if s.Maintenance == nil {
		return
	}
	list, _ := snap["disks"].([]any)
	kinds := map[string]string{}
	for _, x := range list {
		if d, ok := x.(map[string]any); ok {
			id, _ := d["id"].(string)
			kind, _ := d["kind"].(string)
			kinds[id] = kind
		}
	}
	sum, err := s.Maintenance.Summaries(r.Context(), kinds)
	if err != nil {
		s.Logger.Warn("итог SMART дисков", "err", err)
		return
	}
	for _, x := range list {
		d, ok := x.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := d["id"].(string); id != "" {
			if v, ok := sum[id]; ok {
				d["smart"] = v
			}
		}
	}
}

func (s *server) putSchedule(w http.ResponseWriter, r *http.Request) {
	var sc maintenance.Schedule
	if !s.body(w, r, &sc) {
		return
	}
	got, err := s.Maintenance.SetSchedule(r.Context(), sc)
	if errors.Is(err, maintenance.ErrInvalid) {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Logger.Info("расписание обслуживания изменено", "actor", actor(r), "schedule", got)
	writeJSON(w, http.StatusOK, got)
}

func (s *server) diskSmart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if (smart.DiskParams{DiskID: id}).Validate() != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "недопустимый идентификатор диска")
		return
	}
	in, err := s.Maintenance.ReadDisk(r.Context(), id)
	if err != nil {
		s.helperError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

func (s *server) diskSmartTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type string `json:"type"`
	}
	if !s.body(w, r, &req) {
		return
	}
	p := smart.TestParams{DiskID: r.PathValue("id"), Type: req.Type}
	if err := p.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	res, err := s.Maintenance.StartTest(r.Context(), p.DiskID, p.Type)
	if err != nil {
		s.helperError(w, r, err)
		return
	}
	if !res.Started {
		writeError(w, http.StatusUnprocessableEntity, "unsupported", "самотест не запущен: "+res.Message)
		return
	}
	s.Logger.Info("SMART-самотест запущен", "actor", actor(r), "disk", p.DiskID, "type", p.Type)
	writeJSON(w, http.StatusAccepted, res)
}

// diskSmartEnabled — включить/выключить SMART в диске вручную
// ({"on": true|false}); выключение автоматика не перекрывает.
func (s *server) diskSmartEnabled(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On *bool `json:"on"`
	}
	if !s.body(w, r, &req) {
		return
	}
	p := smart.SetParams{DiskID: r.PathValue("id")}
	if err := p.Validate(); err != nil || req.On == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "нужны идентификатор диска и on: true|false")
		return
	}
	in, err := s.Maintenance.SetSmart(r.Context(), p.DiskID, *req.On, actor(r))
	if err != nil {
		s.helperError(w, r, err)
		return
	}
	s.Logger.Info("SMART диска изменён вручную", "actor", actor(r), "disk", p.DiskID, "on", *req.On)
	writeJSON(w, http.StatusOK, in)
}

func (s *server) startScrub(w http.ResponseWriter, r *http.Request) {
	if s.noStorage(w) {
		return
	}
	t, err := s.Storage.Scrub(r.Context(), r.PathValue("name"), home.ScrubManual)
	if err != nil {
		s.storageError(w, r, err)
		return
	}
	s.Logger.Info("проверка пула", "pool", r.PathValue("name"), "actor", actor(r), "task", t.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"task": t})
}

func (s *server) cancelScrub(w http.ResponseWriter, r *http.Request) {
	if s.noStorage(w) {
		return
	}
	if err := s.Storage.ScrubCancel(r.Context(), r.PathValue("name")); err != nil {
		s.storageError(w, r, err)
		return
	}
	s.Logger.Info("проверка пула остановлена", "pool", r.PathValue("name"), "actor", actor(r))
	w.WriteHeader(http.StatusNoContent)
}
