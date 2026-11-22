package apps

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/tasks"
)

// catV2 — версия 1.1.0: новое random-поле NEW_TOKEN у web.
func catV2() string {
	s := catCompose("", "")
	s = strings.Replace(s, "version: 1.0.0", "version: 1.1.0", 1)
	s = strings.Replace(s, `DB_PASS: "${DB_PASS}"}`, `DB_PASS: "${DB_PASS}", NEW_TOKEN: "${NEW_TOKEN}"}`, 1)
	s = strings.Replace(s, "  storage:", "    - {key: NEW_TOKEN, type: random, secret: true, title: {ru: Т, en: T}}\n  storage:", 1)
	s = strings.Replace(s, "nginx:1.27", "nginx:1.28", 1)
	return s
}

func envVal(text, k string) string {
	for _, l := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(l, k+"="); ok {
			return strings.Trim(v, "'")
		}
	}
	return ""
}

// installDemo — установленное demo 1.0.0, в каталоге — 1.1.0.
func installDemo(t *testing.T) (*svcRig, *projHelper, Project) {
	t.Helper()
	r, ph := catSvc(t, catCompose("", ""))
	ctx := context.Background()
	ph.ps = PSResult{Containers: []Container{{App: "demo", Service: "web", State: "running", Health: "healthy"}}}
	tk, err := r.s.InstallCatalog(ctx, CatalogRequest{App: "demo", Values: map[string]string{"MEDIA": "Photos"}}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatalf("%+v", tk.Error)
	}
	ph.data["demo"] = "v1-data"
	r.s.Catalog = fakeCatalog{"demo": catV2()}
	m, _ := r.s.loadProjects(ctx)
	ph.pmu.Lock()
	ph.calls = nil
	ph.pmu.Unlock()
	return r, ph, m["demo"]
}

func eventCount(t *testing.T, r *svcRig, typ string) int {
	var n int
	r.s.DB.QueryRow("SELECT COUNT(*) FROM events WHERE type = ?", typ).Scan(&n)
	return n
}

func txnCount(r *svcRig) int {
	var n int
	r.s.DB.QueryRow("SELECT COUNT(*) FROM settings WHERE key LIKE 'apps.txn.%'").Scan(&n)
	return n
}

func TestCatalogUpdateAndManualRollback(t *testing.T) {
	r, ph, old := installDemo(t)
	ctx := context.Background()
	oldFiles := ph.files["demo"]
	pass := envVal(oldFiles.Secrets["web"], "ADMIN_PASS")
	uc, err := r.s.CheckCatalogUpdate(ctx, "demo", CatalogUpdateRequest{})
	if err != nil || !uc.OK || uc.From != "1.0.0" || uc.To != "1.1.0" {
		t.Fatalf("%+v %v", uc, err)
	}
	tk, err := r.s.UpdateCatalog(ctx, "demo", CatalogUpdateRequest{}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatalf("%+v", tk.Error)
	}
	got := strings.Join(ph.log(), ",")
	if want := "write demo,compose demo pull,compose demo down,point.create "; !strings.HasPrefix(got, want) || !strings.Contains(got, ",compose demo up") {
		t.Fatalf("%s", got)
	}
	f := ph.files["demo"]
	// Прежние секреты сохранены, новый random — сгенерирован; папка — прежняя.
	if envVal(f.Secrets["web"], "ADMIN_PASS") != pass || len(envVal(f.Secrets["web"], "NEW_TOKEN")) != 32 ||
		f.Secrets["db"] != oldFiles.Secrets["db"] || f.Env != oldFiles.Env {
		t.Fatalf("секреты/переменные не сохранены: env %q", f.Env)
	}
	m, _ := r.s.loadProjects(ctx)
	p := m["demo"]
	if p.Catalog.Version != "1.1.0" || len(p.Catalog.Points) != 1 || p.Catalog.Points[0].Version != "1.0.0" ||
		p.Catalog.Points[0].Prev.SHA256 != old.SHA256 || p.SHA256 == old.SHA256 || txnCount(r) != 0 || p.Web == nil {
		t.Fatalf("%+v %+v", p, p.Catalog)
	}
	if _, err := r.s.UpdateCatalog(ctx, "demo", CatalogUpdateRequest{}, "admin"); !errors.Is(err, ErrConflictSvc) {
		t.Fatalf("повтор: %v", err)
	}
	// Ручной откат: данные после обновления теряются.
	ph.data["demo"] = "v2-data"
	ph.pmu.Lock()
	ph.calls = nil
	ph.pmu.Unlock()
	tk, err = r.s.RollbackCatalog(ctx, "demo", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatalf("%+v", tk.Error)
	}
	m, _ = r.s.loadProjects(ctx)
	p = m["demo"]
	if p.Catalog.Version != "1.0.0" || len(p.Catalog.Points) != 0 || p.SHA256 != old.SHA256 || ph.data["demo"] != "v1-data" ||
		ph.files["demo"].SHA256 != old.SHA256 || len(ph.points["demo"]) != 0 || txnCount(r) != 0 {
		t.Fatalf("%+v data=%q points=%v", p.Catalog, ph.data["demo"], ph.points["demo"])
	}
	if eventCount(t, r, EvAppRolledBack) != 1 {
		t.Fatal("нет события отката")
	}
	if _, err := r.s.RollbackCatalog(ctx, "demo", "admin"); !errors.Is(err, ErrConflictSvc) {
		t.Fatalf("откат без точки: %v", err)
	}
}

