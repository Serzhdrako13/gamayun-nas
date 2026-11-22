package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gamayun-nas/internal/nas"
)

// fakeSys — подмена команд: субтомы, systemctl, mount.
type fakeSys struct {
	t       *testing.T
	mu      sync.Mutex
	cmds    []string
	subvols map[string]bool
	docker  string
	ctr     string
	mp      string // точка пула
	fail    map[string]bool
	exe     *Executor
	mounted bool
	ro      bool
	nested  []string // монтирования внутри пула (overlay контейнеров)
}

func (f *fakeSys) Run(ctx context.Context, timeout time.Duration, stdin []byte, path string, args ...string) (nas.CmdResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := filepath.Base(path) + " " + strings.Join(args, " ")
	f.cmds = append(f.cmds, cmd)
	for k := range f.fail {
		if strings.HasPrefix(cmd, k) {
			return nas.CmdResult{ExitCode: 1, Stderr: []byte("сбой " + k)}, nil
		}
	}
	switch {
	case cmd == "systemctl is-active gamayun-docker.service gamayun-containerd.service":
		return nas.CmdResult{ExitCode: 3, Stdout: []byte(f.docker + "\n" + f.ctr + "\n")}, nil
	case strings.HasPrefix(cmd, "systemctl start"):
		if _, err := os.Stat(f.exe.Paths.flag()); err == nil && f.mounted {
			f.docker, f.ctr = "active", "active"
		}
	case strings.HasPrefix(cmd, "systemctl stop"):
		f.docker, f.ctr = "inactive", "inactive"
	case strings.HasPrefix(cmd, "btrfs subvolume create "):
		p := args[2]
		if err := os.Mkdir(p, 0o755); err != nil {
			return nas.CmdResult{ExitCode: 1, Stderr: []byte(err.Error())}, nil
		}
		f.subvols[p] = true
	case strings.HasPrefix(cmd, "btrfs subvolume delete "):
		p := args[2]
		if err := os.RemoveAll(p); err != nil {
			return nas.CmdResult{ExitCode: 1}, nil
		}
		delete(f.subvols, p)
	case strings.HasPrefix(cmd, "btrfs subvolume snapshot "):
		src, dst := args[len(args)-2], args[len(args)-1]
		if _, err := os.Stat(dst); err == nil {
			return nas.CmdResult{ExitCode: 1, Stderr: []byte("exists")}, nil
		}
		if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
			return nas.CmdResult{ExitCode: 1, Stderr: []byte(err.Error())}, nil
		}
		f.subvols[dst] = true
	case strings.HasPrefix(cmd, "btrfs subvolume list -o "):
		root := args[3]
		var out []string
		for p := range f.subvols {
			if strings.HasPrefix(p, root+"/") {
				rel, _ := filepath.Rel(f.mp, p)
				out = append(out, "ID 300 gen 1 top level 5 path "+rel)
			}
		}
		sort.Strings(out)
		return nas.CmdResult{Stdout: []byte(strings.Join(out, "\n"))}, nil
	case cmd == "containerd config default":
		return nas.CmdResult{Stdout: []byte("version = 3\nroot = \"/var/lib/containerd\"\nstate = \"/run/containerd\"\n\n[grpc]\n  address = \"/run/containerd/containerd.sock\"\n")}, nil
	case strings.HasPrefix(cmd, "umount "):
		if args[0] != f.mp {
			var keep []string
			for _, n := range f.nested {
				if n != args[0] {
					keep = append(keep, n)
				}
			}
			f.nested = keep
		} else {
			f.mounted = false
		}
		f.writeMountinfo()
	case strings.HasPrefix(cmd, "mount -t btrfs"):
		f.mounted, f.ro = true, false
		f.writeMountinfo()
	}
	return nas.CmdResult{}, nil
}

