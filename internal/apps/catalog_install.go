package apps

// Установка приложения из каталога. Форма → user.env
// (секреты — по файлу на сервис secrets-<сервис>.env 0600, только нужные ему),
// совместимость, сверка декларации security, затем та же транзакция, что
// у создания пользовательского приложения (учётка → доступ → данные → файлы → up), плюс ожидание
// готовности (health) внутри транзакции: не готово — полный откат.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gamayun-nas/internal/apps/compose"
	"gamayun-nas/internal/catalog"
	"gamayun-nas/internal/proxy"
	"gamayun-nas/internal/tasks"
	"gamayun-nas/internal/version"
)

// FUndeclaredRisk — риск compose, не задекларированный в x-gamayun.security.
const FUndeclaredRisk = "catalog_undeclared_risk"

// CatalogSource — текущий каталог (catalog.Store).
type CatalogSource interface {
	App(id string) (catalog.App, int64, error)
}

// CatalogRef — происхождение приложения из каталога (в записи приложения).
type CatalogRef struct {
	App        string            `json:"app"`
	Version    string            `json:"version"`
	AppVersion string            `json:"app_version"`
	Serial     int64             `json:"serial"`
	Images     map[string]string `json:"images"`
	// Secrets — ключи .env со значениями-секретами (значения не хранятся).
	Secrets []string `json:"secrets,omitempty"`
	// Points — точки восстановления (новые первыми, не больше KeepPoints).
	Points []RestorePoint `json:"points,omitempty"`
}

// CatalogRequest — установка из каталога (тело API).
type CatalogRequest struct {
	App string `json:"app"`
	// ID — идентификатор приложения на NAS; пусто — id из каталога.
	ID      string            `json:"id"`
	Values  map[string]string `json:"values"`
	Network []string          `json:"network"`
	Confirm []string          `json:"confirm"`
	RunAs   map[string]string `json:"run_as"`
}

// catInstall — данные каталога для задачи установки (только в памяти).
type catInstall struct {
	ref     CatalogRef
	m       catalog.Manifest
	secrets map[string]string // значения секретов: ключ → значение
}

// sysInfo — сведения о NAS для совместимости.
func (s *Service) sysInfo(ctx context.Context) (catalog.System, error) {
	if s.SysInfo != nil {
		return s.SysInfo(ctx)
	}
	sys := catalog.System{OSVersion: version.Version, Arch: runtime.GOARCH}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
				kb, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
				sys.RAMMB = kb / 1024
			}
		}
		f.Close()
	}
	st, err := s.status(ctx)
	if err != nil {
		return sys, wrapf(ErrUnavailable, "состояние Docker: %v", err)
	}
	cfg, err := s.load(ctx)
	if err != nil {
		return sys, err
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(filepath.Join(st.MountRoot, cfg.Pool), &fs); err != nil {
		return sys, wrapf(ErrUnavailable, "свободное место пула %s: %v", cfg.Pool, err)
	}
	sys.DiskFreeMB = int64(fs.Bavail) * int64(fs.Bsize) / (1 << 20)
	return sys, nil
}

func joinErrs(errs []error) string {
	l := make([]string, len(errs))
	for i, e := range errs {
		l[i] = e.Error()
	}
	return strings.Join(l, "; ")
}

