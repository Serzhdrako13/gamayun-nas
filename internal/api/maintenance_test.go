package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gamayun-nas/internal/maintenance"
	"gamayun-nas/internal/smart"
)

func TestMaintenanceUnavailable(t *testing.T) {
	e := newEnv(t, nil)
	if r := e.get(t, "/api/v1/maintenance"); r.status != 503 || errCode(r) != "maintenance_unavailable" {
		t.Errorf("%d %s", r.status, r.raw)
	}
}

func TestMaintenanceSchedule(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Maintenance = maintenance.New(o.DB, nil, nil, o.Events, maintenance.Options{})
	})
	r := e.get(t, "/api/v1/maintenance")
	if r.status != 200 || !strings.Contains(r.raw, `"scrub":{"enabled":true,"interval_days":30}`) || !strings.Contains(r.raw, `"hour":3`) {
		t.Fatalf("по умолчанию: %d %s", r.status, r.raw)
	}
	body := `{"scrub":{"enabled":false,"interval_days":14},"smart_short":{"enabled":true,"interval_days":7},"smart_long":{"enabled":true,"interval_days":30},"hour":2}`
	if r := e.do(t, "PUT", "/api/v1/maintenance/schedule", "application/json", body); r.status != 200 {
		t.Fatalf("put: %d %s", r.status, r.raw)
	}
	if r := e.get(t, "/api/v1/maintenance"); !strings.Contains(r.raw, `"scrub":{"enabled":false,"interval_days":14}`) || !strings.Contains(r.raw, `"hour":2`) {
		t.Fatalf("сохранено: %s", r.raw)
	}
	for _, bad := range []string{strings.Replace(body, `"hour":2`, `"hour":30`, 1), strings.Replace(body, `"interval_days":14`, `"interval_days":0`, 1), `{"x":1}`} {
		if r := e.do(t, "PUT", "/api/v1/maintenance/schedule", "application/json", bad); r.status != 400 {
			t.Errorf("%s: %d %s", bad, r.status, r.raw)
		}
	}
	if r := e.do(t, "POST", "/api/v1/disks/a%20b/smart/test", "application/json", `{"type":"short"}`); r.status != 400 {
		t.Errorf("плохой id: %d", r.status)
	}
	if r := e.do(t, "POST", "/api/v1/disks/abc/smart/test", "application/json", `{"type":"rm"}`); r.status != 400 {
		t.Errorf("плохой тип: %d", r.status)
	}
	if r := e.do(t, "POST", "/api/v1/storage/pools/p1/scrub", "application/json", `{}`); r.status != 503 {
		t.Errorf("без Storage: %d %s", r.status, r.raw)
	}
}

// smartHelper — helper для maintenance: один диск с SMART.
type smartHelper struct {
	temp int64
	off  bool // SMART выключен в диске
}

func (h *smartHelper) Call(_ context.Context, op string, params, result any) error {
	var v any
	switch op {
	case "disks.observe":
		v = map[string]any{"disks": []any{map[string]any{"id": "d1", "kernel_name": "sda", "kind": "HDD"}}}
	case smart.OpSet:
		h.off = !params.(smart.SetParams).On
		fallthrough
	case smart.OpRead:
		t, ok := h.temp, true
		v = smart.Info{DiskID: "d1", Supported: ok, Passed: &ok, TemperatureC: &t}
		if h.off {
			v = smart.Info{DiskID: "d1", Disabled: true}
		}
	default:
		return errors.New(op)
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, result)
}

// Итог SMART в списке дисков, история, пороги температуры.
func TestDiskSmartSummaryHistory(t *testing.T) {
	var m *maintenance.Service
	e := newEnv(t, func(o *Options) {
		m = maintenance.New(o.DB, &smartHelper{temp: 57}, nil, o.Events, maintenance.Options{})
		o.Maintenance = m
	})
	e.help.results["disks.observe"] = `{"disks":[{"id":"d1","name":"Диск 1","kind":"HDD"},{"id":"d2","name":"Диск 2","kind":"HDD"}],"complete":true,"issues":[]}`
	if _, err := m.PollSmart(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := e.get(t, "/api/v1/disks")
	if r.status != 200 || !strings.Contains(r.raw, `"smart":{"level":"warning","supported":true,"temperature_c":57,"limits":{"warning_c":55,"critical_c":65}`) {
		t.Fatalf("итог: %d %s", r.status, r.raw)
	}
	if strings.Count(r.raw, `"smart"`) != 1 {
		t.Fatalf("у неопрошенного диска итог: %s", r.raw)
	}
	r = e.get(t, "/api/v1/disks/d1/smart/history?days=1")
	if r.status != 200 || !strings.Contains(r.raw, `"temperature_c":57`) || r.body["days"] != float64(1) {
		t.Fatalf("история: %d %s", r.status, r.raw)
	}
	for _, q := range []string{"?days=0", "?days=91", "?days=x"} {
		if r := e.get(t, "/api/v1/disks/d1/smart/history"+q); r.status != 400 {
			t.Errorf("%s: %d", q, r.status)
		}
	}
	if r := e.get(t, "/api/v1/maintenance"); !strings.Contains(r.raw, `"temperature":{"hdd":{"warning_c":55,"critical_c":65},"ssd":{"warning_c":70,"critical_c":80}}`) {
		t.Fatalf("пороги в /maintenance: %s", r.raw)
	}
	body := `{"hdd":{"warning_c":58,"critical_c":66},"ssd":{"warning_c":70,"critical_c":80}}`
	if r := e.do(t, "PUT", "/api/v1/maintenance/temperature", "application/json", body); r.status != 200 {
		t.Fatalf("put: %d %s", r.status, r.raw)
	}
	if r := e.get(t, "/api/v1/disks"); !strings.Contains(r.raw, `"smart":{"level":"ok"`) {
		t.Fatalf("после смены порогов: %s", r.raw)
	}
	for _, bad := range []string{`{"hdd":{"warning_c":70,"critical_c":60},"ssd":{"warning_c":70,"critical_c":80}}`, `{"x":1}`} {
		if r := e.do(t, "PUT", "/api/v1/maintenance/temperature", "application/json", bad); r.status != 400 {
			t.Errorf("%s: %d %s", bad, r.status, r.raw)
		}
	}
}

// Ручное выключение/включение SMART диска.
func TestDiskSmartEnabled(t *testing.T) {
	h := &smartHelper{temp: 40}
	e := newEnv(t, func(o *Options) {
		o.Maintenance = maintenance.New(o.DB, h, nil, o.Events, maintenance.Options{})
	})
	r := e.do(t, "PUT", "/api/v1/disks/d1/smart/enabled", "application/json", `{"on":false}`)
	if r.status != 200 || !strings.Contains(r.raw, `"disabled":true,"manual_off":true`) {
		t.Fatalf("выключение: %d %s", r.status, r.raw)
	}
	r = e.do(t, "PUT", "/api/v1/disks/d1/smart/enabled", "application/json", `{"on":true}`)
	if r.status != 200 || strings.Contains(r.raw, "disabled") {
		t.Fatalf("включение: %d %s", r.status, r.raw)
	}
	for _, c := range [][2]string{{"d1", `{}`}, {"a%20b", `{"on":true}`}} {
		if r := e.do(t, "PUT", "/api/v1/disks/"+c[0]+"/smart/enabled", "application/json", c[1]); r.status != 400 {
			t.Errorf("%v: %d", c, r.status)
		}
	}
}
