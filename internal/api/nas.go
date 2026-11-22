package api

import (
	"errors"
	"net/http"
	"strconv"

	"gamayun-nas/internal/helper"
	"gamayun-nas/internal/nas"
)

// Пользователи, группы и общие папки NAS.
func (s *server) nasRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/nas/users", s.nasH(s.listNasUsers))
	m.HandleFunc("POST /api/v1/nas/users", s.nasH(s.createNasUser))
	m.HandleFunc("DELETE /api/v1/nas/users/{name}", s.nasH(s.deleteNasUser))
	m.HandleFunc("PUT /api/v1/nas/users/{name}/password", s.nasH(s.nasPassword))
	m.HandleFunc("PUT /api/v1/nas/users/{name}/enabled", s.nasH(s.nasEnabled))
	m.HandleFunc("PUT /api/v1/nas/users/{name}/ssh", s.nasH(s.nasSSH))
	m.HandleFunc("GET /api/v1/nas/groups", s.nasH(s.listNasGroups))
	m.HandleFunc("POST /api/v1/nas/groups", s.nasH(s.createNasGroup))
	m.HandleFunc("DELETE /api/v1/nas/groups/{name}", s.nasH(s.deleteNasGroup))
	m.HandleFunc("PUT /api/v1/nas/groups/{name}/members", s.nasH(s.nasMembers))
	m.HandleFunc("GET /api/v1/nas/shares", s.nasH(s.listNasShares))
	m.HandleFunc("POST /api/v1/nas/shares", s.nasH(s.createNasShare))
	m.HandleFunc("POST /api/v1/nas/shares/apply", s.nasH(s.applyNas))
	m.HandleFunc("GET /api/v1/nas/shares/{name}", s.nasH(s.getNasShare))
	m.HandleFunc("GET /api/v1/nas/shares/{name}/dirs", s.nasH(s.nasShareDirs))
	m.HandleFunc("POST /api/v1/nas/shares/{name}/dirs", s.nasH(s.nasShareMkdir))
	m.HandleFunc("PATCH /api/v1/nas/shares/{name}/dirs", s.nasH(s.nasShareRename))
	m.HandleFunc("DELETE /api/v1/nas/shares/{name}/dirs", s.nasH(s.nasShareRmdir))
	m.HandleFunc("POST /api/v1/nas/shares/{name}/dirs/move", s.nasH(s.nasShareMove))
	m.HandleFunc("DELETE /api/v1/nas/shares/{name}", s.nasH(s.deleteNasShare))
	m.HandleFunc("PUT /api/v1/nas/shares/{name}/access", s.nasH(s.nasAccess))
}

func (s *server) nasH(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.NAS == nil {
			writeError(w, http.StatusServiceUnavailable, "nas_unavailable", "управление пользователями и папками не настроено")
			return
		}
		h(w, r)
	}
}

func (s *server) nasError(w http.ResponseWriter, r *http.Request, err error) {
	var re *helper.RemoteError
	switch {
	case errors.Is(err, nas.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, nas.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, nas.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.As(err, &re) && nasRemote[re.Code].status != 0:
		// Отказы helper по существу запроса — ошибка клиента, не шлюза.
		writeError(w, nasRemote[re.Code].status, nasRemote[re.Code].code, re.Message)
	case errors.Is(err, helper.ErrUnavailable), errors.As(err, &re):
		s.helperError(w, r, err)
	default:
		s.internalError(w, r, err)
	}
}

// nasRemote — HTTP-статус и код API для кодов ошибок helper, означающих
// отказ по существу запроса; прочие коды — 502 (helperError). refused (пул
// не смонтирован, ссылка/файл в пути) — 400: запрос невыполним, а 403 в API
// занят под CSRF/сессию.
var nasRemote = map[string]struct {
	status int
	code   string
}{
	helper.CodeInvalidParams: {http.StatusBadRequest, "invalid_request"},
	helper.CodeRefused:       {http.StatusBadRequest, "refused"},
	helper.CodeNotFound:      {http.StatusNotFound, "not_found"},
	helper.CodeConflict:      {http.StatusConflict, "conflict"},
}

func (s *server) body(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := decodeBody(w, r, 64<<10, v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return false
	}
	return true
}

// NasUserRequest — создание пользователя. Пароль в журналы не попадает.
type NasUserRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Admin    bool   `json:"admin"` // добавить в administrators (вход в веб)
	// SSH — вход по SSH; не задан — как admin.
	SSH *bool `json:"ssh"`
}

func (s *server) listNasUsers(w http.ResponseWriter, r *http.Request) {
	if err := s.NAS.Sync(r.Context()); err != nil {
		s.nasError(w, r, err)
		return
	}
	l, err := s.NAS.ListUsers(r.Context())
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": l})
}

func (s *server) createNasUser(w http.ResponseWriter, r *http.Request) {
	var req NasUserRequest
	if !s.body(w, r, &req) {
		return
	}
	ssh := req.Admin
	if req.SSH != nil {
		ssh = *req.SSH
	}
	u, err := s.NAS.CreateUserSSH(r.Context(), req.Name, req.Password, req.Admin, ssh)
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("пользователь NAS создан", "user", u.Name, "actor", actor(r))
	writeJSON(w, http.StatusCreated, u)
}

