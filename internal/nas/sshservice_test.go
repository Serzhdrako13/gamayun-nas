package nas

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// sshExec — исполнитель с поддельным sshd (файл существует) и drop-in во временном каталоге.
func sshExec(t *testing.T, r *fakeRunner, dev ...string) *Executor {
	e := setupExec(t, r)
	e.SSHInstalled = func() bool { return true }
	e.SSHDev = dev
	return e
}

const isActive = "/usr/bin/systemctl is-active --quiet ssh"

func TestSSHApplyDisableStopsServiceRootNo(t *testing.T) {
	r := &fakeRunner{reply: map[string]CmdResult{"/usr/bin/systemctl is-enabled --quiet ssh.socket": {ExitCode: 1}}}
	e := sshExec(t, r)
	res, err := e.SSHApply(context.Background(), SSHParams{})
	if err != nil || !res.Changed {
		t.Fatal(err, res)
	}
	b, _ := os.ReadFile(e.SshdDropIn)
	for _, w := range []string{"PermitRootLogin no\n", "PasswordAuthentication no\n", "AllowGroups " + SSHGroup + "\n", "Match Group " + SSHGroup + "\n\tPasswordAuthentication yes\n"} {
		if !strings.Contains(string(b), w) {
			t.Fatalf("нет %q: %s", w, b)
		}
	}
	if strings.Contains(string(b), "root ") || strings.Contains(string(b), "debian") {
		t.Fatalf("лишние группы: %s", b)
	}
	if !r.has("/usr/bin/systemctl disable --now ssh") || r.has("/usr/bin/systemctl disable --now ssh.socket") || r.has("/usr/bin/pkill") {
		t.Fatalf("%+v", r.calls)
	}
}

