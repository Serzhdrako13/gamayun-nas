package nas

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"gamayun-nas/internal/auth"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gamayun-nas/internal/store"
)

// ---------- Executor с подменой Runner ----------

type call struct {
	path  string
	args  []string
	stdin string
}

type fakeRunner struct {
	calls []call
	// reply — код и вывод по строке «path args».
	reply map[string]CmdResult
}

func (f *fakeRunner) Run(_ context.Context, _ time.Duration, stdin []byte, path string, args ...string) (CmdResult, error) {
	f.calls = append(f.calls, call{path, args, string(stdin)})
	key := path + " " + strings.Join(args, " ")
	for k, r := range f.reply {
		if strings.HasPrefix(key, k) {
			return r, nil
		}
	}
	return CmdResult{}, nil
}

func (f *fakeRunner) has(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c.path+" "+strings.Join(c.args, " "), prefix) {
			return true
		}
	}
	return false
}

// ours — ответы id для учётки NAS (первичная группа и UID ≥ 1000).
func ours() map[string]CmdResult {
	return map[string]CmdResult{"/usr/bin/id -gn": {Stdout: []byte(UnixGroup + "\n")}, "/usr/bin/id -u": {Stdout: []byte("1001\n")}}
}

func newExec(t *testing.T, r *fakeRunner) *Executor {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "p1"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Executor{Runner: r, Tools: DefaultTools(), MountRoot: root, IncludeFile: filepath.Join(t.TempDir(), "gamayun-shares.conf"),
		Mounted:    func(dir string) bool { st, err := os.Stat(dir); return err == nil && st.IsDir() },
		FS:         func(string) (FSInfo, error) { return FSInfo{}, nil }, // не Btrfs: без подтомов
		ShadowCopy: func() bool { return false }}
}

func TestUserEnsureCreatesOnlyNew(t *testing.T) {
	r := &fakeRunner{reply: map[string]CmdResult{"/usr/bin/id": {ExitCode: 1}}}
	e := newExec(t, r)
	if _, err := e.UserEnsure(context.Background(), UserParams{Name: "ann"}); err != nil {
		t.Fatal(err)
	}
	if !r.has("/usr/sbin/groupadd -f "+UnixGroup) || !r.has("/usr/sbin/useradd --no-create-home --no-user-group --gid "+UnixGroup) {
		t.Fatalf("команды: %+v", r.calls)
	}
	// повтор: учётка уже наша — ничего не создаётся
	r2 := &fakeRunner{reply: ours()}
	if _, err := newExec(t, r2).UserEnsure(context.Background(), UserParams{Name: "ann"}); err != nil {
		t.Fatal(err)
	}
	if r2.has("/usr/sbin/useradd") {
		t.Fatal("повторный ensure не должен создавать учётку")
	}
}

