package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/disks"
)

// shortDir — каталог для сокета: путь Unix-сокета ограничен ~108 байтами,
// а t.TempDir() бывает длиннее.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "dnh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func fakeDeps(t *testing.T) Deps {
	t.Helper()
	root := t.TempDir()
	write := func(rel, s string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("proc/sys/kernel/hostname", "nas-test\n")
	write("proc/sys/kernel/osrelease", "6.12.0-test\n")
	write("proc/cpuinfo", "processor\t: 0\nmodel name\t: Test CPU 3000\n\nprocessor\t: 1\nmodel name\t: Test CPU 3000\n")
	write("proc/meminfo", "MemTotal:        2048 kB\nMemFree:  1 kB\nMemAvailable:    1024 kB\n")
	write("proc/uptime", "123.45 200.00\n")
	write("proc/mountinfo", "")
	write("proc/swaps", "Filename\tType\tSize\tUsed\tPriority\n")
	write("etc/os-release", "NAME=\"Debian\"\nPRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\n")
	os.MkdirAll(filepath.Join(root, "sys/class/block"), 0o755)
	os.MkdirAll(filepath.Join(root, "sys/dev/block"), 0o755)
	return Deps{
		Disks: disks.Paths{
			SysClassBlock: filepath.Join(root, "sys/class/block"),
			SysDevBlock:   filepath.Join(root, "sys/dev/block"),
			SysFsBtrfs:    filepath.Join(root, "sys/fs/btrfs"),
			Mountinfo:     filepath.Join(root, "proc/mountinfo"),
			Swaps:         filepath.Join(root, "proc/swaps"),
			Stat:          func(string) (disks.FileStat, error) { return disks.FileStat{}, errors.New("нет") },
		},
		System: SystemPaths{
			Proc:      filepath.Join(root, "proc"),
			OSRelease: filepath.Join(root, "etc/os-release"),
			DTModel:   filepath.Join(root, "nonexistent"),
		},
		Runner: &Runner{},
	}
}

// testParams — операция с параметрами для проверки строгого разбора.
type testParams struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

func (p testParams) Validate() error {
	if p.Name == "" {
		return errors.New("name обязателен")
	}
	if p.N < 1 || p.N > 10 {
		return errors.New("n должен быть 1..10")
	}
	return nil
}

func testRegistry(t *testing.T) *Registry {
	d := fakeDeps(t)
	base := DefaultRegistry(d)
	ops := []Op{
		Define("test.echo", time.Second, func(_ context.Context, p testParams) (any, error) { return p, nil }),
		Define("test.slow", 50*time.Millisecond, func(ctx context.Context, _ NoParams) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}),
	}
	for _, n := range base.Names() {
		ops = append(ops, base.ops[n])
	}
	return NewRegistry(ops...)
}

func TestDefaultRegistryIsFixed(t *testing.T) {
	got := strings.Join(DefaultRegistry(fakeDeps(t)).Names(), ",")
	if got != "disks.observe,system.info" {
		t.Fatalf("перечень операций изменился: %s — обновите тесты и документацию", got)
	}
}

func TestRegistryExecute(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	cases := []struct {
		name, op, params, code string
	}{
		{"неизвестная операция", "shell.exec", `{"cmd":"rm -rf /"}`, CodeUnknownOp},
		{"пустое имя", "", ``, CodeUnknownOp},
		{"лишнее поле без параметров", "system.info", `{"verbose":true}`, CodeInvalidParams},
		{"params не объект", "system.info", `[]`, CodeInvalidParams},
		{"params строка", "disks.observe", `"x"`, CodeInvalidParams},
		{"лишнее поле", "test.echo", `{"name":"a","n":1,"extra":1}`, CodeInvalidParams},
		{"неверный тип", "test.echo", `{"name":"a","n":"1"}`, CodeInvalidParams},
		{"не прошла Validate", "test.echo", `{"name":"a","n":11}`, CodeInvalidParams},
		{"обязательное поле", "test.echo", `{"n":1}`, CodeInvalidParams},
		{"таймаут операции", "test.slow", ``, CodeTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := r.Execute(ctx, c.op, json.RawMessage(c.params))
			if resp.OK || resp.Error == nil || resp.Error.Code != c.code {
				t.Fatalf("ожидался код %s, ответ %+v", c.code, resp)
			}
		})
	}
	for _, params := range []string{``, `null`, `{}`, ` {} `} {
		if resp := r.Execute(ctx, "system.info", json.RawMessage(params)); !resp.OK {
			t.Errorf("system.info с params %q: %+v", params, resp.Error)
		}
	}
	resp := r.Execute(ctx, "test.echo", json.RawMessage(`{"name":"a","n":3}`))
	if !resp.OK || string(resp.Result) != `{"name":"a","n":3}` {
		t.Errorf("test.echo: %+v", resp)
	}
}

