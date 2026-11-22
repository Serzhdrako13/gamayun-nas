package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/notify"
)

func TestNotifyUnavailable(t *testing.T) {
	e := newEnv(t, nil)
	if r := e.get(t, "/api/v1/notifications"); r.status != 503 || errCode(r) != "notify_unavailable" {
		t.Errorf("%d %s", r.status, r.raw)
	}
}

func TestNotifyCenterAndSecrets(t *testing.T) {
	var logs bytes.Buffer
	var svc *notify.Service
	e := newEnv(t, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		o.Deliver = notify.NewDeliverer(o.DB, notify.DeliverOptions{})
		svc = notify.New(o.DB, o.Events, notify.Options{Channels: o.Deliver.Channels})
		o.Notify = svc
	})
	ctx := context.Background()
	svc.Init(ctx)
	for _, sev := range []string{events.SevWarning, events.SevCritical} {
		if _, err := e.log.Emit(ctx, events.Event{Type: events.DiskRemoved, Severity: sev, Subject: "disk:a"}, true); err != nil {
			t.Fatal(err)
		}
	}
	svc.CatchUp(ctx)

	r := e.get(t, "/api/v1/notifications")
	var list struct {
		Items  []notify.Notification `json:"items"`
		Unread int                   `json:"unread"`
	}
	json.Unmarshal([]byte(r.raw), &list)
	if r.status != 200 || len(list.Items) != 2 || list.Unread != 2 {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	if r := e.do(t, "POST", "/api/v1/notifications/read", "application/json", `{}`); r.status != 200 || !strings.Contains(r.raw, `"unread":0`) {
		t.Fatalf("read: %d %s", r.status, r.raw)
	}

	// Сохранение SMTP с паролем: в ответе и журнале пароля нет.
	const pw = "Sup3r-Secret-pw"
	body := `{"enabled":true,"host":"mail.example.org","port":587,"security":"starttls","username":"nas","password":"` + pw + `","from":"nas@example.org","to":["a@example.org"]}`
	r = e.do(t, "PUT", "/api/v1/notifications/channels/smtp", "application/json", body)
	if r.status != 200 || strings.Contains(r.raw, pw) || !strings.Contains(r.raw, `"password_set":true`) {
		t.Fatalf("put smtp: %d %s", r.status, r.raw)
	}
	// Без поля password — прежний пароль сохраняется.
	r = e.do(t, "PUT", "/api/v1/notifications/channels/smtp", "application/json", strings.Replace(body, `"password":"`+pw+`",`, "", 1))
	if r.status != 200 || !strings.Contains(r.raw, `"password_set":true`) {
		t.Fatalf("keep pw: %d %s", r.status, r.raw)
	}
	c, _ := notify.LoadSMTP(ctx, e.db)
	if c.Password != pw {
		t.Fatal("пароль потерян при сохранении без поля")
	}
	// Без шифрования с паролем — 400.
	if r := e.do(t, "PUT", "/api/v1/notifications/channels/smtp", "application/json", strings.Replace(body, "starttls", "none", 1)); r.status != 400 {
		t.Fatalf("none+pw: %d %s", r.status, r.raw)
	}

	// Webhook с токеном в пути: в журнале только узел.
	const tok = "T0KEN-in-path"
	r = e.do(t, "PUT", "/api/v1/notifications/channels/webhook", "application/json",
		`{"enabled":true,"url":"https://hooks.example.org/`+tok+`","secret":"hm4c-key"}`)
	if r.status != 200 || strings.Contains(r.raw, "hm4c-key") || !strings.Contains(r.raw, `"secret_set":true`) {
		t.Fatalf("put webhook: %d %s", r.status, r.raw)
	}

	r = e.get(t, "/api/v1/notifications/channels")
	if r.status != 200 || strings.Contains(r.raw, pw) || strings.Contains(r.raw, "hm4c-key") {
		t.Fatalf("channels: %d %s", r.status, r.raw)
	}
	var ch struct {
		Status []notify.ChannelStatus `json:"status"`
	}
	json.Unmarshal([]byte(r.raw), &ch)
	if len(ch.Status) != 2 || !ch.Status[0].Enabled || !ch.Status[1].Enabled {
		t.Fatalf("статус каналов: %s", r.raw)
	}
	for _, s := range []string{pw, "hm4c-key", tok} {
		if strings.Contains(logs.String(), s) {
			t.Fatalf("секрет %q в журнале:\n%s", s, logs.String())
		}
	}
}

