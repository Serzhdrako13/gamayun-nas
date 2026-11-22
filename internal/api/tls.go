package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/tlscert"
)

// TLSStore — действующий сертификат HTTPS (реализует *tlscert.Store).
type TLSStore interface {
	Info() tlscert.Info
	Install(certPEM, keyPEM []byte) (tlscert.Info, error)
	Remove() (tlscert.Info, error)
}

// Свой сертификат HTTPS из веба. Только для вошедших (за Auth).
func (s *server) tlsRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/system/tls", s.getTLS)
	m.HandleFunc("PUT /api/v1/system/tls", s.putTLS)
	m.HandleFunc("DELETE /api/v1/system/tls", s.deleteTLS)
}

// TLSRequest — загрузка своего сертификата (PEM). Ключ не журналируется.
type TLSRequest struct {
	Cert string `json:"cert"` // сертификат сервера, затем промежуточные
	Key  string `json:"key"`
}

func (s *server) tlsReady(w http.ResponseWriter) bool {
	if s.TLS == nil {
		writeError(w, http.StatusServiceUnavailable, "tls_disabled", "HTTPS отключён в конфигурации (tls.disable)")
		return false
	}
	return true
}

func (s *server) getTLS(w http.ResponseWriter, r *http.Request) {
	if s.tlsReady(w) {
		writeJSON(w, http.StatusOK, s.TLS.Info())
	}
}

func (s *server) putTLS(w http.ResponseWriter, r *http.Request) {
	if !s.tlsReady(w) {
		return
	}
	var req TLSRequest
	if !s.body(w, r, &req) {
		return
	}
	in, err := s.TLS.Install([]byte(req.Cert), []byte(req.Key))
	if s.tlsError(w, err) {
		return
	}
	s.tlsChanged(r, in)
	writeJSON(w, http.StatusOK, in)
}

func (s *server) deleteTLS(w http.ResponseWriter, r *http.Request) {
	if !s.tlsReady(w) {
		return
	}
	in, err := s.TLS.Remove()
	if s.tlsError(w, err) {
		return
	}
	s.tlsChanged(r, in)
	writeJSON(w, http.StatusOK, in)
}

func (s *server) tlsError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, tlscert.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_certificate", err.Error())
	case errors.Is(err, tlscert.ErrReadOnly):
		writeError(w, http.StatusConflict, "tls_from_config", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
	return true
}

// tlsChanged — журнал gamayund и событие tls.certificate_changed (без ключа).
func (s *server) tlsChanged(r *http.Request, in tlscert.Info) {
	s.Logger.Info("сертификат HTTPS заменён", "source", in.Source, "fingerprint_sha256", in.Fingerprint,
		"subject", in.Subject, "not_after", in.NotAfter, "actor", actor(r))
	if s.Events == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"source": in.Source, "fingerprint_sha256": in.Fingerprint,
		"subject": in.Subject, "not_after": in.NotAfter, "actor": actor(r)})
	if _, err := s.Events.Emit(r.Context(), events.Event{Type: "tls.certificate_changed", Data: b}, true); err != nil {
		s.Logger.Warn("событие tls.certificate_changed", "err", err)
	}
}
