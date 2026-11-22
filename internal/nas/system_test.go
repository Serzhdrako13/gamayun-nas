package nas

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gamayun-nas/internal/store"
)

// ---------- Executor (подмена Runner) ----------

func sysExec(t *testing.T, r *fakeRunner) *Executor {
	t.Helper()
	e := newExec(t, r)
	d := t.TempDir()
	e.HostsFile = filepath.Join(d, "hosts")
	e.GuardDropIn = filepath.Join(d, "jail.d", "gamayun-nas.local")
	e.Tools.Fail2ban = filepath.Join(d, "fail2ban-client")
	os.WriteFile(e.HostsFile, []byte("127.0.0.1\tlocalhost\n127.0.1.1\told\n::1 localhost\n"), 0o644)
	return e
}

func TestHostnameSet(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{reply: map[string]CmdResult{"/usr/bin/hostnamectl --static": {Stdout: []byte("old\n")}, "/usr/bin/systemctl is-active": {ExitCode: 3}}}
	e := sysExec(t, r)
	if _, err := e.HostnameSet(ctx, HostnameParams{Hostname: "nas1"}); err != nil {
		t.Fatal(err)
	}
	if !r.has("/usr/bin/hostnamectl set-hostname --static nas1") || r.has("/usr/bin/systemctl restart") {
		t.Fatalf("%+v", r.calls)
	}
	b, _ := os.ReadFile(e.HostsFile)
	if string(b) != "127.0.0.1\tlocalhost\n127.0.1.1\tnas1\n::1 localhost\n" {
		t.Fatalf("hosts: %q", b)
	}
	// avahi запущен — перезапускается
	r = &fakeRunner{reply: map[string]CmdResult{"/usr/bin/hostnamectl --static": {Stdout: []byte("old\n")}}}
	e = sysExec(t, r)
	if _, err := e.HostnameSet(ctx, HostnameParams{Hostname: "nas1"}); err != nil || !r.has("/usr/bin/systemctl restart avahi-daemon") {
		t.Fatal(err, r.calls)
	}
	// то же имя — ничего не меняется
	r = &fakeRunner{reply: map[string]CmdResult{"/usr/bin/hostnamectl --static": {Stdout: []byte("nas1\n")}}}
	res, err := sysExec(t, r).HostnameSet(ctx, HostnameParams{Hostname: "nas1"})
	if err != nil || res.Changed || r.has("/usr/bin/hostnamectl set-hostname") {
		t.Fatal(err, r.calls)
	}
	// отказ hostnamectl — /etc/hosts возвращается
	r = &fakeRunner{reply: map[string]CmdResult{"/usr/bin/hostnamectl --static": {Stdout: []byte("old\n")}, "/usr/bin/hostnamectl set-hostname": {ExitCode: 1, Stderr: []byte("denied")}}}
	e = sysExec(t, r)
	if _, err := e.HostnameSet(ctx, HostnameParams{Hostname: "nas1"}); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if b, _ := os.ReadFile(e.HostsFile); !strings.Contains(string(b), "127.0.1.1\told") {
		t.Fatalf("hosts не возвращён: %q", b)
	}
}

func TestHostnameValidation(t *testing.T) {
	for _, h := range []string{"", "NAS", "1nas", "nas_1", "nas-", "-nas", "averyveryverylongname", "na s", "nas;x"} {
		if (HostnameParams{h}).Validate() == nil {
			t.Errorf("принято %q", h)
		}
	}
	for _, h := range []string{"nas", "my-nas1", "a"} {
		if err := (HostnameParams{h}).Validate(); err != nil {
			t.Errorf("отклонено %q: %v", h, err)
		}
	}
	for _, z := range []string{"", "../etc/passwd", "Europe/../x", "Europe/Moscow;x", "/Europe/Moscow", "a/b/c/d"} {
		if (TimezoneParams{Timezone: z}).Validate() == nil {
			t.Errorf("принят пояс %q", z)
		}
	}
	if (TimezoneParams{Timezone: "America/Argentina/Buenos_Aires"}).Validate() != nil || (TimezoneParams{Timezone: "Etc/GMT+3"}).Validate() != nil {
		t.Error("отклонён допустимый пояс")
	}
}

