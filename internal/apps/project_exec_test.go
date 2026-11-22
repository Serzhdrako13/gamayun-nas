package apps

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gamayun-nas/internal/apps/compose"
)

// projRig — пул настроен, субтом приложения web создан.
func projRig(t *testing.T) (*rig, string) {
	t.Helper()
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.e.Configure(ctx, PoolParams{Pool: "p1"}); err != nil {
		t.Fatal(err)
	}
	res, err := r.e.AppdataCreate(ctx, AppdataParams{ID: "web"})
	if err != nil {
		t.Fatal(err)
	}
	return r, res.Path
}

func normalized(t *testing.T, r *rig, appdata, src string) compose.Result {
	t.Helper()
	res, err := compose.Normalize([]byte(src), compose.Context{ID: "web", MountRoot: r.e.MountRoot, AppData: appdata})
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res.Forbidden)
	}
	return res
}

func writeParams(res compose.Result, user string) ProjectWriteParams {
	return ProjectWriteParams{ID: "web", UserCompose: user, Env: "PASS=x\n", Normalized: string(res.Normalized),
		SHA256: SHA256(res.Normalized), Mkdirs: res.Mkdirs}
}

func TestProjectWriteComposeRevert(t *testing.T) {
	ctx := context.Background()
	r, appdata := projRig(t)
	r.e.Paths.DockerCLI = "/usr/lib/gamayun-nas-apps/bin/docker"
	src := "services: {a: {image: nginx, volumes: ['./data/x:/x'], ports: ['8080:80']}}\n"
	res := normalized(t, r, appdata, src)
	pr, err := r.e.ProjectWrite(ctx, writeParams(res, src))
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(appdata + "/data/x"); err != nil || !fi.IsDir() {
		t.Fatal("каталог bind не создан")
	}
	for _, f := range []string{FileUser, FileEnv, FileCompose} {
		fi, err := os.Stat(filepath.Join(pr.Dir, f))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, err, fi)
		}
	}
	if fi, _ := os.Stat(pr.Dir); fi.Mode().Perm() != 0o700 {
		t.Fatal(fi.Mode())
	}
	// Неверная сумма — отказ.
	if _, err := r.e.ProjectCompose(ctx, ComposeParams{ID: "web", Action: ActUp, SHA256: strings.Repeat("0", 64)}); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	if _, err := r.e.ProjectCompose(ctx, ComposeParams{ID: "web", Action: ActUp, SHA256: SHA256(res.Normalized)}); err != nil {
		t.Fatal(err)
	}
	cmds := r.f.commands()
	last := cmds[len(cmds)-1]
	want := "docker --config " + r.e.Paths.Run + "/docker-config -H unix://" + r.e.Paths.DockerSock + " compose -p app-web -f " + pr.Dir + "/compose.json --project-directory " + pr.Dir + " up -d --remove-orphans --pull missing --wait"
	if !strings.HasPrefix(last, want) {
		t.Fatalf("%s\n%s", last, want)
	}
	// Подмена файла на диске после записи — helper не запускает.
	tampered := strings.Replace(string(res.Normalized), `"127.0.0.1"`, `"0.0.0.0"`, 1)
	os.WriteFile(filepath.Join(pr.Dir, FileCompose), []byte(tampered), 0o600)
	if _, err := r.e.ProjectCompose(ctx, ComposeParams{ID: "web", Action: ActStart, SHA256: SHA256(res.Normalized)}); !errors.Is(err, ErrRefused) {
		t.Fatal("подменённый файл запущен", err)
	}
	// Изменение с резервной копией и откат.
	os.WriteFile(filepath.Join(pr.Dir, FileCompose), res.Normalized, 0o600)
	src2 := "services: {b: {image: redis}}\n"
	res2 := normalized(t, r, appdata, src2)
	p2 := writeParams(res2, src2)
	p2.Backup = true
	if _, err := r.e.ProjectWrite(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.ProjectRevert(ctx, ProjectParams{ID: "web"}); err != nil {
		t.Fatal(err)
	}
	f, err := r.e.ProjectRead(ctx, ProjectParams{ID: "web"})
	if err != nil || f.UserCompose != src || f.SHA256 != SHA256(res.Normalized) || f.Env != "PASS=x\n" {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := r.e.ProjectRevert(ctx, ProjectParams{ID: "web"}); !errors.Is(err, ErrRefused) {
		t.Fatal("второй откат без .prev", err)
	}
	// Сбой compose — ошибка с выводом.
	r.f.fail["docker"] = true
	if _, err := r.e.ProjectCompose(ctx, ComposeParams{ID: "web", Action: ActUp, SHA256: SHA256(res.Normalized)}); err == nil {
		t.Fatal("сбой compose не виден")
	}
	delete(r.f.fail, "docker")
	if _, err := r.e.ProjectDelete(ctx, ProjectParams{ID: "web"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pr.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("каталог проекта остался")
	}
	// down без файла — по имени проекта.
	if _, err := r.e.ProjectCompose(ctx, ComposeParams{ID: "web", Action: ActDown, RemoveVolumes: true}); err != nil {
		t.Fatal(err)
	}
	cmds = r.f.commands()
	if !strings.HasSuffix(cmds[len(cmds)-1], "compose -p app-web down --remove-orphans --volumes") {
		t.Fatal(cmds[len(cmds)-1])
	}
}

func TestProjectWriteRefusals(t *testing.T) {
	ctx := context.Background()
	r, appdata := projRig(t)
	src := "services: {a: {image: nginx}}\n"
	res := normalized(t, r, appdata, src)
	// Злонамеренный gamayund: файл с bind в /etc с верной суммой.
	evil := []byte(`{"name":"app-web","services":{"a":{"image":"x","labels":{"nas.gamayun.app":"web"},"volumes":[{"type":"bind","source":"/etc","target":"/e","bind":{"create_host_path":false}}]}}}`)
	p := ProjectWriteParams{ID: "web", Normalized: string(evil), SHA256: SHA256(evil)}
	if _, err := r.e.ProjectWrite(ctx, p); !errors.Is(err, ErrRefused) {
		t.Fatalf("bind /etc записан: %v", err)
	}
	// Симлинк из данных приложения наружу.
	os.Symlink("/etc", appdata+"/link")
	lsrc := "services: {a: {image: x, volumes: ['./link:/e']}}\n"
	lres := normalized(t, r, appdata, lsrc)
	if _, err := r.e.ProjectWrite(ctx, writeParams(lres, lsrc)); !errors.Is(err, ErrRefused) {
		t.Fatalf("симлинк наружу: %v", err)
	}
	// Симлинк в середине пути mkdirs.
	lsrc2 := "services: {a: {image: x, volumes: ['./link/sub:/e']}}\n"
	lres2 := normalized(t, r, appdata, lsrc2)
	if _, err := r.e.ProjectWrite(ctx, writeParams(lres2, lsrc2)); !errors.Is(err, ErrRefused) {
		t.Fatalf("mkdir через симлинк: %v", err)
	}
	// Неверная сумма.
	bad := writeParams(res, src)
	bad.SHA256 = strings.Repeat("a", 64)
	if _, err := r.e.ProjectWrite(ctx, bad); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	// Пул только для чтения.
	r.f.ro = true
	if _, err := r.e.ProjectWrite(ctx, writeParams(res, src)); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
}

func TestProjectParams(t *testing.T) {
	sum := strings.Repeat("a", 64)
	bad := []interface{ Validate() error }{
		ProjectWriteParams{ID: "x", SHA256: "zz"}, ProjectWriteParams{ID: "x", SHA256: sum, Mkdirs: []string{"../e"}},
		ProjectWriteParams{ID: "x", SHA256: sum, Mkdirs: []string{"/e"}}, ProjectWriteParams{ID: "X", SHA256: sum},
		ComposeParams{ID: "x", Action: "exec", SHA256: sum}, ComposeParams{ID: "x", Action: ActUp},
		ComposeParams{ID: "x", Action: ActUp, SHA256: sum, RemoveVolumes: true},
		LogsParams{ID: "x", Service: "a", Tail: 0}, LogsParams{ID: "x", Service: "../a", Tail: 10},
		LogsParams{ID: "x", Service: "a", Tail: MaxLogTail + 1}, ProjectParams{ID: "../x"},
	}
	for _, p := range bad {
		if p.Validate() == nil {
			t.Errorf("принято: %#v", p)
		}
	}
	good := []interface{ Validate() error }{
		ProjectWriteParams{ID: "x", SHA256: sum, Mkdirs: []string{"a/b"}}, ComposeParams{ID: "x", Action: ActDown},
		ComposeParams{ID: "x", Action: ActRestart, SHA256: sum}, LogsParams{ID: "x", Service: "db", Tail: 200},
	}
	for _, p := range good {
		if err := p.Validate(); err != nil {
			t.Errorf("%#v: %v", p, err)
		}
	}
}

func TestProjectsPSAndLogs(t *testing.T) {
	r := newRig(t)
	sock := r.root + "/d2.sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[
 {"Id":"aaaaaaaaaaaaaaaa","Names":["/app-web-db-1"],"Image":"pg","State":"running","Status":"Up 1 minute (healthy)",
  "Labels":{"nas.gamayun.app":"web","com.docker.compose.project":"app-web","com.docker.compose.service":"db","SECRET":"x"}},
 {"Id":"bbbbbbbbbbbbbbbb","Names":["/manual"],"Image":"x","State":"exited","Status":"Exited (0)","Labels":{}},
 {"Id":"cccccccccccccccc","Names":["/spoof"],"Image":"x","State":"running","Status":"Up","Labels":{"nas.gamayun.app":"web","com.docker.compose.project":"other"}}]`))
	})
	mux.HandleFunc("/containers/aaaaaaaaaaaaaaaa/logs", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("tail") != "50" {
			http.Error(w, "tail", 400)
			return
		}
		for _, l := range []string{"line1\n", "err2\n"} {
			h := make([]byte, 8)
			h[0] = 1
			binary.BigEndian.PutUint32(h[4:], uint32(len(l)))
			w.Write(append(h, l...))
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	r.e.Docker = NewDockerClient(sock)
	ps, err := r.e.ProjectsPS(context.Background(), NoParams{})
	if err != nil || len(ps.Containers) != 1 || ps.Foreign != 2 {
		t.Fatalf("%+v %v", ps, err)
	}
	c := ps.Containers[0]
	if c.App != "web" || c.Service != "db" || c.Health != "healthy" || c.Name != "app-web-db-1" || c.ID != "aaaaaaaaaaaa" {
		t.Fatalf("%+v", c)
	}
	lg, err := r.e.ProjectLogs(context.Background(), LogsParams{ID: "web", Service: "db", Tail: 50})
	if err != nil || lg.Text != "line1\nerr2\n" {
		t.Fatalf("%q %v", lg.Text, err)
	}
	if _, err := r.e.ProjectLogs(context.Background(), LogsParams{ID: "web", Service: "nope", Tail: 50}); err == nil {
		t.Fatal("нет сервиса")
	}
}

// Файлы секретов сервисов — запись 0600, удаление лишних, резервная копия и возврат.
func TestProjectWriteSecrets(t *testing.T) {
	ctx := context.Background()
	r, appdata := projRig(t)
	norm := func(src string) compose.Result {
		res, err := compose.Normalize([]byte(src), compose.Context{ID: "web", MountRoot: r.e.MountRoot, AppData: appdata,
			Secrets: map[string]bool{"P": true, "Q": true}})
		if err != nil || !res.OK() {
			t.Fatalf("%v %+v", err, res.Forbidden)
		}
		return res
	}
	src1 := "services: {a: {image: x, environment: {P: '${P}'}}, b: {image: y, environment: {Q: '${Q}'}}}\n"
	res1 := norm(src1)
	p1 := writeParams(res1, src1)
	p1.Secrets = map[string]string{"a": "P='1'\n", "b": "Q='2'\n"}
	pr, err := r.e.ProjectWrite(ctx, p1)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"secrets-a.env", "secrets-b.env"} {
		if fi, err := os.Stat(filepath.Join(pr.Dir, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v", f, err)
		}
	}
	// Новая версия без сервиса b: его файл удаляется, старые — в резервной копии.
	src2 := "services: {a: {image: x, environment: {P: '${P}'}}}\n"
	res2 := norm(src2)
	p2 := writeParams(res2, src2)
	p2.Secrets, p2.Backup = map[string]string{"a": "P='3'\n"}, true
	if _, err := r.e.ProjectWrite(ctx, p2); err != nil {
		t.Fatal(err)
	}
	f, _ := r.e.ProjectRead(ctx, ProjectParams{ID: "web"})
	if len(f.Secrets) != 1 || f.Secrets["a"] != "P='3'\n" {
		t.Fatalf("%+v", f.Secrets)
	}
	if _, err := r.e.ProjectRevert(ctx, ProjectParams{ID: "web"}); err != nil {
		t.Fatal(err)
	}
	f, _ = r.e.ProjectRead(ctx, ProjectParams{ID: "web"})
	if len(f.Secrets) != 2 || f.Secrets["a"] != "P='1'\n" || f.Secrets["b"] != "Q='2'\n" {
		t.Fatalf("после возврата: %+v", f.Secrets)
	}
	// Имя сервиса с путём — отказ проверки параметров.
	p3 := writeParams(res1, src1)
	p3.Secrets = map[string]string{"../x": "P='1'\n"}
	if err := p3.Validate(); err == nil {
		t.Fatal("имя с путём принято")
	}
}

// Точка восстановления — снимок данных и прежние файлы; возврат идемпотентен.
func TestPointCreateRestore(t *testing.T) {
	ctx := context.Background()
	r, appdata := projRig(t)
	src := "services: {a: {image: nginx, volumes: ['./data/x:/x'], ports: ['8080:80']}}\n"
	res := normalized(t, r, appdata, src)
	p1 := writeParams(res, src)
	p1.Secrets = map[string]string{"a": "K='v1'\n"}
	if _, err := r.e.ProjectWrite(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.PointCreate(ctx, PointParams{ID: "web", Name: "u1"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("без *.prev: %v", err)
	}
	os.WriteFile(appdata+"/db.txt", []byte("old"), 0o644)
	p2 := p1
	p2.Env, p2.Backup, p2.Secrets = "PASS=new\n", true, map[string]string{"a": "K='v2'\n", "b": "Q='x'\n"}
	if _, err := r.e.ProjectWrite(ctx, p2); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // повтор пересоздаёт
		l, err := r.e.PointCreate(ctx, PointParams{ID: "web", Name: "u1"})
		if err != nil || len(l.Points) != 1 || l.Points[0] != "u1" {
			t.Fatalf("%+v %v", l, err)
		}
	}
	// Новая версия портит данные; возврат — файлы и данные из точки.
	os.WriteFile(appdata+"/db.txt", []byte("new"), 0o644)
	for i := 0; i < 2; i++ {
		if _, err := r.e.PointRestore(ctx, PointParams{ID: "web", Name: "u1", Data: true}); err != nil {
			t.Fatalf("restore %d: %v", i, err)
		}
	}
	if b, _ := os.ReadFile(appdata + "/db.txt"); string(b) != "old" {
		t.Fatalf("данные: %q", b)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(appdata), ".old-web")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("прежний субтом остался")
	}
	f, err := r.e.ProjectRead(ctx, ProjectParams{ID: "web"})
	if err != nil || f.Env != "PASS=x\n" || len(f.Secrets) != 1 || f.Secrets["a"] != "K='v1'\n" {
		t.Fatalf("файлы: %+v %v", f, err)
	}
	// Прервано после rename: субтома данных нет, есть .old — довести.
	old := filepath.Join(filepath.Dir(appdata), ".old-web")
	os.Rename(appdata, old)
	if _, err := r.e.PointRestore(ctx, PointParams{ID: "web", Name: "u1", Data: true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(appdata + "/db.txt"); string(b) != "old" {
		t.Fatalf("данные после прерывания: %q", b)
	}
	if _, err := r.e.PointRestore(ctx, PointParams{ID: "web", Name: "nope"}); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	if l, err := r.e.PointDelete(ctx, PointParams{ID: "web", Name: "u1"}); err != nil || len(l.Points) != 0 {
		t.Fatalf("%+v %v", l, err)
	}
	if (PointParams{ID: "web", Name: "../x"}).Validate() == nil || (PointParams{ID: "web", Name: "U1"}).Validate() == nil {
		t.Fatal("имя точки не проверяется")
	}
}

// Правки override.yaml убраны (2026-11-21): оставшийся от прежней версии файл
// удаляется при записи проекта и не читается.
func TestProjectStaleOverrideRemoved(t *testing.T) {
	ctx := context.Background()
	r, appdata := projRig(t)
	src := "services: {a: {image: nginx}}\n"
	res := normalized(t, r, appdata, src)
	pr, err := r.e.ProjectWrite(ctx, writeParams(res, src))
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(pr.Dir, FileOverride)
	if err := os.WriteFile(f, []byte("services: {a: {privileged: true}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fr, _ := r.e.ProjectRead(ctx, ProjectParams{ID: "web"}); len(fr.Secrets) != 0 {
		t.Fatalf("%+v", fr)
	}
	if _, err := r.e.ProjectWrite(ctx, writeParams(res, src)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatalf("устаревший override.yaml не удалён: %v", err)
	}
}
