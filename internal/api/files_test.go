//go:build linux

package api

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gamayun-nas/internal/files"
	"gamayun-nas/internal/nas"
)

type rootsFor []nas.FileRoot

func (r rootsFor) FileRoots(context.Context, string) ([]nas.FileRoot, error) { return r, nil }

// API файлового менеджера поверх настоящих операций (ExecHelper).
func TestFilesAPI(t *testing.T) {
	mount := t.TempDir()
	docs := nas.ShareDir(mount, "p1", "docs")
	os.MkdirAll(filepath.Join(docs, "d"), 0o755)
	os.MkdirAll(nas.ShareDir(mount, "p1", "ro"), 0o755)
	os.WriteFile(filepath.Join(docs, "d", "a.txt"), []byte("0123456789"), 0o644)
	etc := t.TempDir()
	os.WriteFile(filepath.Join(etc, "passwd"), []byte("auth-disabled:x:1001:1001::/h:/bin/sh\n"), 0o644)
	os.WriteFile(filepath.Join(etc, "group"), []byte("gamayun-users:x:990:\n"), 0o644)
	ex := &nas.Executor{MountRoot: mount, Mounted: func(string) bool { return true },
		PasswdFile: filepath.Join(etc, "passwd"), GroupFile: filepath.Join(etc, "group"),
		DirChown: func(int) error { return nil }, FileChown: func(int, int, int) error { return nil }}
	var svc *files.Service
	e := newEnv(t, func(o *Options) {
		svc = &files.Service{Helper: &files.ExecHelper{Exec: ex}, Tasks: o.Tasks, Logger: o.Logger,
			Roots: rootsFor{{Name: "docs", Pool: "p1", Level: nas.LevelRW}, {Name: "ro", Pool: "p1", Level: nas.LevelRead}}}
		svc.Init()
		o.Files = svc
	})
	if r := e.get(t, "/api/v1/files"); r.status != 200 || len(r.body["roots"].([]any)) != 2 {
		t.Fatalf("roots: %d %s", r.status, r.raw)
	}
	if r := e.get(t, "/api/v1/files/docs/list?path=d"); r.status != 200 || !strings.Contains(r.raw, `"a.txt"`) {
		t.Fatalf("list: %d %s", r.status, r.raw)
	}
	if r := e.get(t, "/api/v1/files/other/list"); r.status != 404 || r.body["error"].(map[string]any)["code"] != "no_access" {
		t.Fatalf("чужая папка: %d %s", r.status, r.raw)
	}
	if r := e.get(t, "/api/v1/files/docs/list?path=..%2Fx"); r.status != 400 {
		t.Fatalf("..: %d %s", r.status, r.raw)
	}
	up := "/api/v1/files/docs/upload?path=d&name=" + url.QueryEscape("отчёт.txt")
	if r := e.do(t, "POST", up, "application/octet-stream", "привет"); r.status != 201 || r.body["path"] != "d/отчёт.txt" {
		t.Fatalf("upload: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "POST", up, "application/octet-stream", "x"); r.status != 409 {
		t.Fatalf("конфликт: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "POST", up+"&on_conflict=rename", "application/octet-stream", "x"); r.status != 201 || r.body["name"] != "отчёт (2).txt" {
		t.Fatalf("rename: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "POST", "/api/v1/files/ro/upload?name=x", "", "x"); r.status != 404 {
		t.Fatalf("запись в read: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "POST", "/api/v1/files/docs/upload?name=CON", "", "x"); r.status != 400 {
		t.Fatalf("имя CON: %d %s", r.status, r.raw)
	}
	r := e.get(t, "/api/v1/files/docs/download?path=d/a.txt")
	if r.status != 200 || r.raw != "0123456789" || !strings.Contains(r.header.Get("Content-Disposition"), "filename*=UTF-8''a.txt") {
		t.Fatalf("download: %d %q %v", r.status, r.raw, r.header)
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/v1/files/docs/download?path=d/a.txt", nil)
	req.Header.Set("Range", "bytes=2-4")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 206 {
		t.Fatalf("range: %v %v", resp, err)
	}
	resp.Body.Close()
	if r := e.get(t, "/api/v1/files/docs/download?path=d"); r.status != 200 || r.header.Get("Content-Type") != "application/zip" || !strings.HasPrefix(r.raw, "PK") {
		t.Fatalf("zip: %d %v", r.status, r.header)
	}
	if r := e.do(t, "POST", "/api/v1/files/docs/mkdir", "application/json", `{"path":"d","name":"new"}`); r.status != 201 {
		t.Fatalf("mkdir: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "POST", "/api/v1/files/docs/rename", "application/json", `{"path":"d/new","name":"new2"}`); r.status != 200 || r.body["path"] != "d/new2" {
		t.Fatalf("rename: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "POST", "/api/v1/files/transfer", "application/json", `{"op":"delete","root":"docs","paths":["d/new2"]}`); r.status != 202 || r.body["id"] == "" {
		t.Fatalf("transfer: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "DELETE", "/api/v1/files/tasks/nope", "", ""); r.status != 404 {
		t.Fatalf("отмена чужой: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "POST", "/api/v1/files/transfer", "application/json", `{"op":"delete","root":"ro","paths":["x"]}`); r.status != 404 {
		t.Fatalf("удаление в read: %d %s", r.status, r.raw)
	}
}

func TestContentDisposition(t *testing.T) {
	got := contentDisposition(`a"b\отчёт.txt`)
	if got != `attachment; filename="a_b______.txt"; filename*=UTF-8''a%22b%5C%D0%BE%D1%82%D1%87%D1%91%D1%82.txt` {
		t.Fatal(got)
	}
}
