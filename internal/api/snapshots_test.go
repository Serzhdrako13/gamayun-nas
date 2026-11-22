package api

import (
	"context"
	"encoding/json"
	"testing"

	"gamayun-nas/internal/helper"
	"gamayun-nas/internal/snapshots"
)

// snapHelper — ответы helper для снапшотов: папка old — не подтом (отказ).
type snapHelper struct{}

func (snapHelper) Call(_ context.Context, op string, params, result any) error {
	switch op {
	case snapshots.OpList:
		return json.Unmarshal([]byte(`{"subvolume":true,"total":100,"avail":40,"snapshots":[{"name":"@GMT-2026.11.18-10.00.00","time":"2026-11-18T10:00:00Z","auto":true}]}`), result)
	case snapshots.OpCreate:
		if params.(snapshots.CreateParams).Share == "old" {
			return &helper.RemoteError{Code: helper.CodeRefused, Message: "папка old — обычный каталог"}
		}
		return json.Unmarshal([]byte(`{"name":"@GMT-2026.11.18-11.00.00","free_pct":40}`), result)
	case snapshots.OpDelete:
		if params.(snapshots.SnapParams).Name == "@GMT-2020.01.01-00.00.00" {
			return &helper.RemoteError{Code: helper.CodeNotFound, Message: "нет снимка"}
		}
		return nil
	case snapshots.OpBrowse:
		return json.Unmarshal([]byte(`{"path":"","entries":[{"name":"a.txt","type":"file","size":1}]}`), result)
	case snapshots.OpRestore:
		return json.Unmarshal([]byte(`{"path":"a (снимок).txt","files":1}`), result)
	}
	return nil
}

func TestSnapshotsUnavailable(t *testing.T) {
	e := newEnv(t, nil)
	if r := e.get(t, "/api/v1/nas/shares/docs/snapshots"); r.status != 503 || errCode(r) != "snapshots_unavailable" {
		t.Errorf("%d %s", r.status, r.raw)
	}
}

func TestSnapshotsFlow(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		s := &snapshots.Service{DB: o.DB, Helper: snapHelper{}, Events: o.Events, Tasks: o.Tasks,
			Targets: func(context.Context) ([]snapshots.Target, error) {
				return []snapshots.Target{{Share: "docs", Pool: "p1"}, {Share: "old", Pool: "p1"}}, nil
			}}
		if err := s.Init(); err != nil {
			t.Fatal(err)
		}
		o.Snapshots = s
	})
	const base = "/api/v1/nas/shares/docs/snapshots"
	steps := []struct {
		method, path, body string
		status             int
	}{
		{"GET", base, ``, 200},
		{"GET", "/api/v1/nas/shares/nope/snapshots", ``, 404},
		{"POST", base, `{}`, 201},
		{"POST", "/api/v1/nas/shares/old/snapshots", `{}`, 400},
		{"DELETE", base + "/@GMT-2026.11.18-10.00.00", ``, 204},
		{"DELETE", base + "/@GMT-2020.01.01-00.00.00", ``, 404},
		{"DELETE", base + "/bad-name", ``, 400},
		{"GET", base + "/@GMT-2026.11.18-10.00.00/files", ``, 200},
		{"GET", base + "/@GMT-2026.11.18-10.00.00/files?path=../etc", ``, 400},
		{"POST", base + "/@GMT-2026.11.18-10.00.00/restore", `{"path":""}`, 400},
		{"POST", base + "/@GMT-2026.11.18-10.00.00/restore", `{"path":"/etc/passwd"}`, 400},
		{"POST", base + "/@GMT-2026.11.18-10.00.00/restore", `{"path":"a.txt"}`, 202},
		{"PUT", "/api/v1/nas/shares/docs/snapshot-policy", `{"enabled":true}`, 400},
		{"PUT", "/api/v1/nas/shares/docs/snapshot-policy", `{"enabled":true,"hourly":12,"daily":7,"weekly":0,"monthly":3}`, 200},
		{"PUT", "/api/v1/nas/shares/nope/snapshot-policy", `{"enabled":false}`, 404},
	}
	for _, s := range steps {
		if r := e.do(t, s.method, s.path, "application/json", s.body); r.status != s.status {
			t.Errorf("%s %s: %d, ожидалось %d: %s", s.method, s.path, r.status, s.status, r.raw)
		}
	}
	r := e.get(t, base)
	var info snapshots.ShareInfo
	if err := json.Unmarshal([]byte(r.raw), &info); err != nil {
		t.Fatal(err)
	}
	if info.Policy.Hourly != 12 || info.Policy.Monthly != 3 || info.FreePct != 40 || len(info.Snapshots) != 1 || !info.Subvolume {
		t.Fatalf("сведения: %+v", info)
	}
}
