package api

import (
	"net/http"
	"strconv"

	"gamayun-nas/internal/apps"
	"gamayun-nas/internal/proxy"
)

// Пользовательские compose-приложения. Значения переменных
// (.env) — только в теле запроса создания/изменения; в ответах, журнале
// gamayund и событиях — только имена.
func (s *server) appsProjectRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/apps/projects", s.appsH(s.appsProjects))
	m.HandleFunc("POST /api/v1/apps/projects", s.appsH(s.appsProjectCreate))
	m.HandleFunc("POST /api/v1/apps/projects/check", s.appsH(s.appsProjectCheck))
	m.HandleFunc("GET /api/v1/apps/projects/{id}", s.appsH(s.appsProjectGet))
	m.HandleFunc("PUT /api/v1/apps/projects/{id}", s.appsH(s.appsProjectUpdate))
	m.HandleFunc("DELETE /api/v1/apps/projects/{id}", s.appsH(s.appsProjectDelete))
	m.HandleFunc("POST /api/v1/apps/projects/{id}/{action}", s.appsH(s.appsProjectAction))
	m.HandleFunc("GET /api/v1/apps/projects/{id}/logs", s.appsH(s.appsProjectLogs))
	m.HandleFunc("GET /api/v1/apps/ports", s.appsH(s.appsPorts))
	// Доступ к общим папкам, перевод приложений «доступ как root» на свою учётку.
	m.HandleFunc("PUT /api/v1/apps/projects/{id}/grants", s.appsH(s.appsProjectGrants))
	m.HandleFunc("POST /api/v1/apps/projects/{id}/migrate", s.appsH(s.appsProjectMigrate))
	m.HandleFunc("GET /api/v1/apps/shares/{name}", s.appsH(s.appsShareApps))
	// Публикация через прокси (Caddy, поддомен).
	m.HandleFunc("PUT /api/v1/apps/projects/{id}/web", s.appsH(s.appsProjectWeb))
	m.HandleFunc("DELETE /api/v1/apps/projects/{id}/web", s.appsH(s.appsProjectWeb))
	m.HandleFunc("GET /api/v1/apps/proxy", s.appsProxy)
	m.HandleFunc("GET /api/v1/apps/proxy/ca.crt", s.appsProxyCA)
}

func (s *server) appsProjectWeb(w http.ResponseWriter, r *http.Request) {
	var web *apps.WebRoute
	if r.Method == http.MethodPut {
		web = &apps.WebRoute{}
		if err := decodeBody(w, r, 4<<10, web); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	}
	id := r.PathValue("id")
	p, err := s.Apps.SetWeb(r.Context(), id, web, actor(r))
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	s.Logger.Info("приложения: публикация через прокси", "app", id, "web", web, "actor", actor(r))
	writeJSON(w, http.StatusOK, p)
}

func (s *server) appsProxy(w http.ResponseWriter, r *http.Request) {
	if s.Proxy == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "прокси приложений недоступен")
		return
	}
	writeJSON(w, http.StatusOK, s.Proxy.Status())
}

