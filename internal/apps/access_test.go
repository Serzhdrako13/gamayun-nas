package apps

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/apps/compose"
	"gamayun-nas/internal/tasks"
)

const shareCompose = `services:
  web:
    image: nginx:1.27
    volumes: ["/mnt/p1/shares/Photos:/photos", "/mnt/p1/shares/Media:/media"]
`

// do — r.do(t)(r.s.CreateProject(...)): ожидание итога задачи.
func (r *svcRig) do(t *testing.T) func(tasks.Task, error) tasks.Task {
	return func(tk tasks.Task, err error) tasks.Task {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r.wait(t, tk.ID)
	}
}

func TestP3CreateWithGrants(t *testing.T) {
	r, ph := projSvc(t)
	ctx := context.Background()
	req := ProjectRequest{ID: "web", Name: "w", Compose: shareCompose}
	// без выдачи — отказ проверки
	if _, err := r.s.CreateProject(ctx, req, "a"); !errors.Is(err, ErrInvalidSvc) || !strings.Contains(err.Error(), "нет доступа") {
		t.Fatal(err)
	}
	req.Grants = map[string]string{"Photos": "rw", "Media": "ro"}
	cr, err := r.s.Check(ctx, req)
	if err != nil || !cr.OK || cr.UID != 59000 || cr.RunAs["web"] != compose.RunUID || len(cr.ShareBinds) != 2 {
		t.Fatalf("%+v %v", cr, err)
	}
	if tk := r.do(t)(r.s.CreateProject(ctx, req, "admin")); tk.State != tasks.Succeeded {
		t.Fatalf("%+v", tk.Error)
	}
	want := "user.ensure web,acl Media ro,acl Photos rw,appdata.create web,own web 59000,write web,compose web up"
	if got := strings.Join(ph.log(), ","); got != want {
		t.Fatalf("%s\n%s", got, want)
	}
	if !strings.Contains(ph.files["web"].Normalized, `"user": "59000:59000"`) {
		t.Fatal(ph.files["web"].Normalized)
	}
	if ph.files["web"].Owner != 59000 {
		t.Fatal("владелец каталогов данных")
	}
	d, _ := r.s.ProjectDetails(ctx, "web")
	if d.UID != 59000 || len(d.Grants) != 2 || d.Grants[0].By != "admin" || d.User != "app-web" || d.Legacy {
		t.Fatalf("%+v", d.ProjectView)
	}
	apps, _ := r.s.ShareApps(ctx, "Photos")
	if len(apps) != 1 || apps[0].Mode != "rw" {
		t.Fatalf("%+v", apps)
	}
	// неизвестная папка, неверный режим
	bad := ProjectRequest{ID: "x", Name: "x", Compose: "services: {a: {image: x}}\n", Grants: map[string]string{"Nope": "ro"}}
	if _, err := r.s.CreateProject(ctx, bad, "a"); !errors.Is(err, ErrInvalidSvc) {
		t.Fatal(err)
	}
	bad.Grants = map[string]string{"Photos": "rwx"}
	if _, err := r.s.CreateProject(ctx, bad, "a"); !errors.Is(err, ErrInvalidSvc) {
		t.Fatal(err)
	}
}

func TestP3CreateRollback(t *testing.T) {
	r, ph := projSvc(t)
	ctx := context.Background()
	req := ProjectRequest{ID: "web", Name: "w", Compose: shareCompose, Grants: map[string]string{"Photos": "rw", "Media": "ro"}}
	// сбой запуска: всё откатывается, включая доступ и учётку
	ph.upFail["web"] = true
	tk := r.do(t)(r.s.CreateProject(ctx, req, "a"))
	if tk.State != tasks.Failed || !strings.Contains(tk.Error.Message, "откат выполнен") {
		t.Fatalf("%+v", tk.Error)
	}
	if len(ph.acl["Photos"])+len(ph.acl["Media"]) != 0 || len(ph.users) != 0 || len(ph.appdata) != 0 {
		t.Fatalf("следы: acl %v users %v appdata %v", ph.acl, ph.users, ph.appdata)
	}
	if reg, _ := r.s.loadUIDs(ctx); len(reg.ByID) != 0 || len(reg.Retired) != 0 {
		t.Fatalf("UID не освобождён: %+v", reg)
	}
	// сбой выдачи доступа: откат выданного, Docker не трогается
	ph.upFail = map[string]bool{}
	ph.calls = nil
	ph.aclFail["Photos:rw"] = true
	tk = r.do(t)(r.s.CreateProject(ctx, req, "a"))
	if tk.State != tasks.Failed || !strings.Contains(tk.Error.Message, "доступ к общим папкам") {
		t.Fatalf("%+v", tk.Error)
	}
	want := "user.ensure web,acl Media ro,acl Photos rw,acl Photos revoke,acl Media revoke,user.delete web"
	if got := strings.Join(ph.log(), ","); got != want {
		t.Fatalf("%s\n%s", got, want)
	}
	if len(ph.acl["Photos"])+len(ph.acl["Media"]) != 0 {
		t.Fatalf("%v", ph.acl)
	}
}