func TestDefineRejectsBadNames(t *testing.T) {
	for _, name := range []string{"exec", "Disks.observe", "a..b", "a.b;rm"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("имя %q должно отклоняться", name)
				}
			}()
			Define(name, time.Second, func(context.Context, NoParams) (any, error) { return nil, nil })
		}()
	}
}

func TestSystemInfo(t *testing.T) {
	info := SystemInfo(fakeDeps(t).System)
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	if str(info.Hostname) != "nas-test" || str(info.Kernel) != "6.12.0-test" ||
		str(info.OS) != "Debian GNU/Linux 13 (trixie)" || str(info.CPU.Model) != "Test CPU 3000" {
		t.Errorf("сведения: %+v", info)
	}
	if info.CPU.Count != 2 || info.Arch != runtime.GOARCH || info.Board != nil {
		t.Errorf("cpu/arch/board: %+v", info)
	}
	if info.Memory.TotalBytes == nil || *info.Memory.TotalBytes != 2048*1024 ||
		info.Memory.AvailableBytes == nil || *info.Memory.AvailableBytes != 1024*1024 {
		t.Errorf("память: %+v", info.Memory)
	}
	if info.UptimeSeconds == nil || *info.UptimeSeconds != 123.45 {
		t.Errorf("uptime: %v", info.UptimeSeconds)
	}
	// Источники недоступны — значения неизвестны, а не нули.
	empty := SystemInfo(SystemPaths{Proc: t.TempDir(), OSRelease: "/nonexistent"})
	if empty.Hostname != nil || empty.Memory.TotalBytes != nil || empty.CPU.Count != runtime.NumCPU() {
		t.Errorf("пустые источники: %+v", empty)
	}
}

func TestARMCPUInfo(t *testing.T) {
	model, n := parseCPUInfo([]byte("processor\t: 0\nBogoMIPS\t: 108\n\nprocessor\t: 1\n\nHardware\t: BCM2835\nModel\t: Raspberry Pi 4 Model B Rev 1.4\n"))
	if n != 2 || model == nil || *model != "BCM2835" {
		t.Errorf("ARM cpuinfo: %v %d", model, n)
	}
}

// --- сервер ---

type testServer struct {
	sock   string
	cancel context.CancelFunc
	done   chan struct{} // закрывается после возврата Serve
}

func startServer(t *testing.T, s *Server) *testServer {
	t.Helper()
	sock := filepath.Join(shortDir(t), "h.sock")
	ln, err := Listen(sock, -1)
	if err != nil {
		t.Fatal(err)
	}
	if s.Logger == nil {
		s.Logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ts := &testServer{sock: sock, cancel: cancel, done: make(chan struct{})}
	go func() {
		if err := s.Serve(ctx, ln); err != nil {
			t.Errorf("Serve: %v", err)
		}
		close(ts.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-ts.done:
		case <-time.After(5 * time.Second):
			t.Error("сервер не остановился")
		}
	})
	return ts
}

func fakePeer(uid int) PeerCredFunc {
	return func(*net.UnixConn) (Cred, error) { return Cred{PID: 42, UID: uid, GID: uid}, nil }
}

// raw отправляет строки и читает по одной строке ответа на каждую.
func raw(t *testing.T, sock string, lines ...string) []Response {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	var out []Response
	for _, l := range lines {
		if _, err := conn.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
		resp, err := readLine(r, MaxResponseBytes)
		if err != nil {
			t.Fatalf("ответ на %q: %v", l, err)
		}
		var rr Response
		if err := json.Unmarshal(resp, &rr); err != nil {
			t.Fatal(err)
		}
		out = append(out, rr)
	}
	return out
}

