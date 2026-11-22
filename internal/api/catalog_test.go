package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/catalog"
)

const testCompose = `services:
  web:
    image: nginx:1.27@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
x-gamayun:
  schema: 1
  id: demo-web
  name: {ru: Демо, en: Demo}
  version: 1.0.0
  app_version: "1.27"
  description: {ru: Тест, en: Test}
  architectures: [amd64]
`

func testArchive(t *testing.T, priv ed25519.PrivateKey, serial int64) string {
	t.Helper()
	ix := catalog.Index{Schema: 1, Serial: serial, Created: time.Unix(0, 0).UTC(), Apps: []catalog.IndexApp{
		{ID: "demo-web", Version: "1.0.0", Files: map[string]string{"compose.yaml": catalog.SHA256([]byte(testCompose))}},
	}}
	idx, err := ix.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	b, err := catalog.Pack(map[string][]byte{"apps/demo-web/compose.yaml": []byte(testCompose),
		catalog.IndexFile: idx, catalog.SigFile: catalog.Sign(priv, idx)})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCatalogAPI(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	e := newEnv(t, func(o *Options) {
		o.Catalog = &catalog.Store{DB: o.DB, Dir: filepath.Join(t.TempDir(), "cat"), Trusted: []ed25519.PublicKey{pub}}
	})
	r := e.get(t, "/api/v1/apps/catalog")
	if r.status != 200 || len(r.body["apps"].([]any)) != 0 {
		t.Fatalf("пустой каталог: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "POST", "/api/v1/apps/catalog/refresh", "", ""); r.status != 409 {
		t.Fatalf("refresh без адреса: %d", r.status)
	}
	if r := e.do(t, "POST", "/api/v1/apps/catalog/import", "text/plain", testArchive(t, priv, 5)); r.status != 400 {
		t.Fatalf("text/plain принят: %d", r.status)
	}
	r = e.do(t, "POST", "/api/v1/apps/catalog/import", "application/octet-stream", testArchive(t, priv, 5))
	if r.status != 200 || len(r.body["apps"].([]any)) != 1 {
		t.Fatalf("импорт: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "POST", "/api/v1/apps/catalog/import", "application/octet-stream", testArchive(t, priv, 4)); r.status != 409 {
		t.Fatalf("откат без подтверждения: %d", r.status)
	}
	if r := e.do(t, "POST", "/api/v1/apps/catalog/import?allow_rollback=1", "application/octet-stream", testArchive(t, priv, 4)); r.status != 200 {
		t.Fatalf("откат с подтверждением: %d %s", r.status, r.raw)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if r := e.do(t, "POST", "/api/v1/apps/catalog/import", "application/octet-stream", testArchive(t, other, 9)); r.status != 422 {
		t.Fatalf("чужая подпись: %d", r.status)
	}
}

func TestCatalogUnavailable(t *testing.T) {
	e := newEnv(t, nil)
	if r := e.get(t, "/api/v1/apps/catalog"); r.status != 503 {
		t.Fatalf("без каталога: %d", r.status)
	}
}

// Маршруты обновления/отката приложения каталога подключены.
func TestCatalogUpdateRoutes(t *testing.T) {
	e := newEnv(t, nil)
	for _, p := range []string{"catalog-update/check", "catalog-update", "catalog-rollback"} {
		r := e.do(t, "POST", "/api/v1/apps/projects/nope/"+p, "application/json", "{}")
		if r.status != 503 || !strings.Contains(r.raw, "apps_unavailable") {
			t.Fatalf("%s: %d %s", p, r.status, r.raw)
		}
	}
}

// Правки override.yaml убраны (2026-11-21): маршрутов нет.
func TestCatalogOverrideRoutesGone(t *testing.T) {
	e := newEnv(t, nil)
	for _, m := range []struct{ method, path string }{{"POST", "override/check"}, {"PUT", "override"}} {
		r := e.do(t, m.method, "/api/v1/apps/projects/nope/"+m.path, "application/json", `{"override":""}`)
		if r.status != 404 && r.status != 405 {
			t.Fatalf("%s: %d %s", m.path, r.status, r.raw)
		}
	}
}
