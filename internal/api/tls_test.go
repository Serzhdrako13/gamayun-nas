package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/tlscert"
)

// TestTLSCertificateAPI — просмотр, загрузка своего сертификата,
// отказ негодного, событие без ключа, возврат самоподписанного.
func TestTLSCertificateAPI(t *testing.T) {
	now := time.Now()
	st, _, err := tlscert.NewStore(filepath.Join(t.TempDir(), "tls"), "", "", func() tlscert.Names {
		return tlscert.Names{Hosts: []string{"nas"}}
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(o *Options) { o.TLS = st })
	r := e.get(t, "/api/v1/system/tls")
	if r.status != http.StatusOK || r.body["source"] != tlscert.SourceSelfSigned || r.body["editable"] != true {
		t.Fatalf("GET: %d %v", r.status, r.body)
	}
	self := r.body["fingerprint_sha256"]

	c, k, _ := tlscert.Generate(tlscert.Names{Hosts: []string{"nas.home"}}, now)
	bad, _ := json.Marshal(TLSRequest{Cert: string(c), Key: "мусор"})
	if r := e.do(t, http.MethodPut, "/api/v1/system/tls", "application/json", string(bad)); r.status != http.StatusBadRequest || errCode(r) != "invalid_certificate" {
		t.Fatalf("негодный: %d %v", r.status, r.body)
	}
	good, _ := json.Marshal(TLSRequest{Cert: string(c), Key: string(k)})
	r = e.do(t, http.MethodPut, "/api/v1/system/tls", "application/json", string(good))
	if r.status != http.StatusOK || r.body["source"] != tlscert.SourceCustom || r.body["subject"] != "nas.home" {
		t.Fatalf("PUT: %d %v", r.status, r.body)
	}
	evs, _ := e.log.Query(context.Background(), events.Filter{Type: "tls.certificate_changed", Limit: 10})
	if len(evs) != 1 {
		t.Fatalf("событий: %d", len(evs))
	}
	if b := string(evs[0].Data); strings.Contains(b, "PRIVATE") || !strings.Contains(b, "custom") {
		t.Errorf("событие: %s", b)
	}
	r = e.do(t, http.MethodDelete, "/api/v1/system/tls", "", "")
	if r.status != http.StatusOK || r.body["fingerprint_sha256"] != self {
		t.Fatalf("DELETE: %d %v", r.status, r.body)
	}

	// TLS отключён — 503.
	off := newEnv(t, nil)
	if r := off.get(t, "/api/v1/system/tls"); r.status != http.StatusServiceUnavailable {
		t.Errorf("без TLS: %d", r.status)
	}
}