func TestP3UpdateGrantsAndDelete(t *testing.T) {
	r, ph := projSvc(t)
	ctx := context.Background()
	src := "services: {web: {image: x, volumes: ['/mnt/p1/shares/Photos:/p']}}\n"
	req := ProjectRequest{ID: "web", Name: "w", Compose: src, Grants: map[string]string{"Photos": "rw"}}
	if tk := r.do(t)(r.s.CreateProject(ctx, req, "a")); tk.State != tasks.Succeeded {
		t.Fatal(tk.Error)
	}
	// Photos → Media ro: выдача до запуска, отзыв — после
	ph.calls = nil
	src2 := "services: {web: {image: x, volumes: ['/mnt/p1/shares/Media:/m']}}\n"
	tk := r.do(t)(r.s.UpdateProject(ctx, ProjectRequest{ID: "web", Compose: src2, Grants: map[string]string{"Media": "ro"}}, "b"))
	if tk.State != tasks.Succeeded {
		t.Fatal(tk.Error)
	}
	if got := strings.Join(ph.log(), ","); got != "acl Media ro,write web,compose web up,acl Photos revoke" {
		t.Fatal(got)
	}
	// неудачное изменение: новая выдача снимается, прежняя остаётся
	ph.calls = nil
	res, _ := compose.Normalize([]byte(src), compose.Context{ID: "web", MountRoot: "/mnt", AppData: "/mnt/p1/.apps/appdata/web",
		Shares: map[string]string{"Photos": "/mnt/p1/shares/Photos", "Media": "/mnt/p1/shares/Media"},
		UID:    59000, GID: 59000, Grants: map[string]string{"Photos": "rw"}})
	ph.upFail["web@"+SHA256(res.Normalized)[:8]] = true
	tk = r.do(t)(r.s.UpdateProject(ctx, ProjectRequest{ID: "web", Compose: src, Grants: map[string]string{"Photos": "rw"}}, "b"))
	if tk.State != tasks.Failed || !strings.Contains(tk.Error.Message, "возвращена прежняя версия") {
		t.Fatalf("%+v", tk.Error)
	}
	if ph.acl["Media"][59000] != "ro" || len(ph.acl["Photos"]) != 0 {
		t.Fatalf("%v", ph.acl)
	}
	// SetGrants: только доступ — compose прежний
	ph.upFail = map[string]bool{}
	tk = r.do(t)(r.s.SetGrants(ctx, "web", nil, map[string]string{"Media": "rw"}, "c"))
	if tk.State != tasks.Succeeded || ph.acl["Media"][59000] != "rw" {
		t.Fatalf("%+v %v", tk.Error, ph.acl)
	}
	// удаление без сохранения данных: отзыв, учётка, UID в retired
	ph.calls = nil
	if tk := r.do(t)(r.s.DeleteProject(ctx, "web", false, "a")); tk.State != tasks.Succeeded {
		t.Fatal(tk.Error)
	}
	if got := strings.Join(ph.log(), ","); !strings.HasSuffix(got, "appdata.delete web,acl Media revoke,user.delete web") {
		t.Fatal(got)
	}
	reg, _ := r.s.loadUIDs(ctx)
	if len(reg.ByID) != 0 || len(reg.Retired) != 1 || reg.Retired[0] != 59000 {
		t.Fatalf("%+v", reg)
	}
	// следующее приложение — следующий UID
	if cr, _ := r.s.Check(ctx, ProjectRequest{ID: "db", Name: "d", Compose: "services: {a: {image: x}}\n"}); cr.UID != 59001 {
		t.Fatalf("UID %d", cr.UID)
	}
}

func TestP3DeleteKeepDataKeepsUID(t *testing.T) {
	r, ph := projSvc(t)
	ctx := context.Background()
	req := ProjectRequest{ID: "web", Name: "w", Compose: "services: {a: {image: x}}\n"}
	r.do(t)(r.s.CreateProject(ctx, req, "a"))
	if tk := r.do(t)(r.s.DeleteProject(ctx, "web", true, "a")); tk.State != tasks.Succeeded {
		t.Fatal(tk.Error)
	}
	if len(ph.users) != 0 {
		t.Fatal("учётка осталась")
	}
	reg, _ := r.s.loadUIDs(ctx)
	if reg.ByID["web"] != 59000 {
		t.Fatalf("%+v", reg)
	}
	// повторное создание — тот же UID, данные не переназначаются
	ph.calls = nil
	if tk := r.do(t)(r.s.CreateProject(ctx, req, "a")); tk.State != tasks.Succeeded {
		t.Fatal(tk.Error)
	}
	if got := strings.Join(ph.log(), ","); strings.Contains(got, "own ") || !strings.Contains(got, "user.ensure web") {
		t.Fatal(got)
	}
}