func TestServerEnvelope(t *testing.T) {
	ts := startServer(t, &Server{Registry: testRegistry(t), AllowedUID: 1000, PeerCred: fakePeer(1000)})
	resps := raw(t, ts.sock,
		`{"v":1,"id":"a","op":"system.info"}`,
		`{"v":1,"id":"b","op":"system.info","params":{},"sudo":true}`,
		`{"v":2,"op":"system.info"}`,
		`не json`,
		`{"v":1,"op":"system.info"} {"v":1}`,
		`{"v":1,"id":"c","op":"shell.exec","params":{"cmd":"id"}}`,
		`{"v":1,"id":"d","op":"disks.observe"}`,
	)
	want := []struct {
		ok   bool
		code string
		id   string
	}{
		{true, "", "a"},
		{false, CodeBadRequest, ""},
		{false, CodeBadRequest, ""},
		{false, CodeBadRequest, ""},
		{false, CodeBadRequest, ""},
		{false, CodeUnknownOp, "c"},
		{true, "", "d"},
	}
	for i, w := range want {
		r := resps[i]
		if r.V != ProtocolVersion || r.OK != w.ok || r.ID != w.id || (!w.ok && (r.Error == nil || r.Error.Code != w.code)) {
			t.Errorf("запрос %d: %+v (err %+v), ожидалось %+v", i, r, r.Error, w)
		}
	}
	var snap disks.Snapshot
	if err := json.Unmarshal(resps[6].Result, &snap); err != nil || snap.Disks == nil {
		t.Errorf("disks.observe: %s, %v", resps[6].Result, err)
	}
}

func TestServerTooLarge(t *testing.T) {
	ts := startServer(t, &Server{Registry: testRegistry(t), AllowedUID: 1000, PeerCred: fakePeer(1000)})
	big := `{"v":1,"op":"test.echo","params":{"name":"` + strings.Repeat("x", MaxRequestBytes) + `","n":1}}`
	resp := raw(t, ts.sock, big)
	if resp[0].OK || resp[0].Error.Code != CodeTooLarge {
		t.Fatalf("ожидался too_large: %+v", resp[0])
	}
}

func TestServerRejectsForeignUID(t *testing.T) {
	for name, peer := range map[string]PeerCredFunc{
		"чужой uid": fakePeer(0),
		"ошибка SO_PEERCRED": func(*net.UnixConn) (Cred, error) {
			return Cred{}, errors.New("сбой")
		},
	} {
		t.Run(name, func(t *testing.T) {
			ts := startServer(t, &Server{Registry: testRegistry(t), AllowedUID: 1000, PeerCred: peer})
			c := &Client{Socket: ts.sock, Timeout: 5 * time.Second}
			err := c.Call(context.Background(), "system.info", nil, nil)
			var re *RemoteError
			if !errors.As(err, &re) || re.Code != CodeForbidden {
				t.Fatalf("ожидался forbidden, получено %v", err)
			}
		})
	}
}

// TestRealPeerCred — настоящий SO_PEERCRED: свой uid допускается, чужой — нет.
func TestRealPeerCred(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SO_PEERCRED только в Linux")
	}
	ts := startServer(t, &Server{Registry: testRegistry(t), AllowedUID: os.Getuid()})
	c := &Client{Socket: ts.sock, Timeout: 5 * time.Second}
	var info SystemInfoResult
	if err := c.Call(context.Background(), "system.info", nil, &info); err != nil {
		t.Fatal(err)
	}
	if info.Hostname == nil || *info.Hostname != "nas-test" {
		t.Errorf("system.info: %+v", info)
	}

	ts2 := startServer(t, &Server{Registry: testRegistry(t), AllowedUID: os.Getuid() + 1})
	err := (&Client{Socket: ts2.sock}).Call(context.Background(), "system.info", nil, nil)
	var re *RemoteError
	if !errors.As(err, &re) || re.Code != CodeForbidden {
		t.Fatalf("чужой uid: %v", err)
	}
}

