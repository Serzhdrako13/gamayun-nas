package api

import (
	"encoding/json"
	"net"
	"net/http"

	"gamayun-nas/internal/brand"
	"gamayun-nas/internal/events"
	"gamayun-nas/internal/nas"
)

// Мастер первоначальной настройки, имя сервера и время,
// домашние папки, защита SSH. Только для вошедших (за Auth).
func (s *server) settingsRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/setup/status", s.nasH(s.setupStatus))
	m.HandleFunc("POST /api/v1/setup/complete", s.nasH(s.setupComplete))
	m.HandleFunc("GET /api/v1/system/identity", s.nasH(s.getIdentity))
	m.HandleFunc("PUT /api/v1/system/identity", s.nasH(s.putIdentity))
	m.HandleFunc("GET /api/v1/nas/homes", s.nasH(s.getHomes))
	m.HandleFunc("PUT /api/v1/nas/homes", s.nasH(s.putHomes))
	m.HandleFunc("GET /api/v1/system/ssh-guard", s.nasH(s.getGuard))
	m.HandleFunc("PUT /api/v1/system/ssh-guard", s.nasH(s.putGuard))
	m.HandleFunc("POST /api/v1/system/ssh-guard/unban", s.nasH(s.unbanGuard))
	m.HandleFunc("POST /api/v1/system/power", s.nasH(s.power))
	m.HandleFunc("GET /api/v1/system/ssh", s.nasH(s.getSSH))
	m.HandleFunc("PUT /api/v1/system/ssh", s.nasH(s.putSSH))
}

// power — выключение или перезагрузка. Подтверждение — в
// интерфейсе и здесь: confirm должен повторять action. Событие в журнал
// пишется до вызова helper — после него gamayund может не успеть.
func (s *server) power(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action  string `json:"action"`
		Confirm string `json:"confirm"`
	}
	if !s.body(w, r, &req) {
		return
	}
	if req.Action != "poweroff" && req.Action != "reboot" {
		writeError(w, http.StatusBadRequest, "invalid_request", "action: poweroff|reboot")
		return
	}
	if req.Confirm != req.Action {
		writeError(w, http.StatusBadRequest, "confirm_required", "нужно подтверждение: confirm = action")
		return
	}
	if s.Events != nil {
		b, _ := json.Marshal(map[string]string{"action": req.Action, "user": actor(r)})
		if _, err := s.Events.Emit(r.Context(), events.Event{Type: "system." + req.Action, Severity: events.SevWarning, Data: b}, true); err != nil {
			s.Logger.Warn("событие выключения не записано", "err", err)
		}
	}
	s.Logger.Warn("запрошено "+map[string]string{"poweroff": "выключение", "reboot": "перезагрузка"}[req.Action], "actor", actor(r))
	if err := s.NAS.Power(r.Context(), req.Action); err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"action": req.Action})
}

func (s *server) setupStatus(w http.ResponseWriter, r *http.Request) {
	done, err := s.NAS.SetupDone(r.Context())
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"done": done, "product": brand.Name})
}

func (s *server) setupComplete(w http.ResponseWriter, r *http.Request) {
	if err := s.NAS.CompleteSetup(r.Context()); err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("первоначальная настройка завершена", "actor", actor(r))
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "product": brand.Name})
}

func (s *server) getIdentity(w http.ResponseWriter, r *http.Request) {
	id, err := s.NAS.Identity(r.Context())
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, id)
}

func (s *server) putIdentity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hostname string `json:"hostname"`
		Timezone string `json:"timezone"`
		NTP      bool   `json:"ntp"`
	}
	if !s.body(w, r, &req) {
		return
	}
	warns, err := s.NAS.SetIdentity(r.Context(), req.Hostname, req.Timezone, req.NTP)
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("имя сервера и время изменены", "hostname", req.Hostname, "timezone", req.Timezone, "ntp", req.NTP, "actor", actor(r))
	id, err := s.NAS.Identity(r.Context())
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	if warns == nil {
		warns = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"identity": id, "warnings": warns})
}

func (s *server) getHomes(w http.ResponseWriter, r *http.Request) {
	h, err := s.NAS.Homes(r.Context())
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *server) putHomes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool   `json:"enabled"`
		Pool    string `json:"pool"`
	}
	if !s.body(w, r, &req) {
		return
	}
	h, err := s.NAS.SetHomes(r.Context(), req.Enabled, req.Pool)
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("служба домашних папок изменена", "enabled", h.Enabled, "pool", h.Pool, "actor", actor(r))
	writeJSON(w, http.StatusOK, h)
}

func (s *server) getGuard(w http.ResponseWriter, r *http.Request) {
	g, st, err := s.NAS.GuardState(r.Context())
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": g, "status": st})
}

func (s *server) putGuard(w http.ResponseWriter, r *http.Request) {
	g := nas.DefaultGuard()
	if !s.body(w, r, &g) {
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	g, err = s.NAS.SetGuard(r.Context(), g, host)
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("защита SSH от перебора изменена", "enabled", g.Enabled, "maxretry", g.MaxRetry, "findtime", g.FindTime, "bantime", g.BanTime, "actor", actor(r))
	s.getGuard(w, r)
}

func (s *server) unbanGuard(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IP string `json:"ip"`
	}
	if !s.body(w, r, &req) {
		return
	}
	if err := s.NAS.Unban(r.Context(), req.IP); err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("адрес разблокирован в защите SSH", "ip", req.IP, "actor", actor(r))
	w.WriteHeader(http.StatusNoContent)
}

// getSSH — служба SSH: сохранённое состояние, служба и число
// пользователей с правом SSH (0 — включённой службой никто не войдёт).
func (s *server) getSSH(w http.ResponseWriter, r *http.Request) {
	c, st, n, err := s.NAS.SSHState(r.Context())
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": c.Enabled, "ssh_users": n,
		"status": map[string]bool{"installed": st.Installed, "active": st.Active, "dev_access": st.DevAccess}})
}

// putSSH включает или выключает службу SSH; kill_sessions — при выключении
// разорвать открытые сессии (по умолчанию остаются).
func (s *server) putSSH(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled      *bool `json:"enabled"`
		KillSessions bool  `json:"kill_sessions"`
	}
	if !s.body(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "enabled обязателен")
		return
	}
	kill := req.KillSessions && !*req.Enabled
	if err := s.NAS.SetSSHService(r.Context(), *req.Enabled, kill); err != nil {
		s.nasError(w, r, err)
		return
	}
	typ := map[bool]string{true: "system.ssh.enabled", false: "system.ssh.disabled"}[*req.Enabled]
	if s.Events != nil {
		b, _ := json.Marshal(map[string]any{"user": actor(r), "kill_sessions": kill})
		if _, err := s.Events.Emit(r.Context(), events.Event{Type: typ, Severity: events.SevInfo, Data: b}, true); err != nil {
			s.Logger.Warn("событие службы SSH не записано", "err", err)
		}
	}
	s.Logger.Info("служба SSH изменена", "enabled", *req.Enabled, "kill_sessions", kill, "actor", actor(r))
	s.getSSH(w, r)
}
