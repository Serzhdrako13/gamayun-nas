package api

import (
	"errors"
	"net"
	"net/http"
	"strconv"

	"gamayun-nas/internal/confbackup"
	"gamayun-nas/internal/notify"
	"gamayun-nas/internal/store"
)

// Резервная копия настроек. Копия без секретов; восстановление из
// веба — только пока не пройден мастер первоначальной настройки (иначе —
// CLI gamayund config restore).
func (s *server) configRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/config/backup/download", s.confH(s.configDownload))
	m.HandleFunc("POST /api/v1/config/backup", s.confH(s.configSaveNow))
	m.HandleFunc("GET /api/v1/config/backups", s.confH(s.configList))
	m.HandleFunc("GET /api/v1/config/backups/{pool}/{name}", s.confH(s.configReadStored))
	m.HandleFunc("GET /api/v1/config/usb", s.confH(s.configUSBList))
	m.HandleFunc("POST /api/v1/config/usb", s.confH(s.configUSBSave))
	m.HandleFunc("GET /api/v1/config/sftp", s.confH(s.configSFTP))
	m.HandleFunc("PUT /api/v1/config/sftp", s.confH(s.configSetSFTP))
	m.HandleFunc("POST /api/v1/config/sftp/scan", s.confH(s.configScanSFTP))
	m.HandleFunc("POST /api/v1/config/sftp/upload", s.confH(s.configUploadSFTP))
	m.HandleFunc("POST /api/v1/config/restore/plan", s.confH(s.nasH(s.configRestorePlan)))
	m.HandleFunc("POST /api/v1/config/restore", s.confH(s.nasH(s.configRestore)))
}

func (s *server) confH(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Config == nil {
			writeError(w, http.StatusServiceUnavailable, "config_unavailable", "резервные копии настроек не настроены")
			return
		}
		h(w, r)
	}
}

func sendBackup(w http.ResponseWriter, name string, data []byte) {
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (s *server) configDownload(w http.ResponseWriter, r *http.Request) {
	data, name, _, err := s.Config.Export(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Logger.Info("копия настроек выгружена", "actor", actor(r), "file", name)
	sendBackup(w, name, data)
}

func (s *server) configSaveNow(w http.ResponseWriter, r *http.Request) {
	res, _, err := s.Config.SaveToPools(r.Context(), true)
	if errors.Is(err, confbackup.ErrNoPools) {
		writeError(w, http.StatusConflict, "no_pools", err.Error())
		return
	}
	if err != nil && res == nil {
		s.nasError(w, r, err)
		return
	}
	status := http.StatusOK
	if err != nil {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]any{"pools": res})
}

func (s *server) configList(w http.ResponseWriter, r *http.Request) {
	res, err := s.Config.List(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": res, "keep": confbackup.Keep})
}

func (s *server) configReadStored(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	data, err := s.Config.Read(r.Context(), r.PathValue("pool"), name)
	if err != nil {
		s.configErr(w, r, err)
		return
	}
	sendBackup(w, name, data)
}

func (s *server) configUSBList(w http.ResponseWriter, r *http.Request) {
	d, err := s.Config.USBDevices(r.Context())
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": d})
}

func (s *server) configUSBSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Device string `json:"device"`
	}
	if !s.body(w, r, &req) {
		return
	}
	res, err := s.Config.SaveUSB(r.Context(), req.Device)
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("копия настроек выгружена на USB", "actor", actor(r), "device", req.Device)
	writeJSON(w, http.StatusOK, res)
}

// restoreSource — файл копии в запросе (base64 в JSON) или копия из пула.
type restoreSource struct {
	File []byte `json:"file,omitempty"`
	Pool string `json:"pool,omitempty"`
	Name string `json:"name,omitempty"`
}

func (s *server) loadBackup(w http.ResponseWriter, r *http.Request, src restoreSource) (confbackup.Backup, confbackup.Data, bool) {
	data := src.File
	if len(data) == 0 {
		var err error
		if data, err = s.Config.Read(r.Context(), src.Pool, src.Name); err != nil {
			s.configErr(w, r, err)
			return confbackup.Backup{}, confbackup.Data{}, false
		}
	}
	b, d, err := confbackup.DecodeBytes(data, store.LatestVersion())
	if err != nil {
		s.configErr(w, r, err)
		return b, d, false
	}
	return b, d, true
}

