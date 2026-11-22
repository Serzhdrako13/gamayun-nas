package api

import (
	"net/http"

	"gamayun-nas/internal/snapshots"
)

// Снапшоты общих папок — список, ручное создание и удаление,
// расписание, просмотр снапшота и восстановление копией (задача).
func (s *server) snapshotRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/nas/shares/{name}/snapshots", s.snapH(s.snapInfo))
	m.HandleFunc("POST /api/v1/nas/shares/{name}/snapshots", s.snapH(s.snapCreate))
	m.HandleFunc("DELETE /api/v1/nas/shares/{name}/snapshots/{snap}", s.snapH(s.snapDelete))
	m.HandleFunc("GET /api/v1/nas/shares/{name}/snapshots/{snap}/files", s.snapH(s.snapBrowse))
	m.HandleFunc("POST /api/v1/nas/shares/{name}/snapshots/{snap}/restore", s.snapH(s.snapRestore))
	m.HandleFunc("PUT /api/v1/nas/shares/{name}/snapshot-policy", s.snapH(s.snapPolicy))
}

func (s *server) snapH(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Snapshots == nil {
			writeError(w, http.StatusServiceUnavailable, "snapshots_unavailable", "снимки не настроены")
			return
		}
		h(w, r)
	}
}

func (s *server) snapInfo(w http.ResponseWriter, r *http.Request) {
	x, err := s.Snapshots.Info(r.Context(), r.PathValue("name"))
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (s *server) snapCreate(w http.ResponseWriter, r *http.Request) {
	x, err := s.Snapshots.Create(r.Context(), r.PathValue("name"))
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("снимок создан вручную", "share", r.PathValue("name"), "snapshot", x.Name, "actor", actor(r))
	writeJSON(w, http.StatusCreated, x)
}

func (s *server) snapDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.Snapshots.Delete(r.Context(), r.PathValue("name"), r.PathValue("snap")); err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("снимок удалён", "share", r.PathValue("name"), "snapshot", r.PathValue("snap"), "actor", actor(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) snapBrowse(w http.ResponseWriter, r *http.Request) {
	x, err := s.Snapshots.Browse(r.Context(), r.PathValue("name"), r.PathValue("snap"), r.URL.Query().Get("path"))
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

// SnapRestoreRequest — тело POST …/snapshots/{snap}/restore.
type SnapRestoreRequest struct {
	Path string `json:"path"`
}

func (s *server) snapRestore(w http.ResponseWriter, r *http.Request) {
	var req SnapRestoreRequest
	if !s.body(w, r, &req) {
		return
	}
	t, err := s.Snapshots.Restore(r.Context(), r.PathValue("name"), r.PathValue("snap"), req.Path)
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("восстановление из снимка", "share", r.PathValue("name"), "snapshot", r.PathValue("snap"), "path", req.Path, "task", t.ID, "actor", actor(r))
	writeJSON(w, http.StatusAccepted, t)
}

// SnapPolicyRequest — тело PUT …/snapshot-policy.
type SnapPolicyRequest = snapshots.Policy

func (s *server) snapPolicy(w http.ResponseWriter, r *http.Request) {
	var req SnapPolicyRequest
	if !s.body(w, r, &req) {
		return
	}
	p, err := s.Snapshots.SetPolicy(r.Context(), r.PathValue("name"), req)
	if err != nil {
		s.nasError(w, r, err)
		return
	}
	s.Logger.Info("расписание снимков изменено", "share", r.PathValue("name"), "policy", p, "actor", actor(r))
	writeJSON(w, http.StatusOK, p)
}