func (f *fakeSys) writeMountinfo() {
	s := "22 1 0:20 / / rw - ext4 /dev/sda1 rw\n"
	if f.mounted {
		o := "rw"
		if f.ro {
			o = "ro"
		}
		s += fmt.Sprintf("36 22 0:32 / %s rw,relatime shared:1 - btrfs %s/mapper/dn_p1-data %s,space_cache=v2\n", f.mp, f.exe.Paths.Dev, o)
		for i, n := range f.nested {
			s += fmt.Sprintf("%d 36 0:%d / %s rw,relatime - overlay overlay rw\n", 60+i, 60+i, n)
		}
	}
	if err := os.WriteFile(f.exe.Paths.Mountinfo, []byte(s), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeSys) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.cmds...)
}

type rig struct {
	e    *Executor
	f    *fakeSys
	root string
	info Info
	evs  string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	p := Paths{Etc: root + "/etc", Run: root + "/run/apps", VarLib: root + "/var/lib", Mountinfo: root + "/mountinfo",
		Dev: root + "/dev", DockerSock: root + "/d.sock", Dockerd: root + "/usr/bin/dockerd", UnitDir: root + "/units", Lock: root + "/versions.lock",
		Systemctl: "/usr/bin/systemctl", Btrfs: "/usr/bin/btrfs", Mount: "/usr/bin/mount", Umount: "/usr/bin/umount", Journalctl: "/usr/bin/journalctl"}
	for _, d := range []string{p.Etc + "/systemd/system", root + "/usr/bin", p.VarLib, root + "/srv/p1", p.Dev + "/dn_p1", p.Dev + "/mapper"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(p.Dev+"/mapper/dn_p1-data", nil, 0o644)
	os.Symlink("../mapper/dn_p1-data", p.Dev+"/dn_p1/data")
	os.MkdirAll(p.UnitDir, 0o755)
	os.WriteFile(p.unit(), []byte("[Service]\n"), 0o644)
	os.WriteFile(p.Lock, []byte("# x\ndocker-static 29.1.0 https://x sum\n"), 0o644)
	os.WriteFile(p.Dockerd, nil, 0o755)
	r := &rig{root: root}
	f := &fakeSys{t: t, subvols: map[string]bool{}, docker: "inactive", ctr: "inactive", mp: root + "/srv/p1", fail: map[string]bool{}, mounted: true}
	e := &Executor{Runner: f, Paths: p, MountRoot: root + "/srv", ReadOnly: func(string) (bool, error) { return f.ro, nil },
		SetNoCOW: func(string) error { return nil },
		Subvol: func(path string) (bool, error) {
			if _, err := os.Stat(path); err != nil {
				return false, err
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.subvols[path], nil
		}}
	f.exe = e
	f.writeMountinfo()
	r.e, r.f = e, f
	r.info = Info{ServerVersion: "29.1.0", DockerRootDir: root + "/srv/p1/.apps/docker"}
	// Подмена Docker: unix-сокет с /info и /events.
	ln, err := net.Listen("unix", p.DockerSock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) { json.NewEncoder(w).Encode(r.info) })
	mux.HandleFunc("/containers/0123456789abcdef/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"State":{"OOMKilled":true}}`))
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("filters") != `{"type":["container"]}` || req.URL.Query().Get("since") == "" {
			http.Error(w, "bad", 400)
			return
		}
		w.Write([]byte(r.evs))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	e.Docker = NewDockerClient(p.DockerSock)
	return r
}

func TestConfigureAndUnconfigure(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// Чужой daemon.json — в резервную копию, при снятии настройки — обратно.
	os.MkdirAll(r.e.Paths.Etc+"/gamayun-nas/apps", 0o755)
	os.WriteFile(r.e.Paths.daemonJSON(), []byte(`{"debug":true}`), 0o644)
	res, err := r.e.Configure(ctx, PoolParams{Pool: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 4 {
		t.Fatalf("субтомы: %v", res.Created)
	}
	var dj map[string]any
	b, _ := os.ReadFile(r.e.Paths.daemonJSON())
	json.Unmarshal(b, &dj)
	if dj["data-root"] != r.root+"/srv/p1/.apps/docker" || dj["log-driver"] != "local" {
		t.Fatalf("daemon.json: %s", b)
	}
	if lo := dj["log-opts"].(map[string]any); lo["max-size"] != "10m" || lo["max-file"] != "3" {
		t.Fatalf("log-opts: %v", lo)
	}
	toml, _ := os.ReadFile(r.e.Paths.ctrTOML())
	if !strings.Contains(string(toml), `root = "`+r.root+`/srv/p1/.apps/containerd"`) {
		t.Fatalf("config.toml: %s", toml)
	}
	di, _ := os.ReadFile(r.e.Paths.dropin("gamayun-docker"))
	if !strings.Contains(string(di), "RequiresMountsFor="+r.root+"/srv/p1\n") || !strings.Contains(string(di), "ConditionPathIsMountPoint="+r.root+"/srv/p1\n") {
		t.Fatalf("drop-in: %s", di)
	}
	st, err := r.e.Status(ctx, NoParams{})
	if err != nil || st.Pool != "p1" || !st.Installed || !st.PoolMounted || st.PoolReadOnly || !st.AppsExists {
		t.Fatalf("status: %+v %v", st, err)
	}
	// Повтор — идемпотентно, ничего не создаёт.
	if res, err := r.e.Configure(ctx, PoolParams{Pool: "p1"}); err != nil || len(res.Created) != 0 {
		t.Fatalf("повтор: %+v %v", res, err)
	}
	// Другой пул — конфликт.
	if _, err := r.e.Configure(ctx, PoolParams{Pool: "p2"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("другой пул: %v", err)
	}
	u, err := r.e.Unconfigure(ctx, UnconfigureParams{PurgeCreated: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Purged) != 4 {
		t.Fatalf("purge: %v", u.Purged)
	}
	if b, _ := os.ReadFile(r.e.Paths.daemonJSON()); string(b) != `{"debug":true}` {
		t.Fatalf("daemon.json не восстановлен: %s", b)
	}
	for _, f := range []string{r.e.Paths.ctrTOML(), r.e.Paths.dropin("gamayun-docker"), r.e.Paths.dropin("gamayun-containerd"), r.e.Paths.state(), r.root + "/srv/p1/.apps"} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("остался %s", f)
		}
	}
}

func TestConfigureRefusals(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.f.ro = true
	r.f.writeMountinfo()
	if _, err := r.e.Configure(ctx, PoolParams{Pool: "p1"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("RO: %v", err)
	}
	r.f.ro, r.f.mounted = false, false
	r.f.writeMountinfo()
	if _, err := r.e.Configure(ctx, PoolParams{Pool: "p1"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("не смонтирован: %v", err)
	}
	r.f.mounted = true
	r.f.writeMountinfo()
	r.f.docker = "active"
	if _, err := r.e.Configure(ctx, PoolParams{Pool: "p1"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("docker работает: %v", err)
	}
	r.f.docker = "inactive"
	os.Remove(r.e.Paths.unit())
	if _, err := r.e.Configure(ctx, PoolParams{Pool: "p1"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("не установлен: %v", err)
	}
	for _, c := range r.f.commands() {
		if strings.HasPrefix(c, "btrfs") {
			t.Fatalf("при отказе — запись: %s", c)
		}
	}
}

func TestConfigureRollback(t *testing.T) {
	r := newRig(t)
	r.f.fail["systemctl daemon-reload"] = true
	if _, err := r.e.Configure(context.Background(), PoolParams{Pool: "p1"}); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	for _, f := range []string{r.e.Paths.daemonJSON(), r.e.Paths.ctrTOML(), r.e.Paths.dropin("gamayun-docker"), r.e.Paths.state(), r.root + "/srv/p1/.apps"} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("после отката остался %s", f)
		}
	}
	if len(r.f.subvols) != 0 {
		t.Fatalf("субтомы: %v", r.f.subvols)
	}
}

func TestStartStop(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	if _, err := r.e.Start(ctx, PoolParams{Pool: "p1"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("не настроен: %v", err)
	}
	if _, err := r.e.Configure(ctx, PoolParams{Pool: "p1"}); err != nil {
		t.Fatal(err)
	}
	res, err := r.e.Start(ctx, PoolParams{Pool: "p1"})
	if err != nil || res.Version != "29.1.0" {
		t.Fatalf("start: %+v %v", res, err)
	}
	if _, err := os.Stat(r.e.Paths.flag()); err != nil {
		t.Fatal("нет флага запуска")
	}
	if _, err := r.e.Stop(ctx, NoParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.e.Paths.flag()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("флаг остался")
	}
	// data-root не на пуле — сбой, Docker остановлен, флаг снят.
	r.info.DockerRootDir = "/var/lib/docker"
	if _, err := r.e.Start(ctx, PoolParams{Pool: "p1"}); err == nil || !strings.Contains(err.Error(), "data-root") {
		t.Fatalf("ожидалась ошибка data-root: %v", err)
	}
	if r.f.docker != "inactive" {
		t.Fatal("после сбоя docker не остановлен")
	}
	if _, err := os.Stat(r.e.Paths.flag()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("после сбоя флаг остался")
	}
	// /var/lib/docker появился — сбой.
	r.info.DockerRootDir = r.root + "/srv/p1/.apps/docker"
	os.Mkdir(r.e.Paths.VarLib+"/containerd", 0o755)
	if _, err := r.e.Start(ctx, PoolParams{Pool: "p1"}); err == nil {
		t.Fatal("ожидалась ошибка: /var/lib/containerd")
	}
	os.Remove(r.e.Paths.VarLib + "/containerd")
	// Пул RO — отказ до запуска.
	r.f.ro = true
	r.f.writeMountinfo()
	if _, err := r.e.Start(ctx, PoolParams{Pool: "p1"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("RO: %v", err)
	}
}

func TestRemount(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	if _, err := r.e.Configure(ctx, PoolParams{Pool: "p1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Remount(ctx, PoolParams{Pool: "p2"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("чужой пул: %v", err)
	}
	r.f.docker = "active"
	if _, err := r.e.Remount(ctx, PoolParams{Pool: "p1"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("docker работает: %v", err)
	}
	r.f.docker = "inactive"
	r.f.ro = true
	r.f.writeMountinfo()
	res, err := r.e.Remount(ctx, PoolParams{Pool: "p1"})
	if err != nil || res.ReadOnly {
		t.Fatalf("remount: %+v %v", res, err)
	}
	c := strings.Join(r.f.commands(), "\n")
	if !strings.Contains(c, "umount "+r.root+"/srv/p1") || !strings.Contains(c, "mount -t btrfs "+r.root+"/dev/dn_p1/data "+r.root+"/srv/p1") {
		t.Fatalf("команды: %s", c)
	}
}

func TestAppdata(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	if _, err := r.e.AppdataCreate(ctx, AppdataParams{ID: "immich"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("не настроено: %v", err)
	}
	if _, err := r.e.Configure(ctx, PoolParams{Pool: "p1"}); err != nil {
		t.Fatal(err)
	}
	var nocow []string
	r.e.SetNoCOW = func(p string) error { nocow = append(nocow, p); return nil }
	res, err := r.e.AppdataCreate(ctx, AppdataParams{ID: "immich", NoCOWDirs: []string{"postgres"}})
	if err != nil || !res.Created || len(nocow) != 1 || nocow[0] != res.Path+"/postgres" {
		t.Fatalf("create: %+v %v %v", res, err, nocow)
	}
	if res, err := r.e.AppdataCreate(ctx, AppdataParams{ID: "immich", NoCOWDirs: []string{"postgres"}}); err != nil || res.Created {
		t.Fatalf("повтор: %+v %v", res, err)
	}
	if _, err := r.e.AppdataDelete(ctx, AppdataParams{ID: "immich"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("субтом приложения остался")
	}
	if _, err := r.e.AppdataDelete(ctx, AppdataParams{ID: "immich"}); err != nil {
		t.Fatalf("повторное удаление: %v", err)
	}
}

func TestParams(t *testing.T) {
	bad := []interface{ Validate() error }{
		PoolParams{Pool: "../x"}, PoolParams{Pool: ""}, AppdataParams{ID: "Bad"}, AppdataParams{ID: "a-"},
		AppdataParams{ID: "ok", NoCOWDirs: []string{"../etc"}}, AppdataParams{ID: "ok", NoCOWDirs: []string{"a/b"}},
		EventsParams{WaitS: MaxWait + 1}, EventsParams{SinceNano: -1},
	}
	for _, p := range bad {
		if p.Validate() == nil {
			t.Errorf("принято: %#v", p)
		}
	}
	good := []interface{ Validate() error }{PoolParams{Pool: "test1"}, AppdataParams{ID: "jellyfin", NoCOWDirs: []string{"db"}}, EventsParams{WaitS: 30}}
	for _, p := range good {
		if err := p.Validate(); err != nil {
			t.Errorf("%#v: %v", p, err)
		}
	}
}

func TestEvents(t *testing.T) {
	r := newRig(t)
	base := time.Now().Add(-time.Minute).UnixNano()
	ev := func(n int64, action string, attrs string) string {
		return fmt.Sprintf(`{"Type":"container","Action":%q,"timeNano":%d,"Actor":{"ID":"0123456789abcdef","Attributes":{%s}}}`+"\n", action, base+n, attrs)
	}
	r.evs = ev(0, "die", `"name":"old","exitCode":"1"`) + // не позже since — пропуск
		ev(1, "start", `"name":"a"`) +
		ev(2, "die", `"name":"a","image":"pg:16","exitCode":"3","SECRET":"x","com.docker.compose.project":"gamayun-a"`) +
		ev(3, "health_status: unhealthy", `"name":"a"`) +
		ev(4, "oom", `"name":"a"`) +
		ev(4, "die", `"name":"a","exitCode":"137"`) +
		ev(5, "exec_start: sh", `"name":"a"`)
	res, err := r.e.Events(context.Background(), EventsParams{SinceNano: base, WaitS: 0})
	if err != nil || !res.Running {
		t.Fatalf("%+v %v", res, err)
	}
	if len(res.Events) != 4 || !res.Events[3].OOM || res.Events[0].OOM {
		t.Fatalf("события: %+v", res.Events)
	}
	d := res.Events[0]
	if d.Action != "die" || d.ExitCode != "3" || d.ID != "0123456789ab" || d.Project != "gamayun-a" || d.Image != "pg:16" {
		t.Fatalf("die: %+v", d)
	}
	if res.Events[1].Action != "health_status" || res.Events[1].Health != "unhealthy" || res.Events[2].Action != "oom" {
		t.Fatalf("health/oom: %+v", res.Events)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "SECRET") {
		t.Fatal("в событие попали метки контейнера")
	}
	// Docker остановлен — не ошибка, Running=false, курсор не сдвигается.
	r.e.Docker = NewDockerClient(r.root + "/нет.sock")
	res, err = r.e.Events(context.Background(), EventsParams{SinceNano: base, WaitS: 0})
	if err != nil || res.Running || res.UntilNano != base {
		t.Fatalf("docker остановлен: %+v %v", res, err)
	}
}

func TestContainerdTOML(t *testing.T) {
	b := string(ContainerdTOML("/srv/p/.apps"))
	if !strings.Contains(b, "version = 3\n") || !strings.Contains(b, `root = "/srv/p/.apps/containerd"`) {
		t.Fatal(b)
	}
}

// Версия Docker не совпала с versions.lock — запуск не готов, Docker остановлен.
func TestStartLockedVersion(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.e.Configure(ctx, PoolParams{Pool: "p1"})
	r.info.ServerVersion = "30.0.0"
	if _, err := r.e.Start(ctx, PoolParams{Pool: "p1"}); err == nil || !strings.Contains(err.Error(), "закреплена 29.1.0") {
		t.Fatalf("%v", err)
	}
	if r.f.docker != "inactive" {
		t.Fatal("docker не остановлен")
	}
}

// У helper UMask=0077: файлы настройки и каталоги — с явными режимами.
func TestModesUnderUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	r := newRig(t)
	if _, err := r.e.Configure(context.Background(), PoolParams{Pool: "p1"}); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{r.e.Paths.dropin("gamayun-docker"): 0o644, filepath.Dir(r.e.Paths.dropin("gamayun-docker")): 0o755 | os.ModeDir,
		r.e.Paths.daemonJSON(): 0o644, filepath.Dir(r.e.Paths.daemonJSON()): 0o755 | os.ModeDir} {
		if fi, err := os.Stat(p); err != nil || fi.Mode() != want {
			t.Errorf("%s: %v %v, ожидалось %v", p, fi.Mode(), err, want)
		}
	}
}

// fakeProc — подмена /proc: процессы с cgroup, exe, mountinfo, fd.
type fakeProc struct {
	t    *testing.T
	root string
}

func (p fakeProc) add(pid int, comm, cgroup, exe, mountinfo string, fds ...string) {
	d := filepath.Join(p.root, fmt.Sprint(pid))
	os.MkdirAll(d+"/fd", 0o755)
	os.WriteFile(d+"/comm", []byte(comm+"\n"), 0o644)
	os.WriteFile(d+"/cgroup", []byte(cgroup+"\n"), 0o644)
	os.WriteFile(d+"/mountinfo", []byte(mountinfo), 0o644)
	if exe != "" {
		os.Symlink(exe, d+"/exe")
	}
	for i, f := range fds {
		os.Symlink(f, filepath.Join(d, "fd", fmt.Sprint(i+3)))
	}
}

func TestStopReapsLeftovers(t *testing.T) {
	leftoverTerm, leftoverKill = 300*time.Millisecond, 300*time.Millisecond
	ctx := context.Background()
	r := newRig(t)
	pr := fakeProc{t: t, root: r.root + "/proc"}
	r.e.Paths.Proc = pr.root
	if _, err := r.e.Configure(ctx, PoolParams{Pool: "p1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Start(ctx, PoolParams{Pool: "p1"}); err != nil {
		t.Fatal(err)
	}
	mp, lv := r.root+"/srv/p1", r.root+"/dev/mapper/dn_p1-data"
	host := fmt.Sprintf("36 22 0:32 / %s rw - btrfs %s rw\n", mp, lv)
	scope := "docker-" + strings.Repeat("ab", 32) + ".scope"
	// Обычный процесс хоста: пул в той же точке, файлы вне .apps — не мешает.
	pr.add(10, "smbd", "0::/system.slice/smbd.service", "/usr/sbin/smbd", host, mp+"/shares/a")
	// Осиротевший контейнер: свой scope, том пула bind-смонтирован в /d.
	pr.add(100, "sleep", "0::/system.slice/"+scope, "/bin/sleep", fmt.Sprintf("50 40 0:32 /.apps/appdata/x /d rw - btrfs %s rw\n", lv))
	pr.add(101, ShimName, "0::/system.slice/gamayun-containerd.service", filepath.Dir(r.e.Paths.Dockerd)+"/"+ShimName, host, mp+"/.apps/containerd/x")
	var sigs []string
	r.e.Kill = func(pid int, sig syscall.Signal) error {
		sigs = append(sigs, fmt.Sprint(pid, " ", sig))
		if sig == syscall.SIGKILL || pid == 101 { // контейнер не реагирует на TERM (sleep — PID 1)
			os.RemoveAll(filepath.Join(pr.root, fmt.Sprint(pid)))
		}
		return nil
	}
	// Корень контейнера (overlay) остался смонтированным: dockerd убит.
	ov := mp + "/.apps/docker/rootfs/overlayfs/" + strings.Repeat("ab", 32)
	r.f.nested = []string{ov}
	r.f.writeMountinfo()
	if st, _ := r.e.Status(ctx, NoParams{}); st.Leftover != 0 {
		t.Fatalf("службы работают — остатки не считаются: %d", st.Leftover)
	}
	r.f.docker, r.f.ctr = "inactive", "inactive"
	if st, _ := r.e.Status(ctx, NoParams{}); st.Leftover != 2 {
		t.Fatalf("остатков %d, ожидалось 2", st.Leftover)
	}
	res, err := r.e.Stop(ctx, NoParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reaped) != 2 || !strings.Contains(res.Reaped[0], scope) {
		t.Fatalf("reaped: %v", res.Reaped)
	}
	if len(res.Unmounted) != 1 || res.Unmounted[0] != ov || len(r.f.nested) != 0 || !r.f.mounted {
		t.Fatalf("overlay: %v, осталось %v, пул смонтирован %v", res.Unmounted, r.f.nested, r.f.mounted)
	}
	if got := strings.Join(sigs, ","); got != "100 terminated,101 terminated,100 killed" {
		t.Fatalf("сигналы: %s", got)
	}
	found := false
	for _, c := range r.f.commands() {
		found = found || c == "systemctl stop "+scope
	}
	if !found {
		t.Fatalf("scope не остановлен: %v", r.f.commands())
	}
	// Процесс не умирает и держит том пула в своём пространстве имён — ошибка с причиной.
	pr.add(200, "app", "0::/user.slice/x.scope", "/usr/bin/app", fmt.Sprintf("50 40 0:32 / /data rw - btrfs %s rw\n", lv))
	pr.add(201, "cat", "0::/user.slice/y.scope", "/usr/bin/cat", host, mp+"/.apps/docker/f")
	r.f.fail["umount "+ov] = true // overlay не снимается
	r.f.nested = []string{ov}
	r.f.writeMountinfo()
	_, err = r.e.Stop(ctx, NoParams{})
	if err == nil || !strings.Contains(err.Error(), "пул p1 занят") || !strings.Contains(err.Error(), "в /data в пространстве имён процесса 200 app") ||
		!strings.Contains(err.Error(), "процесс 201 cat: открыт") || !strings.Contains(err.Error(), "внутри пула смонтировано "+ov) ||
		strings.Contains(err.Error(), "smbd") {
		t.Fatalf("ожидалась ошибка занятости: %v", err)
	}
	// Перемонтирование при занятом пуле — отказ до umount.
	n := len(r.f.commands())
	if _, err := r.e.Remount(ctx, PoolParams{Pool: "p1"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "занят") {
		t.Fatalf("remount: %v", err)
	}
	for _, c := range r.f.commands()[n:] {
		if c == "umount "+mp {
			t.Fatal("umount пула при занятом пуле")
		}
	}
}

// Посторонний процесс с cwd на пуле вне .apps — остановке Docker не
// мешает, перемонтирование отказывает с именем и pid процесса; apps.pool_busy
// показывает его, пока он жив.
func TestRemountNamesHolder(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	pr := fakeProc{t: t, root: r.root + "/proc"}
	r.e.Paths.Proc = pr.root
	if _, err := r.e.Configure(ctx, PoolParams{Pool: "p1"}); err != nil {
		t.Fatal(err)
	}
	mp := r.root + "/srv/p1"
	pr.add(300, "sleep", "0::/user.slice/s.scope", "/bin/sleep", "")
	os.Symlink(mp+"/data", filepath.Join(pr.root, "300", "cwd"))
	r.f.docker, r.f.ctr = "inactive", "inactive"
	if _, err := r.e.Stop(ctx, NoParams{}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	b, err := r.e.PoolBusy(ctx, PoolParams{Pool: "p1"})
	if err != nil || len(b.Apps) != 0 || len(b.Holders) != 1 || !strings.Contains(b.Holders[0], "процесс 300 sleep: cwd "+mp+"/data") {
		t.Fatalf("pool_busy: %+v %v", b, err)
	}
	n := len(r.f.commands())
	if _, err := r.e.Remount(ctx, PoolParams{Pool: "p1"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "пул p1 занят") || !strings.Contains(err.Error(), "300 sleep") {
		t.Fatalf("remount: %v", err)
	}
	for _, c := range r.f.commands()[n:] {
		if c == "umount "+mp {
			t.Fatal("umount пула при занятом пуле")
		}
	}
	os.RemoveAll(filepath.Join(pr.root, "300"))
	if b, _ = r.e.PoolBusy(ctx, PoolParams{Pool: "p1"}); len(b.Holders)+len(b.Apps) != 0 {
		t.Fatalf("после выхода процесса: %+v", b)
	}
	if _, err := r.e.Remount(ctx, PoolParams{Pool: "p1"}); err != nil {
		t.Fatalf("remount: %v", err)
	}
}
