package api

import (
	"context"
	"encoding/json"
	"testing"

	"gamayun-nas/internal/apps"
	"gamayun-nas/internal/storage/home"
)

// appsHelper — пакет установлен, Docker остановлен, приложения не настроены.
type appsHelper struct{}

func (appsHelper) Call(_ context.Context, op string, _, result any) error {
	if op == apps.OpStatus {
		return json.Unmarshal([]byte(`{"installed":true,"pool":"","docker":"inactive","containerd":"inactive"}`), result)
	}
	return nil
}

type appsPools struct{}

func (appsPools) Get(_ context.Context, name string) (home.PoolView, error) {
	if name != "p1" {
		return home.PoolView{}, home.ErrNotFound
	}
	return home.PoolView{Status: &home.PoolStatus{Mounted: true}}, nil
}

func TestAppsUnavailable(t *testing.T) {
	e := newEnv(t, nil)
	if r := e.get(t, "/api/v1/apps"); r.status != 503 || errCode(r) != "apps_unavailable" {
		t.Errorf("%d %s", r.status, r.raw)
	}
}

func TestAppsAPI(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		s := &apps.Service{DB: o.DB, Helper: appsHelper{}, Events: o.Events, Tasks: o.Tasks, Pools: appsPools{}}
		if err := s.Init(); err != nil {
			t.Fatal(err)
		}
		o.Apps = s
	})
	r := e.get(t, "/api/v1/apps")
	var v apps.View
	json.Unmarshal([]byte(r.raw), &v)
	if r.status != 200 || v.State != apps.StateDisabled || !v.Installed {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	steps := []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/api/v1/apps/enable", `{"pool":"../x"}`, 400},
		{"POST", "/api/v1/apps/enable", `{"pool":"p1","x":1}`, 400},
		{"POST", "/api/v1/apps/enable", `{"pool":"p9"}`, 404},
		{"POST", "/api/v1/apps/disable", ``, 409},
		{"POST", "/api/v1/apps/resume", ``, 409},
		{"POST", "/api/v1/apps/enable", `{"pool":"p1"}`, 202},
	}
	for _, s := range steps {
		if r := e.do(t, s.method, s.path, "application/json", s.body); r.status != s.status {
			t.Errorf("%s %s %s: %d, ожидалось %d: %s", s.method, s.path, s.body, r.status, s.status, r.raw)
		}
	}
}

func TestAppsProjectsAPI(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		s := &apps.Service{DB: o.DB, Helper: appsHelper{}, Events: o.Events, Tasks: o.Tasks, Pools: appsPools{}}
		if err := s.Init(); err != nil {
			t.Fatal(err)
		}
		o.Apps = s
	})
	steps := []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/v1/apps/projects", ``, 200},
		{"GET", "/api/v1/apps/ports", ``, 200},
		{"POST", "/api/v1/apps/projects/check", `{"id":"web","compose":"services: {a: {image: x}}"}`, 409},
		{"POST", "/api/v1/apps/projects/check", `{"id":"Bad!","compose":"x"}`, 400},
		{"POST", "/api/v1/apps/projects/check", `{"id":"web","unknown":1}`, 400},
		{"POST", "/api/v1/apps/projects", `{"id":"web","name":"","compose":"x"}`, 400},
		{"GET", "/api/v1/apps/projects/web", ``, 404},
		{"PUT", "/api/v1/apps/projects/web", `{"compose":"x"}`, 404},
		{"DELETE", "/api/v1/apps/projects/web", ``, 404},
		{"POST", "/api/v1/apps/projects/web/start", ``, 404},
		{"GET", "/api/v1/apps/projects/web/logs?service=a", ``, 404},
		// доступ к общим папкам
		{"PUT", "/api/v1/apps/projects/web/grants", `{"grants":{"Photos":"ro"}}`, 404},
		{"PUT", "/api/v1/apps/projects/web/grants", `{"bad":1}`, 400},
		{"POST", "/api/v1/apps/projects/web/migrate", `{"grants":{}}`, 404},
		{"GET", "/api/v1/apps/shares/Photos", ``, 200},
	}
	for _, s := range steps {
		if r := e.do(t, s.method, s.path, "application/json", s.body); r.status != s.status {
			t.Errorf("%s %s %s: %d, ожидалось %d: %s", s.method, s.path, s.body, r.status, s.status, r.raw)
		}
	}
}
