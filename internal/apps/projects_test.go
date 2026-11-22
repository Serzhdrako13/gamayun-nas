package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gamayun-nas/internal/apps/compose"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/tasks"
)

// projHelper — операции проектов поверх fakeHelper.
type projHelper struct {
	*fakeHelper
	pmu      sync.Mutex
	files    map[string]ProjectWriteParams
	prev     map[string]ProjectWriteParams
	appdata  map[string]bool
	calls    []string
	upFail   map[string]bool // id → up падает
	ps       PSResult
	upSHA    []string
	lastLogs LogsParams
	// учётки и доступ
	users   map[string]int
	acl     map[string]map[int]string // папка → UID → ro|rw
	aclFail map[string]bool           // "share:mode" → задание падает
	owner   map[string]int
	lastJob ACLJob
	// Точки восстановления (id → имя → файлы и данные), данные приложения.
	points   map[string]map[string]fakePoint
	data     map[string]string
	pullFail map[string]bool
}

type fakePoint struct {
	files ProjectWriteParams
	data  string
}

func (h *projHelper) Call(ctx context.Context, op string, params, result any) error {
	h.pmu.Lock()
	var v any
	handled := true
	switch op {
	case OpAppdataCreate:
		p := params.(AppdataParams)
		created := !h.appdata[p.ID]
		h.appdata[p.ID] = true
		v = AppdataResult{Path: "/mnt/p1/.apps/appdata/" + p.ID, Created: created}
		h.calls = append(h.calls, "appdata.create "+p.ID)
	case OpAppdataDelete:
		p := params.(AppdataParams)
		delete(h.appdata, p.ID)
		v = AppdataResult{}
		h.calls = append(h.calls, "appdata.delete "+p.ID)
	case OpProjectWrite:
		p := params.(ProjectWriteParams)
		if p.Backup {
			h.prev[p.ID] = h.files[p.ID]
		}
		h.files[p.ID] = p
		v = ProjectResult{}
		h.calls = append(h.calls, "write "+p.ID)
	case OpProjectRevert:
		p := params.(ProjectParams)
		h.files[p.ID] = h.prev[p.ID]
		delete(h.prev, p.ID)
		v = ProjectResult{}
		h.calls = append(h.calls, "revert "+p.ID)
	case OpProjectRead:
		p := params.(ProjectParams)
		f, ok := h.files[p.ID]
		v = ProjectFiles{UserCompose: f.UserCompose, Env: f.Env, Secrets: f.Secrets, SHA256: f.SHA256, Exists: ok}
	case OpPointCreate:
		p := params.(PointParams)
		if h.points[p.ID] == nil {
			h.points[p.ID] = map[string]fakePoint{}
		}
		h.points[p.ID][p.Name] = fakePoint{files: h.prev[p.ID], data: h.data[p.ID]}
		v = PointResult{}
		h.calls = append(h.calls, "point.create "+p.Name)
	case OpPointRestore:
		p := params.(PointParams)
		pt, ok := h.points[p.ID][p.Name]
		if !ok {
			h.pmu.Unlock()
			return errors.New("нет точки")
		}
		h.files[p.ID] = pt.files
		if p.Data {
			h.data[p.ID] = pt.data
		}
		v = PointResult{}
		h.calls = append(h.calls, "point.restore "+p.Name)
	case OpPointDelete:
		p := params.(PointParams)
		delete(h.points[p.ID], p.Name)
		v = PointResult{}
		h.calls = append(h.calls, "point.delete "+p.Name)
	case OpPointList:
		p := params.(PointListParams)
		l := []string{}
		for n := range h.points[p.ID] {
			l = append(l, n)
		}
		v = PointResult{Points: l}
	case OpProjectDelete:
		p := params.(ProjectParams)
		delete(h.files, p.ID)
		v = ProjectResult{}
		h.calls = append(h.calls, "delete "+p.ID)
	case OpProjectCompose:
		p := params.(ComposeParams)
		c := "compose " + p.ID + " " + p.Action
		if p.RemoveVolumes {
			c += " -v"
		}
		h.calls = append(h.calls, c)
		if p.Action == ActPull && h.pullFail[p.ID] {
			h.pmu.Unlock()
			return errors.New("docker compose pull: нет сети")
		}
		if p.Action == ActUp {
			h.upSHA = append(h.upSHA, p.SHA256)
			if h.upFail[p.ID] || len(p.SHA256) >= 8 && h.upFail[p.ID+"@"+p.SHA256[:8]] {
				h.pmu.Unlock()
				return errors.New("docker compose up: код 1: image not found")
			}
		}
		v = ComposeResult{}
	case OpUserEnsure:
		p := params.(UserParams)
		_, had := h.users[p.ID]
		h.users[p.ID] = p.UID
		v = UserResult{Name: AppUser(p.ID), Changed: !had}
		h.calls = append(h.calls, "user.ensure "+p.ID)
	case OpUserDelete:
		p := params.(UserParams)
		delete(h.users, p.ID)
		v = UserResult{Name: AppUser(p.ID), Changed: true}
		h.calls = append(h.calls, "user.delete "+p.ID)
	case OpAppdataOwn:
		p := params.(OwnParams)
		h.owner[p.ID] = p.UID
		v = OwnResult{}
		h.calls = append(h.calls, fmt.Sprintf("own %s %d", p.ID, p.UID))
	case OpACLStart:
		p := params.(ACLParams)
		h.calls = append(h.calls, "acl "+p.Share+" "+p.Mode)
		if h.acl[p.Share] == nil {
			h.acl[p.Share] = map[int]string{}
		}
		st := JobDone
		if h.aclFail[p.Share+":"+p.Mode] {
			st = JobFailed
			h.acl[p.Share][p.UID] = "partial"
		} else if p.Mode == GrantModeRevoke {
			delete(h.acl[p.Share], p.UID)
		} else {
			h.acl[p.Share][p.UID] = p.Mode
		}
		v = ACLJob{Job: strings.Repeat("a", 32), State: st, Total: 10, Done: 10, Error: map[bool]string{true: "нет места"}[st == JobFailed]}
		h.lastJob = v.(ACLJob)
	case OpACLStatus:
		v = h.lastJob
	case OpACLCheck:
		p := params.(ShareParams)
		a := ShareACL{Users: map[string]string{}, Default: map[string]string{}}
		for u, m := range h.acl[p.Share] {
			a.Users[strconv.Itoa(u)], a.Default[strconv.Itoa(u)] = m, m
		}
		v = a
	case OpProjectsPS:
		v = h.ps
	case OpProjectLogs:
		h.lastLogs = params.(LogsParams)
		v = LogsResult{Text: "hello\n"}
	default:
		handled = false
	}
	h.pmu.Unlock()
	if !handled {
		return h.fakeHelper.Call(ctx, op, params, result)
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, result)
}