func TestSSHApplyEnableIdempotent(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{reply: map[string]CmdResult{isActive: {ExitCode: 3}, "/usr/sbin/sshd -T": {Stdout: []byte("permitrootlogin no\n")}}}
	e := sshExec(t, r)
	if res, err := e.SSHApply(ctx, SSHParams{Enabled: true}); err != nil || !res.Changed || len(res.Warnings) > 0 {
		t.Fatal(err, res)
	}
	if !r.has("/usr/sbin/sshd -t") || !r.has("/usr/bin/systemctl enable --now ssh") {
		t.Fatalf("%+v", r.calls)
	}
	// повтор при работающей службе — ни записи, ни reload, ни enable
	r2 := &fakeRunner{reply: map[string]CmdResult{"/usr/sbin/sshd -T": {Stdout: []byte("permitrootlogin no\n")}}}
	e.Runner = r2
	if res, err := e.SSHApply(ctx, SSHParams{Enabled: true}); err != nil || res.Changed {
		t.Fatal(err, res)
	}
	if r2.has("/usr/bin/systemctl enable") || r2.has("/usr/bin/systemctl reload") || r2.has("/usr/sbin/sshd -t") {
		t.Fatalf("%+v", r2.calls)
	}
	// конфиг не применяется sshd — предупреждение
	r3 := &fakeRunner{reply: map[string]CmdResult{"/usr/sbin/sshd -T": {Stdout: []byte("permitrootlogin yes\n")}}}
	e.Runner = r3
	if res, _ := e.SSHApply(ctx, SSHParams{Enabled: true}); len(res.Warnings) != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestSSHApplyRollbackOnSshdT(t *testing.T) {
	r := &fakeRunner{reply: map[string]CmdResult{"/usr/sbin/sshd -t": {ExitCode: 255, Stderr: []byte("bad")}}}
	e := sshExec(t, r)
	os.WriteFile(e.SshdDropIn, []byte("# old\n"), 0o644)
	if _, err := e.SSHApply(context.Background(), SSHParams{Enabled: true}); err == nil {
		t.Fatal("ожидалась ошибка sshd -t")
	}
	if b, _ := os.ReadFile(e.SshdDropIn); string(b) != "# old\n" {
		t.Fatalf("не восстановлено: %q", b)
	}
	if r.has("/usr/bin/systemctl enable") || r.has("/usr/bin/systemctl reload") || r.has("/usr/bin/systemctl disable") {
		t.Fatalf("служба тронута после ошибки: %+v", r.calls)
	}
}

func TestSSHApplyDisableSocketAndKill(t *testing.T) {
	r := &fakeRunner{reply: map[string]CmdResult{"/usr/bin/pkill": {ExitCode: 1}}}
	e := sshExec(t, r)
	if _, err := e.SSHApply(context.Background(), SSHParams{KillSessions: true}); err != nil {
		t.Fatal(err)
	}
	if !r.has("/usr/bin/systemctl disable --now ssh.socket") || !r.has("/usr/bin/pkill -KILL -G "+UnixGroup+" -x sshd-session") {
		t.Fatalf("%+v", r.calls)
	}
	// ошибка pkill (не «нет процессов») — ошибка операции
	r = &fakeRunner{reply: map[string]CmdResult{"/usr/bin/pkill": {ExitCode: 2}}}
	e.Runner = r
	if _, err := e.SSHApply(context.Background(), SSHParams{KillSessions: true}); err == nil {
		t.Fatal("ожидалась ошибка pkill")
	}
}

func TestSSHApplyDevModeKeepsService(t *testing.T) {
	r := &fakeRunner{reply: map[string]CmdResult{"/usr/sbin/sshd -T": {Stdout: []byte("permitrootlogin without-password\n")}}}
	e := sshExec(t, r, "debian", "root", "nosuch")
	res, err := e.SSHApply(context.Background(), SSHParams{})
	if err != nil || len(res.Warnings) > 0 {
		t.Fatal(err, res)
	}
	b, _ := os.ReadFile(e.SshdDropIn)
	if !strings.Contains(string(b), "PermitRootLogin prohibit-password\n") || !strings.Contains(string(b), "AllowGroups debian root\n") {
		t.Fatalf("%s", b)
	}
	if r.has("/usr/bin/systemctl disable") || !r.has("/usr/bin/systemctl reload ssh") {
		t.Fatalf("служба в режиме разработки: %+v", r.calls)
	}
	// включено — учётки NAS и разработки
	e.Runner = &fakeRunner{reply: r.reply}
	e.SSHApply(context.Background(), SSHParams{Enabled: true})
	if b, _ := os.ReadFile(e.SshdDropIn); !strings.Contains(string(b), "AllowGroups "+SSHGroup+" debian root\n") {
		t.Fatalf("%s", b)
	}
}

func TestSSHApplyNotInstalled(t *testing.T) {
	r := &fakeRunner{}
	e := sshExec(t, r)
	e.SSHInstalled = func() bool { return false }
	if _, err := e.SSHApply(context.Background(), SSHParams{Enabled: true}); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	if res, err := e.SSHApply(context.Background(), SSHParams{}); err != nil || res.Changed || len(r.calls) > 0 {
		t.Fatal(err, res, r.calls)
	}
}

func TestServiceSSHLifecycle(t *testing.T) {
	ctx := context.Background()
	s, h := newService(t)
	h.reply = map[string]string{OpSSHStatus: `{"installed":true,"active":true}`}
	// до мастера: служба не трогается, значение не сохраняется
	if _, err := s.MigrateOS(ctx); err != nil {
		t.Fatal(err)
	}
	if h.count(OpSSHApply) != 0 {
		t.Fatalf("SSH тронут до мастера: %v", h.ops)
	}
	// мастер пройден — выключается и сохраняется
	if err := s.CompleteSetup(ctx); err != nil {
		t.Fatal(err)
	}
	if h.count(OpSSHApply) != 1 || h.raw[OpSSHApply][0] != `{"enabled":false,"kill_sessions":false}` {
		t.Fatalf("%v", h.raw[OpSSHApply])
	}
	if c, _, _, err := s.SSHState(ctx); err != nil || c.Enabled {
		t.Fatal(err, c)
	}
	// старт gamayund — приводит к сохранённому (выключено), хотя служба активна
	s.MigrateOS(ctx)
	if l := h.raw[OpSSHApply]; len(l) != 2 || l[1] != `{"enabled":false,"kill_sessions":false}` {
		t.Fatalf("%v", l)
	}
	// включение из веба сохраняется; сбой helper — прежнее значение
	if err := s.SetSSHService(ctx, true, false); err != nil {
		t.Fatal(err)
	}
	h.fail = map[string]error{OpSSHApply: errors.New("boom")}
	if err := s.SetSSHService(ctx, false, true); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if c, _, _, _ := s.SSHState(ctx); !c.Enabled {
		t.Fatal("сохранено при сбое helper")
	}
}

func TestServiceSSHUpgradeKeepsCurrent(t *testing.T) {
	ctx := context.Background()
	s, h := newService(t)
	s.kvSet(ctx, keySetupDone, "1") // существующая установка без значения SSH
	h.reply = map[string]string{OpSSHStatus: `{"installed":true,"active":true}`}
	if _, err := s.MigrateOS(ctx); err != nil {
		t.Fatal(err)
	}
	if l := h.raw[OpSSHApply]; len(l) != 1 || l[0] != `{"enabled":true,"kill_sessions":false}` {
		t.Fatalf("%v", l)
	}
	if c, _, n, _ := s.SSHState(ctx); !c.Enabled || n != 0 {
		t.Fatal(c, n)
	}
}