func (s *server) configErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, confbackup.ErrIncompatible):
		writeError(w, http.StatusUnprocessableEntity, "incompatible", err.Error())
	case errors.Is(err, confbackup.ErrFormat):
		writeError(w, http.StatusBadRequest, "bad_backup", err.Error())
	case errors.Is(err, confbackup.ErrSFTP):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, confbackup.ErrAnswers):
		writeError(w, http.StatusBadRequest, "answers_required", err.Error())
	default:
		s.nasError(w, r, err)
	}
}

func (s *server) configRestorePlan(w http.ResponseWriter, r *http.Request) {
	var req restoreSource
	if err := decodeBody(w, r, 128<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	b, d, ok := s.loadBackup(w, r, req)
	if !ok {
		return
	}
	p, err := confbackup.MakePlan(r.Context(), s.DB, b, d)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) configRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		restoreSource
		Answers confbackup.Answers `json:"answers"`
	}
	if err := decodeBody(w, r, 128<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	done, err := s.NAS.SetupDone(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if done {
		writeError(w, http.StatusConflict, "setup_done",
			"восстановление из веба возможно только до завершения первоначальной настройки; на настроенной системе — gamayund config restore с консоли")
		return
	}
	b, d, ok := s.loadBackup(w, r, req.restoreSource)
	if !ok {
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	res, err := confbackup.Apply(r.Context(), s.DB, s.NAS, d, req.Answers, host)
	if err != nil {
		s.configErr(w, r, err)
		return
	}
	s.applyNotify(r)
	s.Logger.Info("восстановление настроек", "actor", actor(r), "backup_created", b.CreatedAt, "steps", len(res.Steps), "error", res.Error)
	// Частичный сбой — 200 с полем error и шагами: мастеру нужны сделанные шаги.
	writeJSON(w, http.StatusOK, res)
}

func (s *server) configSFTP(w http.ResponseWriter, r *http.Request) {
	info, err := s.Config.SFTP(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *server) configSetSFTP(w http.ResponseWriter, r *http.Request) {
	var c confbackup.SFTPConfig
	if !s.body(w, r, &c) {
		return
	}
	info, err := s.Config.SetSFTP(r.Context(), c)
	if err != nil {
		s.configErr(w, r, err)
		return
	}
	s.Logger.Info("копия настроек: назначение SFTP изменено", "actor", actor(r), "enabled", c.Enabled, "host", c.Host, "port", c.Port, "user", c.User)
	writeJSON(w, http.StatusOK, info)
}

func (s *server) configScanSFTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if !s.body(w, r, &req) {
		return
	}
	key, err := confbackup.ScanHostKey(r.Context(), req.Host, req.Port)
	if errors.Is(err, confbackup.ErrSFTP) {
		s.configErr(w, r, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "sftp_unreachable", "сервер SFTP недоступен: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"host_key": key, "fingerprint": confbackup.Fingerprint(key)})
}

func (s *server) configUploadSFTP(w http.ResponseWriter, r *http.Request) {
	res, err := s.Config.UploadNow(r.Context())
	if errors.Is(err, confbackup.ErrSFTP) {
		s.configErr(w, r, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "sftp_failed", err.Error())
		return
	}
	s.Logger.Info("копия настроек выгружена по SFTP", "actor", actor(r))
	writeJSON(w, http.StatusOK, res)
}

// applyNotify — восстановленные каналы уведомлений сразу в доставку.
func (s *server) applyNotify(r *http.Request) {
	if s.Deliver == nil {
		return
	}
	if c, err := notify.LoadSMTP(r.Context(), s.DB); err == nil {
		s.Deliver.ApplySMTP(c)
	}
	if c, err := notify.LoadWebhook(r.Context(), s.DB); err == nil {
		s.Deliver.ApplyWebhook(c)
	}
}
