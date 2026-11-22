package api

import (
	"errors"
	"gamayun-nas/internal/apps"
	"io"
	"mime"
	"net/http"

	"gamayun-nas/internal/catalog"
)

// Каталог приложений — состояние и список, обновление по
// адресу из конфигурации, импорт архива файлом (офлайн).
func (s *server) catalogRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/apps/catalog", s.catalogH(s.catalogGet))
	m.HandleFunc("POST /api/v1/apps/catalog/refresh", s.catalogH(s.catalogRefresh))
	m.HandleFunc("POST /api/v1/apps/catalog/import", s.catalogH(s.catalogImport))
	// Шаг 3: установка из каталога (форма → .env, совместимость, транзакция с откатом).
	m.HandleFunc("POST /api/v1/apps/catalog/{app}/check", s.appsH(s.catalogInstall(false)))
	m.HandleFunc("POST /api/v1/apps/catalog/{app}/install", s.appsH(s.catalogInstall(true)))
	m.HandleFunc("POST /api/v1/apps/projects/{id}/catalog-update/check", s.appsH(s.catalogUpdate(false)))
	m.HandleFunc("POST /api/v1/apps/projects/{id}/catalog-update", s.appsH(s.catalogUpdate(true)))
	m.HandleFunc("POST /api/v1/apps/projects/{id}/catalog-rollback", s.appsH(s.catalogRollback))
}

func (s *server) catalogH(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Catalog == nil {
			writeError(w, http.StatusServiceUnavailable, "catalog_unavailable", "каталог приложений не настроен")
			return
		}
		h(w, r)
	}
}

// CatalogView — ответ GET /api/v1/apps/catalog.
type CatalogView struct {
	State     catalog.State   `json:"state"`
	SourceURL string          `json:"source_url,omitempty"`
	Keys      int             `json:"keys"`
	Apps      []catalog.Entry `json:"apps"`
	// Error — каталог на диске не прошёл повторную проверку.
	Error string `json:"error,omitempty"`
}

func (s *server) catalogView(r *http.Request) (CatalogView, error) {
	st, err := s.Catalog.State(r.Context())
	if err != nil {
		return CatalogView{}, err
	}
	v := CatalogView{State: st, SourceURL: s.CatalogURL, Keys: len(s.Catalog.Trusted), Apps: []catalog.Entry{}}
	if l, err := s.Catalog.Apps(); err != nil {
		v.Error = err.Error()
	} else if l != nil {
		v.Apps = l
	}
	return v, nil
}

func (s *server) catalogGet(w http.ResponseWriter, r *http.Request) {
	v, err := s.catalogView(r)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) catalogLoaded(w http.ResponseWriter, r *http.Request, st catalog.State, err error, how string) {
	switch {
	case errors.Is(err, catalog.ErrSerialRollback):
		writeError(w, http.StatusConflict, "catalog_rollback", err.Error())
		return
	case err != nil:
		s.Logger.Warn("каталог: отказ", "how", how, "err", err, "actor", actor(r))
		writeError(w, http.StatusUnprocessableEntity, "catalog_invalid", err.Error())
		return
	}
	s.Logger.Info("каталог: загружен", "how", how, "serial", st.Serial, "key", st.KeyID, "apps", st.Apps, "actor", actor(r))
	v, err := s.catalogView(r)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) catalogRefresh(w http.ResponseWriter, r *http.Request) {
	if s.CatalogURL == "" {
		writeError(w, http.StatusConflict, "catalog_no_source", "адрес каталога не задан — используйте импорт файлом")
		return
	}
	st, err := s.Catalog.Fetch(r.Context(), s.CatalogURL)
	s.catalogLoaded(w, r, st, err, "url")
}

// catalogImport — тело application/octet-stream (не «простой» запрос —
// межсайтовая отправка формой невозможна); ?allow_rollback=1 — пользователь
// подтвердил установку более старого каталога.
func (s *server) catalogImport(w http.ResponseWriter, r *http.Request) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/octet-stream" {
		writeError(w, http.StatusBadRequest, "invalid_request", "ожидается Content-Type: application/octet-stream")
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, catalog.MaxArchive))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "архив каталога больше предела")
		return
	}
	st, err := s.Catalog.Load(r.Context(), b, "file", r.URL.Query().Get("allow_rollback") == "1")
	s.catalogLoaded(w, r, st, err, "file")
}

// catalogInstall — проверка (install=false) или установка приложения каталога.
// Значения формы (в т. ч. секреты) в журнал не пишутся.
func (s *server) catalogInstall(install bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req apps.CatalogRequest
		if err := decodeBody(w, r, 64<<10, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		req.App = r.PathValue("app")
		if !install {
			cr, err := s.Apps.CatalogCheck(r.Context(), req)
			if err != nil {
				s.appsError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, cr)
			return
		}
		t, err := s.Apps.InstallCatalog(r.Context(), req, actor(r))
		if err != nil {
			s.appsError(w, r, err)
			return
		}
		s.Logger.Info("приложения: установка из каталога", "app", req.App, "id", req.ID, "task", t.ID, "actor", actor(r), "confirm", req.Confirm)
		writeJSON(w, http.StatusAccepted, t)
	}
}

// catalogUpdate — проверка (run=false) или обновление приложения каталога до
// версии текущего каталога. Значения формы в журнал не пишутся.
func (s *server) catalogUpdate(run bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req apps.CatalogUpdateRequest
		if err := decodeBody(w, r, 64<<10, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		id := r.PathValue("id")
		if !run {
			uc, err := s.Apps.CheckCatalogUpdate(r.Context(), id, req)
			if err != nil {
				s.appsError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, uc)
			return
		}
		t, err := s.Apps.UpdateCatalog(r.Context(), id, req, actor(r))
		if err != nil {
			s.appsError(w, r, err)
			return
		}
		s.Logger.Info("приложения: обновление из каталога", "id", id, "task", t.ID, "actor", actor(r), "confirm", req.Confirm)
		writeJSON(w, http.StatusAccepted, t)
	}
}

// catalogRollback — ручной откат на предыдущую версию (данные после
// обновления теряются).
func (s *server) catalogRollback(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.Apps.RollbackCatalog(r.Context(), id, actor(r))
	if err != nil {
		s.appsError(w, r, err)
		return
	}
	s.Logger.Info("приложения: откат версии каталога", "id", id, "task", t.ID, "actor", actor(r))
	writeJSON(w, http.StatusAccepted, t)
}
