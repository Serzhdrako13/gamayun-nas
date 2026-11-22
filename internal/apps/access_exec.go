package apps

// Операции helper — учётка приложения app-<id>, владелец данных
// приложения, выдача и отзыв доступа к общим папкам (POSIX ACL).
//
// Выдача/отзыв — фоновое задание helper (apps.acl.start → apps.acl.status):
// большая папка (сотни тысяч файлов) обрабатывается минутами, gamayund не
// держит вызов helper и показывает ход выполнения. Путь папки helper строит
// сам (<пулы>/<пул>/shares/<имя>), из запроса — только имена.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gamayun-nas/internal/nas"
)

// Операции helper: учётки приложений и доступ к общим папкам.
const (
	OpUserEnsure   = "apps.user.ensure"
	OpUserDelete   = "apps.user.delete"
	OpAppdataOwn   = "apps.appdata.own"
	OpACLStart     = "apps.acl.start"
	OpACLStatus    = "apps.acl.status"
	OpACLCheck     = "apps.acl.check"
	aclJobKeep     = time.Hour
	maxACLJobs     = 64
	appUserComment = "gamayun-nas app"
)

// AppUser — имя учётки приложения (не длиннее 32 символов: предел useradd).
func AppUser(id string) string {
	n := nas.AppUserPrefix + id
	if len(n) <= 32 {
		return n
	}
	h := sha256.Sum256([]byte(id))
	return nas.AppUserPrefix + id[:19] + "-" + hex.EncodeToString(h[:])[:8]
}

// ValidAppUID — UID из диапазона учёток приложений.
func ValidAppUID(uid int) bool { return uid >= nas.AppUIDMin && uid <= nas.AppUIDMax }

// UserParams — apps.user.ensure/delete.
type UserParams struct {
	ID  string `json:"id"`
	UID int    `json:"uid"`
}

// Validate — идентификатор и диапазон UID.
func (p UserParams) Validate() error {
	if !ValidID(p.ID) {
		return errors.New("недопустимый идентификатор приложения")
	}
	if !ValidAppUID(p.UID) {
		return fmt.Errorf("UID %d вне диапазона учёток приложений %d–%d", p.UID, nas.AppUIDMin, nas.AppUIDMax)
	}
	return nil
}

// UserResult — итог apps.user.*.
type UserResult struct {
	Name    string `json:"name"`
	Changed bool   `json:"changed"`
}

// OwnParams — apps.appdata.own: владелец данных приложения. UID 0 —
// возврат root (откат перевода приложения «доступ как root»).
type OwnParams struct {
	ID  string `json:"id"`
	UID int    `json:"uid"`
}

// Validate — UID приложения или 0.
func (p OwnParams) Validate() error {
	if !ValidID(p.ID) {
		return errors.New("недопустимый идентификатор приложения")
	}
	if p.UID != 0 && !ValidAppUID(p.UID) {
		return fmt.Errorf("UID %d вне диапазона", p.UID)
	}
	return nil
}

// OwnResult — итог apps.appdata.own.
type OwnResult struct {
	Path  string `json:"path"`
	Count int64  `json:"count"`
}

// ACLParams — apps.acl.start: выдача (ro/rw) или отзыв доступа приложения
// к общей папке.
type ACLParams struct {
	ID    string `json:"id"`
	UID   int    `json:"uid"`
	Pool  string `json:"pool"`
	Share string `json:"share"`
	Mode  string `json:"mode"` // ro | rw | revoke
	// DropGroupDefault — при отзыве снять d:g:gamayun-users (других выдач
	// на запись в папке нет).
	DropGroupDefault bool `json:"drop_group_default,omitempty"`
}

// Validate — имена и режим.
func (p ACLParams) Validate() error {
	switch {
	case !ValidID(p.ID):
		return errors.New("недопустимый идентификатор приложения")
	case !ValidAppUID(p.UID):
		return fmt.Errorf("UID %d вне диапазона", p.UID)
	case !ValidPool(p.Pool):
		return errors.New("недопустимое имя пула")
	case !nas.ValidShare(p.Share) || strings.HasPrefix(p.Share, "."):
		return errors.New("недопустимое имя общей папки")
	}
	switch p.Mode {
	case GrantModeRO, GrantModeRW, GrantModeRevoke:
	default:
		return fmt.Errorf("режим %q", p.Mode)
	}
	return nil
}