func (h *projHelper) aclMode(share string, uid int) string {
	h.pmu.Lock()
	defer h.pmu.Unlock()
	return h.acl[share][uid]
}

func (h *projHelper) log() []string {
	h.pmu.Lock()
	defer h.pmu.Unlock()
	return append([]string{}, h.calls...)
}

type fakeShares []nas.ShareInfo

func (f fakeShares) ListShares(context.Context) ([]nas.ShareInfo, error) { return f, nil }

const secret = "Sup3r-S3cret-Value"

func projSvc(t *testing.T) (*svcRig, *projHelper) {
	t.Helper()
	r := newSvcRig(t)
	ph := &projHelper{fakeHelper: r.h, files: map[string]ProjectWriteParams{}, prev: map[string]ProjectWriteParams{},
		appdata: map[string]bool{}, upFail: map[string]bool{}, users: map[string]int{}, acl: map[string]map[int]string{},
		aclFail: map[string]bool{}, owner: map[string]int{}, points: map[string]map[string]fakePoint{}, data: map[string]string{},
		pullFail: map[string]bool{}}
	r.h.set(func(st *Status) { st.MountRoot = "/mnt"; st.Pool = "p1"; st.Docker, st.Containerd = "active", "active" })
	r.s.Helper = ph
	r.s.Shares = fakeShares{{Name: "Photos", Pool: "p1", Path: "shares/Photos"}, {Name: "Media", Pool: "p1", Path: "shares/Media"}}
	dir := t.TempDir()
	r.s.Passwd, r.s.Group = filepath.Join(dir, "passwd"), filepath.Join(dir, "group")
	os.WriteFile(filepath.Join(dir, "tcp"), []byte("  sl local rem st\n   0: 00000000:1F90 00000000:0000 0A x\n   1: 0100007F:2382 00000000:0000 01 x\n"), 0o644)
	r.s.ProcNet = dir
	ctx := context.Background()
	if err := r.s.save(ctx, Config{Enabled: true, Pool: "p1"}); err != nil {
		t.Fatal(err)
	}
	if v := r.s.Refresh(ctx); v.State != StateRunning {
		t.Fatalf("%+v", v)
	}
	return r, ph
}