func TestForeignAccountRefused(t *testing.T) {
	r := &fakeRunner{reply: map[string]CmdResult{"/usr/bin/id": {Stdout: []byte("root\n")}}}
	e := newExec(t, r)
	ctx := context.Background()
	if _, err := e.UserEnsure(ctx, UserParams{Name: "root"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := e.UserPassword(ctx, PasswordParams{Name: "root", Password: "password1"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("password: %v", err)
	}
	if _, err := e.UserDelete(ctx, UserParams{Name: "root"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("delete: %v", err)
	}
	if r.has("/usr/sbin/useradd") || r.has("/usr/sbin/userdel") || r.has("/usr/bin/smbpasswd") {
		t.Fatalf("изменяющие команды недопустимы: %+v", r.calls)
	}
}

func TestPasswordViaStdinNotArgs(t *testing.T) {
	r := &fakeRunner{reply: ours()}
	e := newExec(t, r)
	res, err := e.UserPassword(context.Background(), PasswordParams{Name: "ann", Password: "s3cret-pass"})
	if err != nil {
		t.Fatal(err)
	}
	var sm call
	for _, c := range r.calls {
		if c.path == "/usr/bin/smbpasswd" {
			sm = c
		}
	}
	if sm.stdin != "s3cret-pass\ns3cret-pass\n" {
		t.Fatalf("stdin: %q", sm.stdin)
	}
	for _, c := range append(res.Commands, strings.Join(sm.args, " ")) {
		if strings.Contains(c, "s3cret") {
			t.Fatalf("пароль утёк в аргументы/результат: %s", c)
		}
	}
}

func TestUserDelete(t *testing.T) {
	r := accessRunner("/bin/bash", UnixGroup+" "+SSHGroup)
	if _, err := newExec(t, r).UserDelete(context.Background(), UserParams{Name: "ann"}); err != nil {
		t.Fatal(err)
	}
	if !r.has("/usr/bin/smbpasswd -x -- ann") || !r.has("/usr/sbin/userdel -- ann") {
		t.Fatalf("%+v", r.calls)
	}
	// порядок: отзыв SSH → завершение процессов → userdel (без -r: дом не удаляется)
	idx := map[string]int{}
	for i, c := range r.calls {
		idx[c.path+" "+strings.Join(c.args, " ")] = i
	}
	g, k, d := idx["/usr/bin/gpasswd --delete ann "+SSHGroup], idx["/usr/bin/pkill --signal KILL --uid ann"], idx["/usr/sbin/userdel -- ann"]
	if !(g > 0 && g < k && k < d) {
		t.Fatalf("порядок: %+v", r.calls)
	}
}

func TestSharesApplyWritesIncludeAndReloads(t *testing.T) {
	r := &fakeRunner{}
	e := newExec(t, r)
	_, err := e.SharesApply(context.Background(), SharesParams{Shares: []Share{
		{Name: "docs", Pool: "p1", Comment: "Документы", Read: []string{"bob"}, Write: []string{"ann"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(e.IncludeFile)
	if err != nil {
		t.Fatal(err)
	}
	conf := string(b)
	// Регрессия: «read only = no» давал запись всем из valid users (read-пользователь писал).
	if strings.Contains(conf, "read only = no") || !strings.Contains(conf, "read only = yes") {
		t.Errorf("rw-пользователи задаются write list при read only = yes:\n%s", conf)
	}
	for _, want := range []string{"[docs]", "path = " + e.MountRoot + "/p1/shares/docs", "valid users = bob ann",
		"write list = ann", "guest ok = no", "force group = " + UnixGroup} {
		if !strings.Contains(conf, want) {
			t.Errorf("в конфиге нет %q:\n%s", want, conf)
		}
	}
	if !r.has("/usr/bin/install -d -m 2770 -o root -g "+UnixGroup) || !r.has("/usr/bin/testparm") || !r.has("/usr/bin/smbcontrol all reload-config") {
		t.Fatalf("команды: %+v", r.calls)
	}
	if _, err := os.Stat(e.IncludeFile + ".new"); err == nil {
		t.Fatal("временный файл не убран")
	}
}

func TestSharesApplyKeepsOldOnTestparmFailure(t *testing.T) {
	r := &fakeRunner{}
	e := newExec(t, r)
	os.WriteFile(e.IncludeFile, []byte("OLD"), 0o644)
	r.reply = map[string]CmdResult{"/usr/bin/testparm": {ExitCode: 1, Stderr: []byte("syntax")}}
	if _, err := e.SharesApply(context.Background(), SharesParams{Shares: []Share{{Name: "docs", Pool: "p1"}}}); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if b, _ := os.ReadFile(e.IncludeFile); string(b) != "OLD" {
		t.Fatalf("прежний файл изменён: %q", b)
	}
	if r.has("/usr/bin/smbcontrol") {
		t.Fatal("reload при ошибке проверки")
	}
}

// Шара несмонтированного пула не публикуется и каталог не
// создаётся; шары смонтированных пулов применяются.
func TestSharesApplySkipsUnmountedPool(t *testing.T) {
	r := &fakeRunner{}
	e := newExec(t, r)
	os.MkdirAll(filepath.Join(e.MountRoot, "p2"), 0o755) // каталог есть, ФС нет
	e.Mounted = func(dir string) bool { return filepath.Base(dir) == "p1" }
	res, err := e.SharesApply(context.Background(), SharesParams{Shares: []Share{{Name: "x", Pool: "p2"}, {Name: "y", Pool: "p1"}}, Homes: "p2"})
	if err != nil {
		t.Fatal(err)
	}
	conf, _ := os.ReadFile(e.IncludeFile)
	if strings.Contains(string(conf), "[x]") || !strings.Contains(string(conf), "[y]") || strings.Contains(string(conf), "[homes]") {
		t.Fatalf("конфигурация:\n%s", conf)
	}
	if r.has("/usr/bin/install -d -m 2770 -o root -g "+UnixGroup+" -- "+filepath.Join(e.MountRoot, "p2")) || len(res.Warnings) < 2 {
		t.Fatalf("%+v %v", r.calls, res.Warnings)
	}
}

func TestIsMountPoint(t *testing.T) {
	d := t.TempDir()
	if isMountPoint(d) {
		t.Fatal("временный каталог — не точка монтирования")
	}
	if !isMountPoint("/proc") {
		t.Skip("/proc не смонтирован")
	}
}

func TestSharesApplyWarnsWithoutInclude(t *testing.T) {
	e := newExec(t, &fakeRunner{})
	e.SmbConf = filepath.Join(t.TempDir(), "smb.conf")
	os.WriteFile(e.SmbConf, []byte("[global]\n"), 0o644)
	res, err := e.SharesApply(context.Background(), SharesParams{})
	if err != nil || len(res.Warnings) == 0 {
		t.Fatalf("%v %+v", err, res)
	}
	os.WriteFile(e.SmbConf, []byte("[global]\ninclude = "+e.IncludeFile+"\n"), 0o644)
	if res, _ = e.SharesApply(context.Background(), SharesParams{}); len(res.Warnings) != 0 {
		t.Fatalf("лишнее предупреждение: %v", res.Warnings)
	}
}

func TestParamsValidation(t *testing.T) {
	bad := []SharesParams{
		{Shares: []Share{{Name: "a b", Pool: "p1"}}},
		{Shares: []Share{{Name: "ok", Pool: "../x"}}},
		{Shares: []Share{{Name: "ok", Pool: "p1", Comment: "a\nb"}}},
		{Shares: []Share{{Name: "ok", Pool: "p1", Read: []string{"x\ny"}}}},
		{Shares: []Share{{Name: "ok", Pool: "p1"}, {Name: "ok", Pool: "p1"}}},
	}
	for i, p := range bad {
		if p.Validate() == nil {
			t.Errorf("%d: ожидалась ошибка", i)
		}
	}
	if (PasswordParams{Name: "ann", Password: "short"}).Validate() == nil {
		t.Error("короткий пароль принят")
	}
}

func TestRenderNoAccessAndReadOnly(t *testing.T) {
	c := Render("/srv", []Share{
		{Name: "empty", Pool: "p1"},
		{Name: "ro", Pool: "p1", ReadOnly: true, Read: []string{"bob"}, Write: []string{"ann"}},
	})
	if !strings.Contains(c, "valid users = gamayun-nobody") {
		t.Error("пустой список прав не должен открывать папку всем")
	}
	i := strings.Index(c, "[ro]")
	ro := c[i:]
	if !strings.Contains(ro, "read only = yes") || strings.Contains(ro, "write list") {
		t.Errorf("read-only пула: %s", ro)
	}
}

// ---------- Effective ----------

func TestEffective(t *testing.T) {
	enabled := []string{"ann", "bob", "cat", "dan", "eve"}
	mem := map[string][]string{"staff": {"ann", "bob", "cat", "gone"}, "guests": {"cat", "dan"}}
	acc := []Access{
		{KindGroup, "staff", LevelRW},
		{KindGroup, "guests", LevelRead},
		{KindUser, "bob", LevelNone}, // пользователь важнее группы
	}
	r, w := Effective(enabled, mem, acc)
	// ann: rw (staff); bob: none (прямое); cat: rw (макс из rw/read); dan: read; eve: нет записей
	if !reflect.DeepEqual(w, []string{"ann", "cat"}) || !reflect.DeepEqual(r, []string{"dan"}) {
		t.Fatalf("read=%v write=%v", r, w)
	}
	// none из группы сильнее
	r, w = Effective(enabled, mem, []Access{{KindGroup, "staff", LevelRW}, {KindGroup, "guests", LevelNone}})
	if !reflect.DeepEqual(w, []string{"ann", "bob"}) || len(r) != 0 {
		t.Fatalf("none группы: read=%v write=%v", r, w)
	}
	// прямое rw поверх none группы
	_, w = Effective(enabled, mem, []Access{{KindGroup, "guests", LevelNone}, {KindUser, "cat", LevelRW}})
	if !reflect.DeepEqual(w, []string{"cat"}) {
		t.Fatalf("прямое поверх группы: %v", w)
	}
	// отключённый пользователь исключён
	_, w = Effective([]string{"ann"}, mem, []Access{{KindGroup, "staff", LevelRW}, {KindUser, "bob", LevelRW}})
	if !reflect.DeepEqual(w, []string{"ann"}) {
		t.Fatalf("отключённые: %v", w)
	}
}

// ---------- Service ----------

type fakeHelper struct {
	mu    sync.Mutex
	ops   []string
	last  SharesParams
	fail  map[string]error
	pwds  []PasswordParams
	users []string
	acc   []AccessParams
	// Параметры всех вызовов и подставные ответы.
	raw   map[string][]string
	reply map[string]string
}

func (f *fakeHelper) Call(_ context.Context, op string, params, result any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, op)
	if err := f.fail[op]; err != nil {
		return err
	}
	b, _ := json.Marshal(params)
	if f.raw == nil {
		f.raw = map[string][]string{}
	}
	f.raw[op] = append(f.raw[op], string(b))
	if r := f.reply[op]; r != "" {
		json.Unmarshal([]byte(r), result)
	}
	switch op {
	case OpSharesApply:
		f.last = SharesParams{}
		json.Unmarshal(b, &f.last)
	case OpUserAccess:
		var p AccessParams
		json.Unmarshal(b, &p)
		f.acc = append(f.acc, p)
	case OpUserPassword:
		var p PasswordParams
		json.Unmarshal(b, &p)
		f.pwds = append(f.pwds, p)
	}
	return nil
}

func (f *fakeHelper) count(op string) int {
	n := 0
	for _, o := range f.ops {
		if o == op {
			n++
		}
	}
	return n
}

func (f *fakeHelper) share(name string) (Share, bool) {
	for _, s := range f.last.Shares {
		if s.Name == name {
			return s, true
		}
	}
	return Share{}, false
}

func newService(t *testing.T) (*Service, *fakeHelper) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("INSERT INTO pools(name, type, layout, created_at, updated_at) VALUES ('p1', 'shr', '{}', 0, 0), ('p2', 'shr', '{}', 0, 0)"); err != nil {
		t.Fatal(err)
	}
	h := &fakeHelper{}
	return NewService(h, db), h
}

func TestServiceUsersGroupsShares(t *testing.T) {
	s, h := newService(t)
	ctx := context.Background()
	for _, n := range []string{"ann", "bob"} {
		if _, err := s.CreateUser(ctx, n, "password-"+n, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateUser(ctx, "ann", "password-x", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("повтор: %v", err)
	}
	if _, err := s.CreateUser(ctx, "Bad Name", "password-x", false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("имя: %v", err)
	}
	if _, err := s.CreateUser(ctx, "cat", "short", false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("пароль: %v", err)
	}
	if h.count(OpUserEnsure) != 2 || h.pwds[0].Password != "password-ann" {
		t.Fatalf("helper: %v %v", h.ops, h.pwds)
	}
	if _, err := s.CreateGroup(ctx, "staff"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMembers(ctx, "staff", []string{"ann", "nobody"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("участник: %v", err)
	}
	if err := s.SetMembers(ctx, "staff", []string{"ann", "bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateShare(ctx, "docs", "nope", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("пул: %v", err)
	}
	sh, err := s.CreateShare(ctx, "docs", "p1", "Документы")
	if err != nil || sh.Access == nil || len(sh.Access) != 0 {
		t.Fatalf("%v %+v", err, sh)
	}
	// Служебные секции Samba и дубль по регистру.
	for _, n := range []string{"global", "GLOBAL", "printers", "Homes"} {
		if _, err := s.CreateShare(ctx, n, "p1", ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", n, err)
		}
	}
	// Имена устройств Windows; похожие допустимые имена — создаются.
	for _, n := range []string{"con", "CON", "Nul", "prn", "aux", "COM1", "lpt9"} {
		if _, err := s.CreateShare(ctx, n, "p1", ""); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "Windows") {
			t.Fatalf("%s: %v", n, err)
		}
	}
	for _, n := range []string{"console", "com10", "lpt0x"} {
		if _, err := s.CreateShare(ctx, n, "p1", ""); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if err := s.DeleteShare(ctx, n); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	if _, err := s.CreateShare(ctx, "DOCS", "p1", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("регистр: %v", err)
	}
	if x, _ := h.share("docs"); len(x.Read)+len(x.Write) != 0 {
		t.Fatalf("новая папка открыта: %+v", x)
	}
	if err := s.SetAccess(ctx, "docs", []Access{{KindGroup, "staff", LevelRead}, {KindUser, "ann", LevelRW}}); err != nil {
		t.Fatal(err)
	}
	x, _ := h.share("docs")
	if !reflect.DeepEqual(x.Write, []string{"ann"}) || !reflect.DeepEqual(x.Read, []string{"bob"}) {
		t.Fatalf("права: %+v", x)
	}
	// ошибочные записи
	for _, bad := range [][]Access{{{KindUser, "ann", "admin"}}, {{"other", "ann", LevelRead}}, {{KindUser, "ann", LevelRead}, {KindUser, "ann", LevelRW}}} {
		if err := s.SetAccess(ctx, "docs", bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if err := s.SetAccess(ctx, "docs", []Access{{KindUser, "ghost", LevelRead}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("субъект: %v", err)
	}
	// отключение пользователя
	if err := s.SetEnabled(ctx, "ann", false); err != nil {
		t.Fatal(err)
	}
	if x, _ = h.share("docs"); len(x.Write) != 0 {
		t.Fatalf("отключённый пользователь в правах: %+v", x)
	}
	// удаление пользователя чистит группы и права, затем удаляет учётку
	if err := s.DeleteUser(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	gs, _ := s.ListGroups(ctx)
	if gs[1].Name != "staff" || !reflect.DeepEqual(gs[1].Members, []string{"ann"}) || h.count(OpUserDelete) != 1 {
		t.Fatalf("%+v %v", gs, h.ops)
	}
	// удаление группы убирает её права
	if err := s.DeleteGroup(ctx, "staff"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetShare(ctx, "docs")
	if len(got.Access) != 1 || got.Access[0].Kind != KindUser {
		t.Fatalf("права удалённой группы остались: %+v", got.Access)
	}
	if err := s.DeleteShare(ctx, "docs"); err != nil {
		t.Fatal(err)
	}
	if len(h.last.Shares) != 0 {
		t.Fatalf("папка осталась в конфигурации: %+v", h.last)
	}
	if err := s.DeleteShare(ctx, "docs"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

// Хук ShareGuard — read-only пула отражается в шарах только этого пула.
func TestSetPoolReadOnlyHook(t *testing.T) {
	s, h := newService(t)
	ctx := context.Background()
	s.CreateUser(ctx, "ann", "password-ann", false)
	s.CreateShare(ctx, "a", "p1", "")
	s.CreateShare(ctx, "b", "p2", "")
	for _, n := range []string{"a", "b"} {
		if err := s.SetAccess(ctx, n, []Access{{KindUser, "ann", LevelRW}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetPoolReadOnly(ctx, "p1", true); err != nil {
		t.Fatal(err)
	}
	a, _ := h.share("a")
	b, _ := h.share("b")
	if !a.ReadOnly || b.ReadOnly {
		t.Fatalf("a.ro=%v b.ro=%v", a.ReadOnly, b.ReadOnly)
	}
	if info, _ := s.GetShare(ctx, "a"); !info.ReadOnly {
		t.Fatal("API не показывает read-only")
	}
	if !strings.Contains(Render("/srv", h.last.Shares), "read only = yes") {
		t.Fatal("в конфигурации нет read only")
	}
	// после снятия запись возвращается
	if err := s.SetPoolReadOnly(ctx, "p1", false); err != nil {
		t.Fatal(err)
	}
	if a, _ = h.share("a"); a.ReadOnly || !reflect.DeepEqual(a.Write, []string{"ann"}) {
		t.Fatalf("после снятия: %+v", a)
	}
	// пул без шар: helper не дёргается
	n := h.count(OpSharesApply)
	if err := s.SetPoolReadOnly(ctx, "p3", true); err != nil || h.count(OpSharesApply) != n {
		t.Fatalf("%v, вызовов %d → %d", err, n, h.count(OpSharesApply))
	}
}

func TestCreateShareRollsBackOnHelperFailure(t *testing.T) {
	s, h := newService(t)
	h.fail = map[string]error{OpSharesApply: errors.New("пул не смонтирован")}
	if _, err := s.CreateShare(context.Background(), "docs", "p1", ""); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if l, _ := s.ListShares(context.Background()); len(l) != 0 {
		t.Fatalf("запись осталась: %+v", l)
	}
}

func TestResyncOnlyWithShares(t *testing.T) {
	s, h := newService(t)
	ctx := context.Background()
	if err := s.Resync(ctx); err != nil || h.count(OpSharesApply) != 0 {
		t.Fatalf("%v %v", err, h.ops)
	}
	s.CreateShare(ctx, "docs", "p1", "")
	n := h.count(OpSharesApply)
	if err := s.Resync(ctx); err != nil || h.count(OpSharesApply) != n+1 {
		t.Fatalf("%v %v", err, h.ops)
	}
}

// ---------- Единая учётка, стандартные группы ----------

func newUnified(t *testing.T) (*Service, *fakeHelper, *auth.Service, *sql.DB) {
	t.Helper()
	s, h := newService(t)
	as := auth.New(s.db, auth.Options{Params: auth.Params{Memory: 8, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}})
	s.Web = auth.Accounts{S: as}
	return s, h, as, s.db
}

func webRow(t *testing.T, db *sql.DB, name string) (admin, disabled, hasPw bool, found bool) {
	t.Helper()
	var pw sql.NullString
	err := db.QueryRow("SELECT is_admin, disabled, password FROM users WHERE name = ?", name).Scan(&admin, &disabled, &pw)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	return admin, disabled, pw.Valid, true
}

func TestSystemGroupsEnsuredAndProtected(t *testing.T) {
	s, _, _, _ := newUnified(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ { // идемпотентно
		if err := s.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	gs, _ := s.ListGroups(ctx)
	if len(gs) != 2 || gs[0].Name != GroupAdmins || gs[1].Name != GroupUsers || !gs[0].System || !gs[1].System {
		t.Fatalf("%+v", gs)
	}
	for _, g := range SystemGroups {
		if err := s.DeleteGroup(ctx, g); !errors.Is(err, ErrConflict) {
			t.Fatalf("удаление %s: %v", g, err)
		}
		if _, err := s.CreateGroup(ctx, g); !errors.Is(err, ErrConflict) {
			t.Fatalf("повторное создание %s: %v", g, err)
		}
	}
}

func TestNewUserDefaultsToUsersGroupAndWebAccount(t *testing.T) {
	s, h, _, db := newUnified(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "ann", "correct-horse-1", false)
	if err != nil || u.Admin || !reflect.DeepEqual(u.Groups, []string{GroupUsers}) {
		t.Fatalf("%+v %v", u, err)
	}
	a, err := s.CreateUser(ctx, "root1", "correct-horse-2", true)
	if err != nil || !a.Admin || !reflect.DeepEqual(a.Groups, []string{GroupAdmins, GroupUsers}) {
		t.Fatalf("%+v %v", a, err)
	}
	if adm, _, pw, ok := webRow(t, db, "ann"); !ok || adm || !pw {
		t.Fatal("ann: учётка веба без прав администратора с паролем")
	}
	if adm, _, _, ok := webRow(t, db, "root1"); !ok || !adm {
		t.Fatal("root1: администратор веба")
	}
	// пароль Samba — тот же, что у веба, одним действием
	if len(h.pwds) != 2 || h.pwds[1].Password != "correct-horse-2" {
		t.Fatalf("%+v", h.pwds)
	}
	// пароль, не проходящий правила веба (совпадает с именем), отказывается до любых действий
	n := len(h.ops)
	if _, err := s.CreateUser(ctx, "bob12345", "bob12345", false); !errors.Is(err, ErrInvalid) || len(h.ops) != n {
		t.Fatalf("%v %v", err, h.ops[n:])
	}
}

func TestSetPasswordUpdatesSambaAndWeb(t *testing.T) {
	s, h, as, db := newUnified(t)
	ctx := context.Background()
	s.CreateUser(ctx, "root1", "correct-horse-2", true)
	if err := s.SetPassword(ctx, "root1", "another-pass-3"); err != nil {
		t.Fatal(err)
	}
	if h.pwds[len(h.pwds)-1].Password != "another-pass-3" {
		t.Fatalf("%+v", h.pwds)
	}
	var enc string
	db.QueryRow("SELECT password FROM users WHERE name = 'root1'").Scan(&enc)
	if ok, _, err := auth.VerifyPassword("another-pass-3", enc, as.Params()); err != nil || !ok {
		t.Fatalf("хэш веба не обновлён: %v %v", ok, err)
	}
}

func TestLastAdminProtected(t *testing.T) {
	s, _, _, db := newUnified(t)
	ctx := context.Background()
	s.CreateUser(ctx, "root1", "correct-horse-2", true)
	s.CreateUser(ctx, "ann", "correct-horse-1", false)
	if err := s.DeleteUser(ctx, "root1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("удаление последнего: %v", err)
	}
	if err := s.SetEnabled(ctx, "root1", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("отключение последнего: %v", err)
	}
	if err := s.SetMembers(ctx, GroupAdmins, []string{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("пустая administrators: %v", err)
	}
	if err := s.SetMembers(ctx, GroupAdmins, []string{"ann"}); err != nil { // замена админа
		t.Fatal(err)
	}
	if adm, _, _, _ := webRow(t, db, "root1"); adm {
		t.Fatal("root1 разжалован в вебе")
	}
	if adm, _, _, _ := webRow(t, db, "ann"); !adm {
		t.Fatal("ann стала администратором веба")
	}
	// теперь последний — ann
	if err := s.DeleteUser(ctx, "ann"); !errors.Is(err, ErrConflict) {
		t.Fatalf("%v", err)
	}
	if err := s.DeleteUser(ctx, "root1"); err != nil { // обычного можно
		t.Fatal(err)
	}
	if _, _, _, ok := webRow(t, db, "root1"); ok {
		t.Fatal("учётка веба удалена вместе с пользователем")
	}
}

func TestDisableBlocksWeb(t *testing.T) {
	s, _, _, db := newUnified(t)
	ctx := context.Background()
	s.CreateUser(ctx, "root1", "correct-horse-2", true)
	s.CreateUser(ctx, "root2", "correct-horse-3", true)
	if err := s.SetEnabled(ctx, "root2", false); err != nil {
		t.Fatal(err)
	}
	if _, dis, _, _ := webRow(t, db, "root2"); !dis {
		t.Fatal("отключённый не входит в веб")
	}
	// отключённый админ не считается: root1 теперь последний
	if err := s.DeleteUser(ctx, "root1"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

func TestSyncAdoptsExistingWebAdmin(t *testing.T) {
	s, h, as, db := newUnified(t)
	ctx := context.Background()
	hash, _ := auth.HashPassword("correct-horse-2", as.Params())
	if _, err := (auth.Store{DB: db}).CreateUser(ctx, "admin", hash, true, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // повторный запуск ничего не дублирует
		if err := s.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	us, _ := s.ListUsers(ctx)
	if len(us) != 1 || us[0].Name != "admin" || !us[0].Admin || !reflect.DeepEqual(us[0].Groups, []string{GroupAdmins, GroupUsers}) {
		t.Fatalf("%+v", us)
	}
	if h.count(OpUserEnsure) != 1 {
		t.Fatalf("учётка ОС создаётся один раз: %v", h.ops)
	}
	// вход в веб сохранён: хэш и признак на месте
	if adm, dis, pw, _ := webRow(t, db, "admin"); !adm || dis || !pw {
		t.Fatal("вход администратора потерян")
	}
	// пароль, заданный в вебе, попадает в Samba
	if err := s.SetSambaPassword(ctx, "admin", "correct-horse-2"); err != nil || len(h.pwds) != 1 {
		t.Fatalf("%v %+v", err, h.pwds)
	}
	// неизвестное имя — без действий
	if err := s.SetSambaPassword(ctx, "ghost", "correct-horse-2"); err != nil || len(h.pwds) != 1 {
		t.Fatalf("%v", err)
	}
}

func TestExistingUsersJoinUsersGroupOnce(t *testing.T) {
	s, _ := newService(t) // без Web
	ctx := context.Background()
	s.db.Exec("INSERT INTO nas_users(name, enabled, created_at) VALUES ('old', 1, 0)")
	s.Sync(ctx)
	us, _ := s.ListUsers(ctx)
	if !reflect.DeepEqual(us[0].Groups, []string{GroupUsers}) {
		t.Fatalf("%+v", us)
	}
	s.SetMembers(ctx, GroupUsers, []string{}) // оператор убрал — повторный Sync не возвращает
	s.Sync(ctx)
	if us, _ = s.ListUsers(ctx); len(us[0].Groups) != 0 {
		t.Fatalf("%+v", us)
	}
}

// Учётки приложений app-* (и UID из их диапазона) не трогает
// ни одна операция nas.user.*: отказ на проверке параметров и в isOurs.
func TestAppAccountsRefusedByUserOps(t *testing.T) {
	for _, err := range []error{
		UserParams{Name: "app-web"}.Validate(),
		PasswordParams{Name: "app-web", Password: "password1"}.Validate(),
		AccessParams{Name: "app-web"}.Validate(),
	} {
		if err == nil {
			t.Fatal("имя app-* прошло проверку параметров")
		}
	}
	// учётка с первичной группой NAS, но UID из диапазона приложений
	r := &fakeRunner{reply: map[string]CmdResult{"/usr/bin/id -gn": {Stdout: []byte(UnixGroup + "\n")}, "/usr/bin/id -u": {Stdout: []byte("59003\n")}}}
	e := newExec(t, r)
	ctx := context.Background()
	var res Result
	if ours, _, _ := e.isOurs(ctx, &res, "bob"); ours {
		t.Fatal("UID 59003 признан учёткой NAS")
	}
	if _, err := (&Service{}).CreateUserSSH(ctx, "app-web", "password1", false, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("пользователь NAS app-web: %v", err)
	}
	if !IsAppUser("x", AppUIDMin) || !IsAppUser("x", AppUIDMax) || IsAppUser("x", AppUIDMax+1) || IsAppUser("ann", 1000) {
		t.Fatal("IsAppUser: границы")
	}
}

// В шарах inherit acls — default ACL выдачи приложению
// наследуется файлами SMB.
func TestRenderInheritACLs(t *testing.T) {
	out := Render("/mnt", []Share{{Name: "docs", Pool: "p1", Write: []string{"ann"}}})
	if !strings.Contains(out, "\tinherit acls = yes\n") {
		t.Fatal(out)
	}
}
