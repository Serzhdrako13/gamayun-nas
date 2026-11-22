package nas

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- Учётка ОС, SSH, sudo ----------

func TestLowUIDRefusedEvenWithOurGroup(t *testing.T) {
	r := &fakeRunner{reply: map[string]CmdResult{"/usr/bin/id -gn": {Stdout: []byte(UnixGroup + "\n")}, "/usr/bin/id -u": {Stdout: []byte("999\n")}}}
	e := newExec(t, r)
	ctx := context.Background()
	if _, err := e.UserAccess(ctx, AccessParams{Name: "svc", SSH: true}); !errors.Is(err, ErrRefused) {
		t.Fatalf("access: %v", err)
	}
	if _, err := e.UserPassword(ctx, PasswordParams{Name: "svc", Password: "password1"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("password: %v", err)
	}
	if _, err := e.UserDelete(ctx, UserParams{Name: "svc"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("delete: %v", err)
	}
	if r.has("/usr/sbin/usermod") || r.has("/usr/sbin/chpasswd") || r.has("/usr/sbin/userdel") {
		t.Fatalf("%+v", r.calls)
	}
}

func TestOSPasswordViaChpasswdStdin(t *testing.T) {
	r := &fakeRunner{reply: ours()}
	res, err := newExec(t, r).UserPassword(context.Background(), PasswordParams{Name: "ann", Password: "s3cret:pass"})
	if err != nil {
		t.Fatal(err)
	}
	var ch call
	for _, c := range r.calls {
		if c.path == "/usr/sbin/chpasswd" {
			ch = c
		}
	}
	if ch.stdin != "ann:s3cret:pass\n" || len(ch.args) != 0 {
		t.Fatalf("chpasswd: %+v", ch)
	}
	for _, c := range res.Commands {
		if strings.Contains(c, "s3cret") {
			t.Fatalf("пароль в командах: %s", c)
		}
	}
}

func accessRunner(shell, groups string) *fakeRunner {
	m := ours()
	m["/usr/bin/getent passwd ann"] = CmdResult{Stdout: []byte("ann:x:1001:1000:gamayun-nas:/nonexistent:" + shell + "\n")}
	m["/usr/bin/id -nG"] = CmdResult{Stdout: []byte(groups + "\n")}
	return &fakeRunner{reply: m}
}

func TestUserAccessGrantRevokeIdempotent(t *testing.T) {
	ctx := context.Background()
	r := accessRunner("/usr/sbin/nologin", UnixGroup)
	res, err := newExec(t, r).UserAccess(ctx, AccessParams{Name: "ann", SSH: true, Sudo: true})
	if err != nil || !res.Changed {
		t.Fatal(err, res)
	}
	for _, want := range []string{"/usr/sbin/usermod --shell /bin/bash -- ann",
		"/usr/sbin/usermod --append --groups " + SSHGroup + " -- ann", "/usr/sbin/usermod --append --groups " + SudoGroup + " -- ann"} {
		if !r.has(want) {
			t.Fatalf("нет %q: %+v", want, r.calls)
		}
	}
	// состояние уже совпадает — ничего не меняется
	r = accessRunner("/bin/bash", UnixGroup+" "+SSHGroup+" "+SudoGroup)
	res, err = newExec(t, r).UserAccess(ctx, AccessParams{Name: "ann", SSH: true, Sudo: true})
	if err != nil || res.Changed || r.has("/usr/sbin/usermod") || r.has("/usr/bin/gpasswd") {
		t.Fatalf("повтор изменил: %v %+v", err, r.calls)
	}
	// отзыв: nologin, выход из групп, прочие группы не трогаются
	r = accessRunner("/bin/bash", UnixGroup+" "+SSHGroup+" "+SudoGroup+" audio")
	if _, err := newExec(t, r).UserAccess(ctx, AccessParams{Name: "ann"}); err != nil {
		t.Fatal(err)
	}
	if !r.has("/usr/sbin/usermod --shell /usr/sbin/nologin -- ann") || !r.has("/usr/bin/gpasswd --delete ann "+SSHGroup) ||
		!r.has("/usr/bin/gpasswd --delete ann "+SudoGroup) || r.has("/usr/bin/gpasswd --delete ann audio") {
		t.Fatalf("%+v", r.calls)
	}
}

func setupExec(t *testing.T, r *fakeRunner) *Executor {
	e := newExec(t, r)
	d := t.TempDir()
	e.SshdDropIn, e.SudoersDropIn = filepath.Join(d, "50-gamayun-nas.conf"), filepath.Join(d, "gamayun-nas")
	e.PasswdFile, e.GroupFile = filepath.Join(d, "passwd"), filepath.Join(d, "group")
	os.WriteFile(e.PasswdFile, []byte("root:x:0:0:root:/root:/bin/bash\nsshd:x:110:65534::/run/sshd:/usr/sbin/nologin\n"+
		"daemon:x:1:1::/:/bin/sh\ndebian:x:1000:1000::/home/debian:/bin/bash\nann:x:1001:1001::/:/bin/bash\nbob:x:1002:1002::/:/usr/sbin/nologin\n"), 0o644)
	os.WriteFile(e.GroupFile, []byte("root:x:0:\ndaemon:x:1:\ndebian:x:1000:\n"+UnixGroup+":x:1001:\nbob:x:1002:\n"), 0o644)
	return e
}

func TestAccessSetupSudoersAndIdempotent(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{}
	e := setupExec(t, r)
	res, err := e.AccessSetup(ctx, SetupParams{})
	if err != nil || !res.Changed {
		t.Fatal(err, res)
	}
	b, _ := os.ReadFile(e.SudoersDropIn)
	if !strings.Contains(string(b), "%"+SudoGroup+" ALL=(ALL:ALL) ALL") {
		t.Fatalf("sudoers: %s", b)
	}
	if st, _ := os.Stat(e.SudoersDropIn); st.Mode().Perm() != 0o440 {
		t.Fatalf("права sudoers %v", st.Mode())
	}
	// drop-in sshd пишет только SSHApply.
	if _, err := os.Stat(e.SshdDropIn); !os.IsNotExist(err) || r.has("/usr/sbin/sshd") || r.has("/usr/bin/systemctl") {
		t.Fatalf("AccessSetup тронул sshd: %+v", r.calls)
	}
	r2 := &fakeRunner{}
	e.Runner = r2
	if res, err := e.AccessSetup(ctx, SetupParams{}); err != nil || res.Changed || r2.has("/usr/sbin/visudo") {
		t.Fatalf("повтор: %v %+v %+v", err, res, r2.calls)
	}
	// visudo отклонил — файл sudoers не появляется
	r = &fakeRunner{reply: map[string]CmdResult{"/usr/sbin/visudo": {ExitCode: 1}}}
	e = setupExec(t, r)
	if _, err := e.AccessSetup(ctx, SetupParams{}); err == nil {
		t.Fatal("ожидалась ошибка visudo")
	}
	if _, err := os.Stat(e.SudoersDropIn); !os.IsNotExist(err) {
		t.Fatal("sudoers записан без проверки")
	}
}

func lastAccess(h *fakeHelper, name string) (AccessParams, bool) {
	for i := len(h.acc) - 1; i >= 0; i-- {
		if h.acc[i].Name == name {
			return h.acc[i], true
		}
	}
	return AccessParams{}, false
}

func TestServiceSSHModel(t *testing.T) {
	s, h, _, _ := newUnified(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "admin", "Password-123", true); err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser(ctx, "ann", "Password-123", false)
	if err != nil || u.SSH {
		t.Fatal(err, u)
	}
	if a, _ := lastAccess(h, "admin"); !a.SSH || !a.Sudo {
		t.Fatalf("admin: %+v", a)
	}
	if a, _ := lastAccess(h, "ann"); a.SSH || a.Sudo {
		t.Fatalf("ann: %+v", a)
	}
	if err := s.SetSSH(ctx, "ann", true); err != nil {
		t.Fatal(err)
	}
	if a, _ := lastAccess(h, "ann"); !a.SSH || a.Sudo {
		t.Fatalf("ann ssh: %+v", a)
	}
	// последний администратор не теряет SSH
	if err := s.SetSSH(ctx, "admin", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("последний админ: %v", err)
	}
	// новый администратор получает SSH; тогда первый может его выключить
	if err := s.SetMembers(ctx, GroupAdmins, []string{"admin", "ann"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSSH(ctx, "ann", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSSH(ctx, "admin", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("ann без SSH — admin последний: %v", err)
	}
	if err := s.SetMembers(ctx, GroupAdmins, []string{"ann"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("в администраторах не осталось SSH: %v", err)
	}
	// отключённый — без SSH и sudo
	if err := s.SetSSH(ctx, "ann", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnabled(ctx, "ann", false); err != nil {
		t.Fatal(err)
	}
	if a, _ := lastAccess(h, "ann"); a.SSH || a.Sudo {
		t.Fatalf("отключённый: %+v", a)
	}
	us, _ := s.ListUsers(ctx)
	if !us[0].SSH || !us[1].SSH {
		t.Fatalf("флаг ssh в модели: %+v", us)
	}
}

func TestMigrateOSAppliesAll(t *testing.T) {
	s, h, _, _ := newUnified(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "admin", "Password-123", true); err != nil {
		t.Fatal(err)
	}
	h.ops, h.acc = nil, nil
	if _, err := s.MigrateOS(ctx); err != nil {
		t.Fatal(err)
	}
	if h.count(OpAccessSetup) != 1 || h.count(OpUserEnsure) != 1 || h.count(OpUserAccess) != 1 {
		t.Fatalf("%v", h.ops)
	}
	if a, _ := lastAccess(h, "admin"); !a.SSH || !a.Sudo {
		t.Fatalf("%+v", a)
	}
}

func TestDeleteUserKeepsModelOnHelperFailure(t *testing.T) {
	s, h, _, _ := newUnified(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "admin", "Password-123", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "ann", "Password-123", false); err != nil {
		t.Fatal(err)
	}
	h.fail = map[string]error{OpUserDelete: errors.New("busy")}
	if err := s.DeleteUser(ctx, "ann"); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if err := s.userExists(ctx, "ann"); err != nil {
		t.Fatalf("модель изменена при отказе helper: %v", err)
	}
	h.fail = nil
	if err := s.DeleteUser(ctx, "ann"); err != nil || s.userExists(ctx, "ann") == nil {
		t.Fatal(err)
	}
}

// Отключённый — запись Samba отключена, сессии smbd завершены;
// включённый — запись включена, сессии не трогаются.
func TestUserAccessSamba(t *testing.T) {
	ctx := context.Background()
	r := accessRunner("/usr/sbin/nologin", UnixGroup)
	r.reply["/usr/bin/smbstatus -p -u ann"] = CmdResult{Stdout: []byte("Samba version 4\nPID     Username     Group\n----\n4242    ann          users   127.0.0.1\n4243    annie        users   x\n")}
	if _, err := newExec(t, r).UserAccess(ctx, AccessParams{Name: "ann", SambaDisabled: true}); err != nil {
		t.Fatal(err)
	}
	if !r.has("/usr/bin/smbpasswd -d -- ann") || !r.has("/usr/bin/smbcontrol 4242 shutdown") || r.has("/usr/bin/smbcontrol 4243") {
		t.Fatalf("%+v", r.calls)
	}
	r = accessRunner("/usr/sbin/nologin", UnixGroup)
	if _, err := newExec(t, r).UserAccess(ctx, AccessParams{Name: "ann"}); err != nil {
		t.Fatal(err)
	}
	if !r.has("/usr/bin/smbpasswd -e -- ann") || r.has("/usr/bin/smbcontrol") {
		t.Fatalf("%+v", r.calls)
	}
}