// appsProxyCA — корневой сертификат внутреннего CA для установки на клиентах.
func (s *server) appsProxyCA(w http.ResponseWriter, r *http.Request) {
	if s.Proxy == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "прокси приложений недоступен")
		return
	}
	pem, err := s.Proxy.RootCA(r.Context())
	if proxy.ErrNoCaddy(err) {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "прокси приложений не установлен или не запущен")
		return
	}
	if err != nil {
		s.Logger.Warn("прокси: корневой сертификат", "err", err)
		writeError(w, http.StatusBadGateway, "proxy_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="gamayun-nas-ca.crt"`)
	w.Write(pem)
}

// accessBody — режим запуска и выдачи доступа.
type accessBody struct {
	RunAs  map[string]string `json:"run_as"`
	Grants map[string]string `json:"grants"`
	// Confirm — подтверждённые риски (перевод: root_share_write и т. п.).
	Confirm []string `json:"confirm"`
}

func (s *server) appsProjectGrants(w http.ResponseWriter, r *http.Request) {
	var b accessBody
	if err := decodeBody(w, r, 64<<10, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	id := r.PathValue("id")
	t, err := s.Apps.SetGrants(r.Context(), id, b.RunAs, b.Grants, actor(r))
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	s.Logger.Info("приложения: доступ к папкам", "app", id, "grants", b.Grants, "task", t.ID, "actor", actor(r))
	writeJSON(w, http.StatusAccepted, t)
}

func (s *server) appsProjectMigrate(w http.ResponseWriter, r *http.Request) {
	var b accessBody
	if err := decodeBody(w, r, 64<<10, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	id := r.PathValue("id")
	t, err := s.Apps.MigrateProject(r.Context(), id, b.RunAs, b.Grants, b.Confirm, actor(r))
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	s.Logger.Info("приложения: перевод на свою учётку", "app", id, "grants", b.Grants, "task", t.ID, "actor", actor(r))
	writeJSON(w, http.StatusAccepted, t)
}

func (s *server) appsShareApps(w http.ResponseWriter, r *http.Request) {
	l, err := s.Apps.ShareApps(r.Context(), r.PathValue("name"))
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": l})
}

// projectBody — тело с compose (до 256 КиБ + .env).
func (s *server) projectBody(w http.ResponseWriter, r *http.Request, v *apps.ProjectRequest) bool {
	if err := decodeBody(w, r, 512<<10, v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return false
	}
	return true
}

func (s *server) appsProjects(w http.ResponseWriter, r *http.Request) {
	l, foreign, err := s.Apps.Projects(r.Context())
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	out := map[string]any{"projects": l, "foreign": foreign, "apps": s.Apps.View()}
	if s.Proxy != nil {
		out["domain"] = s.Proxy.Domain() // адреса опубликованных приложений
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) appsProjectCheck(w http.ResponseWriter, r *http.Request) {
	var req apps.ProjectRequest
	if !s.projectBody(w, r, &req) {
		return
	}
	cr, err := s.Apps.Check(r.Context(), req)
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cr)
}

func (s *server) appsProjectCreate(w http.ResponseWriter, r *http.Request) {
	var req apps.ProjectRequest
	if !s.projectBody(w, r, &req) {
		return
	}
	t, err := s.Apps.CreateProject(r.Context(), req, actor(r))
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	s.Logger.Info("приложения: создание", "app", req.ID, "task", t.ID, "actor", actor(r), "confirm", req.Confirm)
	writeJSON(w, http.StatusAccepted, t)
}

func (s *server) appsProjectGet(w http.ResponseWriter, r *http.Request) {
	d, err := s.Apps.ProjectDetails(r.Context(), r.PathValue("id"))
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *server) appsProjectUpdate(w http.ResponseWriter, r *http.Request) {
	var req apps.ProjectRequest
	if !s.projectBody(w, r, &req) {
		return
	}
	req.ID = r.PathValue("id")
	t, err := s.Apps.UpdateProject(r.Context(), req, actor(r))
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	s.Logger.Info("приложения: изменение", "app", req.ID, "task", t.ID, "actor", actor(r), "confirm", req.Confirm)
	writeJSON(w, http.StatusAccepted, t)
}

func (s *server) appsProjectDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	keep := r.URL.Query().Get("keep_data") != "0" // по умолчанию данные сохраняются
	t, err := s.Apps.DeleteProject(r.Context(), id, keep, actor(r))
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	s.Logger.Info("приложения: удаление", "app", id, "keep_data", keep, "task", t.ID, "actor", actor(r))
	writeJSON(w, http.StatusAccepted, t)
}

func (s *server) appsProjectAction(w http.ResponseWriter, r *http.Request) {
	id, act := r.PathValue("id"), r.PathValue("action")
	if act == "check" {
		writeError(w, http.StatusNotFound, "not_found", "нет такого действия")
		return
	}
	t, err := s.Apps.ProjectAction(r.Context(), id, act, actor(r))
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	s.Logger.Info("приложения: действие", "app", id, "action", act, "task", t.ID, "actor", actor(r))
	writeJSON(w, http.StatusAccepted, t)
}

func (s *server) appsProjectLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tail, _ := strconv.Atoi(q.Get("tail"))
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	l, err := s.Apps.Logs(r.Context(), r.PathValue("id"), q.Get("service"), tail, since)
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *server) appsPorts(w http.ResponseWriter, r *http.Request) {
	l, err := s.Apps.Ports(r.Context())
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ports": l})
}
