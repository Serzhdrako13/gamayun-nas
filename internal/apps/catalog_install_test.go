package apps

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/catalog"
	"gamayun-nas/internal/tasks"
)

const catDigest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func catCompose(svcExtra, xExtra string) string {
	return `services:
  web:
    image: nginx:1.27` + catDigest + `
    environment: {ADMIN_PASS: "${ADMIN_PASS}", DB_PASS: "${DB_PASS}"}
    ports: ["8090:80"]
    volumes: ["${MEDIA}:/media", "./data:/data"]
` + svcExtra + `  db:
    image: postgres:17` + catDigest + `
    environment: ["DB_PASS=${DB_PASS}"]
x-gamayun:
  schema: 1
  id: demo
  name: {ru: Демо, en: Demo}
  version: 1.0.0
  app_version: "1.27"
  description: {ru: Тест, en: Test}
  architectures: [amd64, arm64]
  os_min: 0.1.0
  web: {service: web, port: 80}
  health: {service: web, timeout_s: 1}
  form:
    - {key: ADMIN_PASS, type: random, secret: true, title: {ru: Пароль, en: Password}}
    - {key: DB_PASS, type: random, secret: true, length: 20, title: {ru: БД, en: DB}}
    - {key: MEDIA, type: share, required: true, title: {ru: Папка, en: Folder}}
  storage:
    - {service: web, target: /media, kind: share, key: MEDIA, readonly: true, title: {ru: Медиа, en: Media}}
` + xExtra
}

type fakeCatalog map[string]string

func (f fakeCatalog) App(id string) (catalog.App, int64, error) {
	src, ok := f[id]
	if !ok {
		return catalog.App{}, 0, catalog.ErrNoApp
	}
	app, errs := catalog.Parse([]byte(src))
	if len(errs) > 0 {
		return catalog.App{}, 0, errs[0]
	}
	return app, 7, nil
}

func catSvc(t *testing.T, src string) (*svcRig, *projHelper) {
	r, ph := projSvc(t)
	r.s.Catalog = fakeCatalog{"demo": src}
	r.s.SysInfo = func(context.Context) (catalog.System, error) {
		return catalog.System{OSVersion: "0.2.0", Arch: "amd64", RAMMB: 4096, DiskFreeMB: 10000}, nil
	}
	r.s.HealthPoll = 10 * time.Millisecond
	return r, ph
}

func TestCatalogInstall(t *testing.T) {
	r, ph := catSvc(t, catCompose("", ""))
	ctx := context.Background()
	ph.ps = PSResult{Containers: []Container{{App: "demo", Service: "web", State: "running", Health: "healthy"}}}
	req := CatalogRequest{App: "demo", Values: map[string]string{"MEDIA": "Photos"}}
	cr, err := r.s.CatalogCheck(ctx, req)
	if err != nil || !cr.OK {
		t.Fatalf("%+v %v", cr, err)
	}
	tk, err := r.s.InstallCatalog(ctx, req, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatalf("%+v", tk.Error)
	}
	f := ph.files["demo"]
	if f.Env != "MEDIA='/mnt/p1/shares/Photos'\n" || len(f.Secrets) != 2 {
		t.Fatalf("env %q, файлов секретов %d", f.Env, len(f.Secrets))
	}
	// Каждому сервису — только свои секреты.
	val := func(text, k string) string {
		for _, l := range strings.Split(text, "\n") {
			if v, ok := strings.CutPrefix(l, k+"="); ok {
				return strings.Trim(v, "'")
			}
		}
		return ""
	}
	pass, dbPass := val(f.Secrets["web"], "ADMIN_PASS"), val(f.Secrets["web"], "DB_PASS")
	if len(pass) != catalog.DefaultRandomLength || len(dbPass) != 20 || f.Secrets["db"] != "DB_PASS='"+dbPass+"'\n" {
		t.Fatalf("секреты по сервисам: web %d/%d, db ok=%v", len(pass), len(dbPass), f.Secrets["db"] == "DB_PASS='"+dbPass+"'\n")
	}
	// Секретов нет в compose.json; у сервисов — свои env_file.
	if strings.Contains(f.Normalized, pass) || strings.Contains(f.Normalized, dbPass) || strings.Contains(f.Normalized, "_PASS") ||
		!strings.Contains(f.Normalized, `"secrets-web.env"`) || !strings.Contains(f.Normalized, `"secrets-db.env"`) {
		t.Fatal(f.Normalized)
	}
	if ph.aclMode("Photos", 59000) != "ro" {
		t.Fatal("выдача ro не применена")
	}
	m, _ := r.s.loadProjects(ctx)
	p := m["demo"]
	if p.Kind != "catalog" || p.Catalog == nil || p.Catalog.Version != "1.0.0" || p.Catalog.Serial != 7 ||
		strings.Join(p.Catalog.Secrets, ",") != "ADMIN_PASS,DB_PASS" || p.Web == nil || p.Web.Name != "demo" || p.Web.Port != 8090 {
		t.Fatalf("%+v %+v", p, p.Catalog)
	}
	var n int
	r.s.DB.QueryRow("SELECT COUNT(*) FROM events WHERE data LIKE ? OR data LIKE ?", "%"+pass+"%", "%"+dbPass+"%").Scan(&n)
	var n2 int
	r.s.DB.QueryRow("SELECT COUNT(*) FROM settings WHERE value LIKE ?", "%"+pass+"%").Scan(&n2)
	var n3 int
	r.s.DB.QueryRow("SELECT COUNT(*) FROM tasks WHERE params LIKE ? OR COALESCE(result,'') LIKE ?", "%"+pass+"%", "%"+pass+"%").Scan(&n3)
	if n+n2+n3 != 0 {
		t.Fatal("секрет в БД")
	}
	if _, err := r.s.InstallCatalog(ctx, req, "admin"); !errors.Is(err, ErrConflictSvc) {
		t.Fatal(err)
	}
	// Копия под другим идентификатором — тоже отказ (одна установка на приложение каталога).
	req.ID = "demo-2"
	if _, err := r.s.CatalogCheck(ctx, req); !errors.Is(err, ErrConflictSvc) || !strings.Contains(err.Error(), "уже установлено как") {
		t.Fatal(err)
	}
}