func TestClientErrors(t *testing.T) {
	c := &Client{Socket: filepath.Join(shortDir(t), "нет.sock"), Timeout: time.Second}
	if err := c.Call(context.Background(), "system.info", nil, nil); !errors.Is(err, ErrUnavailable) {
		t.Errorf("нет сокета: %v", err)
	}
	ts := startServer(t, &Server{Registry: testRegistry(t), AllowedUID: 7, PeerCred: fakePeer(7)})
	c = &Client{Socket: ts.sock, Timeout: 5 * time.Second}
	err := c.Call(context.Background(), "test.echo", map[string]any{"name": "a", "n": 1, "rm": true}, nil)
	var re *RemoteError
	if !errors.As(err, &re) || re.Code != CodeInvalidParams {
		t.Errorf("лишнее поле через клиент: %v", err)
	}
	var echo testParams
	if err := c.Call(context.Background(), "test.echo", testParams{Name: "b", N: 2}, &echo); err != nil || echo.Name != "b" {
		t.Errorf("test.echo: %+v, %v", echo, err)
	}
}

func TestListenPermissions(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "h.sock")
	ln, err := Listen(sock, os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(sock)
	if st.Mode().Perm() != 0o660 {
		t.Errorf("права сокета %v", st.Mode().Perm())
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	// Устаревший сокет заменяется.
	ln, err = Listen(sock, -1)
	if err != nil {
		t.Fatalf("устаревший сокет: %v", err)
	}
	ln.Close()
	// Обычный файл на месте сокета не удаляется.
	plain := filepath.Join(dir, "file")
	os.WriteFile(plain, []byte("data"), 0o600)
	if _, err := Listen(plain, -1); err == nil {
		t.Fatal("обычный файл не должен заменяться сокетом")
	}
	if b, _ := os.ReadFile(plain); string(b) != "data" {
		t.Error("файл изменён")
	}
}

func TestServerShutdownClosesIdleConn(t *testing.T) {
	ts := startServer(t, &Server{Registry: testRegistry(t), AllowedUID: 1, PeerCred: fakePeer(1), IdleTimeout: time.Minute})
	conn, err := net.Dial("unix", ts.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ts.cancel()
	select {
	case <-ts.done:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve не завершился при открытом простаивающем соединении")
	}
}

// --- Runner ---

func TestRunnerRejectsUnsafePaths(t *testing.T) {
	r := &Runner{}
	for _, p := range []string{"sh", "bin/sh", "/bin/../bin/sh", "/bin//sh", ""} {
		if _, err := r.Run(context.Background(), time.Second, p, "-c", "true"); err == nil {
			t.Errorf("путь %q должен отклоняться", p)
		}
	}
	if _, err := r.Run(context.Background(), 0, "/bin/true"); err == nil {
		t.Error("нулевой таймаут должен отклоняться")
	}
	if _, err := r.Run(context.Background(), time.Second, "/bin/echo", "a\x00b"); err == nil {
		t.Error("NUL в аргументе должен отклоняться")
	}
}

func TestRunnerArgsNotShell(t *testing.T) {
	res, err := (&Runner{}).Run(context.Background(), 5*time.Second, "/bin/echo", "$HOME", "; id", "`id`")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Stdout); got != "$HOME ; id `id`\n" || res.ExitCode != 0 {
		t.Errorf("аргументы интерпретированы: %q, код %d", got, res.ExitCode)
	}
}

func TestRunnerExitCodeAndEnv(t *testing.T) {
	r := &Runner{}
	res, err := r.Run(context.Background(), 5*time.Second, "/bin/sh", "-c", "echo $LC_ALL; echo err >&2; exit 3")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || string(res.Stdout) != "C\n" || string(res.Stderr) != "err\n" {
		t.Errorf("результат: %+v", res)
	}
	if _, err := r.Run(context.Background(), time.Second, "/nonexistent/prog"); err == nil {
		t.Error("отсутствующая программа должна давать ошибку")
	}
}

func TestRunnerTimeoutKillsGroup(t *testing.T) {
	start := time.Now()
	// Дочерний sleep держит stdout открытым: без убийства группы Run ждал бы его.
	res, err := (&Runner{}).Run(context.Background(), 200*time.Millisecond, "/bin/sh", "-c", "sleep 30 & sleep 30")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("ожидалась ErrTimeout, получено %v (%+v)", err, res)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("остановка заняла %s — группа процессов не убита", d)
	}
}

