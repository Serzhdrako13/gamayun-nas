package apps

// Пользовательские compose-приложения в gamayund — записи,
// проверка, задачи создания/изменения/пуска/остановки/удаления с откатом,
// состояния, детектор «падает».
//
// Секреты (значения .env) не попадают в журнал gamayund, события, параметры
// задач (хранятся в БД) и ответы API: запрос с секретами живёт в памяти до
// выполнения задачи, в API отдаются только имена переменных.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gamayun-nas/internal/catalog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gamayun-nas/internal/apps/compose"
	"gamayun-nas/internal/events"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/tasks"
)

const projectsKey = "apps.projects"

// Состояния приложения (коды для перевода).
const (
	PStateRunning     = "running"
	PStateNoCheck     = "running_no_check" // работает, проверки готовности нет
	PStateStarting    = "starting"
	PStateUnhealthy   = "unhealthy"
	PStateCrashing    = "crashing" // падает: остановлено детектором
	PStateStopped     = "stopped"
	PStateError       = "error"
	PStateWaitingPool = "waiting_pool"
	PStatePaused      = "paused"
	PStateRemoved     = "removed" // удалено, данные сохранены
	PStateUnknown     = "unknown"
)

// Задачи пользовательских приложений.
const (
	TaskProjectCreate  = "apps.project.create"
	TaskProjectUpdate  = "apps.project.update"
	TaskProjectAction  = "apps.project.action"
	TaskProjectDelete  = "apps.project.delete"
	TaskProjectMigrate = "apps.project.migrate"
	EvAppCreated       = "apps.app_created"
	EvAppUpdated       = "apps.app_updated"
	EvAppDeleted       = "apps.app_deleted"
	EvAppFailed        = "apps.app_failed"
	EvAppCrashing      = "apps.app_crashing"
	EvRiskConfirmed    = "apps.risk_confirmed"
	EvAppStarted       = "apps.app_started"
	EvAppStopped       = "apps.app_stopped"
	CrashWindow        = 10 * time.Minute
	CrashLimit         = 5
	restartNoiseWindow = 10 * time.Second
)