func TestTimezoneSet(t *testing.T) {
	ctx := context.Background()
	reply := func() map[string]CmdResult {
		return map[string]CmdResult{
			"/usr/bin/timedatectl list-timezones": {Stdout: []byte("Europe/Moscow\nUTC\n")},
			"/usr/bin/timedatectl show":           {Stdout: []byte("Timezone=UTC\nNTP=yes\nNTPSynchronized=yes\n")},
		}
	}
	r := &fakeRunner{reply: reply()}
	if _, err := sysExec(t, r).TimezoneSet(ctx, TimezoneParams{Timezone: "Europe/Nowhere", NTP: true}); !errors.Is(err, ErrRefused) || r.has("/usr/bin/timedatectl set-") {
		t.Fatal(err, r.calls)
	}
	r = &fakeRunner{reply: reply()}
	res, err := sysExec(t, r).TimezoneSet(ctx, TimezoneParams{Timezone: "Europe/Moscow", NTP: false})
	if err != nil || !res.Changed || !r.has("/usr/bin/timedatectl set-timezone Europe/Moscow") || !r.has("/usr/bin/timedatectl set-ntp false") {
		t.Fatal(err, r.calls)
	}
	r = &fakeRunner{reply: reply()}
	res, err = sysExec(t, r).TimezoneSet(ctx, TimezoneParams{Timezone: "UTC", NTP: true})
	if err != nil || res.Changed || r.has("/usr/bin/timedatectl set-") {
		t.Fatalf("повтор изменил: %v %+v", err, r.calls)
	}
}

func TestIdentityGet(t *testing.T) {
	r := &fakeRunner{reply: map[string]CmdResult{
		"/usr/bin/hostnamectl --static":       {Stdout: []byte("nas\n")},
		"/usr/bin/timedatectl list-timezones": {Stdout: []byte("Europe/Moscow\nUTC\n")},
		"/usr/bin/timedatectl show":           {Stdout: []byte("Timezone=Europe/Moscow\nNTP=no\nNTPSynchronized=no\n")},
	}}
	id, err := sysExec(t, r).IdentityGet(context.Background(), SetupParams{})
	if err != nil || id.Hostname != "nas" || id.Timezone != "Europe/Moscow" || id.NTP || len(id.Timezones) != 2 {
		t.Fatal(err, id)
	}
}

func homesRunner() *fakeRunner {
	m := ours()
	m["/usr/bin/getent passwd ann"] = CmdResult{Stdout: []byte("ann:x:1001:1000:gamayun-nas:/nonexistent:/usr/sbin/nologin\n")}
	return &fakeRunner{reply: m}
}

func TestHomesApplyEnableDisable(t *testing.T) {
	ctx := context.Background()
	r := homesRunner()
	e := sysExec(t, r)
	res, err := e.HomesApply(ctx, HomesParams{Enabled: true, Pool: "p1", Users: []string{"ann"}})
	if err != nil || !res.Changed {
		t.Fatal(err, res)
	}
	base := HomesDir(e.MountRoot, "p1")
	for _, want := range []string{
		"/usr/bin/install -d -m 0711 -o root -g " + UnixGroup + " -- " + base,
		"/usr/bin/install -d -m 0700 -o ann -g " + UnixGroup + " -- " + base + "/ann",
		"/usr/sbin/usermod --home " + base + "/ann -- ann",
	} {
		if !r.has(want) {
			t.Fatalf("нет %q: %+v", want, r.calls)
		}
	}
	// дом уже там — usermod не вызывается
	r = homesRunner()
	r.reply["/usr/bin/getent passwd ann"] = CmdResult{Stdout: []byte("ann:x:1001:1000:x:" + base + "/ann:/bin/bash\n")}
	if _, err := e.withRunner(r).HomesApply(ctx, HomesParams{Enabled: true, Pool: "p1", Users: []string{"ann"}}); err != nil || r.has("/usr/sbin/usermod") {
		t.Fatal(err, r.calls)
	}
	// выключение: домашний каталог снова без дома, каталоги не создаются и не удаляются
	r = homesRunner()
	r.reply["/usr/bin/getent passwd ann"] = CmdResult{Stdout: []byte("ann:x:1001:1000:x:" + base + "/ann:/bin/bash\n")}
	if _, err := e.withRunner(r).HomesApply(ctx, HomesParams{Enabled: false, Pool: "p1", Users: []string{"ann"}}); err != nil ||
		!r.has("/usr/sbin/usermod --home /nonexistent -- ann") || r.has("/usr/bin/install") || r.has("/bin/rm") {
		t.Fatal(err, r.calls)
	}
}