// ShareParams — apps.acl.check.
type ShareParams struct {
	Pool  string `json:"pool"`
	Share string `json:"share"`
}

// Validate — имена.
func (p ShareParams) Validate() error {
	if !ValidPool(p.Pool) || !nas.ValidShare(p.Share) || strings.HasPrefix(p.Share, ".") {
		return errors.New("недопустимая папка")
	}
	return nil
}

// JobParams — apps.acl.status.
type JobParams struct {
	Job string `json:"job"`
}

// Validate — формат идентификатора.
func (p JobParams) Validate() error {
	if len(p.Job) != 32 {
		return errors.New("недопустимый идентификатор задания")
	}
	return nil
}

// Состояния задания ACL.
const (
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// ACLJob — состояние задания выдачи/отзыва.
type ACLJob struct {
	Job      string  `json:"job"`
	State    string  `json:"state"`
	Phase    string  `json:"phase"` // count | apply
	Total    int64   `json:"total"`
	Done     int64   `json:"done"`
	Changed  int64   `json:"changed"`
	Skipped  int64   `json:"skipped"`
	Error    string  `json:"error,omitempty"`
	Share    string  `json:"share"`
	Mode     string  `json:"mode"`
	Seconds  float64 `json:"seconds"`
	finished time.Time
}

type aclJobRun struct {
	mu      sync.Mutex
	job     ACLJob
	done    atomic.Int64
	changed atomic.Int64
	started time.Time
	key     string
}

func (r *aclJobRun) snapshot() ACLJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.job
	j.Done, j.Changed = r.done.Load(), r.changed.Load()
	if j.State == JobRunning {
		j.Seconds = time.Since(r.started).Seconds()
	}
	return j
}

// ShareACL — записи учёток приложений в ACL корня папки (apps.acl.check).
type ShareACL struct {
	Path string `json:"path"`
	// Users — UID → права (r, rw) в access ACL; Default — в default ACL.
	Users   map[string]string `json:"users"`
	Default map[string]string `json:"default"`
	// GroupDefault — есть d:g:gamayun-users.
	GroupDefault bool `json:"group_default"`
}

type aclJobs struct {
	mu   sync.Mutex
	runs map[string]*aclJobRun
}

func (e *Executor) jobs() *aclJobs {
	e.jobsOnce.Do(func() { e.aclJobs = &aclJobs{runs: map[string]*aclJobRun{}} })
	return e.aclJobs
}

// sharePath — путь общей папки на смонтированном пуле: каталог, не ссылка.
func (e *Executor) sharePath(pool, share string) (string, error) {
	p := nas.ShareDir(e.MountRoot, pool, share)
	fi, err := os.Lstat(p)
	if err != nil {
		return "", refuse("общая папка %s: %v", share, err)
	}
	if !fi.IsDir() {
		return "", refuse("общая папка %s — не каталог", share)
	}
	if mounted, ro := e.poolMount(pool); !mounted {
		return "", refuse("пул %s не смонтирован", pool)
	} else if ro {
		return "", refuse("пул %s только для чтения", pool)
	}
	return p, nil
}

// ---------- учётки ----------

type pwEntry struct {
	name string
	id   int
}

func readIDs(path string) ([]pwEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []pwEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fs := strings.SplitN(sc.Text(), ":", 4)
		if len(fs) < 3 {
			continue
		}
		id, err := strconv.Atoi(fs[2])
		if err != nil {
			continue
		}
		out = append(out, pwEntry{fs[0], id})
	}
	return out, sc.Err()
}

func lookup(l []pwEntry, name string, id int) (byName, byID *pwEntry) {
	for i := range l {
		if l[i].name == name && byName == nil {
			byName = &l[i]
		}
		if l[i].id == id && byID == nil {
			byID = &l[i]
		}
	}
	return
}