// legacy — приложение «доступ как root»: UID 0, подтверждённый share_bind.
func legacy(t *testing.T, r *svcRig, ph *projHelper) {
	t.Helper()
	ctx := context.Background()
	src := "services: {web: {image: x, volumes: ['/mnt/p1/shares/Photos:/p']}}\n"
	res, _ := compose.Normalize([]byte(src), compose.Context{ID: "web", MountRoot: "/mnt", AppData: "/mnt/p1/.apps/appdata/web",
		Shares: map[string]string{"Photos": "/mnt/p1/shares/Photos"}})
	ph.files["web"] = ProjectWriteParams{ID: "web", UserCompose: src, Normalized: string(res.Normalized), SHA256: SHA256(res.Normalized)}
	ph.appdata["web"] = true
	p := Project{ID: "web", Name: "w", Kind: "user", Desired: "running", SHA256: SHA256(res.Normalized), Services: []string{"web"},
		Risks: []ConfirmedRisk{{Key: "share_bind:Photos", Code: compose.RShareBind, By: "old", At: time.Now()}}}
	if err := r.s.putProject(ctx, p); err != nil {
		t.Fatal(err)
	}
}

func TestP3MigrateLegacy(t *testing.T) {
	r, ph := projSvc(t)
	ctx := context.Background()
	legacy(t, r, ph)
	d, _ := r.s.ProjectDetails(ctx, "web")
	if !d.Legacy || d.User != "" {
		t.Fatalf("%+v", d.ProjectView)
	}
	if a, _ := r.s.ShareApps(ctx, "Photos"); len(a) != 1 || a[0].Mode != "root" {
		t.Fatalf("%+v", a)
	}
	// изменение приложения «доступ как root» без перевода — по-прежнему как root
	if _, err := r.s.SetGrants(ctx, "web", nil, map[string]string{"Photos": "rw"}, "a"); !errors.Is(err, ErrConflictSvc) {
		t.Fatal(err)
	}
	// неудачный перевод: всё как было
	ph.upFail["web"] = true
	tk := r.do(t)(r.s.MigrateProject(ctx, "web", nil, map[string]string{"Photos": "rw"}, nil, "boss"))
	if tk.State != tasks.Failed {
		t.Fatal("перевод должен был упасть")
	}
	got := strings.Join(ph.log(), ",")
	if !strings.Contains(got, "user.ensure web,own web 59000,acl Photos rw,write web,compose web up,revert web") ||
		!strings.HasSuffix(got, "acl Photos revoke,own web 0,user.delete web") {
		t.Fatal(got)
	}
	m, _ := r.s.loadProjects(ctx)
	if m["web"].UID != 0 || len(ph.users) != 0 || len(ph.acl["Photos"]) != 0 || ph.owner["web"] != 0 {
		t.Fatalf("%+v %v %v", m["web"], ph.users, ph.acl)
	}
	// удачный перевод
	ph.upFail = map[string]bool{}
	tk = r.do(t)(r.s.MigrateProject(ctx, "web", nil, map[string]string{"Photos": "rw"}, nil, "boss"))
	if tk.State != tasks.Succeeded {
		t.Fatal(tk.Error)
	}
	m, _ = r.s.loadProjects(ctx)
	p := m["web"]
	if p.UID != 59000 || len(p.Grants) != 1 || ph.owner["web"] != 59000 || ph.acl["Photos"][59000] != "rw" {
		t.Fatalf("%+v", p)
	}
	for _, rk := range p.Risks {
		if rk.Code == compose.RShareBind {
			t.Fatal("share_bind остался")
		}
	}
	if !strings.Contains(ph.files["web"].Normalized, `"user": "59000:59000"`) {
		t.Fatal("compose не от учётки")
	}
	if !contains(r.types(t), EvAppMigrated) {
		t.Fatal(r.types(t))
	}
	if _, err := r.s.MigrateProject(ctx, "web", nil, nil, nil, "a"); !errors.Is(err, ErrConflictSvc) {
		t.Fatal(err)
	}
}

func TestP3AccessDrift(t *testing.T) {
	r, ph := projSvc(t)
	ctx := context.Background()
	src := "services: {web: {image: x, volumes: ['/mnt/p1/shares/Photos:/p']}}\n"
	r.do(t)(r.s.CreateProject(ctx, ProjectRequest{ID: "web", Name: "w", Compose: src, Grants: map[string]string{"Photos": "ro"}}, "a"))
	// откат папки из снапшота: ACL пропали
	delete(ph.acl["Photos"], 59000)
	r.s.checkAccess(ctx)
	if !contains(r.types(t), EvACLDrift) {
		t.Fatal(r.types(t))
	}
	deadline := time.Now().Add(2 * time.Second)
	for ph.aclMode("Photos", 59000) != "ro" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ph.aclMode("Photos", 59000) != "ro" {
		t.Fatal("выдача не восстановлена")
	}
	// повтор раньше периода — не проверяется
	ph.calls = nil
	r.s.checkAccess(ctx)
	if len(ph.log()) != 0 {
		t.Fatal(ph.log())
	}
}

func TestP3AppBusy(t *testing.T) {
	r, _ := projSvc(t)
	if err := r.s.lockApp("web"); err != nil {
		t.Fatal(err)
	}
	if err := r.s.lockApp("web"); !errors.Is(err, ErrConflictSvc) {
		t.Fatal(err)
	}
	r.s.unlockApp("web")
	if err := r.s.lockApp("web"); err != nil {
		t.Fatal(err)
	}
}