func strp(s string) *string { return &s }

const webCompose = `services:
  web:
    image: nginx:1.27
    environment: {PASSWORD: "${PASS}"}
    ports: ["8081:80"]
    volumes: ["./html:/usr/share/nginx/html:ro"]
`

func TestProjectCreateAndSecrets(t *testing.T) {
	r, ph := projSvc(t)
	ctx := context.Background()
	req := ProjectRequest{ID: "web", Name: "Сайт", Compose: webCompose, Env: strp("PASS=" + secret + "\n")}
	cr, err := r.s.Check(ctx, req)
	if err != nil || !cr.OK || len(cr.Ports) != 1 || cr.Ports[0].Network {
		t.Fatalf("%+v %v", cr, err)
	}
	if strings.Contains(cr.Normalized, secret) || !strings.Contains(cr.Normalized, "***") {
		t.Fatalf("секрет в предпросмотре: %s", cr.Normalized)
	}
	tk, err := r.s.CreateProject(ctx, req, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatalf("%+v", tk)
	}
	if got := strings.Join(ph.log(), ","); got != "user.ensure web,appdata.create web,own web 59000,write web,compose web up" {
		t.Fatal(got)
	}
	if !strings.Contains(ph.files["web"].Normalized, secret) {
		t.Fatal("в файл проекта секрет должен попасть")
	}
	list, _, err := r.s.Projects(ctx)
	if err != nil || len(list) != 1 || list[0].Desired != "running" || list[0].Kind != "user" {
		t.Fatalf("%+v %v", list, err)
	}
	d, err := r.s.ProjectDetails(ctx, "web")
	if err != nil || d.Compose != webCompose || strings.Join(d.EnvKeys, ",") != "PASS" {
		t.Fatalf("%+v %v", d, err)
	}
	// Секрет — нигде в БД (события, задачи, настройки).
	for _, q := range []string{"SELECT COUNT(*) FROM events WHERE data LIKE ?", "SELECT COUNT(*) FROM tasks WHERE params LIKE ? OR COALESCE(result,'') LIKE ? OR COALESCE(error_msg,'') LIKE ?", "SELECT COUNT(*) FROM settings WHERE value LIKE ?"} {
		var n int
		args := []any{}
		for i := 0; i < strings.Count(q, "?"); i++ {
			args = append(args, "%"+secret+"%")
		}
		if err := r.s.DB.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != 0 {
			t.Fatalf("секрет в БД: %s", q)
		}
	}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), secret) {
		t.Fatal("секрет в деталях")
	}
	if ty := r.types(t); !contains(ty, EvAppCreated) {
		t.Fatal(ty)
	}
	// Повторное создание — конфликт.
	if _, err := r.s.CreateProject(ctx, req, "admin"); !errors.Is(err, ErrConflictSvc) {
		t.Fatal(err)
	}
	// Тот же порт другим приложением — конфликт.
	req2 := ProjectRequest{ID: "web2", Name: "x", Compose: "services: {a: {image: x, ports: ['8081:80']}}\n"}
	if _, err := r.s.CreateProject(ctx, req2, "admin"); !errors.Is(err, ErrConflictSvc) || !strings.Contains(err.Error(), "web") {
		t.Fatal(err)
	}
	ports, _ := r.s.Ports(ctx)
	var app, sys bool
	for _, p := range ports {
		app = app || p.Owner == OwnerApp && p.Port == 8081 && p.App == "web"
		sys = sys || p.Owner == OwnerSystem && p.Port == 8080
	}
	if !app || !sys {
		t.Fatalf("%+v", ports)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestProjectCreateRollback(t *testing.T) {
	r, ph := projSvc(t)
	ctx := context.Background()
	ph.upFail["web"] = true
	tk, err := r.s.CreateProject(ctx, ProjectRequest{ID: "web", Name: "w", Compose: webCompose, Env: strp("PASS=" + secret)}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	tk = r.wait(t, tk.ID)
	if tk.State != tasks.Failed || tk.Error == nil || !strings.Contains(tk.Error.Message, "откат выполнен") || !strings.Contains(tk.Error.Message, "image not found") {
		t.Fatalf("%+v", tk)
	}
	want := "user.ensure web,appdata.create web,own web 59000,write web,compose web up,compose web down -v,delete web,appdata.delete web,user.delete web"
	if got := strings.Join(ph.log(), ","); got != want {
		t.Fatalf("%s\n%s", got, want)
	}
	if len(ph.files) != 0 || len(ph.appdata) != 0 {
		t.Fatal("мусор после отката")
	}
	if l, _, _ := r.s.Projects(ctx); len(l) != 0 {
		t.Fatal(l)
	}
	if !contains(r.types(t), EvAppFailed) {
		t.Fatal(r.types(t))
	}
}

func TestProjectRisksAndPorts(t *testing.T) {
	r, _ := projSvc(t)
	ctx := context.Background()
	src := "services: {a: {image: x, privileged: true, ports: ['8090:80'], volumes: ['/mnt/p1/shares/Photos:/p:ro']}}\n"
	req := ProjectRequest{ID: "risky", Name: "r", Compose: src, Network: []string{"8090/tcp"}, Grants: map[string]string{"Photos": "ro"}}
	cr, err := r.s.Check(ctx, req)
	if err != nil || cr.OK || len(cr.Unconfirmed) != 2 {
		t.Fatalf("%+v %v", cr, err)
	}
	if _, err := r.s.CreateProject(ctx, req, "admin"); !errors.Is(err, ErrConflictSvc) || !strings.Contains(err.Error(), "не подтверждены") {
		t.Fatal(err)
	}
	req.Confirm = cr.Unconfirmed
	tk, err := r.s.CreateProject(ctx, req, "boss")
	if err != nil {
		t.Fatal(err)
	}
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatalf("%+v", tk)
	}
	evs, _ := r.log.Recent(ctx, 50)
	found := false
	for _, e := range evs {
		if e.Type == EvRiskConfirmed {
			found = strings.Contains(string(e.Data), `"actor":"boss"`) && strings.Contains(string(e.Data), "network_port:8090/tcp") &&
				strings.Contains(string(e.Data), "мимо фаервола")
		}
	}
	if !found {
		t.Fatal("нет события подтверждения риска с кем и каким")
	}
	// Зарезервированные порты.
	bad := ProjectRequest{ID: "smb", Name: "s", Compose: "services: {a: {image: x, ports: ['445:445', '8080:80']}}\n"}
	cr, _ = r.s.Check(ctx, bad)
	if len(cr.Conflicts) != 2 || cr.Conflicts[0].Code != PortReserved || cr.Conflicts[1].Code != PortBusy {
		t.Fatalf("%+v", cr.Conflicts)
	}
	// Запрет — 400.
	if _, err := r.s.CreateProject(ctx, ProjectRequest{ID: "evil", Name: "e", Compose: "services: {a: {image: x, volumes: ['/:/h']}}\n"}, "a"); !errors.Is(err, ErrInvalidSvc) {
		t.Fatal(err)
	}
}

func TestProjectUpdateRollback(t *testing.T) {
	r, ph := projSvc(t)
	ctx := context.Background()
	tk, _ := r.s.CreateProject(ctx, ProjectRequest{ID: "web", Name: "w", Compose: webCompose, Env: strp("PASS=" + secret)}, "a")
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatal(tk)
	}
	oldSHA := ph.files["web"].SHA256
	// Изменение без env — переменные прежние; up нового падает → откат.
	newSrc := strings.Replace(webCompose, "nginx:1.27", "nginx:broken", 1)
	res, _ := compose.Normalize([]byte(newSrc), compose.Context{ID: "web", MountRoot: "/mnt", AppData: "/mnt/p1/.apps/appdata/web",
		Env: map[string]string{"PASS": secret}, UID: 59000, GID: 59000})
	ph.upFail["web@"+SHA256(res.Normalized)[:8]] = true
	tk, err := r.s.UpdateProject(ctx, ProjectRequest{ID: "web", Compose: newSrc}, "a")
	if err != nil {
		t.Fatal(err)
	}
	tk = r.wait(t, tk.ID)
	if tk.State != tasks.Failed || tk.Error == nil || !strings.Contains(tk.Error.Message, "возвращена прежняя версия") {
		t.Fatalf("%+v", tk)
	}
	if ph.files["web"].SHA256 != oldSHA || ph.upSHA[len(ph.upSHA)-1] != oldSHA {
		t.Fatal("прежняя версия не возвращена/не запущена", ph.log())
	}
	if !strings.Contains(ph.files["web"].Env, secret) {
		t.Fatal("переменные потеряны")
	}
	// Успешное изменение.
	ph.upFail = map[string]bool{}
	tk, _ = r.s.UpdateProject(ctx, ProjectRequest{ID: "web", Compose: newSrc}, "a")
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatal(tk)
	}
	if ph.files["web"].SHA256 == oldSHA {
		t.Fatal("не изменено")
	}
}