func TestNotifyChannelTest(t *testing.T) {
	var logs bytes.Buffer
	e := newEnv(t, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		o.Deliver = notify.NewDeliverer(o.DB, notify.DeliverOptions{})
		o.Notify = notify.New(o.DB, o.Events, notify.Options{})
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	// http:// без allow_http — 400; с allow_http — проверка проходит до ответа 503.
	if r := e.do(t, "POST", "/api/v1/notifications/channels/webhook/test", "application/json", `{"url":"`+srv.URL+`/tok"}`); r.status != 400 {
		t.Fatalf("http: %d %s", r.status, r.raw)
	}
	r := e.do(t, "POST", "/api/v1/notifications/channels/webhook/test", "application/json", `{"url":"`+srv.URL+`/tok","allow_http":true}`)
	if r.status != 200 || !strings.Contains(r.raw, `"ok":false`) || !strings.Contains(r.raw, "503") {
		t.Fatalf("test: %d %s", r.status, r.raw)
	}
	// Проверка не сохраняет настройки.
	if c, _ := notify.LoadWebhook(context.Background(), e.db); c.URL != "" {
		t.Fatal("проверка сохранила настройки")
	}
	// Недоступный webhook: адрес с путём не попадает в ответ.
	r = e.do(t, "POST", "/api/v1/notifications/channels/webhook/test", "application/json", `{"url":"https://127.0.0.1:1/secret-path"}`)
	if r.status != 200 || strings.Contains(r.raw, "secret-path") || strings.Contains(logs.String(), "secret-path") {
		t.Fatalf("недоступный: %s", r.raw)
	}
}

// Тишина через API.
func TestNotifyQuietAPI(t *testing.T) {
	var svc *notify.Service
	e := newEnv(t, func(o *Options) {
		o.Deliver = notify.NewDeliverer(o.DB, notify.DeliverOptions{})
		svc = notify.New(o.DB, o.Events, notify.Options{Channels: func() []string { return []string{notify.ChannelWebhook} }})
		o.Notify = svc
	})
	ctx := context.Background()
	svc.Init(ctx)
	if r := e.get(t, "/api/v1/notifications/quiet"); r.status != 200 || !strings.Contains(r.raw, `"quiet":null`) {
		t.Fatalf("get: %d %s", r.status, r.raw)
	}
	for _, body := range []string{`{"reason":"x"}`, `{"minutes":60}`, `{"minutes":60,"until":"2030-01-01T00:00:00Z","reason":"x"}`,
		`{"minutes":20000,"reason":"x"}`, `{"until":"2000-01-01T00:00:00Z","reason":"x"}`} {
		if r := e.do(t, "PUT", "/api/v1/notifications/quiet", "application/json", body); r.status != 400 {
			t.Errorf("%s: %d %s", body, r.status, r.raw)
		}
	}
	r := e.do(t, "PUT", "/api/v1/notifications/quiet", "application/json", `{"minutes":60,"reason":"замена диска"}`)
	if r.status != 200 || !strings.Contains(r.raw, `"reason":"замена диска"`) || !strings.Contains(r.raw, `"mute_critical":false`) {
		t.Fatalf("put: %d %s", r.status, r.raw)
	}
	e.log.Emit(ctx, events.Event{Type: events.DiskRemoved, Severity: events.SevWarning, Subject: "disk:a"}, true)
	svc.CatchUp(ctx)
	if r := e.get(t, "/api/v1/notifications/quiet"); !strings.Contains(r.raw, `"suppressed":1`) {
		t.Fatalf("счётчик: %s", r.raw)
	}
	if r := e.do(t, "DELETE", "/api/v1/notifications/quiet", "", ""); r.status != 200 || !strings.Contains(r.raw, `"ended":true`) {
		t.Fatalf("delete: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "PUT", "/api/v1/notifications/quiet", "application/json", `{"minutes":5,"reason":"x"}`); r.status != 409 {
		t.Fatalf("при завершении: %d %s", r.status, r.raw)
	}
	svc.CatchUp(ctx)
	if r := e.get(t, "/api/v1/notifications/quiet"); !strings.Contains(r.raw, `"quiet":null`) {
		t.Fatalf("после окончания: %s", r.raw)
	}
	if r := e.do(t, "DELETE", "/api/v1/notifications/quiet", "", ""); r.status != 200 || !strings.Contains(r.raw, `"ended":false`) {
		t.Fatalf("delete без тишины: %d %s", r.status, r.raw)
	}
	r = e.get(t, "/api/v1/notifications")
	if !strings.Contains(r.raw, notify.TypeQuietEnded) || !strings.Contains(r.raw, `"suppressed":1`) {
		t.Fatalf("центр: %s", r.raw)
	}
}