func TestCatalogUpdateAutoRollback(t *testing.T) {
	r, ph, old := installDemo(t)
	ctx := context.Background()
	// Новая версия не здорова; за время попытки она «испортила» данные.
	ph.ps = PSResult{Containers: []Container{{App: "demo", Service: "web", State: "running", Health: "unhealthy"}}}
	tk, err := r.s.UpdateCatalog(ctx, "demo", CatalogUpdateRequest{}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		ph.pmu.Lock()
		ph.data["demo"] = "broken"
		ph.pmu.Unlock()
	}()
	tk = r.wait(t, tk.ID)
	if tk.State != tasks.Failed || !strings.Contains(tk.Error.Message, "готовность") || !strings.Contains(tk.Error.Message, "автооткат выполнен") {
		t.Fatalf("%+v", tk.Error)
	}
	got := strings.Join(ph.log(), ",")
	if !strings.Contains(got, "point.restore u") || !strings.HasSuffix(got[:strings.LastIndex(got, ",point.delete")], "compose demo up") {
		t.Fatalf("%s", got)
	}
	m, _ := r.s.loadProjects(ctx)
	p := m["demo"]
	if p.Catalog.Version != "1.0.0" || p.SHA256 != old.SHA256 || ph.files["demo"].SHA256 != old.SHA256 || ph.data["demo"] != "v1-data" ||
		len(ph.points["demo"]) != 0 || txnCount(r) != 0 || p.Error != "" {
		t.Fatalf("%+v data=%q points=%v", p, ph.data["demo"], ph.points["demo"])
	}
	if eventCount(t, r, EvAppRolledBack) != 1 {
		t.Fatal("нет события автоотката")
	}
	if ups := ph.upSHA; ups[len(ups)-1] != old.SHA256 {
		t.Fatal("последний up — не прежняя версия")
	}
}

func TestCatalogUpdatePullFails(t *testing.T) {
	r, ph, old := installDemo(t)
	ctx := context.Background()
	ph.pullFail["demo"] = true
	tk, _ := r.s.UpdateCatalog(ctx, "demo", CatalogUpdateRequest{}, "admin")
	tk = r.wait(t, tk.ID)
	if tk.State != tasks.Failed || !strings.Contains(tk.Error.Message, "загрузка образов") {
		t.Fatalf("%+v", tk.Error)
	}
	got := strings.Join(ph.log(), ",")
	if strings.Contains(got, "down") || strings.Contains(got, "point.") || !strings.Contains(got, "revert demo") {
		t.Fatalf("приложение тронуто: %s", got)
	}
	if ph.files["demo"].SHA256 != old.SHA256 || txnCount(r) != 0 {
		t.Fatal("файлы не возвращены")
	}
}

// Прерывание (gamayund убит посреди обновления): при старте Docker — откат по журналу.
// reconcileLocked — сверка приложений как в Run: под s.mu; зависание —
// провал теста, а не вечное ожидание.
func reconcileLocked(t *testing.T, s *Service, ctx context.Context) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		s.mu.Lock()
		s.reconcileProjects(ctx)
		s.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("сверка под s.mu зависла (взаимоблокировка)")
	}
}

func TestCatalogUpdateRecoverOnStart(t *testing.T) {
	r, ph, old := installDemo(t)
	ctx := context.Background()
	// Состояние «после up новой версии»: новые файлы, точка, данные изменены.
	nw := ph.files["demo"]
	nw.Backup, nw.SHA256 = true, strings.Repeat("e", 64)
	ph.prev["demo"], ph.files["demo"] = ph.files["demo"], nw
	ph.points["demo"] = map[string]fakePoint{"u1": {files: ph.prev["demo"], data: "v1-data"}}
	ph.data["demo"] = "v2-data"
	if err := r.s.saveTxn(ctx, Txn{Op: txnUpdate, App: "demo", Step: stepUp, Point: "u1", Old: old, To: "1.1.0"}); err != nil {
		t.Fatal(err)
	}
	r.s.proj.lastSub = "" // первая сверка после запуска gamayund
	reconcileLocked(t, r.s, ctx)
	if ph.files["demo"].SHA256 != old.SHA256 || ph.data["demo"] != "v1-data" || txnCount(r) != 0 || len(ph.points["demo"]) != 0 {
		t.Fatalf("не откатилось: data=%q txn=%d", ph.data["demo"], txnCount(r))
	}
	if eventCount(t, r, EvAppRolledBack) != 1 {
		t.Fatal("нет события")
	}
	// Прервано на записи файлов, которые не успели измениться, — revert не нужен.
	ph.pmu.Lock()
	ph.calls = nil
	ph.pmu.Unlock()
	r.s.saveTxn(ctx, Txn{Op: txnUpdate, App: "demo", Step: stepWriting, Old: old, To: "1.1.0"})
	r.s.proj.lastSub = ""
	reconcileLocked(t, r.s, ctx)
	if got := strings.Join(ph.log(), ","); strings.Contains(got, "revert") || strings.Contains(got, "down") || txnCount(r) != 0 {
		t.Fatalf("%s", got)
	}
}

// Изменение доступа/compose приложения каталога (путь пользовательского приложения) сохраняет секреты.
func TestCatalogAppUpdateKeepsSecrets(t *testing.T) {
	r, ph, p := installDemo(t)
	ctx := context.Background()
	before := ph.files["demo"].Secrets
	req, _, err := r.s.currentRequest(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	req.Env = nil
	tk, err := r.s.UpdateProject(ctx, req, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatalf("%+v", tk.Error)
	}
	after := ph.files["demo"].Secrets
	if len(after) != 2 || after["web"] != before["web"] || after["db"] != before["db"] {
		t.Fatalf("секреты потеряны: %d", len(after))
	}
}
