package notify

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gamayun-nas/internal/events"
)

type hook struct {
	mu     sync.Mutex
	status int
	bodies [][]byte
	hdr    []http.Header
}

func newHook(t *testing.T, status int) (*hook, *httptest.Server, string) {
	t.Helper()
	h := &hook{status: status}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies = append(h.bodies, b)
		h.hdr = append(h.hdr, r.Header.Clone())
		st := h.status
		h.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(srv.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	return h, srv, ca
}

func TestWebhookSignedPost(t *testing.T) {
	h, srv, ca := newHook(t, 204)
	now := time.Unix(1_800_000_000, 0)
	s := &WebhookSender{Cfg: WebhookConfig{Enabled: true, URL: srv.URL + "/hook", Secret: "k3y", CACert: ca},
		Hostname: "nas1", Now: func() time.Time { return now }}
	if err := s.Send(context.Background(), note()); err != nil {
		t.Fatal(err)
	}
	var p WebhookPayload
	if err := json.Unmarshal(h.bodies[0], &p); err != nil {
		t.Fatal(err)
	}
	if p.Version != 1 || p.Host != "nas1" || p.Event.Type != events.ArrayDegraded || p.Event.EventID != 42 || !strings.Contains(p.Title, "Массив RAID неполный: выбыл диск") {
		t.Fatalf("тело: %+v", p)
	}
	ts := h.hdr[0].Get(HeaderTimestamp)
	if ts != "1800000000" || h.hdr[0].Get(HeaderSignature) != Sign("k3y", ts, h.bodies[0]) {
		t.Fatalf("подпись: %v", h.hdr[0])
	}
	if strings.Contains(string(h.bodies[0]), "k3y") {
		t.Fatal("ключ в теле")
	}
}

func TestWebhookTLSAndStatus(t *testing.T) {
	h, srv, ca := newHook(t, 500)
	c := WebhookConfig{Enabled: true, URL: srv.URL}
	if err := (&WebhookSender{Cfg: c}).Test(context.Background()); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("ждали отказ проверки сертификата: %v", err)
	}
	if len(h.bodies) != 0 {
		t.Fatal("запрос ушёл без проверки TLS")
	}
	c.CACert = ca
	if err := (&WebhookSender{Cfg: c}).Test(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("ждали ошибку 500: %v", err)
	}
	h.status = 302 // перенаправление — не успех и не выполняется
	if err := (&WebhookSender{Cfg: c}).Test(context.Background()); err == nil {
		t.Fatal("302 принят как успех")
	}
	c.CACert, c.InsecureSkipVerify = "", true
	h.status = 200
	if err := (&WebhookSender{Cfg: c}).Test(context.Background()); err != nil {
		t.Fatalf("insecure_skip_verify: %v", err)
	}
}

func TestWebhookValidate(t *testing.T) {
	ok := WebhookConfig{Enabled: true, URL: "https://mon.example.org/x"}
	if ok.Validate() != nil {
		t.Fatal("https отклонён")
	}
	for i, c := range []WebhookConfig{
		{Enabled: true, URL: "http://mon/x"},
		{Enabled: true, URL: "http://mon/x", AllowHTTP: true, Secret: "s"},
		{Enabled: true, URL: "ftp://mon/x"},
		{Enabled: true, URL: "https://u:p@mon/x"},
		{Enabled: true, URL: "мусор"},
		{Enabled: true, URL: "https://mon/x", CACert: "x"},
	} {
		if c.Validate() == nil {
			t.Fatalf("случай %d прошёл", i)
		}
	}
	if (WebhookConfig{Enabled: true, URL: "http://mon/x", AllowHTTP: true}).Validate() != nil {
		t.Fatal("явный http отклонён")
	}
}

func TestWebhookEndToEndViaOutbox(t *testing.T) {
	ctx := context.Background()
	h, srv, ca := newHook(t, 200)
	db, l := open(t, filepath.Join(t.TempDir(), "d.db"))
	if err := SaveWebhook(ctx, db, WebhookConfig{Enabled: true, URL: srv.URL, CACert: ca, Secret: "s"}); err != nil {
		t.Fatal(err)
	}
	c, err := LoadWebhook(ctx, db)
	if err != nil || c.Secret != "s" {
		t.Fatalf("%+v %v", c, err)
	}
	d := NewDeliverer(db, DeliverOptions{})
	d.ApplyWebhook(c)
	s := New(db, l, Options{Channels: d.Channels})
	s.Init(ctx)
	emit(t, l, events.DiskRemoved, events.SevWarning)
	s.CatchUp(ctx)
	if _, err := d.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.bodies) != 1 {
		t.Fatalf("запросов %d", len(h.bodies))
	}
}
