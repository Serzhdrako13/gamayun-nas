package api

import (
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"

	"gamayun-nas/internal/files"
	"gamayun-nas/internal/helper"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/tasks"
)

// Веб-файловый менеджер. Права — как в Samba (files.Service).
func (s *server) filesRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/files", s.filesH(s.filesRoots))
	m.HandleFunc("GET /api/v1/files/{root}/list", s.filesH(s.filesList))
	m.HandleFunc("GET /api/v1/files/{root}/download", s.filesH(s.filesDownload))
	m.HandleFunc("POST /api/v1/files/{root}/upload", s.filesH(s.filesUpload))
	m.HandleFunc("POST /api/v1/files/{root}/mkdir", s.filesH(s.filesMkdir))
	m.HandleFunc("POST /api/v1/files/{root}/rename", s.filesH(s.filesRename))
	m.HandleFunc("POST /api/v1/files/transfer", s.filesH(s.filesTransfer))
	m.HandleFunc("DELETE /api/v1/files/tasks/{id}", s.filesH(s.filesCancel))
}

func (s *server) filesH(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Files == nil {
			writeError(w, http.StatusServiceUnavailable, "files_unavailable", "файловый менеджер не настроен")
			return
		}
		h(w, r)
	}
}

func (s *server) filesError(w http.ResponseWriter, r *http.Request, err error) {
	var re *helper.RemoteError
	switch {
	case errors.Is(err, files.ErrNotTransfer):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, tasks.ErrNotActive):
		writeError(w, http.StatusConflict, "not_active", err.Error())
	case errors.Is(err, files.ErrForbidden):
		// 403 в API занят под CSRF/сессию; «нет доступа к папке» — 404, как
		// будто папки нет (не раскрываем имена чужих папок).
		writeError(w, http.StatusNotFound, "no_access", err.Error())
	case errors.Is(err, nas.ErrNoSpace), errors.As(err, &re) && re.Code == helper.CodeNoSpace:
		writeError(w, http.StatusInsufficientStorage, "no_space", err.Error())
	default:
		s.nasError(w, r, err)
	}
}

func (s *server) filesRoots(w http.ResponseWriter, r *http.Request) {
	roots, err := s.Files.List(r.Context(), actor(r))
	if err != nil {
		s.filesError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"roots": roots})
}

func (s *server) filesList(w http.ResponseWriter, r *http.Request) {
	d, err := s.Files.ListDir(r.Context(), actor(r), r.PathValue("root"), r.URL.Query().Get("path"))
	if err != nil {
		s.filesError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// contentDisposition — attachment с именем в UTF-8 (RFC 6266/5987) и
// ASCII-запасом без кавычек и управляющих символов.
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return `attachment; filename="` + ascii + `"; filename*=UTF-8''` + url.PathEscape(name)
}

func (s *server) filesDownload(w http.ResponseWriter, r *http.Request) {
	ctx, user, root, p := r.Context(), actor(r), r.PathValue("root"), r.URL.Query().Get("path")
	fi, err := s.Files.Stat(ctx, user, root, p)
	if err != nil {
		s.filesError(w, r, err)
		return
	}
	switch fi.Type {
	case "file":
		f, fi, err := s.Files.Open(ctx, user, root, p)
		if err != nil {
			s.filesError(w, r, err)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Disposition", contentDisposition(fi.Name))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, "", fi.MTime, f) // Range, If-Modified-Since
	case "dir":
		name := fi.Name
		if name == "" {
			name = root
		}
		w.Header().Set("Content-Disposition", contentDisposition(name+".zip"))
		w.Header().Set("Content-Type", "application/zip")
		if err := s.Files.Zip(ctx, user, root, p, w); err != nil {
			// Заголовки уже ушли: обрываем соединение, чтобы архив не выглядел целым.
			s.Logger.Warn("файлы: архив прерван", "root", root, "path", p, "err", err, "actor", user)
			panic(http.ErrAbortHandler)
		}
	default:
		writeError(w, http.StatusBadRequest, "refused", "скачать можно файл или папку; это ссылка или особый файл")
	}
}

// filesUpload — тело запроса — содержимое файла; Content-Length обязателен
// (по нему проверяется место). Параметры — в строке запроса: path (каталог),
// name, on_conflict (error | rename).
func (s *server) filesUpload(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength < 0 {
		writeError(w, http.StatusLengthRequired, "length_required", "нужен заголовок Content-Length")
		return
	}
	q := r.URL.Query()
	p, err := s.Files.Upload(r.Context(), actor(r), r.PathValue("root"), q.Get("path"), q.Get("name"), q.Get("on_conflict"), r.ContentLength, r.Body)
	if err != nil {
		s.filesError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"path": p, "name": path.Base(p)})
}

// FilesNameRequest — mkdir (name в каталоге path) и rename (path → name).
type FilesNameRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

func (s *server) filesMkdir(w http.ResponseWriter, r *http.Request) {
	var req FilesNameRequest
	if !s.body(w, r, &req) {
		return
	}
	p, err := s.Files.Mkdir(r.Context(), actor(r), r.PathValue("root"), req.Path, req.Name)
	if err != nil {
		s.filesError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"path": p})
}

func (s *server) filesRename(w http.ResponseWriter, r *http.Request) {
	var req FilesNameRequest
	if !s.body(w, r, &req) {
		return
	}
	p, err := s.Files.Rename(r.Context(), actor(r), r.PathValue("root"), req.Path, req.Name)
	if err != nil {
		s.filesError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p})
}

func (s *server) filesTransfer(w http.ResponseWriter, r *http.Request) {
	var req files.TransferRequest
	if !s.body(w, r, &req) {
		return
	}
	t, err := s.Files.Transfer(r.Context(), actor(r), req)
	if err != nil {
		s.filesError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/tasks/"+t.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"task": t, "id": t.ID})
}

func (s *server) filesCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.Files.CancelTransfer(r.Context(), actor(r), r.PathValue("id")); err != nil {
		s.filesError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