func (s *server) deleteNasUser(w http.ResponseWriter, r *http.Request) {
	if err := s.NAS.DeleteUser(r.Context(), r.PathValue("name")); err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Warn("пользователь NAS удалён", "user", r.PathValue("name"), "actor", actor(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) nasPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !s.body(w, r, &req) {
		return
	}
	if err := s.NAS.SetPassword(r.Context(), r.PathValue("name"), req.Password); err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("пароль пользователя NAS изменён", "user", r.PathValue("name"), "actor", actor(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) nasEnabled(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !s.body(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "enabled обязателен")
		return
	}
	if err := s.NAS.SetEnabled(r.Context(), r.PathValue("name"), *req.Enabled); err != nil {
		s.nasError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) nasSSH(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SSH *bool `json:"ssh"`
	}
	if !s.body(w, r, &req) {
		return
	}
	if req.SSH == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "ssh обязателен")
		return
	}
	if err := s.NAS.SetSSH(r.Context(), r.PathValue("name"), *req.SSH); err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("вход по SSH пользователя NAS изменён", "user", r.PathValue("name"), "ssh", *req.SSH, "actor", actor(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listNasGroups(w http.ResponseWriter, r *http.Request) {
	l, err := s.NAS.ListGroups(r.Context())
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": l})
}

func (s *server) createNasGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !s.body(w, r, &req) {
		return
	}
	g, err := s.NAS.CreateGroup(r.Context(), req.Name)
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

func (s *server) deleteNasGroup(w http.ResponseWriter, r *http.Request) {
	if err := s.NAS.DeleteGroup(r.Context(), r.PathValue("name")); err != nil {
		s.nasError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) nasMembers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Members []string `json:"members"`
	}
	if !s.body(w, r, &req) {
		return
	}
	if err := s.NAS.SetMembers(r.Context(), r.PathValue("name"), req.Members); err != nil {
		s.nasError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listNasShares(w http.ResponseWriter, r *http.Request) {
	l, err := s.NAS.ListShares(r.Context())
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": l})
}

func (s *server) getNasShare(w http.ResponseWriter, r *http.Request) {
	x, err := s.NAS.GetShare(r.Context(), r.PathValue("name"))
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (s *server) createNasShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Pool    string `json:"pool"`
		Comment string `json:"comment"`
	}
	if !s.body(w, r, &req) {
		return
	}
	x, err := s.NAS.CreateShare(r.Context(), req.Name, req.Pool, req.Comment)
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("общая папка создана", "share", x.Name, "pool", x.Pool, "actor", actor(r))
	writeJSON(w, http.StatusCreated, x)
}

func (s *server) deleteNasShare(w http.ResponseWriter, r *http.Request) {
	if err := s.NAS.DeleteShare(r.Context(), r.PathValue("name")); err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Warn("общая папка удалена из Samba (данные на пуле сохранены)", "share", r.PathValue("name"), "actor", actor(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) nasAccess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Access []nas.Access `json:"access"`
	}
	if !s.body(w, r, &req) {
		return
	}
	if err := s.NAS.SetAccess(r.Context(), r.PathValue("name"), req.Access); err != nil {
		s.nasError(w, r, err)
		return
	}
	x, err := s.NAS.GetShare(r.Context(), r.PathValue("name"))
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("права общей папки изменены", "share", x.Name, "actor", actor(r))
	writeJSON(w, http.StatusOK, x)
}

func (s *server) applyNas(w http.ResponseWriter, r *http.Request) {
	res, err := s.NAS.Apply(r.Context())
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// nasShareDirs — вложенные каталоги общей папки: ?path=a/b&depth=1..3.
func (s *server) nasShareDirs(w http.ResponseWriter, r *http.Request) {
	depth := 1
	if v := r.URL.Query().Get("depth"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "depth: число")
			return
		}
		depth = n
	}
	x, err := s.NAS.ShareDirs(r.Context(), r.PathValue("name"), r.URL.Query().Get("path"), depth)
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

// NasDirRequest — создание (path — родитель) и переименование (path —
// каталог, name — новое имя) вложенного каталога общей папки.
type NasDirRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

func (s *server) nasShareMkdir(w http.ResponseWriter, r *http.Request) {
	var req NasDirRequest
	if !s.body(w, r, &req) {
		return
	}
	x, err := s.NAS.ShareMkdir(r.Context(), r.PathValue("name"), req.Path, req.Name)
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("каталог в общей папке создан", "share", r.PathValue("name"), "path", x.Path, "actor", actor(r))
	writeJSON(w, http.StatusCreated, x)
}

func (s *server) nasShareRename(w http.ResponseWriter, r *http.Request) {
	var req NasDirRequest
	if !s.body(w, r, &req) {
		return
	}
	x, err := s.NAS.ShareRename(r.Context(), r.PathValue("name"), req.Path, req.Name)
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("каталог в общей папке переименован", "share", r.PathValue("name"), "from", req.Path, "path", x.Path, "actor", actor(r))
	writeJSON(w, http.StatusOK, x)
}

// NasDirMoveRequest — перенос вложенного каталога path в каталог to той же
// общей папки ("" — корень).
type NasDirMoveRequest struct {
	Path string `json:"path"`
	To   string `json:"to"`
}

func (s *server) nasShareMove(w http.ResponseWriter, r *http.Request) {
	var req NasDirMoveRequest
	if !s.body(w, r, &req) {
		return
	}
	x, err := s.NAS.ShareMove(r.Context(), r.PathValue("name"), req.Path, req.To)
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("каталог в общей папке перенесён", "share", r.PathValue("name"), "from", req.Path, "path", x.Path, "actor", actor(r))
	writeJSON(w, http.StatusOK, x)
}

// nasShareRmdir — удалить пустой каталог: ?path=a/b.
func (s *server) nasShareRmdir(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	if _, err := s.NAS.ShareRmdir(r.Context(), r.PathValue("name"), rel); err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Warn("каталог в общей папке удалён", "share", r.PathValue("name"), "path", rel, "actor", actor(r))
	w.WriteHeader(http.StatusNoContent)
}