// catalogRequest строит запрос создания из приложения каталога и формы.
func (s *Service) catalogRequest(ctx context.Context, cr CatalogRequest) (ProjectRequest, *catInstall, error) {
	if s.Catalog == nil {
		return ProjectRequest{}, nil, wrapf(ErrUnavailable, "каталог приложений не настроен")
	}
	app, serial, err := s.Catalog.App(cr.App)
	switch {
	case errors.Is(err, catalog.ErrNoApp):
		return ProjectRequest{}, nil, wrapf(ErrNotFound, "%v", err)
	case err != nil:
		return ProjectRequest{}, nil, wrapf(ErrInvalidSvc, "каталог не прошёл проверку: %v", err)
	}
	m := app.Manifest
	sys, err := s.sysInfo(ctx)
	if err != nil {
		return ProjectRequest{}, nil, err
	}
	if errs := catalog.Compat(m, sys); len(errs) > 0 {
		return ProjectRequest{}, nil, wrapf(ErrInvalidSvc, "несовместимо с NAS: %s", joinErrs(errs))
	}
	fr, errs := catalog.RenderForm(m.Form, cr.Values, nil)
	if len(errs) > 0 {
		return ProjectRequest{}, nil, wrapf(ErrInvalidSvc, "форма: %s", joinErrs(errs))
	}
	id := cr.ID
	if id == "" {
		id = m.ID
	}
	grants := map[string]string{}
	if len(fr.Shares) > 0 {
		nc, _, err := s.normContext(ctx, id, nil, nil)
		if err != nil {
			return ProjectRequest{}, nil, err
		}
		for key, name := range fr.Shares {
			p, ok := nc.Shares[name]
			if !ok {
				return ProjectRequest{}, nil, wrapf(ErrInvalidSvc, "форма: %s: общей папки %q нет", key, name)
			}
			fr.Env[key] = p
			mode := compose.GrantRO
			for _, st := range m.Storage {
				if st.Kind == "share" && st.Key == key && !st.ReadOnly {
					mode = compose.GrantRW
				}
			}
			if grants[name] != compose.GrantRW {
				grants[name] = mode
			}
		}
	}
	// Секреты — в secrets-<сервис>.env (только нужные сервису): в user.env и compose их нет.
	plain, sec := map[string]string{}, map[string]string{}
	keys := map[string]bool{}
	for k, v := range fr.Env {
		plain[k] = v
	}
	for _, k := range fr.Secrets {
		if v, ok := plain[k]; ok {
			sec[k] = v
			delete(plain, k)
		}
		keys[k] = true
	}
	env, err := catalog.EnvText(plain)
	if err == nil {
		_, err = catalog.EnvText(sec) // проверка значений до задачи
	}
	if err != nil {
		return ProjectRequest{}, nil, wrapf(ErrInvalidSvc, "форма: %v", err)
	}
	req := ProjectRequest{ID: id, Name: m.Name.RU, Compose: string(app.Compose), Env: &env, Network: cr.Network,
		Confirm: cr.Confirm, RunAs: cr.RunAs, Grants: grants, SecretKeys: keys}
	ci := &catInstall{m: m, secrets: sec, ref: CatalogRef{App: m.ID, Version: m.Version, AppVersion: m.AppVersion, Serial: serial,
		Images: app.Images, Secrets: fr.Secrets}}
	return req, ci, nil
}

// catalogVerify — недекларированные риски compose каталога → запрет.
func catalogVerify(cr *CheckResult, ci *catInstall) {
	undeclared, _ := catalog.CheckSecurity(ci.m.Security, cr.Risks)
	for _, r := range undeclared {
		cr.Forbidden = append(cr.Forbidden, compose.Issue{Code: FUndeclaredRisk, Service: r.Service,
			Detail: "риск не задекларирован в каталоге (x-gamayun.security): " + r.Detail})
	}
	if len(undeclared) > 0 {
		cr.OK = false
	}
}

// CatalogCheck — проверка установки без побочных эффектов (секреты не возвращаются).
func (s *Service) CatalogCheck(ctx context.Context, req CatalogRequest) (CheckResult, error) {
	pr, ci, err := s.catalogRequest(ctx, req)
	if err != nil {
		return CheckResult{}, err
	}
	projects, err := s.loadProjects(ctx)
	if err != nil {
		return CheckResult{}, err
	}
	if p, ok := projects[pr.ID]; ok && !p.Removed {
		return CheckResult{}, wrapf(ErrConflictSvc, "приложение %s уже есть", pr.ID)
	}
	// Приложение каталога ставится один раз: копии под другим идентификатором не нужны
	// (другой вариант — свой compose в «Приложениях»).
	for _, p := range projects {
		if !p.Removed && p.Catalog != nil && p.Catalog.App == req.App {
			return CheckResult{}, wrapf(ErrConflictSvc, "приложение каталога %s уже установлено как %s", req.App, p.ID)
		}
	}
	acc, err := s.accessFor(ctx, pr, nil, false)
	if err != nil {
		return CheckResult{}, err
	}
	cr, err := s.check(ctx, pr, *pr.Env, nil, acc)
	if err != nil {
		return cr, err
	}
	catalogVerify(&cr, ci)
	return cr, nil
}