func (e *Executor) withRunner(r Runner) *Executor { c := *e; c.Runner = r; return &c }

func TestHomesApplyRefusals(t *testing.T) {
	ctx := context.Background()
	e := sysExec(t, homesRunner())
	if _, err := e.HomesApply(ctx, HomesParams{Enabled: true, Pool: "nopool"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("несмонтированный пул: %v", err)
	}
	// чужая учётка: не трогается
	r := &fakeRunner{reply: map[string]CmdResult{"/usr/bin/id -gn": {Stdout: []byte("root\n")}}}
	res, err := sysExec(t, r).HomesApply(ctx, HomesParams{Enabled: true, Pool: "p1", Users: []string{"daemon"}})
	if err != nil || r.has("/usr/sbin/usermod") || len(res.Warnings) == 0 || r.has("/usr/bin/install -d -m 0700") {
		t.Fatal(err, r.calls)
	}
	for _, p := range []HomesParams{{Enabled: true, Pool: "../x"}, {Enabled: true}, {Users: []string{"A b"}}} {
		if p.Validate() == nil {
			t.Errorf("принято %+v", p)
		}
	}
}

func TestRenderHomesAndNetbios(t *testing.T) {
	out := RenderParams("/m", SharesParams{Netbios: "nas1", Homes: "p1", Shares: []Share{{Name: "docs", Pool: "p1", Write: []string{"ann"}}}})
	i, j, k := strings.Index(out, "netbios name = nas1"), strings.Index(out, "[homes]"), strings.Index(out, "[docs]")
	if !(i >= 0 && i < j && j < k) {
		t.Fatalf("порядок: %s", out)
	}
	for _, want := range []string{"path = /m/p1/shares/homes/%S", "valid users = %S", "read only = no", "browseable = no", "directory mask = 0700"} {
		if !strings.Contains(out[j:k], want) {
			t.Fatalf("нет %q: %s", want, out)
		}
	}
	if strings.Contains(out[j:k], "force group") {
		t.Fatal("в [homes] не должно быть force group: владелец — сам пользователь")
	}
	if !strings.Contains(RenderParams("/m", SharesParams{Homes: "p1", HomesReadOnly: true}), "read only = yes") {
		t.Fatal("пул read-only: [homes] только чтение")
	}
	if strings.Contains(Render("/m", nil), "[homes]") || strings.Contains(Render("/m", nil), "netbios") {
		t.Fatal("без службы и имени ничего лишнего")
	}
	if (SharesParams{Shares: []Share{{Name: "Homes", Pool: "p1"}}}).Validate() == nil {
		t.Fatal("share homes зарезервирован")
	}
	if (SharesParams{Netbios: "Bad Name"}).Validate() == nil {
		t.Fatal("netbios name не проверен")
	}
}

func TestSharesApplyRestartsSambaOnNameChange(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{}
	e := newExec(t, r)
	if _, err := e.SharesApply(ctx, SharesParams{Netbios: "nas1"}); err != nil || !r.has("/usr/bin/systemctl try-restart smbd") {
		t.Fatal(err, r.calls)
	}
	r.calls = nil
	if _, err := e.SharesApply(ctx, SharesParams{Netbios: "nas1"}); err != nil || r.has("/usr/bin/systemctl") {
		t.Fatalf("то же имя — без перезапуска: %v %+v", err, r.calls)
	}
}

func guardExec(t *testing.T, r *fakeRunner) *Executor {
	e := sysExec(t, r)
	os.WriteFile(e.Tools.Fail2ban, []byte("#!/bin/sh\n"), 0o755)
	return e
}

var okGuard = GuardParams{Enabled: true, MaxRetry: 3, FindTime: 300, BanTime: 900, Ignore: []string{"192.168.5.0/24", "10.0.0.7"}}

func TestGuardApplyEnableDisable(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{reply: map[string]CmdResult{"/usr/bin/systemctl is-active": {ExitCode: 3}}}
	e := guardExec(t, r)
	res, err := e.GuardApply(ctx, okGuard)
	if err != nil || !res.Changed {
		t.Fatal(err, res)
	}
	b, _ := os.ReadFile(e.GuardDropIn)
	for _, want := range []string{"enabled = true", "maxretry = 3", "findtime = 300s", "bantime = 900s", "ignoreip = 127.0.0.0/8 ::1 192.168.5.0/24 10.0.0.7", "_COMM=sshd _COMM=sshd-session"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("нет %q: %s", want, b)
		}
	}
	if !r.has(e.Tools.Fail2ban+" -t") || !r.has("/usr/bin/systemctl enable --now fail2ban") {
		t.Fatalf("%+v", r.calls)
	}
	// работает и без изменений — ничего не запускает
	r = &fakeRunner{}
	e = e.withRunner(r)
	res, err = e.GuardApply(ctx, okGuard)
	if err != nil || res.Changed || r.has("/usr/bin/systemctl enable") || r.has(e.Tools.Fail2ban+" reload") {
		t.Fatalf("повтор изменил: %v %+v", err, r.calls)
	}
	// смена параметров при работающей службе — reload
	p := okGuard
	p.MaxRetry = 4
	if _, err := e.GuardApply(ctx, p); err != nil || !r.has(e.Tools.Fail2ban+" reload") {
		t.Fatal(err, r.calls)
	}
	// выключение
	p.Enabled = false
	if _, err := e.GuardApply(ctx, p); err != nil || !r.has("/usr/bin/systemctl disable --now fail2ban") {
		t.Fatal(err, r.calls)
	}
	if b, _ := os.ReadFile(e.GuardDropIn); !strings.Contains(string(b), "enabled = false") {
		t.Fatalf("%s", b)
	}
}