func (p Paths) passwd() string { return or(p.Passwd, "/etc/passwd") }
func (p Paths) group() string  { return or(p.Group, "/etc/group") }

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// UserEnsure создаёт учётку app-<id> (группа того же имени и номера, без
// дома и входа). Идемпотентна; занятые чужими имя или номер — отказ.
func (e *Executor) UserEnsure(ctx context.Context, p UserParams) (UserResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	res := UserResult{Name: AppUser(p.ID)}
	users, err := readIDs(e.Paths.passwd())
	if err != nil {
		return res, err
	}
	groups, err := readIDs(e.Paths.group())
	if err != nil {
		return res, err
	}
	un, ui := lookup(users, res.Name, p.UID)
	gn, gi := lookup(groups, res.Name, p.UID)
	switch {
	case un != nil && un.id != p.UID:
		return res, refuse("учётка %s уже есть с UID %d", res.Name, un.id)
	case ui != nil && ui.name != res.Name:
		return res, refuse("UID %d занят учёткой %s", p.UID, ui.name)
	case gn != nil && gn.id != p.UID:
		return res, refuse("группа %s уже есть с GID %d", res.Name, gn.id)
	case gi != nil && gi.name != res.Name:
		return res, refuse("GID %d занят группой %s", p.UID, gi.name)
	}
	if gn == nil {
		if _, err := e.run(ctx, time.Minute, or(e.Paths.Groupadd, "/usr/sbin/groupadd"), "--gid", strconv.Itoa(p.UID), "--", res.Name); err != nil {
			return res, err
		}
		res.Changed = true
	}
	if un == nil {
		if _, err := e.run(ctx, time.Minute, or(e.Paths.Useradd, "/usr/sbin/useradd"), "--uid", strconv.Itoa(p.UID),
			"--gid", strconv.Itoa(p.UID), "--no-create-home", "--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin",
			"--comment", appUserComment, "--", res.Name); err != nil {
			if gn == nil {
				e.run(context.WithoutCancel(ctx), time.Minute, or(e.Paths.Groupdel, "/usr/sbin/groupdel"), "--", res.Name)
			}
			return res, err
		}
		res.Changed = true
	}
	return res, nil
}

// UserDelete удаляет учётку и группу app-<id> (нет — не ошибка). Учётка
// с другим UID — отказ (не наша).
func (e *Executor) UserDelete(ctx context.Context, p UserParams) (UserResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	res := UserResult{Name: AppUser(p.ID)}
	users, err := readIDs(e.Paths.passwd())
	if err != nil {
		return res, err
	}
	groups, err := readIDs(e.Paths.group())
	if err != nil {
		return res, err
	}
	un, _ := lookup(users, res.Name, -1)
	gn, _ := lookup(groups, res.Name, -1)
	if un != nil {
		if un.id != p.UID {
			return res, refuse("учётка %s с UID %d — не приложения %s", res.Name, un.id, p.ID)
		}
		if _, err := e.run(ctx, time.Minute, or(e.Paths.Userdel, "/usr/sbin/userdel"), "--", res.Name); err != nil {
			return res, err
		}
		res.Changed = true
	}
	if gn != nil {
		if gn.id != p.UID {
			return res, refuse("группа %s с GID %d — не приложения %s", res.Name, gn.id, p.ID)
		}
		// userdel мог уже удалить одноимённую первичную группу.
		groups, _ = readIDs(e.Paths.group())
		if g, _ := lookup(groups, res.Name, -1); g != nil {
			if _, err := e.run(ctx, time.Minute, or(e.Paths.Groupdel, "/usr/sbin/groupdel"), "--", res.Name); err != nil {
				return res, err
			}
		}
		res.Changed = true
	}
	return res, nil
}

// AppdataOwn — владелец субтома данных приложения (рекурсивно, без
// перехода по ссылкам). Только внутри <пул>/.apps/appdata/<id>.
func (e *Executor) AppdataOwn(ctx context.Context, p OwnParams) (OwnResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var res OwnResult
	s, err := e.loadState()
	if err != nil {
		return res, err
	}
	if s.Pool == "" {
		return res, refuse("приложения не настроены")
	}
	if err := e.needPoolRW(s.Pool); err != nil {
		return res, err
	}
	res.Path = filepath.Join(e.appsRoot(s.Pool), AppdataDir, p.ID)
	if sv, err := e.isSubvol(res.Path); err != nil || !sv {
		return res, refuse("нет субтома данных %s", res.Path)
	}
	res.Count, err = chownTree(res.Path, p.UID, p.UID, func() bool { return ctx.Err() != nil })
	return res, err
}

// ---------- выдача доступа ----------