// InstallCatalog проверяет запрос и ставит задачу установки (задача создания
// приложения с данными каталога).
func (s *Service) InstallCatalog(ctx context.Context, req CatalogRequest, actor string) (tasks.Task, error) {
	cr, err := s.CatalogCheck(ctx, req)
	if err != nil {
		return tasks.Task{}, err
	}
	if err := checkErr(cr); err != nil {
		return tasks.Task{}, err
	}
	if err := s.needRunning(); err != nil {
		return tasks.Task{}, err
	}
	// Запрос строится заново: random-секреты, показанные в проверке, не
	// возвращались клиенту, поэтому новые значения ничего не ломают.
	pr, ci, err := s.catalogRequest(ctx, req)
	if err != nil {
		return tasks.Task{}, err
	}
	tok := s.stash(pr, *pr.Env, actor)
	s.proj.mu.Lock()
	p := s.proj.pending[tok]
	p.cat = ci
	s.proj.pending[tok] = p
	s.proj.mu.Unlock()
	return s.submit(ctx, TaskProjectCreate, projTask{ID: pr.ID, Token: tok, Actor: actor})
}

// healthPoll — период опроса готовности.
func (s *Service) healthPoll() time.Duration {
	if s.HealthPoll > 0 {
		return s.HealthPoll
	}
	return 2 * time.Second
}

// waitReady — ожидание готовности после up: все контейнеры running,
// контейнер health.service — healthy (если у него есть проверка), затем
// grace_s без падений. Без health в манифесте — только grace по умолчанию 0.
func (s *Service) waitReady(ctx context.Context, id string, h *catalog.Health) error {
	timeout, grace := ComposeUpWait, 0
	svc := ""
	if h != nil {
		timeout, grace, svc = h.TimeoutS, h.GraceS, h.Service
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	ready := func() (bool, string) {
		s.refreshPS(ctx)
		cs := s.containersOf(id)
		if len(cs) == 0 {
			return false, "контейнеров нет"
		}
		seen := svc == ""
		for _, c := range cs {
			if c.State != "running" {
				return false, fmt.Sprintf("сервис %s: %s", c.Service, c.State)
			}
			if c.Service == svc {
				seen = true
				if c.Health != "" && c.Health != "healthy" {
					return false, fmt.Sprintf("сервис %s: %s", c.Service, c.Health)
				}
			}
		}
		if !seen {
			return false, "сервис " + svc + " не запущен"
		}
		return true, ""
	}
	why := ""
	for {
		ok, w := ready()
		if ok {
			break
		}
		why = w
		if time.Now().After(deadline) {
			return fmt.Errorf("не готово за %d с: %s", timeout, why)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.healthPoll()):
		}
	}
	if grace > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(grace) * time.Second):
		}
		if ok, w := ready(); !ok {
			return fmt.Errorf("упало в первые %d с после запуска: %s", grace, w)
		}
	}
	return nil
}

// catalogWeb — публикация по умолчанию: имя поддомена = id, порт —
// TCP-порт сервиса web.service на 127.0.0.1. Имя занято или не подходит —
// без публикации (пользователь опубликует сам), причина в журнале.
func (s *Service) catalogWeb(id string, m catalog.Manifest, ports []compose.Port, projects map[string]Project) *WebRoute {
	w := m.Web
	if w == nil || w.Disabled {
		return nil
	}
	host := 0
	for _, p := range ports {
		if p.Service == w.Service && p.Container == w.Port && p.Proto == "tcp" && !p.Network {
			host = p.Host
		}
	}
	why := ""
	switch {
	case host == 0:
		why = "порт веб-интерфейса не опубликован на 127.0.0.1"
	case !proxy.ValidName(id):
		why = "имя не подходит для поддомена"
	}
	if why == "" && s.WebName != nil {
		if err := s.WebName(id); err != nil {
			why = err.Error()
		}
	}
	for oid, o := range projects {
		if why == "" && oid != id && o.Web != nil && o.Web.Name == id {
			why = "поддомен занят приложением " + oid
		}
	}
	if why != "" {
		s.Logger.Warn("каталог: веб-интерфейс не опубликован", "app", id, "why", why)
		return nil
	}
	return &WebRoute{Name: id, Port: host}
}