func TestProjectActionsAndDelete(t *testing.T) {
	r, ph := projSvc(t)
	ctx := context.Background()
	tk, _ := r.s.CreateProject(ctx, ProjectRequest{ID: "web", Name: "w", Compose: webCompose}, "a")
	r.wait(t, tk.ID)
	for _, a := range []string{ActStop, ActStart, ActRestart} {
		tk, err := r.s.ProjectAction(ctx, "web", a, "a")
		if err != nil {
			t.Fatal(err)
		}
		if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
			t.Fatal(tk)
		}
	}
	if _, err := r.s.ProjectAction(ctx, "web", "exec", "a"); !errors.Is(err, ErrInvalidSvc) {
		t.Fatal(err)
	}
	// Удаление с сохранением данных → «удалено (данные сохранены)», порт свободен.
	tk, _ = r.s.DeleteProject(ctx, "web", true, "a")
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatal(tk)
	}
	l, _, _ := r.s.Projects(ctx)
	if len(l) != 1 || l[0].State != PStateRemoved || len(l[0].Ports) != 0 || !ph.appdata["web"] {
		t.Fatalf("%+v", l)
	}
	// Создание заново с тем же id — данные те же.
	tk, _ = r.s.CreateProject(ctx, ProjectRequest{ID: "web", Name: "w", Compose: webCompose}, "a")
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatal(tk)
	}
	// Полное удаление.
	tk, _ = r.s.DeleteProject(ctx, "web", false, "a")
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatal(tk)
	}
	if l, _, _ := r.s.Projects(ctx); len(l) != 0 || ph.appdata["web"] {
		t.Fatal(l)
	}
	got := strings.Join(ph.log(), ",")
	for _, w := range []string{"compose web stop", "compose web up", "compose web restart", "compose web down,", "compose web down -v", "appdata.delete web"} {
		if !strings.Contains(got+",", w) {
			t.Errorf("нет %s: %s", w, got)
		}
	}
}