func newJobID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ACLStart запускает задание выдачи/отзыва; одно задание на папку.
func (e *Executor) ACLStart(_ context.Context, p ACLParams) (ACLJob, error) {
	path, err := e.sharePath(p.Pool, p.Share)
	if err != nil {
		return ACLJob{}, err
	}
	groups, err := readIDs(e.Paths.group())
	if err != nil {
		return ACLJob{}, err
	}
	g, _ := lookup(groups, nas.UnixGroup, -1)
	if g == nil {
		return ACLJob{}, refuse("нет группы %s", nas.UnixGroup)
	}
	ch := aclChange{Mode: p.Mode, UID: uint32(p.UID), GID: uint32(g.id), DropGroupDefault: p.DropGroupDefault}
	js := e.jobs()
	js.mu.Lock()
	defer js.mu.Unlock()
	key := p.Pool + "/" + p.Share
	for id, r := range js.runs {
		j := r.snapshot()
		if j.State != JobRunning && time.Since(j.finished) > aclJobKeep {
			delete(js.runs, id)
			continue
		}
		if r.key == key && j.State == JobRunning {
			return ACLJob{}, refuse("для папки %s уже идёт изменение доступа", p.Share)
		}
	}
	if len(js.runs) >= maxACLJobs {
		return ACLJob{}, refuse("слишком много заданий")
	}
	r := &aclJobRun{job: ACLJob{Job: newJobID(), State: JobRunning, Phase: "count", Share: p.Share, Mode: p.Mode}, started: time.Now(), key: key}
	js.runs[r.job.Job] = r
	go e.runACL(r, path, ch)
	return r.snapshot(), nil
}

func (e *Executor) runACL(r *aclJobRun, path string, ch aclChange) {
	finish := func(err error, skipped int64) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.job.Skipped = skipped
		r.job.finished = time.Now()
		r.job.Seconds = time.Since(r.started).Seconds()
		if err != nil {
			r.job.State, r.job.Error = JobFailed, err.Error()
			return
		}
		r.job.State = JobDone
	}
	// Подсчёт — для хода выполнения (обход без записи быстрее правки ACL).
	cw := &fdWalker{}
	var total int64
	cw.visit = func(string, *statT) error { total++; return nil }
	if err := cw.walk(path); err != nil {
		finish(err, 0)
		return
	}
	r.mu.Lock()
	r.job.Total, r.job.Phase = total, "apply"
	r.mu.Unlock()
	w := &fdWalker{}
	w.visit = func(p string, st *statT) error {
		c, err := ch.apply(p, st)
		if err != nil {
			return err
		}
		r.done.Add(1)
		if c {
			r.changed.Add(1)
		}
		return nil
	}
	err := w.walk(path)
	finish(err, w.Skipped)
}

// ACLStatus — состояние задания (неизвестное — отказ: helper перезапускался).
func (e *Executor) ACLStatus(_ context.Context, p JobParams) (ACLJob, error) {
	js := e.jobs()
	js.mu.Lock()
	r, ok := js.runs[p.Job]
	js.mu.Unlock()
	if !ok {
		return ACLJob{}, refuse("задание %s не найдено (helper перезапускался?)", p.Job)
	}
	return r.snapshot(), nil
}

// ACLCheck — записи учёток приложений в ACL корня общей папки.
func (e *Executor) ACLCheck(_ context.Context, p ShareParams) (ShareACL, error) {
	res := ShareACL{Path: nas.ShareDir(e.MountRoot, p.Pool, p.Share), Users: map[string]string{}, Default: map[string]string{}}
	fi, err := os.Lstat(res.Path)
	if err != nil || !fi.IsDir() {
		return res, refuse("общая папка %s недоступна", p.Share)
	}
	acc, def, err := readACLs(res.Path)
	if err != nil {
		return res, err
	}
	groups, _ := readIDs(e.Paths.group())
	g, _ := lookup(groups, nas.UnixGroup, -1)
	pm := func(v uint16) string {
		if v&permW != 0 {
			return GrantModeRW
		}
		return GrantModeRO
	}
	for _, x := range acc {
		if x.Tag == aclUser && ValidAppUID(int(x.ID)) {
			res.Users[strconv.Itoa(int(x.ID))] = pm(x.Perm)
		}
	}
	for _, x := range def {
		if x.Tag == aclUser && ValidAppUID(int(x.ID)) {
			res.Default[strconv.Itoa(int(x.ID))] = pm(x.Perm)
		}
		if x.Tag == aclGroup && g != nil && int(x.ID) == g.id {
			res.GroupDefault = true
		}
	}
	return res, nil
}