func TestGuardApplyRollsBackOnBadConfig(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{}
	e := guardExec(t, r)
	if _, err := e.GuardApply(ctx, okGuard); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(e.GuardDropIn)
	bad := &fakeRunner{reply: map[string]CmdResult{e.Tools.Fail2ban + " -t": {ExitCode: 1, Stderr: []byte("ERROR")}}}
	p := okGuard
	p.MaxRetry = 9
	if _, err := e.withRunner(bad).GuardApply(ctx, p); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if b, _ := os.ReadFile(e.GuardDropIn); string(b) != string(old) {
		t.Fatalf("прежний файл не возвращён: %s", b)
	}
	if bad.has("/usr/bin/systemctl enable") || bad.has("/usr/bin/systemctl disable") {
		t.Fatalf("служба тронута при отклонённой конфигурации: %+v", bad.calls)
	}
}

func TestGuardNotInstalledAndValidation(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{}
	e := sysExec(t, r) // файла fail2ban-client нет
	if _, err := e.GuardApply(ctx, okGuard); !errors.Is(err, ErrRefused) {
		t.Fatalf("включение без пакета: %v", err)
	}
	p := okGuard
	p.Enabled = false
	if res, err := e.GuardApply(ctx, p); err != nil || res.Changed || len(r.calls) != 0 {
		t.Fatal(err, r.calls)
	}
	if st, err := e.GuardStatus(ctx, SetupParams{}); err != nil || st.Installed || st.Banned == nil {
		t.Fatal(err, st)
	}
	bad := []GuardParams{
		{MaxRetry: 0, FindTime: 600, BanTime: 600}, {MaxRetry: 21, FindTime: 600, BanTime: 600},
		{MaxRetry: 5, FindTime: 59, BanTime: 600}, {MaxRetry: 5, FindTime: 600, BanTime: 30},
		{MaxRetry: 5, FindTime: 600, BanTime: 600, Ignore: []string{"1.2.3.4; reboot"}},
		{MaxRetry: 5, FindTime: 600, BanTime: 600, Ignore: []string{"example.com"}},
	}
	for _, b := range bad {
		if b.Validate() == nil {
			t.Errorf("принято %+v", b)
		}
	}
	if (UnbanParams{"1.2.3.0/24"}).Validate() == nil || (UnbanParams{"10.1.1.1"}).Validate() != nil {
		t.Error("unban: только одиночный адрес")
	}
}