func TestCrashDetectorAndNoise(t *testing.T) {
	r, ph := projSvc(t)
	ctx := context.Background()
	tk, _ := r.s.CreateProject(ctx, ProjectRequest{ID: "web", Name: "w", Compose: webCompose}, "a")
	r.wait(t, tk.ID)
	base := time.Now().Add(time.Minute)
	for i := 0; i <= CrashLimit; i++ {
		r.s.dockerEvent(ctx, DockerEvent{TimeNano: base.Add(time.Duration(i) * time.Second).UnixNano(), Action: "die", Container: "app-web-web-1", ExitCode: "1", Project: "app-web"})
	}
	var p ProjectView
	for i := 0; i < 200; i++ {
		l, _, _ := r.s.Projects(ctx)
		if len(l) == 1 && l[0].Crashing {
			p = l[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p.State != PStateCrashing || !strings.Contains(strings.Join(ph.log(), ","), "compose web stop") {
		t.Fatalf("%+v %v", p, ph.log())
	}
	if !contains(r.types(t), EvAppCrashing) {
		t.Fatal(r.types(t))
	}
	// Пуск снимает «падает».
	tk, _ = r.s.ProjectAction(ctx, "web", ActStart, "a")
	r.wait(t, tk.ID)
	if l, _, _ := r.s.Projects(ctx); l[0].Crashing {
		t.Fatal("crashing остался")
	}
	// Шум docker restart: die 137 + restart — события die нет.
	n0 := count(r.types(t), EvContainerDied)
	at := base.Add(time.Hour)
	r.s.dockerEvent(ctx, DockerEvent{TimeNano: at.UnixNano(), Action: "die", Container: "c1", ExitCode: "137"})
	r.s.dockerEvent(ctx, DockerEvent{TimeNano: at.Add(2 * time.Second).UnixNano(), Action: "restart", Container: "c1"})
	r.s.flushNoise(ctx, at.Add(time.Minute).UnixNano())
	if count(r.types(t), EvContainerDied) != n0 {
		t.Fatal("die 137 перед restart записан")
	}
	// die 137 без restart — записан после окна.
	r.s.dockerEvent(ctx, DockerEvent{TimeNano: at.Add(time.Hour).UnixNano(), Action: "die", Container: "c2", ExitCode: "137"})
	r.s.flushNoise(ctx, at.Add(time.Hour+5*time.Second).UnixNano())
	if count(r.types(t), EvContainerDied) != n0 {
		t.Fatal("записан раньше окна")
	}
	r.s.flushNoise(ctx, at.Add(time.Hour+time.Minute).UnixNano())
	if count(r.types(t), EvContainerDied) != n0+1 {
		t.Fatal("die 137 потерян")
	}
}

func count(l []string, s string) int {
	n := 0
	for _, x := range l {
		if x == s {
			n++
		}
	}
	return n
}

func TestAutoUpAfterDockerStart(t *testing.T) {
	r, ph := projSvc(t)
	ctx := context.Background()
	tk, _ := r.s.CreateProject(ctx, ProjectRequest{ID: "web", Name: "w", Compose: webCompose}, "a")
	r.wait(t, tk.ID)
	ups := func() int { return strings.Count(strings.Join(ph.log(), ","), "compose web up") }
	r.s.reconcileProjects(ctx) // первая сверка после запуска gamayund: Docker уже работает — up
	n := ups()
	r.s.reconcileProjects(ctx)
	if ups() != n {
		t.Fatal("up без перехода")
	}
	// Пул пропал и вернулся (задача возобновления сама сверяет подсистему).
	r.h.set(func(st *Status) { st.PoolMounted = false })
	r.s.Refresh(ctx)
	r.s.reconcileProjects(ctx)
	r.h.set(func(st *Status) { st.PoolMounted = true })
	r.s.Refresh(ctx)
	r.s.reconcileProjects(ctx)
	if ups() != n+1 {
		t.Fatal("нет up после старта Docker")
	}
}

func TestProjectState(t *testing.T) {
	run := Project{Desired: "running"}
	c := func(state, health, status string) Container {
		return Container{State: state, Health: health, Status: status}
	}
	cases := []struct {
		p    Project
		sub  string
		cs   []Container
		want string
	}{
		{Project{Removed: true}, StateRunning, nil, PStateRemoved},
		{run, StateUnavailable, nil, PStateWaitingPool},
		{run, StatePaused, nil, PStatePaused},
		{Project{Desired: "running", Crashing: true}, StateRunning, nil, PStateCrashing},
		{Project{Desired: "stopped"}, StateRunning, nil, PStateStopped},
		{run, StateRunning, nil, PStateStarting},
		{Project{Desired: "running", Error: "x"}, StateRunning, nil, PStateError},
		{run, StateRunning, []Container{c("running", "healthy", "")}, PStateRunning},
		{run, StateRunning, []Container{c("running", "", "")}, PStateNoCheck},
		{run, StateRunning, []Container{c("running", "unhealthy", "")}, PStateUnhealthy},
		{run, StateRunning, []Container{c("running", "starting", "")}, PStateStarting},
		{run, StateRunning, []Container{c("restarting", "", "")}, PStateStarting},
		{run, StateRunning, []Container{c("running", "healthy", ""), c("exited", "", "Exited (1) 3s ago")}, PStateError},
		{run, StateRunning, []Container{c("running", "healthy", ""), c("exited", "", "Exited (0) 3s ago")}, PStateRunning},
	}
	for i, x := range cases {
		if got := projectState(x.p, x.sub, x.cs); got != x.want {
			t.Errorf("%d: %s, ожидалось %s", i, got, x.want)
		}
	}
}

func TestListening(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tcp6"), []byte("hdr\n 0: 00000000000000000000000000000000:01BB 00000000000000000000000000000000:0000 0A\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "udp"), []byte("hdr\n 0: 00000000:14E9 00000000:0000 07\n 1: 00000000:0035 00000000:0000 01\n"), 0o644)
	l := Listening(dir)
	if !l["443/tcp"] || !l["5353/udp"] || l["53/udp"] || len(l) != 2 {
		t.Fatal(l)
	}
}