func TestCatalogInstallNotReadyRollback(t *testing.T) {
	r, ph := catSvc(t, catCompose("", ""))
	ctx := context.Background()
	ph.ps = PSResult{Containers: []Container{{App: "demo", Service: "web", State: "running", Health: "unhealthy"}}}
	tk, err := r.s.InstallCatalog(ctx, CatalogRequest{App: "demo", Values: map[string]string{"MEDIA": "Photos"}}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	tk = r.wait(t, tk.ID)
	if tk.State != tasks.Failed || !strings.Contains(tk.Error.Message, "готовность") || !strings.Contains(tk.Error.Message, "откат выполнен") {
		t.Fatalf("%+v", tk.Error)
	}
	got := strings.Join(ph.log(), ",")
	want := "user.ensure demo,acl Photos ro,appdata.create demo,own demo 59000,write demo,compose demo up," +
		"compose demo down -v,delete demo,appdata.delete demo,acl Photos revoke,user.delete demo"
	if got != want {
		t.Fatalf("%s\n%s", got, want)
	}
	if ph.aclMode("Photos", 59000) != "" {
		t.Fatal("выдача не отозвана")
	}
	if m, _ := r.s.loadProjects(ctx); len(m) != 0 {
		t.Fatalf("%+v", m)
	}
}

func TestCatalogInstallRefusals(t *testing.T) {
	ctx := context.Background()
	// Недекларированный cap_add — отказ; декларированный — подтверждение.
	src := catCompose("    cap_add: [NET_ADMIN]\n", "")
	r, _ := catSvc(t, src)
	req := CatalogRequest{App: "demo", Values: map[string]string{"MEDIA": "Photos"}}
	if _, err := r.s.InstallCatalog(ctx, req, "admin"); !errors.Is(err, ErrInvalidSvc) || !strings.Contains(err.Error(), "не задекларирован") {
		t.Fatal(err)
	}
	r, _ = catSvc(t, catCompose("    cap_add: [NET_ADMIN]\n", "  security: {cap_add: [NET_ADMIN]}\n"))
	cr, err := r.s.CatalogCheck(ctx, req)
	if err != nil || cr.OK || len(cr.Unconfirmed) != 1 || len(cr.Forbidden) != 0 {
		t.Fatalf("%+v %v", cr, err)
	}
	// Секрет вне environment KEY: ${KEY} — каталог не принимается.
	r.s.Catalog = fakeCatalog{"demo": catCompose("    command: [\"--pass=${ADMIN_PASS}\"]\n", "")}
	if _, err := r.s.CatalogCheck(ctx, req); !errors.Is(err, ErrInvalidSvc) || !strings.Contains(err.Error(), "секрет ADMIN_PASS") {
		t.Fatal(err)
	}
	r.s.Catalog = fakeCatalog{"demo": catCompose("    cap_add: [NET_ADMIN]\n", "  security: {cap_add: [NET_ADMIN]}\n")}
	// Форма, совместимость, нет приложения.
	if _, err := r.s.CatalogCheck(ctx, CatalogRequest{App: "demo"}); !errors.Is(err, ErrInvalidSvc) || !strings.Contains(err.Error(), "MEDIA") {
		t.Fatal(err)
	}
	if _, err := r.s.CatalogCheck(ctx, CatalogRequest{App: "demo", Values: map[string]string{"MEDIA": "Nope"}}); !errors.Is(err, ErrInvalidSvc) {
		t.Fatal(err)
	}
	if _, err := r.s.CatalogCheck(ctx, CatalogRequest{App: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	r.s.SysInfo = func(context.Context) (catalog.System, error) {
		return catalog.System{OSVersion: "0.0.9", Arch: "amd64", RAMMB: 4096, DiskFreeMB: 10000}, nil
	}
	if _, err := r.s.CatalogCheck(ctx, req); !errors.Is(err, ErrInvalidSvc) || !strings.Contains(err.Error(), "несовместимо") {
		t.Fatal(err)
	}
	r.s.Catalog = nil
	if _, err := r.s.CatalogCheck(ctx, req); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}