// ConfirmedRisk — подтверждённый риск (кто и когда).
type ConfirmedRisk struct {
	Key    string    `json:"key"`
	Code   string    `json:"code"`
	Detail string    `json:"detail"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
}

// Project — запись приложения в settings.
type Project struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Kind     string          `json:"kind"`    // user — «Пользовательское»
	Desired  string          `json:"desired"` // running | stopped
	SHA256   string          `json:"sha256"`
	Ports    []compose.Port  `json:"ports"`
	Network  []string        `json:"network"` // порты «доступно в сети»
	Risks    []ConfirmedRisk `json:"risks"`
	Services []string        `json:"services"`
	NoHealth []string        `json:"no_health"`
	Crashing bool            `json:"crashing,omitempty"`
	Error    string          `json:"error,omitempty"`
	Removed  bool            `json:"removed,omitempty"` // удалено, данные сохранены
	// Учётка app-<id> (UID=GID; 0 — приложение «доступ как root»),
	// режим запуска сервисов, выдачи доступа к общим папкам, расхождения ACL.
	UID         int               `json:"uid,omitempty"`
	RunAs       map[string]string `json:"run_as,omitempty"`
	Grants      []Grant           `json:"grants,omitempty"`
	AccessDrift []string          `json:"access_drift,omitempty"`
	// Публикация через прокси (поддомен), nil — не опубликовано.
	Web *WebRoute `json:"web,omitempty"`
	// Приложение из каталога (Kind "catalog"); nil — пользовательское.
	Catalog *CatalogRef `json:"catalog,omitempty"`
	Created time.Time   `json:"created"`
	Updated time.Time   `json:"updated"`
}

// ProjectView — приложение для API.
type ProjectView struct {
	Project
	State      string      `json:"state"`
	Containers []Container `json:"containers"`
	// User — учётка приложения; Legacy — «доступ как root» (перевести).
	User   string `json:"user,omitempty"`
	Legacy bool   `json:"legacy_root_share,omitempty"`
}

// ProjectDetail — детали: исходный compose и имена переменных (значения — нет).
type ProjectDetail struct {
	ProjectView
	Compose  string   `json:"compose"`
	EnvKeys  []string `json:"env_keys"`
	Normal   string   `json:"normalized"` // секреты замаскированы
	FilesErr string   `json:"files_error,omitempty"`
}

// Shares — общие папки NAS (nas.Service).
type Shares interface {
	ListShares(ctx context.Context) ([]nas.ShareInfo, error)
}

// ProjectRequest — создание/изменение/проверка (тело API).
type ProjectRequest struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Compose string  `json:"compose"`
	Env     *string `json:"env"` // nil при изменении — оставить текущие
	// Network — порты «доступно в сети» ("8080/tcp").
	Network []string `json:"network"`
	// Confirm — ключи подтверждённых рисков.
	Confirm []string `json:"confirm"`
	// RunAs — режим запуска сервисов (uid/puid/root); nil при изменении — прежний.
	RunAs map[string]string `json:"run_as"`
	// Grants — доступ к общим папкам: имя → ro|rw; nil при изменении — прежний.
	Grants map[string]string `json:"grants"`
	// Migrate — проверка перевода приложения «доступ как root» на свою учётку.
	Migrate bool `json:"migrate,omitempty"`
	// SecretKeys — ключи секретов приложения каталога: значения в
	// secrets-<сервис>.env, в compose — только environment KEY: ${KEY}. Не из API.
	SecretKeys map[string]bool `json:"-"`
}

// CheckResult — итог проверки (без секретов).
type CheckResult struct {
	compose.Result
	Conflicts  []compose.Issue `json:"conflicts"`
	Normalized string          `json:"normalized"`
	EnvKeys    []string        `json:"env_keys"`
	// Unconfirmed — риски, которые ещё нужно подтвердить.
	Unconfirmed []string `json:"unconfirmed"`
	OK          bool     `json:"ok"`
	// Shares — общие папки: имя → путь для bind в compose.
	Shares map[string]string `json:"shares"`
	// UID — учётка, от которой будет работать приложение (0 — как root).
	UID int `json:"uid"`
	// SharePools — пул каждой общей папки (для выдач).
	SharePools map[string]string `json:"-"`
}

type projState struct {
	mu      sync.Mutex
	pending map[string]pendingReq // токен → запрос с секретами
	ps      PSResult
	psAt    time.Time
	dies    map[string][]time.Time
	noise   map[string]DockerEvent // die 137 в ожидании restart
	lastSub string                 // состояние подсистемы при прошлой сверке приложений
	busy    map[string]bool        // приложения с выполняющейся задачей
	aclAt   time.Time              // последняя сверка ACL
}

type pendingReq struct {
	req   ProjectRequest
	env   string
	actor string
	at    time.Time
	cat   *catInstall // установка из каталога
}

// secretFiles — файлы секретов сервисов: только ключи, на которые сервис
// ссылается (SecretUse нормализации); только каталог.
func (p pendingReq) secretFiles(cr CheckResult) (map[string]string, error) {
	if p.cat == nil || len(cr.SecretUse) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for svc, keys := range cr.SecretUse {
		m := map[string]string{}
		for _, k := range keys {
			m[k] = p.cat.secrets[k]
		}
		t, err := catalog.EnvText(m)
		if err != nil {
			return nil, err
		}
		out[svc] = t
	}
	return out, nil
}

func (s *Service) initProjects() error {
	s.proj = &projState{pending: map[string]pendingReq{}, dies: map[string][]time.Time{}, noise: map[string]DockerEvent{}, busy: map[string]bool{}}
	if s.Tasks == nil {
		return nil
	}
	for k, fn := range map[string]tasks.RunFunc{TaskProjectCreate: s.runProjectCreate, TaskProjectUpdate: s.runProjectUpdate,
		TaskProjectAction: s.runProjectAction, TaskProjectDelete: s.runProjectDelete, TaskProjectGrant: s.runProjectGrants,
		TaskProjectMigrate: s.runProjectMigrate, TaskCatalogUpdate: s.runCatalogUpdate, TaskCatalogRollback: s.runCatalogRollback} {
		if err := s.Tasks.Register(k, fn); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) loadProjects(ctx context.Context) (map[string]Project, error) {
	m := map[string]Project{}
	var v string
	err := s.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", projectsKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	return m, json.Unmarshal([]byte(v), &m)
}

func (s *Service) saveProjects(ctx context.Context, m map[string]Project) error {
	b, _ := json.Marshal(m)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, projectsKey, string(b))
	return err
}

func (s *Service) putProject(ctx context.Context, p Project) error {
	m, err := s.loadProjects(ctx)
	if err != nil {
		return err
	}
	m[p.ID] = p
	return s.saveProjects(ctx, m)
}

func (s *Service) dropProject(ctx context.Context, id string) error {
	m, err := s.loadProjects(ctx)
	if err != nil {
		return err
	}
	delete(m, id)
	return s.saveProjects(ctx, m)
}

// procNet — /proc/net (тесты подменяют).
func (s *Service) procNet() string {
	if s.ProcNet != "" {
		return s.ProcNet
	}
	return "/proc/net"
}

// normContext — окружение нормализации: пути пула приложений и общих папок.
func (s *Service) normContext(ctx context.Context, id string, env map[string]string, network []string) (compose.Context, map[string]string, error) {
	pools := map[string]string{}
	nc, err := s.normContext0(ctx, id, env, network, pools)
	return nc, pools, err
}

func (s *Service) normContext0(ctx context.Context, id string, env map[string]string, network []string, pools map[string]string) (compose.Context, error) {
	c, err := s.load(ctx)
	if err != nil {
		return compose.Context{}, err
	}
	if !c.Enabled {
		return compose.Context{}, wrapf(ErrConflictSvc, "приложения не включены")
	}
	st, err := s.status(ctx)
	if err != nil {
		return compose.Context{}, wrapf(ErrUnavailable, "%v", err)
	}
	if st.MountRoot == "" {
		return compose.Context{}, wrapf(ErrUnavailable, "helper не сообщил каталог пулов")
	}
	nc := compose.Context{ID: id, MountRoot: st.MountRoot, Env: env, Network: map[string]bool{}, Shares: map[string]string{},
		AppData: filepath.Join(st.MountRoot, c.Pool, AppsDir, AppdataDir, id)}
	for _, k := range network {
		nc.Network[k] = true
	}
	if s.Shares != nil {
		sh, err := s.Shares.ListShares(ctx)
		if err != nil {
			return nc, fmt.Errorf("общие папки: %w", err)
		}
		for _, x := range sh {
			p := x.Path
			if p == "" {
				p = "shares/" + x.Name
			}
			nc.Shares[x.Name] = filepath.Join(st.MountRoot, x.Pool, p)
			pools[x.Name] = x.Pool
		}
	}
	return nc, nil
}

// envFor — переменные запроса или текущие файла проекта.
func (s *Service) envFor(ctx context.Context, req ProjectRequest) (string, error) {
	if req.Env != nil {
		return *req.Env, nil
	}
	var f ProjectFiles
	if err := s.Helper.Call(ctx, OpProjectRead, ProjectParams{ID: req.ID}, &f); err != nil {
		return "", err
	}
	return f.Env, nil
}

// check — нормализация + конфликты портов + неподтверждённые риски.
// access — учётка и доступ для нормализации.
type access struct {
	UID    int
	RunAs  map[string]string
	Grants map[string]string
}

// accessFor — учётка и доступ приложения: новое (и перевод «доступ как root») — свободный
// или закреплённый UID; существующее — свой UID; без учётки — 0.
func (s *Service) accessFor(ctx context.Context, req ProjectRequest, p *Project, migrate bool) (access, error) {
	a := access{RunAs: req.RunAs, Grants: req.Grants}
	if p != nil && !p.Removed && !migrate {
		a.UID = p.UID
		if a.UID == 0 {
			return access{}, nil
		}
		if a.RunAs == nil {
			a.RunAs = p.RunAs
		}
		if a.Grants == nil {
			a.Grants = grantMap(p.Grants)
		}
		return a, nil
	}
	uid, err := s.peekUID(ctx, req.ID)
	if err != nil {
		return a, err
	}
	a.UID = uid
	if a.Grants == nil {
		a.Grants = map[string]string{}
	}
	return a, nil
}

func (s *Service) check(ctx context.Context, req ProjectRequest, env string, prev []ConfirmedRisk, acc access) (CheckResult, error) {
	var cr CheckResult
	if !ValidID(req.ID) {
		return cr, wrapf(ErrInvalidSvc, "идентификатор: латиница в нижнем регистре, цифры и '-', до 32 символов, начинается с буквы")
	}
	ev, err := compose.ParseEnv(env)
	if err != nil {
		return cr, wrapf(ErrInvalidSvc, "%v", err)
	}
	nc, pools, err := s.normContext(ctx, req.ID, ev, req.Network)
	if err != nil {
		return cr, err
	}
	nc.UID, nc.GID, nc.RunAs, nc.Grants = acc.UID, acc.UID, acc.RunAs, acc.Grants
	nc.Secrets = req.SecretKeys
	for sh, m := range acc.Grants {
		if _, ok := pools[sh]; !ok {
			return cr, wrapf(ErrInvalidSvc, "доступ: общей папки %s нет", sh)
		}
		if m != compose.GrantRO && m != compose.GrantRW {
			return cr, wrapf(ErrInvalidSvc, "доступ к папке %s: %q (ro или rw)", sh, m)
		}
	}
	cr.UID, cr.SharePools = acc.UID, pools
	res, err := compose.Normalize([]byte(req.Compose), nc)
	if err != nil {
		return cr, err
	}
	cr.Result = res
	cr.Shares = nc.Shares
	cr.EnvKeys = sortedKeys(ev)
	cr.Conflicts = []compose.Issue{}
	cr.Unconfirmed = []string{}
	projects, err := s.loadProjects(ctx)
	if err != nil {
		return cr, err
	}
	if res.OK() {
		cr.Conflicts = portConflicts(req.ID, res.Ports, projects, Listening(s.procNet()))
		cr.Normalized = maskSecrets(string(res.Normalized), ev)
	}
	ok := map[string]bool{}
	for _, k := range req.Confirm {
		ok[k] = true
	}
	for _, r := range prev {
		ok[r.Key] = true
	}
	for _, r := range res.Risks {
		if !ok[r.Key] {
			cr.Unconfirmed = append(cr.Unconfirmed, r.Key)
		}
	}
	cr.OK = res.OK() && len(cr.Conflicts) == 0 && len(cr.Unconfirmed) == 0
	return cr, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// maskSecrets заменяет значения переменных .env (от 3 символов) на ***.
func maskSecrets(text string, env map[string]string) string {
	vals := make([]string, 0, len(env))
	for _, v := range env {
		if len(v) >= 3 {
			vals = append(vals, v, strings.ReplaceAll(v, "$", "$$"))
		}
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		text = strings.ReplaceAll(text, v, "***")
	}
	return text
}

// Check — проверка без побочных эффектов (POST /apps/projects/check).
func (s *Service) Check(ctx context.Context, req ProjectRequest) (CheckResult, error) {
	projects, err := s.loadProjects(ctx)
	if err != nil {
		return CheckResult{}, err
	}
	var prev []ConfirmedRisk
	var cur *Project
	if p, ok := projects[req.ID]; ok {
		cur = &p
		if !p.Removed {
			prev = p.Risks
		}
	}
	env := ""
	if req.Env != nil {
		env = *req.Env
	} else if _, ok := projects[req.ID]; ok {
		if env, err = s.envFor(ctx, req); err != nil {
			return CheckResult{}, wrapf(ErrUnavailable, "%v", err)
		}
	}
	acc, err := s.accessFor(ctx, req, cur, req.Migrate)
	if err != nil {
		return CheckResult{}, err
	}
	if req.Migrate {
		prev = withoutShareBind(prev)
	}
	return s.check(ctx, req, env, prev, acc)
}

// withoutShareBind — подтверждения без share_bind (перевод: доступ
// «как root» заменяется выдачами).
func withoutShareBind(l []ConfirmedRisk) []ConfirmedRisk {
	out := []ConfirmedRisk{}
	for _, r := range l {
		if r.Code != compose.RShareBind {
			out = append(out, r)
		}
	}
	return out
}

func (s *Service) needRunning() error {
	if v := s.View(); v.State != StateRunning {
		return wrapf(ErrConflictSvc, "Docker не работает (%s)", v.State)
	}
	return nil
}

func token() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type projTask struct {
	ID       string `json:"id"`
	Token    string `json:"token,omitempty"`
	Action   string `json:"action,omitempty"`
	KeepData bool   `json:"keep_data,omitempty"`
	Actor    string `json:"actor,omitempty"`
}

func (s *Service) submit(ctx context.Context, kind string, t projTask) (tasks.Task, error) {
	raw, _ := json.Marshal(t)
	return s.Tasks.Submit(ctx, kind, raw)
}

func (s *Service) stash(req ProjectRequest, env, actor string) string {
	tok := token()
	s.proj.mu.Lock()
	defer s.proj.mu.Unlock()
	for k, p := range s.proj.pending {
		if time.Since(p.at) > time.Hour {
			delete(s.proj.pending, k)
		}
	}
	s.proj.pending[tok] = pendingReq{req: req, env: env, actor: actor, at: time.Now()}
	return tok
}

func (s *Service) unstash(tok string) (pendingReq, bool) {
	s.proj.mu.Lock()
	defer s.proj.mu.Unlock()
	p, ok := s.proj.pending[tok]
	delete(s.proj.pending, tok)
	return p, ok
}

func validName(n string) error {
	if strings.TrimSpace(n) == "" || len([]rune(n)) > 64 || strings.ContainsAny(n, "\x00\n\r") {
		return wrapf(ErrInvalidSvc, "название: от 1 до 64 символов")
	}
	return nil
}

// CreateProject проверяет запрос и ставит задачу создания.
func (s *Service) CreateProject(ctx context.Context, req ProjectRequest, actor string) (tasks.Task, error) {
	if err := validName(req.Name); err != nil {
		return tasks.Task{}, err
	}
	if req.Env == nil {
		e := ""
		req.Env = &e
	}
	projects, err := s.loadProjects(ctx)
	if err != nil {
		return tasks.Task{}, err
	}
	if p, ok := projects[req.ID]; ok && !p.Removed {
		return tasks.Task{}, wrapf(ErrConflictSvc, "приложение %s уже есть", req.ID)
	}
	if err := s.needRunning(); err != nil {
		return tasks.Task{}, err
	}
	req.Migrate = false
	acc, err := s.accessFor(ctx, req, nil, false)
	if err != nil {
		return tasks.Task{}, err
	}
	cr, err := s.check(ctx, req, *req.Env, nil, acc)
	if err != nil {
		return tasks.Task{}, err
	}
	if err := checkErr(cr); err != nil {
		return tasks.Task{}, err
	}
	return s.submit(ctx, TaskProjectCreate, projTask{ID: req.ID, Token: s.stash(req, *req.Env, actor), Actor: actor})
}

// UpdateProject — изменение compose/переменных/портов.
func (s *Service) UpdateProject(ctx context.Context, req ProjectRequest, actor string) (tasks.Task, error) {
	req, env, cr, cat, err := s.updateCheck(ctx, req)
	if err != nil {
		return tasks.Task{}, err
	}
	if err := checkErr(cr); err != nil {
		return tasks.Task{}, err
	}
	tok := s.stash(req, env, actor)
	if cat != nil {
		s.proj.mu.Lock()
		st := s.proj.pending[tok]
		st.cat = cat
		s.proj.pending[tok] = st
		s.proj.mu.Unlock()
	}
	return s.submit(ctx, TaskProjectUpdate, projTask{ID: req.ID, Token: tok, Actor: actor})
}

// updateCheck — проверка изменения приложения без побочных эффектов.
func (s *Service) updateCheck(ctx context.Context, req ProjectRequest) (ProjectRequest, string, CheckResult, *catInstall, error) {
	fail := func(err error) (ProjectRequest, string, CheckResult, *catInstall, error) {
		return req, "", CheckResult{}, nil, err
	}
	projects, err := s.loadProjects(ctx)
	if err != nil {
		return fail(err)
	}
	p, ok := projects[req.ID]
	if !ok || p.Removed {
		return fail(wrapf(ErrNotFound, "приложение %s", req.ID))
	}
	if req.Name == "" {
		req.Name = p.Name
	}
	if err := validName(req.Name); err != nil {
		return fail(err)
	}
	if err := s.needRunning(); err != nil {
		return fail(err)
	}
	env, err := s.envFor(ctx, req)
	if err != nil {
		return fail(wrapf(ErrUnavailable, "%v", err))
	}
	req.Migrate = false
	// У приложения каталога секреты — в файлах сервисов; сохраняются.
	var cat *catInstall
	if p.Catalog != nil {
		var f ProjectFiles
		if err := s.Helper.Call(ctx, OpProjectRead, ProjectParams{ID: p.ID}, &f); err != nil {
			return fail(wrapf(ErrUnavailable, "%v", err))
		}
		sec, err := currentSecrets(f)
		if err != nil {
			return fail(wrapf(ErrUnavailable, "%v", err))
		}
		req.SecretKeys = map[string]bool{}
		for _, k := range p.Catalog.Secrets {
			req.SecretKeys[k] = true
		}
		cat = &catInstall{secrets: sec, ref: *p.Catalog}
	}
	acc, err := s.accessFor(ctx, req, &p, false)
	if err != nil {
		return fail(err)
	}
	cr, err := s.check(ctx, req, env, p.Risks, acc)
	if err != nil {
		return fail(err)
	}
	return req, env, cr, cat, nil
}

// MigrateProject — перевод приложения «доступ как root» на свою
// учётку: compose и переменные прежние, режим запуска и выдачи — из запроса.
func (s *Service) MigrateProject(ctx context.Context, id string, runAs, grants map[string]string, confirm []string, actor string) (tasks.Task, error) {
	projects, err := s.loadProjects(ctx)
	if err != nil {
		return tasks.Task{}, err
	}
	p, ok := projects[id]
	if !ok || p.Removed {
		return tasks.Task{}, wrapf(ErrNotFound, "приложение %s", id)
	}
	if p.UID != 0 {
		return tasks.Task{}, wrapf(ErrConflictSvc, "приложение %s уже работает от своей учётки", id)
	}
	if err := s.needRunning(); err != nil {
		return tasks.Task{}, err
	}
	req, env, err := s.currentRequest(ctx, p)
	if err != nil {
		return tasks.Task{}, err
	}
	req.RunAs, req.Grants, req.Migrate, req.Confirm = runAs, grants, true, confirm
	if req.Grants == nil {
		req.Grants = map[string]string{}
	}
	acc, err := s.accessFor(ctx, req, &p, true)
	if err != nil {
		return tasks.Task{}, err
	}
	cr, err := s.check(ctx, req, env, withoutShareBind(p.Risks), acc)
	if err != nil {
		return tasks.Task{}, err
	}
	if err := checkErr(cr); err != nil {
		return tasks.Task{}, err
	}
	return s.submit(ctx, TaskProjectMigrate, projTask{ID: id, Token: s.stash(req, env, actor), Actor: actor})
}

// SetGrants — изменить только доступ к папкам (и режим запуска): compose прежний.
func (s *Service) SetGrants(ctx context.Context, id string, runAs, grants map[string]string, actor string) (tasks.Task, error) {
	projects, err := s.loadProjects(ctx)
	if err != nil {
		return tasks.Task{}, err
	}
	p, ok := projects[id]
	if !ok || p.Removed {
		return tasks.Task{}, wrapf(ErrNotFound, "приложение %s", id)
	}
	if p.UID == 0 {
		return tasks.Task{}, wrapf(ErrConflictSvc, "приложение %s работает с доступом как root — сначала переведите его на свою учётку", id)
	}
	req, _, err := s.currentRequest(ctx, p)
	if err != nil {
		return tasks.Task{}, err
	}
	req.RunAs, req.Grants = runAs, grants
	if req.Grants == nil {
		req.Grants = map[string]string{}
	}
	req.Env = nil
	return s.UpdateProject(ctx, req, actor)
}

// currentRequest — запрос с текущими compose, переменными, портами, подтверждениями.
func (s *Service) currentRequest(ctx context.Context, p Project) (ProjectRequest, string, error) {
	var f ProjectFiles
	if err := s.Helper.Call(ctx, OpProjectRead, ProjectParams{ID: p.ID}, &f); err != nil {
		return ProjectRequest{}, "", wrapf(ErrUnavailable, "%v", err)
	}
	env := f.Env
	return ProjectRequest{ID: p.ID, Name: p.Name, Compose: f.UserCompose, Env: &env, Network: append([]string{}, p.Network...)}, env, nil
}

func checkErr(cr CheckResult) error {
	switch {
	case len(cr.Forbidden) > 0:
		return wrapf(ErrInvalidSvc, "compose: %s", cr.Forbidden[0].Detail)
	case len(cr.Conflicts) > 0:
		return wrapf(ErrConflictSvc, "%s", cr.Conflicts[0].Detail)
	case len(cr.Unconfirmed) > 0:
		return wrapf(ErrConflictSvc, "не подтверждены риски: %s", strings.Join(cr.Unconfirmed, ", "))
	}
	return nil
}

// ProjectActionReq — пуск/стоп/перезапуск.
func (s *Service) ProjectAction(ctx context.Context, id, action, actor string) (tasks.Task, error) {
	switch action {
	case ActStart, ActStop, ActRestart:
	default:
		return tasks.Task{}, wrapf(ErrInvalidSvc, "действие %s", action)
	}
	projects, err := s.loadProjects(ctx)
	if err != nil {
		return tasks.Task{}, err
	}
	if p, ok := projects[id]; !ok || p.Removed {
		return tasks.Task{}, wrapf(ErrNotFound, "приложение %s", id)
	}
	if err := s.needRunning(); err != nil {
		return tasks.Task{}, err
	}
	return s.submit(ctx, TaskProjectAction, projTask{ID: id, Action: action, Actor: actor})
}

// DeleteProject — удаление; keepData — сохранить субтом данных.
func (s *Service) DeleteProject(ctx context.Context, id string, keepData bool, actor string) (tasks.Task, error) {
	projects, err := s.loadProjects(ctx)
	if err != nil {
		return tasks.Task{}, err
	}
	p, ok := projects[id]
	if !ok || p.Removed && keepData {
		return tasks.Task{}, wrapf(ErrNotFound, "приложение %s", id)
	}
	if err := s.needRunning(); err != nil {
		return tasks.Task{}, err
	}
	return s.submit(ctx, TaskProjectDelete, projTask{ID: id, KeepData: keepData, Actor: actor})
}

// ---------- задачи ----------

func (s *Service) compose(ctx context.Context, id, action, sum string, rmVol bool) error {
	var r ComposeResult
	err := s.Helper.Call(ctx, OpProjectCompose, ComposeParams{ID: id, Action: action, SHA256: sum, RemoveVolumes: rmVol}, &r)
	if err != nil && r.Output != "" {
		s.Logger.Warn("приложения: docker compose", "app", id, "action", action, "output", r.Output)
	}
	return err
}

// quiet — действия gamayund над контейнерами: die 0/137/143 — не сбой.
func (s *Service) quiet(d time.Duration) func() {
	s.evMu.Lock()
	s.stopFrom, s.stopTo = s.now(), s.now().Add(d)
	s.evMu.Unlock()
	return func() {
		s.evMu.Lock()
		s.stopTo = s.now().Add(10 * time.Second)
		s.evMu.Unlock()
	}
}

func (s *Service) taskParams(raw json.RawMessage) (projTask, pendingReq, error) {
	var t projTask
	if err := json.Unmarshal(raw, &t); err != nil || !ValidID(t.ID) {
		return t, pendingReq{}, fmt.Errorf("параметры задачи: %v", err)
	}
	if t.Token == "" {
		return t, pendingReq{}, nil
	}
	p, ok := s.unstash(t.Token)
	if !ok {
		return t, p, errors.New("запрос устарел (перезапуск gamayund?) — повторите действие")
	}
	return t, p, nil
}

func risksOf(cr CheckResult, prev []ConfirmedRisk, actor string, now time.Time) (all, fresh []ConfirmedRisk) {
	old := map[string]ConfirmedRisk{}
	for _, r := range prev {
		old[r.Key] = r
	}
	for _, r := range cr.Risks {
		if o, ok := old[r.Key]; ok {
			all = append(all, o)
			continue
		}
		c := ConfirmedRisk{Key: r.Key, Code: r.Code, Detail: r.Detail, By: actor, At: now.UTC()}
		all = append(all, c)
		fresh = append(fresh, c)
	}
	return
}

func (s *Service) emitRisks(ctx context.Context, id, actor string, fresh []ConfirmedRisk) {
	if len(fresh) == 0 {
		return
	}
	s.emit(ctx, EvRiskConfirmed, events.SevWarning, "app:"+id, map[string]any{"app": id, "actor": actor, "risks": fresh})
}

// runProjectCreate: учётка → доступ к папкам → данные → файлы → up
// (pull, ожидание running/healthy). Сбой — откат: down -v, файлы, созданный
// субтом данных, выдачи, учётка; failed с причиной. Правка ACL — без s.mu.
func (s *Service) runProjectCreate(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	t, pr, err := s.taskParams(raw)
	if err != nil {
		return nil, err
	}
	if err := s.lockApp(t.ID); err != nil {
		return nil, err
	}
	defer s.unlockApp(t.ID)
	if err := s.needRunning(); err != nil {
		return nil, err
	}
	projects, err := s.loadProjects(ctx)
	if err != nil {
		return nil, err
	}
	old, existed := projects[t.ID]
	if existed && !old.Removed {
		return nil, wrapf(ErrConflictSvc, "приложение %s уже есть", t.ID)
	}
	uid, freshUID, err := s.reserveUID(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	acc, err := s.accessFor(ctx, pr.req, nil, false)
	if err == nil {
		acc.UID = uid
	}
	var cr CheckResult
	if err == nil {
		cr, err = s.check(ctx, pr.req, pr.env, nil, acc)
	}
	if err == nil && pr.cat != nil {
		catalogVerify(&cr, pr.cat)
	}
	if err == nil {
		err = checkErr(cr)
	}
	now := s.now().UTC()
	var grants []Grant
	if err == nil {
		grants, err = buildGrants(acc.Grants, nil, cr.SharePools, pr.actor, now)
	}
	if err != nil {
		if freshUID {
			s.releaseUID(context.WithoutCancel(ctx), t.ID, false)
		}
		return nil, err
	}
	// Откат учётки и доступа (вне s.mu).
	var userMade bool
	var applied []Grant
	undoAccess := func(bg context.Context) []string {
		var errs []string
		if len(applied) > 0 {
			if e := s.revokeAll(bg, t.ID, uid, applied); e != nil {
				errs = append(errs, "доступ: "+e.Error())
			}
		}
		if userMade {
			var ur UserResult
			if e := s.Helper.Call(bg, OpUserDelete, UserParams{ID: t.ID, UID: uid}, &ur); e != nil {
				errs = append(errs, "учётка: "+e.Error())
			}
		}
		if freshUID && len(errs) == 0 {
			s.releaseUID(bg, t.ID, false)
		}
		return errs
	}
	failAccess := func(stage string, err error, extra []string) (any, error) {
		bg := context.WithoutCancel(ctx)
		errs := append(extra, undoAccess(bg)...)
		s.emit(bg, EvAppFailed, events.SevWarning, "app:"+t.ID, map[string]any{"app": t.ID, "stage": stage, "reason": err.Error()})
		if len(errs) > 0 {
			return nil, fmt.Errorf("%s: %v; откат не полный: %s", stage, err, strings.Join(errs, "; "))
		}
		return nil, fmt.Errorf("%s: %v; откат выполнен, система в исходном состоянии", stage, err)
	}
	var ur UserResult
	if err := s.Helper.Call(ctx, OpUserEnsure, UserParams{ID: t.ID, UID: uid}, &ur); err != nil {
		return failAccess("учётка приложения", err, nil)
	}
	userMade = ur.Changed
	progress(5)
	steps, _ := grantDiff(nil, grants)
	if err := s.applySteps(ctx, t.ID, uid, steps, nil, progress, 5, 40); err != nil {
		return failAccess("доступ к общим папкам", err, nil)
	}
	applied = grants

	s.mu.Lock()
	p, stage, err := s.createDocker(ctx, t, pr, cr, uid, grants, old, existed, progress)
	s.mu.Unlock()
	if err != nil {
		return failAccess(stage, err, p.Error2)
	}
	s.emit(ctx, EvAppCreated, events.SevInfo, "app:"+t.ID, map[string]any{"app": t.ID, "name": p.Name, "actor": pr.actor, "uid": uid})
	return s.projectView(p.Project), nil
}

// createResult — итог шагов Docker создания (Error2 — ошибки их отката).
type createResult struct {
	Project
	Error2 []string
}

// createDocker — шаги создания под s.mu; при сбое откатывает только их.
func (s *Service) createDocker(ctx context.Context, t projTask, pr pendingReq, cr CheckResult, uid int, grants []Grant,
	old Project, existed bool, progress func(int)) (createResult, string, error) {
	var res createResult
	if err := s.needRunning(); err != nil {
		return res, "запуск", err
	}
	var ad AppdataResult
	if err := s.Helper.Call(ctx, OpAppdataCreate, AppdataParams{ID: t.ID}, &ad); err != nil {
		return res, "данные приложения", err
	}
	sum := SHA256(cr.Result.Normalized)
	fail := func(stage string, err error) (createResult, string, error) {
		bg := context.WithoutCancel(ctx)
		done := s.quiet(5 * time.Minute)
		if e := s.compose(bg, t.ID, ActDown, "", true); e != nil {
			res.Error2 = append(res.Error2, "down: "+e.Error())
		}
		done()
		var r ProjectResult
		if e := s.Helper.Call(bg, OpProjectDelete, ProjectParams{ID: t.ID}, &r); e != nil {
			res.Error2 = append(res.Error2, "файлы: "+e.Error())
		}
		if ad.Created {
			if e := s.Helper.Call(bg, OpAppdataDelete, AppdataParams{ID: t.ID}, &ad); e != nil {
				res.Error2 = append(res.Error2, "данные: "+e.Error())
			}
		}
		return res, stage, err
	}
	// Данные новые или остались от приложения «доступ как root» — владелец учётка.
	if uid > 0 && (ad.Created || existed && old.UID == 0) {
		var o OwnResult
		if err := s.Helper.Call(ctx, OpAppdataOwn, OwnParams{ID: t.ID, UID: uid}, &o); err != nil {
			return fail("владелец данных приложения", err)
		}
	}
	progress(45)
	secrets, err := pr.secretFiles(cr)
	if err != nil {
		return fail("секреты", err)
	}
	var w ProjectResult
	if err := s.Helper.Call(ctx, OpProjectWrite, ProjectWriteParams{ID: t.ID, UserCompose: pr.req.Compose, Env: pr.env,
		Secrets: secrets, Normalized: string(cr.Result.Normalized), SHA256: sum, Mkdirs: cr.Mkdirs, Owner: uid}, &w); err != nil {
		return fail("запись файлов", err)
	}
	progress(55)
	if err := s.compose(ctx, t.ID, ActUp, sum, false); err != nil {
		return fail("запуск", err)
	}
	if pr.cat != nil {
		progress(70)
		if err := s.waitReady(ctx, t.ID, pr.cat.m.Health); err != nil {
			return fail("готовность приложения", err)
		}
	}
	progress(90)
	now := s.now().UTC()
	all, fresh := risksOf(cr, nil, pr.actor, now)
	p := Project{ID: t.ID, Name: pr.req.Name, Kind: "user", Desired: "running", SHA256: sum, Ports: cr.Ports,
		Network: append([]string{}, pr.req.Network...), Risks: all, Services: cr.Services, NoHealth: cr.NoHealth, Created: now, Updated: now,
		UID: uid, RunAs: cr.RunAs, Grants: grants}
	if existed {
		p.Created = old.Created
		p.Web = keepWeb(old.Web, p.Ports) // публикация сохраняется, если порт остался
	}
	if pr.cat != nil {
		ref := pr.cat.ref
		p.Kind, p.Catalog = "catalog", &ref
		if all, err := s.loadProjects(ctx); err == nil {
			p.Web = s.catalogWeb(t.ID, pr.cat.m, p.Ports, all)
		}
	}
	if err := s.putProject(ctx, p); err != nil {
		return fail("запись приложения", err)
	}
	if existed && old.Web != nil || p.Web != nil {
		s.routesChanged()
	}
	s.emitRisks(ctx, t.ID, pr.actor, fresh)
	s.refreshPS(ctx)
	res.Project = p
	return res, "", nil
}

// runProjectUpdate: новые выдачи → файлы с резервной копией → up →
// отзыв снятых выдач; сбой — прежние файлы, up, прежние выдачи.
func (s *Service) runProjectUpdate(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	return s.runUpdate(ctx, raw, progress, false)
}

// runProjectMigrate — перевод приложения «доступ как root» на свою
// учётку: учётка → владелец данных → выдачи → compose с user/PUID → up.
// Сбой — прежние файлы и up, выдачи сняты, данные — снова root, учётки нет.
func (s *Service) runProjectMigrate(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	return s.runUpdate(ctx, raw, progress, true)
}

func (s *Service) runUpdate(ctx context.Context, raw json.RawMessage, progress func(int), migrate bool) (any, error) {
	t, pr, err := s.taskParams(raw)
	if err != nil {
		return nil, err
	}
	if err := s.lockApp(t.ID); err != nil {
		return nil, err
	}
	defer s.unlockApp(t.ID)
	if err := s.needRunning(); err != nil {
		return nil, err
	}
	projects, err := s.loadProjects(ctx)
	if err != nil {
		return nil, err
	}
	old, ok := projects[t.ID]
	if !ok || old.Removed {
		return nil, wrapf(ErrNotFound, "приложение %s", t.ID)
	}
	if migrate && old.UID != 0 {
		return nil, wrapf(ErrConflictSvc, "приложение %s уже работает от своей учётки", t.ID)
	}
	uid, freshUID := old.UID, false
	if migrate {
		if uid, freshUID, err = s.reserveUID(ctx, t.ID); err != nil {
			return nil, err
		}
	}
	prevRisks := old.Risks
	if migrate {
		prevRisks = withoutShareBind(old.Risks)
	}
	acc, err := s.accessFor(ctx, pr.req, &old, migrate)
	if err == nil && migrate {
		acc.UID = uid
	}
	var cr CheckResult
	if err == nil {
		cr, err = s.check(ctx, pr.req, pr.env, prevRisks, acc)
	}
	if err == nil {
		err = checkErr(cr)
	}
	var grants []Grant
	if err == nil && uid > 0 {
		grants, err = buildGrants(acc.Grants, old.Grants, cr.SharePools, pr.actor, s.now())
	}
	if err != nil {
		if freshUID {
			s.releaseUID(context.WithoutCancel(ctx), t.ID, false)
		}
		return nil, err
	}
	apply, revoke := grantDiff(old.Grants, grants)
	prev := byShare(old.Grants)
	var userMade, owned bool
	// undo — откат учётки/доступа/владельца (вне s.mu).
	undo := func(bg context.Context) []string {
		var errs []string
		if e := s.undoSteps(bg, t.ID, uid, apply, prev); e != nil {
			errs = append(errs, "доступ: "+e.Error())
		}
		if owned {
			var o OwnResult
			if e := s.Helper.Call(bg, OpAppdataOwn, OwnParams{ID: t.ID, UID: 0}, &o); e != nil {
				errs = append(errs, "владелец данных: "+e.Error())
			}
		}
		if userMade {
			var ur UserResult
			if e := s.Helper.Call(bg, OpUserDelete, UserParams{ID: t.ID, UID: uid}, &ur); e != nil {
				errs = append(errs, "учётка: "+e.Error())
			}
		}
		if freshUID && len(errs) == 0 {
			s.releaseUID(bg, t.ID, false)
		}
		return errs
	}
	what := "изменение"
	if migrate {
		what = "перевод на свою учётку"
	}
	failed := func(stage string, err error, extra []string) (any, error) {
		bg := context.WithoutCancel(ctx)
		errs := append(extra, undo(bg)...)
		s.emit(bg, EvAppFailed, events.SevWarning, "app:"+t.ID, map[string]any{"app": t.ID, "stage": what + ": " + stage, "reason": err.Error()})
		if len(errs) > 0 {
			return nil, fmt.Errorf("%s: %v; откат не полный: %s", stage, err, strings.Join(errs, "; "))
		}
		return nil, fmt.Errorf("%s: %v; возвращена прежняя версия", stage, err)
	}
	if migrate {
		var ur UserResult
		if err := s.Helper.Call(ctx, OpUserEnsure, UserParams{ID: t.ID, UID: uid}, &ur); err != nil {
			return failed("учётка приложения", err, nil)
		}
		userMade = ur.Changed
		s.mu.Lock()
		var o OwnResult
		err := s.Helper.Call(ctx, OpAppdataOwn, OwnParams{ID: t.ID, UID: uid}, &o)
		s.mu.Unlock()
		owned = true // частичная смена тоже откатывается
		if err != nil {
			return failed("владелец данных приложения", err, nil)
		}
	}
	progress(5)
	if err := s.applySteps(ctx, t.ID, uid, apply, prev, progress, 5, 40); err != nil {
		apply = nil // applySteps откатил сам
		return failed("доступ к общим папкам", err, nil)
	}
	s.mu.Lock()
	p, errs, err := s.updateDocker(ctx, t, pr, cr, old, uid, grants, progress)
	s.mu.Unlock()
	if err != nil {
		return failed("запуск", err, errs)
	}
	// Снятые выдачи — после успешного запуска (приложение уже без этих папок).
	if len(revoke) > 0 {
		if err := s.applySteps(ctx, t.ID, uid, revoke, nil, progress, 90, 99); err != nil {
			s.emit(ctx, EvAppFailed, events.SevWarning, "app:"+t.ID, map[string]any{"app": t.ID, "stage": "отзыв доступа", "reason": err.Error()})
			return nil, fmt.Errorf("приложение изменено, но доступ к папкам не отозван: %v", err)
		}
	}
	if len(apply)+len(revoke) > 0 {
		s.emit(ctx, EvAccessChanged, events.SevInfo, "app:"+t.ID, map[string]any{"app": t.ID, "actor": pr.actor, "grants": grants})
	}
	if migrate {
		s.emit(ctx, EvAppMigrated, events.SevInfo, "app:"+t.ID, map[string]any{"app": t.ID, "actor": pr.actor, "uid": uid})
	} else {
		s.emit(ctx, EvAppUpdated, events.SevInfo, "app:"+t.ID, map[string]any{"app": t.ID, "actor": pr.actor})
	}
	return s.projectView(p), nil
}

// updateDocker — запись файлов и up под s.mu; сбой — прежние файлы и up.
func (s *Service) updateDocker(ctx context.Context, t projTask, pr pendingReq, cr CheckResult, old Project, uid int,
	grants []Grant, progress func(int)) (Project, []string, error) {
	if err := s.needRunning(); err != nil {
		return old, nil, err
	}
	sum := SHA256(cr.Result.Normalized)
	secrets, err := pr.secretFiles(cr)
	if err != nil {
		return old, nil, fmt.Errorf("секреты: %w", err)
	}
	var w ProjectResult
	if err := s.Helper.Call(ctx, OpProjectWrite, ProjectWriteParams{ID: t.ID, UserCompose: pr.req.Compose, Env: pr.env,
		Secrets: secrets, Normalized: string(cr.Result.Normalized), SHA256: sum, Mkdirs: cr.Mkdirs, Backup: true, Owner: uid}, &w); err != nil {
		return old, nil, fmt.Errorf("запись файлов: %w", err)
	}
	progress(50)
	done := s.quiet(5 * time.Minute)
	err = s.compose(ctx, t.ID, ActUp, sum, false)
	done()
	if err != nil {
		bg := context.WithoutCancel(ctx)
		var r ProjectResult
		if e := s.Helper.Call(bg, OpProjectRevert, ProjectParams{ID: t.ID}, &r); e != nil {
			return old, []string{"прежняя версия не возвращена: " + e.Error()}, err
		}
		if old.Desired == "running" && !old.Crashing {
			// прежние выдачи вернёт откат доступа; прежний compose — сейчас
			if e := s.compose(bg, t.ID, ActUp, old.SHA256, false); e != nil {
				return old, []string{"прежняя версия возвращена, но не запустилась: " + e.Error()}, err
			}
		}
		return old, nil, err
	}
	progress(90)
	all, fresh := risksOf(cr, old.Risks, pr.actor, s.now())
	p := old
	p.Name, p.SHA256, p.Ports, p.Network, p.Risks = pr.req.Name, sum, cr.Ports, append([]string{}, pr.req.Network...), all
	p.Services, p.NoHealth, p.Desired, p.Crashing, p.Error, p.Updated = cr.Services, cr.NoHealth, "running", false, "", s.now().UTC()
	p.UID, p.Grants, p.AccessDrift = uid, grants, nil
	p.RunAs = cr.RunAs
	if uid == 0 {
		p.RunAs, p.Grants = nil, nil
	}
	if err := s.putProject(ctx, p); err != nil {
		return old, nil, err
	}
	s.clearDies(t.ID)
	s.emitRisks(ctx, t.ID, pr.actor, fresh)
	s.refreshPS(ctx)
	return p, nil, nil
}

func (s *Service) runProjectAction(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	t, _, err := s.taskParams(raw)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.needRunning(); err != nil {
		return nil, err
	}
	projects, err := s.loadProjects(ctx)
	if err != nil {
		return nil, err
	}
	p, ok := projects[t.ID]
	if !ok || p.Removed {
		return nil, wrapf(ErrNotFound, "приложение %s", t.ID)
	}
	act := t.Action
	if act == ActStart {
		act = ActUp // контейнеров может не быть (после down/сбоя)
	}
	done := s.quiet(5 * time.Minute)
	err = s.compose(ctx, t.ID, act, p.SHA256, false)
	done()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.Action, err)
	}
	if t.Action == ActStop {
		p.Desired = "stopped"
		s.emit(ctx, EvAppStopped, events.SevInfo, "app:"+t.ID, map[string]any{"app": t.ID, "actor": t.Actor})
	} else {
		p.Desired, p.Crashing, p.Error = "running", false, ""
		s.clearDies(t.ID)
		s.emit(ctx, EvAppStarted, events.SevInfo, "app:"+t.ID, map[string]any{"app": t.ID, "action": t.Action, "actor": t.Actor})
	}
	p.Updated = s.now().UTC()
	if err := s.putProject(ctx, p); err != nil {
		return nil, err
	}
	s.refreshPS(ctx)
	return s.projectView(p), nil
}

func (s *Service) runProjectDelete(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	t, _, err := s.taskParams(raw)
	if err != nil {
		return nil, err
	}
	if err := s.lockApp(t.ID); err != nil {
		return nil, err
	}
	defer s.unlockApp(t.ID)
	p, err := s.deleteDocker(ctx, t, progress)
	if err != nil {
		return nil, err
	}
	// Доступ и учётка — после остановки контейнеров, вне s.mu.
	if p.UID > 0 && (len(p.Grants) > 0 || !p.Removed || !t.KeepData) {
		if err := s.revokeAll(ctx, t.ID, p.UID, p.Grants); err != nil {
			return nil, fmt.Errorf("приложение удалено, но доступ к общим папкам не отозван: %v — повторите удаление", err)
		}
		var ur UserResult
		if err := s.Helper.Call(ctx, OpUserDelete, UserParams{ID: t.ID, UID: p.UID}, &ur); err != nil {
			return nil, fmt.Errorf("приложение удалено, но учётка %s не удалена: %v — повторите удаление", AppUser(t.ID), err)
		}
		s.mu.Lock()
		if m, err := s.loadProjects(ctx); err == nil {
			if q, ok := m[t.ID]; ok {
				q.Grants = nil
				s.putProject(ctx, q)
			}
		}
		s.mu.Unlock()
		if !t.KeepData {
			// Данных нет — UID больше не выдаётся (файлы в общих папках остались).
			s.releaseUID(ctx, t.ID, true)
		}
	}
	s.emit(ctx, EvAppDeleted, events.SevInfo, "app:"+t.ID, map[string]any{"app": t.ID, "keep_data": t.KeepData, "actor": t.Actor})
	return map[string]any{"id": t.ID, "keep_data": t.KeepData}, nil
}

// deleteDocker — down, файлы, данные под s.mu; запись — «удалено» или убрана.
func (s *Service) deleteDocker(ctx context.Context, t projTask, progress func(int)) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	projects, err := s.loadProjects(ctx)
	if err != nil {
		return Project{}, err
	}
	p, ok := projects[t.ID]
	if !ok {
		return p, wrapf(ErrNotFound, "приложение %s", t.ID)
	}
	if !p.Removed {
		done := s.quiet(5 * time.Minute)
		err := s.compose(ctx, t.ID, ActDown, "", !t.KeepData)
		done()
		if err != nil {
			return p, fmt.Errorf("остановка и удаление контейнеров: %w; приложение не удалено", err)
		}
		var r ProjectResult
		if err := s.Helper.Call(ctx, OpProjectDelete, ProjectParams{ID: t.ID}, &r); err != nil {
			return p, fmt.Errorf("файлы приложения: %w", err)
		}
	}
	progress(60)
	if t.KeepData {
		q := p
		q.Removed, q.Desired, q.Ports, q.Network, q.Crashing, q.Error, q.Updated = true, "stopped", nil, nil, false, "", s.now().UTC()
		q.Web = nil
		if err := s.putProject(ctx, q); err != nil {
			return p, err
		}
	} else {
		var ad AppdataResult
		if err := s.Helper.Call(ctx, OpAppdataDelete, AppdataParams{ID: t.ID}, &ad); err != nil {
			// Контейнеры и файлы уже удалены: запись остаётся как «удалено, данные сохранены».
			q := p
			q.Removed, q.Desired, q.Ports, q.Network, q.Web = true, "stopped", nil, nil, nil
			s.putProject(ctx, q)
			s.routesChanged()
			return p, fmt.Errorf("данные приложения не удалены: %w; приложение удалено, данные остались", err)
		}
		if err := s.dropProject(ctx, t.ID); err != nil {
			return p, err
		}
	}
	s.routesChanged()
	s.refreshPS(ctx)
	return p, nil
}

// ---------- состояния ----------

func (s *Service) refreshPS(ctx context.Context) {
	var ps PSResult
	if err := s.Helper.Call(ctx, OpProjectsPS, nil, &ps); err != nil {
		s.Logger.Debug("приложения: контейнеры не прочитаны", "err", err)
		ps = PSResult{}
	}
	s.proj.mu.Lock()
	s.proj.ps, s.proj.psAt = ps, s.now()
	s.proj.mu.Unlock()
}

func (s *Service) containersOf(id string) []Container {
	s.proj.mu.Lock()
	defer s.proj.mu.Unlock()
	out := []Container{}
	for _, c := range s.proj.ps.Containers {
		if c.App == id {
			out = append(out, c)
		}
	}
	return out
}

func (s *Service) projectView(p Project) ProjectView {
	cs := s.containersOf(p.ID)
	v := ProjectView{Project: p, Containers: cs, State: projectState(p, s.View().State, cs)}
	if p.UID > 0 {
		v.User = AppUser(p.ID)
	} else if !p.Removed {
		for _, r := range p.Risks {
			v.Legacy = v.Legacy || r.Code == compose.RShareBind
		}
	}
	return v
}

// projectState — состояние приложения по записи, подсистеме и контейнерам.
func projectState(p Project, sub string, cs []Container) string {
	switch {
	case p.Removed:
		return PStateRemoved
	case sub == StateUnavailable || sub == StateNotInstalled || sub == StateDisabled:
		return PStateWaitingPool
	case sub == StatePaused || sub == StateBusy:
		return PStatePaused
	case sub != StateRunning:
		return PStateUnknown
	case p.Crashing:
		return PStateCrashing
	case p.Desired == "stopped":
		return PStateStopped
	}
	if len(cs) == 0 {
		if p.Error != "" {
			return PStateError
		}
		return PStateStarting
	}
	var running, failed, restarting, unhealthy, starting, checked int
	for _, c := range cs {
		switch c.State {
		case "running":
			running++
		case "restarting":
			restarting++
		case "exited", "dead":
			if !strings.Contains(c.Status, "(0)") {
				failed++
			}
		case "created":
			starting++
		}
		switch c.Health {
		case "unhealthy":
			unhealthy++
		case "starting":
			starting++
		}
		if c.Health != "" {
			checked++
		}
	}
	switch {
	case failed > 0:
		return PStateError
	case restarting > 0 || starting > 0:
		return PStateStarting
	case unhealthy > 0:
		return PStateUnhealthy
	case running == 0:
		return PStateStopped
	case checked == 0:
		return PStateNoCheck
	}
	return PStateRunning
}

// Projects — список приложений (GET /apps/projects).
func (s *Service) Projects(ctx context.Context) ([]ProjectView, int, error) {
	m, err := s.loadProjects(ctx)
	if err != nil {
		return nil, 0, err
	}
	if s.View().State == StateRunning {
		s.proj.mu.Lock()
		stale := s.now().Sub(s.proj.psAt) > 5*time.Second
		s.proj.mu.Unlock()
		if stale {
			s.refreshPS(ctx)
		}
	}
	out := make([]ProjectView, 0, len(m))
	for _, p := range m {
		out = append(out, s.projectView(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	s.proj.mu.Lock()
	foreign := s.proj.ps.Foreign
	s.proj.mu.Unlock()
	return out, foreign, nil
}

// ProjectDetails — приложение с исходным compose (переменные — только имена).
func (s *Service) ProjectDetails(ctx context.Context, id string) (ProjectDetail, error) {
	m, err := s.loadProjects(ctx)
	if err != nil {
		return ProjectDetail{}, err
	}
	p, ok := m[id]
	if !ok {
		return ProjectDetail{}, wrapf(ErrNotFound, "приложение %s", id)
	}
	d := ProjectDetail{ProjectView: s.projectView(p), EnvKeys: []string{}}
	if p.Removed {
		return d, nil
	}
	var f ProjectFiles
	if err := s.Helper.Call(ctx, OpProjectRead, ProjectParams{ID: id}, &f); err != nil {
		d.FilesErr = err.Error()
		return d, nil
	}
	d.Compose = f.UserCompose
	env, _ := compose.ParseEnv(f.Env)
	d.EnvKeys = sortedKeys(env)
	return d, nil
}

// Logs — журнал контейнера сервиса приложения.
func (s *Service) Logs(ctx context.Context, id, service string, tail int, since int64) (LogsResult, error) {
	m, err := s.loadProjects(ctx)
	if err != nil {
		return LogsResult{}, err
	}
	if p, ok := m[id]; !ok || p.Removed {
		return LogsResult{}, wrapf(ErrNotFound, "приложение %s", id)
	}
	if tail <= 0 {
		tail = 200
	}
	p := LogsParams{ID: id, Service: service, Tail: tail, Since: since}
	if err := p.Validate(); err != nil {
		return LogsResult{}, wrapf(ErrInvalidSvc, "%v", err)
	}
	if err := s.needRunning(); err != nil {
		return LogsResult{}, err
	}
	var r LogsResult
	if err := s.Helper.Call(ctx, OpProjectLogs, p, &r); err != nil {
		if errors.Is(err, nas.ErrNotFound) || strings.Contains(err.Error(), "не найден") {
			return r, wrapf(ErrNotFound, "%v", err)
		}
		return r, err
	}
	return r, nil
}

// Ports — реестр портов.
func (s *Service) Ports(ctx context.Context) ([]PortEntry, error) {
	m, err := s.loadProjects(ctx)
	if err != nil {
		return nil, err
	}
	return registry(m, Listening(s.procNet())), nil
}

// ---------- сверка и события ----------

// reconcileProjects — после сверки подсистемы: контейнеры; при входе
// Docker в «работает» (с прошлой сверки приложений — в т. ч. после задач
// включения и возобновления) — up приложений, которые должны работать.
func (s *Service) reconcileProjects(ctx context.Context) {
	if s.proj == nil {
		return
	}
	cur := s.View().State
	s.proj.mu.Lock()
	prev := s.proj.lastSub
	s.proj.lastSub = cur
	s.proj.mu.Unlock()
	if cur != StateRunning {
		return
	}
	if prev != StateRunning {
		s.recoverTxns(ctx) // прерванные обновления/откаты — до автозапуска
		m, err := s.loadProjects(ctx)
		if err != nil {
			return
		}
		for _, p := range m {
			if p.Removed || p.Desired != "running" || p.Crashing {
				continue
			}
			if err := s.compose(ctx, p.ID, ActUp, p.SHA256, false); err != nil {
				s.Logger.Warn("приложения: приложение не запущено после старта Docker", "app", p.ID, "err", err)
				p.Error = "не запущено после старта Docker: " + err.Error()
				s.putProject(ctx, p)
				s.emit(ctx, EvAppFailed, events.SevWarning, "app:"+p.ID, map[string]any{"app": p.ID, "stage": "автозапуск", "reason": p.Error})
			} else if p.Error != "" {
				p.Error = ""
				s.putProject(ctx, p)
			}
		}
	}
	s.refreshPS(ctx)
}

func (s *Service) clearDies(id string) {
	s.proj.mu.Lock()
	delete(s.proj.dies, id)
	s.proj.mu.Unlock()
}

// countDie — падение контейнера приложения; true — превышен предел за окно.
func (s *Service) countDie(id string, at time.Time) bool {
	s.proj.mu.Lock()
	defer s.proj.mu.Unlock()
	l := s.proj.dies[id][:0:0]
	for _, t := range s.proj.dies[id] {
		if at.Sub(t) < CrashWindow {
			l = append(l, t)
		}
	}
	l = append(l, at)
	s.proj.dies[id] = l
	if len(l) > CrashLimit {
		delete(s.proj.dies, id)
		return true
	}
	return false
}

// markCrashing — приложение падает в цикле: стоп, статус «падает», событие.
func (s *Service) markCrashing(ctx context.Context, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.loadProjects(ctx)
	if err != nil {
		return
	}
	p, ok := m[id]
	if !ok || p.Removed || p.Crashing {
		return
	}
	done := s.quiet(5 * time.Minute)
	err = s.compose(ctx, id, ActStop, p.SHA256, false)
	done()
	p.Crashing = true
	p.Error = fmt.Sprintf("падает: больше %d падений за %d мин — остановлено", CrashLimit, int(CrashWindow/time.Minute))
	if err != nil {
		p.Error += "; остановка не удалась: " + err.Error()
	}
	s.putProject(ctx, p)
	s.emit(ctx, EvAppCrashing, events.SevWarning, "app:"+id, map[string]any{"app": id, "reason": p.Error})
	s.refreshPS(ctx)
}

// appOf — id приложения по проекту Compose ("app-<id>").
func appOf(project string) string {
	if id, ok := strings.CutPrefix(project, "app-"); ok && ValidID(id) {
		return id
	}
	return ""
}

// holdNoise — die 137 вне действий gamayund: ждём restart (docker restart
// пользователя — не сбой). true — событие отложено.
func (s *Service) holdNoise(ev DockerEvent) bool {
	if s.proj == nil || ev.Action != "die" || ev.ExitCode != "137" || ev.OOM {
		return false
	}
	s.proj.mu.Lock()
	s.proj.noise[ev.Container] = ev
	s.proj.mu.Unlock()
	return true
}

// dropNoise — restart после die 137 в пределах окна: die не публикуется.
func (s *Service) dropNoise(ev DockerEvent) {
	if s.proj == nil || ev.Action != "restart" {
		return
	}
	s.proj.mu.Lock()
	defer s.proj.mu.Unlock()
	if p, ok := s.proj.noise[ev.Container]; ok && ev.TimeNano-p.TimeNano <= int64(restartNoiseWindow) {
		delete(s.proj.noise, ev.Container)
	}
}

// flushNoise — отложенные die 137 старше окна — в журнал.
func (s *Service) flushNoise(ctx context.Context, nowNano int64) {
	if s.proj == nil {
		return
	}
	s.proj.mu.Lock()
	var out []DockerEvent
	for k, ev := range s.proj.noise {
		if nowNano-ev.TimeNano > int64(restartNoiseWindow) {
			out = append(out, ev)
			delete(s.proj.noise, k)
		}
	}
	s.proj.mu.Unlock()
	for _, ev := range out {
		s.publishDocker(ctx, ev)
	}
}