func TestGuardStatusParsesBanned(t *testing.T) {
	r := &fakeRunner{reply: map[string]CmdResult{}}
	e := guardExec(t, r)
	r.reply[e.Tools.Fail2ban+" status sshd"] = CmdResult{Stdout: []byte("Status for the jail: sshd\n`- Actions\n   |- Currently banned:\t2\n   `- Banned IP list:\t192.0.2.7 2001:db8::1 bogus;rm\n")}
	st, err := e.GuardStatus(context.Background(), SetupParams{})
	if err != nil || !st.Active || len(st.Banned) != 2 || st.Banned[0] != "192.0.2.7" {
		t.Fatal(err, st)
	}
}

// ---------- Service ----------

func jsonOf(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatal(err)
	}
}

func TestSetupWizardStateOnce(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if d, err := s.SetupDone(ctx); err != nil || d {
		t.Fatal(d, err)
	}
	if err := s.CompleteSetup(ctx); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.SetupDone(ctx); !d {
		t.Fatal("не отмечено")
	}
	if err := s.CompleteSetup(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("повторный проход: %v", err)
	}
}

func TestMigrationMarksConfiguredInstallDone(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"DROP TABLE smart_history", "DROP TABLE notify_groups", "DROP TABLE outbox", "DROP TABLE notifications", "DROP TABLE settings", "ALTER TABLE nas_users DROP COLUMN password_sync_error",
		"ALTER TABLE tasks DROP COLUMN alive_at", "ALTER TABLE tasks DROP COLUMN alive_note",
		"DROP TABLE collector_state", "DROP INDEX events_subject", "ALTER TABLE events DROP COLUMN severity", "ALTER TABLE events DROP COLUMN subject", "DELETE FROM schema_migrations WHERE version >= 7",
		"INSERT INTO users(name, is_admin, created_at, updated_at) VALUES ('boss', 1, 0, 0)"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if d, err := NewService(&fakeHelper{}, db).SetupDone(ctx); err != nil || !d {
		t.Fatalf("установка с администратором должна считаться настроенной: %v %v", d, err)
	}
}

func TestSetIdentity(t *testing.T) {
	ctx := context.Background()
	s, h := newService(t)
	if _, err := s.SetIdentity(ctx, "Bad_Name", "UTC", true); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.SetIdentity(ctx, "nas1", "../x", true); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if len(h.ops) != 0 {
		t.Fatalf("helper вызван при неверных значениях: %v", h.ops)
	}
	if _, err := s.SetIdentity(ctx, "nas1", "Europe/Moscow", false); err != nil {
		t.Fatal(err)
	}
	var tz TimezoneParams
	jsonOf(t, h.raw[OpTimezoneSet][0], &tz)
	if tz.Timezone != "Europe/Moscow" || tz.NTP || h.count(OpHostnameSet) != 1 {
		t.Fatalf("%+v %v", tz, h.ops)
	}
	if h.last.Netbios != "nas1" {
		t.Fatalf("имя Samba не передано в shares.apply: %+v", h.last)
	}
	// отказ helper — имя в модели не сохраняется
	s2, h2 := newService(t)
	h2.fail = map[string]error{OpHostnameSet: errors.New("boom")}
	if _, err := s2.SetIdentity(ctx, "nas2", "UTC", true); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if v, _ := s2.kvGet(ctx, keyServerName); v != "" {
		t.Fatalf("имя сохранено при отказе: %q", v)
	}
}

