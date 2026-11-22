package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/notify"
)

func TestRetentionAPI(t *testing.T) {
	var svc *notify.Service
	e := newEnv(t, func(o *Options) {
		o.Deliver = notify.NewDeliverer(o.DB, notify.DeliverOptions{})
		svc = notify.New(o.DB, o.Events, notify.Options{Channels: o.Deliver.Channels})
		o.Notify = svc
	})
	ctx := context.Background()
	svc.Init(ctx)
	old := time.Now().Add(-40 * 24 * time.Hour)
	for _, sev := range []string{events.SevWarning, events.SevCritical} {
		if _, err := e.log.Emit(ctx, events.Event{Type: "pool.check", Severity: sev, Time: old}, true); err != nil {
			t.Fatal(err)
		}
	}
	svc.CatchUp(ctx)

	r := e.get(t, "/api/v1/system/retention")
	if r.status != 200 || !strings.Contains(r.raw, `"info_days":90,"warning_days":90,"critical_days":365`) {
		t.Fatalf("по умолчанию: %d %s", r.status, r.raw)
	}
	r = e.do(t, "PUT", "/api/v1/system/retention", "application/json", `{"info_days":30,"warning_days":30,"critical_days":365}`)
	if r.status != 200 || !strings.Contains(r.raw, `"events":1`) {
		t.Fatalf("put: %d %s", r.status, r.raw)
	}
	if r := e.get(t, "/api/v1/system/retention"); !strings.Contains(r.raw, `"info_days":30`) {
		t.Errorf("сохранено: %s", r.raw)
	}
	list, err := e.log.Recent(ctx, 10)
	if err != nil || len(list) != 1 || list[0].Severity != events.SevCritical {
		t.Errorf("журнал после обрезки: %+v %v", list, err)
	}
	for _, bad := range []string{`{"info_days":0,"warning_days":30,"critical_days":365}`, `{"info_days":30,"warning_days":30,"critical_days":4000}`, `{"x":1}`} {
		if r := e.do(t, "PUT", "/api/v1/system/retention", "application/json", bad); r.status != 400 {
			t.Errorf("%s: %d %s", bad, r.status, r.raw)
		}
	}
}

func TestRetentionUnavailable(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Events = nil })
	if r := e.get(t, "/api/v1/system/retention"); r.status != 503 || errCode(r) != "retention_unavailable" {
		t.Errorf("%d %s", r.status, r.raw)
	}
}