func TestRunnerTruncatesOutput(t *testing.T) {
	res, err := (&Runner{MaxOutput: 10}).Run(context.Background(), 5*time.Second, "/bin/sh", "-c", "printf '%050d' 0")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != 10 || !res.Truncated {
		t.Errorf("обрезка: %d байт, truncated=%v", len(res.Stdout), res.Truncated)
	}
}

// Новые операции есть в реестре; чтение не помечено изменяющим.
func TestRegistryHasStage2Ops(t *testing.T) {
	reg := DefaultRegistry(DefaultDeps(DefaultConfig()))
	for op, mut := range map[string]bool{
		"system.identity.get": false, "system.hostname.set": true, "system.timezone.set": true,
		"nas.homes.apply": true, "ssh.guard.apply": true, "ssh.guard.status": false, "ssh.guard.unban": true,
	} {
		found := false
		for _, n := range reg.Names() {
			found = found || n == op
		}
		if !found || reg.IsMutating(op) != mut {
			t.Errorf("%s: в реестре %v, mutating %v (ожидалось %v)", op, found, reg.IsMutating(op), mut)
		}
	}
	// строгий разбор: неизвестное поле и неверные значения отклоняются
	for op, params := range map[string]string{
		"system.hostname.set": `{"hostname":"a b"}`, "ssh.guard.apply": `{"enabled":true,"maxretry":0,"findtime":600,"bantime":600}`,
		"ssh.guard.unban": `{"ip":"1.2.3.4","x":1}`, "nas.homes.apply": `{"enabled":true,"pool":"../x"}`,
	} {
		if r := reg.Execute(context.Background(), op, []byte(params)); r.OK || r.Error == nil || r.Error.Code != CodeInvalidParams {
			t.Errorf("%s %s: %+v", op, params, r)
		}
	}
}

// Журнал аудита изменяющих операций не содержит секретов.
func TestAuditRequestRedactsSecrets(t *testing.T) {
	line := []byte(`{"v":1,"id":"a","op":"nas.user.password","params":{"name":"ann","password":"S3cr3t-pw!","nested":[{"new_password":"x2"}],"api_token":"t","psk":"w"}}`)
	got := auditRequest(line)
	for _, s := range []string{"S3cr3t-pw!", `"x2"`, `"t"`, `"w"`} {
		if strings.Contains(got, s) {
			t.Fatalf("секрет %s в журнале: %s", s, got)
		}
	}
	if !strings.Contains(got, `"name":"ann"`) || !strings.Contains(got, `"op":"nas.user.password"`) || strings.Count(got, "***") != 4 {
		t.Fatalf("аудит потерял поля: %s", got)
	}
	if bad := auditRequest([]byte(`{"op":"nas.user.password","params":{"password":"S3cr3t`)); strings.Contains(bad, "S3cr3t") {
		t.Fatalf("битый запрос в журнале: %s", bad)
	}
}

// apps.project.write — переменные и compose не в журнал аудита.
func TestAuditRedactsAppProject(t *testing.T) {
	line := []byte(`{"v":1,"id":"a","op":"apps.project.write","params":{"id":"web","env":"PASS=S3cr3t","user_compose":"x: S3cr3t","normalized":"{\"P\":\"S3cr3t\"}","sha256":"ab"}}`)
	got := auditRequest(line)
	if strings.Contains(got, "S3cr3t") || !strings.Contains(got, `"id":"web"`) || !strings.Contains(got, `"sha256":"ab"`) {
		t.Fatalf("%s", got)
	}
}

// Нет утилиты — понятная ошибка с пакетом, а не fork/exec.
func TestRunnerMissingTool(t *testing.T) {
	r := &Runner{}
	_, err := r.Run(context.Background(), time.Second, "/nonexistent-dn060/sgdisk", "--zap-all")
	if !errors.Is(err, ErrToolMissing) || !strings.Contains(err.Error(), "sgdisk (пакет gdisk)") {
		t.Fatalf("%v", err)
	}
	_, err = r.Run(context.Background(), time.Second, "/nonexistent-dn060/foo")
	if !errors.Is(err, ErrToolMissing) || !strings.Contains(err.Error(), "/nonexistent-dn060/foo") {
		t.Fatalf("%v", err)
	}
}