func TestHomesServiceFlow(t *testing.T) {
	ctx := context.Background()
	s, h := newService(t)
	if _, err := s.CreateUser(ctx, "ann", "password-1", false); err != nil {
		t.Fatal(err)
	}
	if h.count(OpHomesApply) != 0 {
		t.Fatal("служба выключена: домашних каталогов нет")
	}
	if _, err := s.SetHomes(ctx, true, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.SetHomes(ctx, true, "zz"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.SetHomes(ctx, true, "p1"); err != nil {
		t.Fatal(err)
	}
	var hp HomesParams
	jsonOf(t, h.raw[OpHomesApply][0], &hp)
	if !hp.Enabled || hp.Pool != "p1" || len(hp.Users) != 1 || hp.Users[0] != "ann" || h.last.Homes != "p1" {
		t.Fatalf("%+v %+v", hp, h.last)
	}
	// helper вызывается до применения Samba: ресурс не ссылается на несуществующие каталоги
	if h.ops[len(h.ops)-2] != OpHomesApply || h.ops[len(h.ops)-1] != OpSharesApply {
		t.Fatalf("порядок: %v", h.ops)
	}
	// новый пользователь получает домашний каталог
	if _, err := s.CreateUser(ctx, "bob", "password-1", false); err != nil {
		t.Fatal(err)
	}
	var np HomesParams
	jsonOf(t, h.raw[OpHomesApply][len(h.raw[OpHomesApply])-1], &np)
	if len(np.Users) != 1 || np.Users[0] != "bob" || !np.Enabled {
		t.Fatalf("%+v", np)
	}
	// смена пула без выключения — конфликт; общая папка homes — недопустима
	if _, err := s.SetHomes(ctx, true, "p2"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := s.CreateShare(ctx, "Homes", "p1", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("share homes: %v", err)
	}
	if on, _ := s.SharesOnPool(ctx, "p1"); len(on) != 1 || on[0] != HomesShare {
		t.Fatalf("пул с homes должен числиться занятым: %v", on)
	}
	// выключение: [homes] убирается, учётки без дома
	if _, err := s.SetHomes(ctx, false, ""); err != nil {
		t.Fatal(err)
	}
	var dp HomesParams
	jsonOf(t, h.raw[OpHomesApply][len(h.raw[OpHomesApply])-1], &dp)
	if dp.Enabled || len(dp.Users) != 2 || h.last.Homes != "" {
		t.Fatalf("%+v %+v", dp, h.last)
	}
	if hi, _ := s.Homes(ctx); hi.Enabled {
		t.Fatal("служба должна быть выключена")
	}
}

func TestHomesHelperFailureLeavesServiceOff(t *testing.T) {
	ctx := context.Background()
	s, h := newService(t)
	h.fail = map[string]error{OpHomesApply: errors.New("boom")}
	if _, err := s.SetHomes(ctx, true, "p1"); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if hi, _ := s.Homes(ctx); hi.Enabled || h.count(OpSharesApply) != 0 {
		t.Fatal("служба включилась при отказе helper")
	}
}

func TestGuardServiceDefaultsOffAndKeepsClientIP(t *testing.T) {
	ctx := context.Background()
	s, h := newService(t)
	g, err := s.Guard(ctx)
	if err != nil || g.Enabled || g.MaxRetry != 5 || g.FindTime != 600 || g.BanTime != 600 {
		t.Fatalf("по умолчанию выключена: %+v %v", g, err)
	}
	if _, err := s.SetGuard(ctx, GuardSettings{Enabled: true, MaxRetry: 0, FindTime: 600, BanTime: 600}, "192.168.5.9"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if h.count(OpGuardApply) != 0 {
		t.Fatal("helper вызван при неверных значениях")
	}
	g, err = s.SetGuard(ctx, GuardSettings{Enabled: true, MaxRetry: 3, FindTime: 300, BanTime: 900, Ignore: []string{"10.0.0.0/8"}}, "192.168.5.9")
	if err != nil {
		t.Fatal(err)
	}
	var p GuardParams
	jsonOf(t, h.raw[OpGuardApply][0], &p)
	if !p.Enabled || len(p.Ignore) != 2 || p.Ignore[1] != "192.168.5.9" {
		t.Fatalf("адрес администратора должен попасть в исключения: %+v", p)
	}
	if got, _ := s.Guard(ctx); !got.Enabled || got.MaxRetry != 3 || len(got.Ignore) != 2 {
		t.Fatalf("%+v", got)
	}
	// отказ helper — настройки не сохраняются
	s2, h2 := newService(t)
	h2.fail = map[string]error{OpGuardApply: errors.New("boom")}
	if _, err := s2.SetGuard(ctx, GuardSettings{Enabled: true, MaxRetry: 3, FindTime: 300, BanTime: 900}, ""); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if got, _ := s2.Guard(ctx); got.Enabled {
		t.Fatal("сохранено при отказе")
	}
	if err := s.Unban(ctx, "bad ip"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestMigrateOSAppliesGuardAndHomes(t *testing.T) {
	ctx := context.Background()
	s, h := newService(t)
	if _, err := s.CreateUser(ctx, "ann", "password-1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MigrateOS(ctx); err != nil {
		t.Fatal(err)
	}
	var p GuardParams
	jsonOf(t, h.raw[OpGuardApply][0], &p)
	var hp0 HomesParams
	jsonOf(t, h.raw[OpHomesApply][0], &hp0)
	if p.Enabled || h.count(OpHomesApply) != 1 || hp0.Enabled {
		t.Fatalf("после старта защита выключена, дома нет: %+v %+v", p, hp0)
	}
	s.SetHomes(ctx, true, "p1")
	n := h.count(OpHomesApply)
	if _, err := s.MigrateOS(ctx); err != nil || h.count(OpHomesApply) != n+1 {
		t.Fatal(err, h.ops)
	}
	// выключенная служба тоже сверяется: учётки, не успевшие потерять дом, догоняются
	s.SetHomes(ctx, false, "")
	n = h.count(OpHomesApply)
	if _, err := s.MigrateOS(ctx); err != nil || h.count(OpHomesApply) != n+1 {
		t.Fatal(err, h.ops)
	}
	var rp HomesParams
	jsonOf(t, h.raw[OpHomesApply][len(h.raw[OpHomesApply])-1], &rp)
	if rp.Enabled || len(rp.Users) != 1 {
		t.Fatalf("%+v", rp)
	}
}

func TestHomesApplyBusyUserIsWarningNotError(t *testing.T) {
	r := homesRunner()
	r.reply["/usr/sbin/usermod --home"] = CmdResult{ExitCode: 8, Stderr: []byte("user ann is currently used by process 1")}
	res, err := sysExec(t, r).HomesApply(context.Background(), HomesParams{Enabled: true, Pool: "p1", Users: []string{"ann"}})
	if err != nil || len(res.Warnings) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	r.reply["/usr/sbin/usermod --home"] = CmdResult{ExitCode: 6}
	if _, err := sysExec(t, r).HomesApply(context.Background(), HomesParams{Enabled: true, Pool: "p1", Users: []string{"ann"}}); err == nil {
		t.Fatal("другой код usermod — ошибка")
	}
}

func TestSharesApplyWarnsAboutStockHomes(t *testing.T) {
	e := newExec(t, &fakeRunner{})
	e.SmbConf = filepath.Join(t.TempDir(), "smb.conf")
	os.WriteFile(e.SmbConf, []byte("[global]\ninclude = "+e.IncludeFile+"\n[homes]\n\tread only = yes\n"), 0o644)
	res, err := e.SharesApply(context.Background(), SharesParams{Homes: "p1"})
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "[homes]") {
		t.Fatalf("%v %+v", err, res.Warnings)
	}
	if res, _ = e.SharesApply(context.Background(), SharesParams{}); len(res.Warnings) != 0 {
		t.Fatalf("без службы предупреждения нет: %v", res.Warnings)
	}
	os.WriteFile(e.SmbConf, []byte("[global]\ninclude = "+e.IncludeFile+"\n;[homes]\n"), 0o644)
	if res, _ = e.SharesApply(context.Background(), SharesParams{Homes: "p1"}); len(res.Warnings) != 0 {
		t.Fatalf("закомментированный [homes]: %v", res.Warnings)
	}
}

// Выключение и перезагрузка — только systemctl --no-block, без лишнего.
func TestPower(t *testing.T) {
	ctx := context.Background()
	for _, a := range []string{"poweroff", "reboot"} {
		r := &fakeRunner{}
		e := sysExec(t, r)
		if _, err := e.Power(ctx, PowerParams{Action: a}); err != nil {
			t.Fatal(err)
		}
		if len(r.calls) != 1 || !r.has("/usr/bin/systemctl --no-block "+a) {
			t.Fatalf("%s: %+v", a, r.calls)
		}
	}
	r := &fakeRunner{reply: map[string]CmdResult{"/usr/bin/systemctl": {ExitCode: 1, Stderr: []byte("denied")}}}
	if _, err := sysExec(t, r).Power(ctx, PowerParams{Action: "reboot"}); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("ошибка systemctl не передана: %v", err)
	}
	for _, bad := range []string{"", "halt", "reboot --force", "kexec"} {
		if (PowerParams{Action: bad}).Validate() == nil {
			t.Errorf("%q принят", bad)
		}
	}
}
