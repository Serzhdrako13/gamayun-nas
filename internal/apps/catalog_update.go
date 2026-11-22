package apps

// Обновление приложения каталога и откат.
//
// Обновление (задача): новые файлы с резервной копией → pull → down →
// точка восстановления (ro-снимок appdata + прежние файлы) → up → ожидание
// готовности. Сбой на любом шаге — автооткат: down, файлы и данные из точки
// (до точки — прежние файлы), up прежней версии, событие warning.
// Каждый шаг пишется в журнал транзакции (settings apps.txn.<id>) до
// действия; при старте Docker незавершённая транзакция доводится: обновление
// откатывается, ручной откат — завершается. Запись приложения и удаление
// журнала — одной транзакцией БД.
//
// Секреты: прежние значения — объединение ProjectRead.Secrets, новые random
// генерирует форма. Доступ к общим папкам при обновлении не меняется.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gamayun-nas/internal/apps/compose"
	"gamayun-nas/internal/catalog"
	"gamayun-nas/internal/events"
	"gamayun-nas/internal/tasks"
)

// Задачи и события обновления и отката приложений каталога.
const (
	TaskCatalogUpdate   = "apps.catalog.update"
	TaskCatalogRollback = "apps.catalog.rollback"
	EvAppRolledBack     = "apps.app_rolled_back"
	// KeepPoints — сколько точек восстановления хранить.
	KeepPoints = 3
	txnPrefix  = "apps.txn."
)

// Шаги транзакции (пишутся до действия).
const (
	txnUpdate   = "update"
	txnRollback = "rollback"

	stepWriting  = 1 // запись новых файлов (prev — прежние)
	stepStopping = 2 // down прежней версии
	stepPoint    = 3 // точка создана: данные и файлы возвращаются из неё
	stepUp       = 4 // up новой версии и ожидание готовности
)

// RestorePoint — точка восстановления: прежняя версия приложения.
type RestorePoint struct {
	Name       string    `json:"name"`
	At         time.Time `json:"at"`
	Version    string    `json:"version"`
	AppVersion string    `json:"app_version"`
	// Prev — запись приложения до обновления (без своих точек).
	Prev *Project `json:"prev"`
}

// Txn — журнал транзакции обновления/отката приложения.
type Txn struct {
	Op    string    `json:"op"`
	App   string    `json:"app"`
	Step  int       `json:"step"`
	Point string    `json:"point,omitempty"`
	Old   Project   `json:"old"`
	To    string    `json:"to,omitempty"` // версия каталога, на которую обновляли
	Actor string    `json:"actor"`
	At    time.Time `json:"at"`
}

// CatalogUpdateRequest — обновление (тело API): значения новых полей формы
// (прежние берутся из файлов приложения), подтверждения новых рисков.
type CatalogUpdateRequest struct {
	Values  map[string]string `json:"values"`
	Confirm []string          `json:"confirm"`
}

// CatalogUpdateCheck — итог проверки обновления.
type CatalogUpdateCheck struct {
	CheckResult
	From string `json:"from"`
	To   string `json:"to"`
	// Irreversible — миграция данных необратима: откат только вместе с данными.
	Irreversible bool `json:"migration_irreversible"`
}

func (s *Service) saveTxn(ctx context.Context, tx Txn) error {
	b, _ := json.Marshal(tx)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, txnPrefix+tx.App, string(b))
	return err
}

func (s *Service) loadTxns(ctx context.Context) ([]Txn, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT value FROM settings WHERE key LIKE ? ORDER BY key", txnPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Txn
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		var tx Txn
		if err := json.Unmarshal([]byte(v), &tx); err != nil || !ValidID(tx.App) {
			s.Logger.Warn("приложения: повреждённый журнал транзакции пропущен", "err", err)
			continue
		}
		out = append(out, tx)
	}
	return out, rows.Err()
}

// commitProject — запись приложения и удаление журнала его транзакции атомарно.
func (s *Service) commitProject(ctx context.Context, p Project) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	m := map[string]Project{}
	var v string
	err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", projectsKey).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal([]byte(v), &m); err != nil {
			return err
		}
	}
	m[p.ID] = p
	b, _ := json.Marshal(m)
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, projectsKey, string(b)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", txnPrefix+p.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// currentSecrets — значения секретов приложения: объединение файлов сервисов.
func currentSecrets(f ProjectFiles) (map[string]string, error) {
	out := map[string]string{}
	for svc, text := range f.Secrets {
		m, err := compose.ParseEnv(text)
		if err != nil {
			return nil, fmt.Errorf("секреты сервиса %s: %v", svc, err)
		}
		for k, v := range m {
			out[k] = v
		}
	}
	return out, nil
}

// catalogProject — приложение каталога по id.
func (s *Service) catalogProject(ctx context.Context, id string) (Project, error) {
	projects, err := s.loadProjects(ctx)
	if err != nil {
		return Project{}, err
	}
	p, ok := projects[id]
	if !ok || p.Removed {
		return Project{}, wrapf(ErrNotFound, "приложение %s", id)
	}
	if p.Catalog == nil {
		return Project{}, wrapf(ErrConflictSvc, "приложение %s не из каталога", id)
	}
	return p, nil
}

// updateRequest строит запрос обновления: прежние значения формы из файлов
// приложения (папки — по имени), поверх — новые значения пользователя.
func (s *Service) updateRequest(ctx context.Context, p Project, req CatalogUpdateRequest) (ProjectRequest, *catInstall, catalog.Manifest, error) {
	var m catalog.Manifest
	if s.Catalog == nil {
		return ProjectRequest{}, nil, m, wrapf(ErrUnavailable, "каталог приложений не настроен")
	}
	app, _, err := s.Catalog.App(p.Catalog.App)
	switch {
	case errors.Is(err, catalog.ErrNoApp):
		return ProjectRequest{}, nil, m, wrapf(ErrNotFound, "%v", err)
	case err != nil:
		return ProjectRequest{}, nil, m, wrapf(ErrInvalidSvc, "каталог не прошёл проверку: %v", err)
	}
	m = app.Manifest
	if catalog.CompareVersion(m.Version, p.Catalog.Version) <= 0 {
		return ProjectRequest{}, nil, m, wrapf(ErrConflictSvc, "установлена версия %s, в каталоге %s — обновлять нечего", p.Catalog.Version, m.Version)
	}
	var f ProjectFiles
	if err := s.Helper.Call(ctx, OpProjectRead, ProjectParams{ID: p.ID}, &f); err != nil {
		return ProjectRequest{}, nil, m, wrapf(ErrUnavailable, "%v", err)
	}
	values, err := compose.ParseEnv(f.Env)
	if err != nil {
		return ProjectRequest{}, nil, m, wrapf(ErrUnavailable, "переменные приложения: %v", err)
	}
	sec, err := currentSecrets(f)
	if err != nil {
		return ProjectRequest{}, nil, m, wrapf(ErrUnavailable, "%v", err)
	}
	for k, v := range sec {
		values[k] = v
	}
	// Папки: в user.env — путь, в форме — имя.
	nc, _, err := s.normContext(ctx, p.ID, nil, nil)
	if err != nil {
		return ProjectRequest{}, nil, m, err
	}
	for _, fl := range m.Form {
		if fl.Type != "share" {
			continue
		}
		for name, path := range nc.Shares {
			if values[fl.Key] == path {
				values[fl.Key] = name
			}
		}
	}
	for k, v := range req.Values {
		values[k] = v
	}
	pr, ci, err := s.catalogRequest(ctx, CatalogRequest{App: p.Catalog.App, ID: p.ID, Values: values,
		Network: append([]string{}, p.Network...), Confirm: req.Confirm, RunAs: p.RunAs})
	if err != nil {
		return pr, ci, m, err
	}
	old := byShare(p.Grants)
	same := len(old) == len(pr.Grants)
	for name, mode := range pr.Grants {
		if old[name].Mode != mode {
			same = false
		}
	}
	if !same {
		return pr, ci, m, wrapf(ErrInvalidSvc, "обновление меняет доступ к общим папкам — сначала измените доступ, затем обновите")
	}
	return pr, ci, m, nil
}

// CheckCatalogUpdate — проверка обновления без побочных эффектов.
func (s *Service) CheckCatalogUpdate(ctx context.Context, id string, req CatalogUpdateRequest) (CatalogUpdateCheck, error) {
	p, err := s.catalogProject(ctx, id)
	if err != nil {
		return CatalogUpdateCheck{}, err
	}
	pr, ci, m, err := s.updateRequest(ctx, p, req)
	if err != nil {
		return CatalogUpdateCheck{}, err
	}
	acc, err := s.accessFor(ctx, pr, &p, false)
	if err != nil {
		return CatalogUpdateCheck{}, err
	}
	cr, err := s.check(ctx, pr, *pr.Env, p.Risks, acc)
	if err != nil {
		return CatalogUpdateCheck{CheckResult: cr}, err
	}
	catalogVerify(&cr, ci)
	return CatalogUpdateCheck{CheckResult: cr, From: p.Catalog.Version, To: m.Version, Irreversible: m.Update.MigrationIrreversible}, nil
}

// UpdateCatalog проверяет и ставит задачу обновления.
func (s *Service) UpdateCatalog(ctx context.Context, id string, req CatalogUpdateRequest, actor string) (tasks.Task, error) {
	uc, err := s.CheckCatalogUpdate(ctx, id, req)
	if err != nil {
		return tasks.Task{}, err
	}
	if err := checkErr(uc.CheckResult); err != nil {
		return tasks.Task{}, err
	}
	if err := s.needRunning(); err != nil {
		return tasks.Task{}, err
	}
	p, err := s.catalogProject(ctx, id)
	if err != nil {
		return tasks.Task{}, err
	}
	pr, ci, _, err := s.updateRequest(ctx, p, req)
	if err != nil {
		return tasks.Task{}, err
	}
	tok := s.stash(pr, *pr.Env, actor)
	s.proj.mu.Lock()
	st := s.proj.pending[tok]
	st.cat = ci
	s.proj.pending[tok] = st
	s.proj.mu.Unlock()
	return s.submit(ctx, TaskCatalogUpdate, projTask{ID: id, Token: tok, Actor: actor})
}

func pointName(t time.Time) string { return "u" + strings.ToLower(t.UTC().Format("20060102t150405")) }

// stripPoints — копия записи без точек (для RestorePoint.Prev).
func stripPoints(p Project) *Project {
	c := p
	if p.Catalog != nil {
		ref := *p.Catalog
		ref.Points = nil
		c.Catalog = &ref
	}
	return &c
}

func (s *Service) runCatalogUpdate(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	t, pr, err := s.taskParams(raw)
	if err != nil {
		return nil, err
	}
	if pr.cat == nil {
		return nil, errors.New("параметры задачи: нет данных каталога")
	}
	if err := s.lockApp(t.ID); err != nil {
		return nil, err
	}
	defer s.unlockApp(t.ID)
	if err := s.needRunning(); err != nil {
		return nil, err
	}
	old, err := s.catalogProject(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	acc, err := s.accessFor(ctx, pr.req, &old, false)
	var cr CheckResult
	if err == nil {
		cr, err = s.check(ctx, pr.req, pr.env, old.Risks, acc)
	}
	if err == nil {
		catalogVerify(&cr, pr.cat)
		err = checkErr(cr)
	}
	var secrets map[string]string
	if err == nil {
		secrets, err = pr.secretFiles(cr)
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	tx := Txn{Op: txnUpdate, App: t.ID, Old: old, To: pr.cat.ref.Version, Actor: pr.actor, At: now}
	fail := func(stage string, err error) (any, error) {
		bg := context.WithoutCancel(ctx)
		errs := s.undoUpdate(bg, tx, stage+": "+err.Error())
		if len(errs) > 0 {
			return nil, fmt.Errorf("%s: %v; автооткат не полный: %s", stage, err, strings.Join(errs, "; "))
		}
		return nil, fmt.Errorf("%s: %v; автооткат выполнен, работает прежняя версия %s", stage, err, old.Catalog.Version)
	}
	step := func(n int) error {
		tx.Step = n
		return s.saveTxn(ctx, tx)
	}
	if err := step(stepWriting); err != nil {
		return nil, fmt.Errorf("журнал транзакции: %w", err)
	}
	sum := SHA256(cr.Result.Normalized)
	var w ProjectResult
	if err := s.Helper.Call(ctx, OpProjectWrite, ProjectWriteParams{ID: t.ID, UserCompose: pr.req.Compose, Env: pr.env,
		Secrets: secrets, Normalized: string(cr.Result.Normalized), SHA256: sum, Mkdirs: cr.Mkdirs, Backup: true, Owner: old.UID}, &w); err != nil {
		return fail("запись файлов", err)
	}
	progress(15)
	if err := s.compose(ctx, t.ID, ActPull, sum, false); err != nil {
		return fail("загрузка образов", err)
	}
	progress(35)
	if err := step(stepStopping); err != nil {
		return fail("журнал транзакции", err)
	}
	done := s.quiet(5 * time.Minute)
	err = s.compose(ctx, t.ID, ActDown, "", false)
	done()
	if err != nil {
		return fail("остановка", err)
	}
	progress(45)
	name := pointName(now)
	var pt PointResult
	if err := s.Helper.Call(ctx, OpPointCreate, PointParams{ID: t.ID, Name: name}, &pt); err != nil {
		return fail("точка восстановления", err)
	}
	tx.Point = name
	if err := step(stepPoint); err != nil {
		return fail("журнал транзакции", err)
	}
	progress(55)
	if err := step(stepUp); err != nil {
		return fail("журнал транзакции", err)
	}
	done = s.quiet(5 * time.Minute)
	err = s.compose(ctx, t.ID, ActUp, sum, false)
	done()
	if err != nil {
		return fail("запуск", err)
	}
	progress(70)
	if err := s.waitReady(ctx, t.ID, pr.cat.m.Health); err != nil {
		return fail("готовность приложения", err)
	}
	progress(90)
	all, fresh := risksOf(cr, old.Risks, pr.actor, s.now())
	p := old
	p.Name, p.SHA256, p.Ports, p.Risks = pr.req.Name, sum, cr.Ports, all
	p.Services, p.NoHealth, p.Desired, p.Crashing, p.Error, p.Updated = cr.Services, cr.NoHealth, "running", false, "", s.now().UTC()
	p.RunAs = cr.RunAs
	p.Web = keepWeb(old.Web, p.Ports)
	ref := pr.cat.ref
	ref.Points = append([]RestorePoint{{Name: name, At: now, Version: old.Catalog.Version, AppVersion: old.Catalog.AppVersion,
		Prev: stripPoints(old)}}, old.Catalog.Points...)
	if len(ref.Points) > KeepPoints {
		ref.Points = ref.Points[:KeepPoints]
	}
	p.Catalog = &ref
	if err := s.commitProject(ctx, p); err != nil {
		return fail("запись приложения", err)
	}
	s.prunePoints(ctx, p)
	if (old.Web == nil) != (p.Web == nil) || old.Web != nil && p.Web != nil && *old.Web != *p.Web {
		s.routesChanged()
	}
	s.clearDies(t.ID)
	s.emitRisks(ctx, t.ID, pr.actor, fresh)
	s.refreshPS(ctx)
	s.emit(ctx, EvAppUpdated, events.SevInfo, "app:"+t.ID, map[string]any{"app": t.ID, "actor": pr.actor,
		"from": old.Catalog.Version, "to": ref.Version})
	return s.projectView(p), nil
}

// prunePoints — удалить точки, которых нет в записи (старше KeepPoints и
// остатки прерванных попыток).
func (s *Service) prunePoints(ctx context.Context, p Project) {
	var l PointResult
	if err := s.Helper.Call(ctx, OpPointList, PointListParams{ID: p.ID}, &l); err != nil {
		s.Logger.Warn("приложения: список точек восстановления", "app", p.ID, "err", err)
		return
	}
	keep := map[string]bool{}
	if p.Catalog != nil {
		for _, pt := range p.Catalog.Points {
			keep[pt.Name] = true
		}
	}
	for _, n := range l.Points {
		if keep[n] {
			continue
		}
		var r PointResult
		if err := s.Helper.Call(ctx, OpPointDelete, PointParams{ID: p.ID, Name: n}, &r); err != nil {
			s.Logger.Warn("приложения: точка восстановления не удалена", "app", p.ID, "point", n, "err", err)
		}
	}
}

// undoUpdate — откат обновления по журналу (в задаче и при старте):
// down → файлы и данные из точки (до точки — прежние файлы) → up прежней
// версии → запись прежнего приложения и удаление журнала. Вызывать под s.mu.
func (s *Service) undoUpdate(ctx context.Context, tx Txn, reason string) []string {
	id := tx.App
	var errs []string
	done := s.quiet(5 * time.Minute)
	defer done()
	if tx.Step >= stepStopping {
		if err := s.compose(ctx, id, ActDown, "", false); err != nil {
			errs = append(errs, "остановка новой версии: "+err.Error())
		}
	}
	switch {
	case tx.Step >= stepPoint && tx.Point != "":
		var r PointResult
		if err := s.Helper.Call(ctx, OpPointRestore, PointParams{ID: id, Name: tx.Point, Data: true}, &r); err != nil {
			errs = append(errs, "данные и файлы из точки: "+err.Error())
		}
	case tx.Step >= stepWriting:
		var f ProjectFiles
		err := s.Helper.Call(ctx, OpProjectRead, ProjectParams{ID: id}, &f)
		if err == nil && f.SHA256 != tx.Old.SHA256 {
			var r ProjectResult
			err = s.Helper.Call(ctx, OpProjectRevert, ProjectParams{ID: id}, &r)
		}
		if err != nil {
			errs = append(errs, "прежние файлы: "+err.Error())
		}
	}
	old := tx.Old
	if len(errs) == 0 && old.Desired == "running" && !old.Crashing {
		if err := s.compose(ctx, id, ActUp, old.SHA256, false); err != nil {
			errs = append(errs, "запуск прежней версии: "+err.Error())
		}
	}
	if len(errs) > 0 {
		old.Error = "откат обновления не полный: " + strings.Join(errs, "; ")
	}
	if tx.Point != "" && old.Catalog != nil {
		// Точка неудачной попытки не нужна: данные в ней — те же, что сейчас.
		defer s.prunePoints(ctx, old)
	}
	if err := s.commitProject(ctx, old); err != nil {
		errs = append(errs, "запись приложения: "+err.Error())
	}
	sev := events.SevWarning
	s.emit(ctx, EvAppRolledBack, sev, "app:"+id, map[string]any{"app": id, "auto": true, "from": tx.To,
		"to": versionOf(old), "reason": reason, "complete": len(errs) == 0})
	s.refreshPS(ctx)
	return errs
}

func versionOf(p Project) string {
	if p.Catalog == nil {
		return ""
	}
	return p.Catalog.Version
}

// RollbackCatalog — ручной откат на предыдущую версию (последняя точка):
// данные после обновления теряются.
func (s *Service) RollbackCatalog(ctx context.Context, id, actor string) (tasks.Task, error) {
	p, err := s.catalogProject(ctx, id)
	if err != nil {
		return tasks.Task{}, err
	}
	if len(p.Catalog.Points) == 0 || p.Catalog.Points[0].Prev == nil {
		return tasks.Task{}, wrapf(ErrConflictSvc, "у приложения %s нет предыдущей версии", id)
	}
	if err := s.needRunning(); err != nil {
		return tasks.Task{}, err
	}
	return s.submit(ctx, TaskCatalogRollback, projTask{ID: id, Actor: actor})
}

func (s *Service) runCatalogRollback(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	t, _, err := s.taskParams(raw)
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
	cur, err := s.catalogProject(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	if len(cur.Catalog.Points) == 0 || cur.Catalog.Points[0].Prev == nil {
		return nil, wrapf(ErrConflictSvc, "у приложения %s нет предыдущей версии", t.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := Txn{Op: txnRollback, App: t.ID, Step: stepStopping, Point: cur.Catalog.Points[0].Name, Old: cur,
		To: cur.Catalog.Points[0].Version, Actor: t.Actor, At: s.now().UTC()}
	if err := s.saveTxn(ctx, tx); err != nil {
		return nil, fmt.Errorf("журнал транзакции: %w", err)
	}
	progress(10)
	p, err := s.finishRollback(ctx, tx)
	if err != nil {
		return nil, err
	}
	return s.projectView(p), nil
}

// finishRollback — ручной откат по журналу (в задаче и доведение при
// старте): down → файлы и данные из точки → up → запись предыдущей версии с
// оставшимися точками → удаление точки. Повторяемо. Вызывать под s.mu.
func (s *Service) finishRollback(ctx context.Context, tx Txn) (Project, error) {
	id, cur := tx.App, tx.Old
	var pt RestorePoint
	for _, x := range cur.Catalog.Points {
		if x.Name == tx.Point {
			pt = x
		}
	}
	fail := func(stage string, err error) (Project, error) {
		bg := context.WithoutCancel(ctx)
		cur.Error = "откат не выполнен (" + stage + "): " + err.Error() + " — повторите откат"
		cur.Desired = "stopped"
		if e := s.commitProject(bg, cur); e != nil {
			s.Logger.Warn("приложения: запись после сбоя отката", "app", id, "err", e)
		}
		s.emit(bg, EvAppFailed, events.SevWarning, "app:"+id, map[string]any{"app": id, "stage": "откат: " + stage, "reason": err.Error()})
		return cur, fmt.Errorf("%s: %v; приложение остановлено, повторите откат", stage, err)
	}
	if pt.Prev == nil {
		return fail("точка восстановления", fmt.Errorf("точки %s нет в записи", tx.Point))
	}
	done := s.quiet(5 * time.Minute)
	defer done()
	if err := s.compose(ctx, id, ActDown, "", false); err != nil {
		return fail("остановка", err)
	}
	var r PointResult
	if err := s.Helper.Call(ctx, OpPointRestore, PointParams{ID: id, Name: tx.Point, Data: true}, &r); err != nil {
		return fail("данные и файлы из точки", err)
	}
	p := *pt.Prev
	ref := *p.Catalog
	ref.Points = nil
	for _, x := range cur.Catalog.Points {
		if x.Name != tx.Point && x.At.Before(pt.At) {
			ref.Points = append(ref.Points, x)
		}
	}
	sort.Slice(ref.Points, func(i, j int) bool { return ref.Points[i].At.After(ref.Points[j].At) })
	p.Catalog = &ref
	p.Error, p.Crashing, p.Updated = "", false, s.now().UTC()
	if cur.Desired == "running" {
		p.Desired = "running"
		if err := s.compose(ctx, id, ActUp, p.SHA256, false); err != nil {
			return fail("запуск предыдущей версии", err)
		}
	}
	if err := s.commitProject(ctx, p); err != nil {
		return fail("запись приложения", err)
	}
	s.prunePoints(ctx, p)
	if (cur.Web == nil) != (p.Web == nil) || cur.Web != nil && p.Web != nil && *cur.Web != *p.Web {
		s.routesChanged()
	}
	s.clearDies(id)
	s.refreshPS(ctx)
	s.emit(ctx, EvAppRolledBack, events.SevWarning, "app:"+id, map[string]any{"app": id, "auto": false, "actor": tx.Actor,
		"from": versionOf(cur), "to": versionOf(p)})
	return p, nil
}

// recoverTxns — доведение прерванных транзакций (перезапуск gamayund, сбой
// питания) при старте Docker: обновление откатывается, ручной откат
// завершается. Приложения с выполняющейся задачей пропускаются.
// Вызывается из reconcileProjects под s.mu (Run) — сам s.mu не берёт:
// повторный Lock — взаимоблокировка цикла сверки (найдено на стенде).
func (s *Service) recoverTxns(ctx context.Context) {
	l, err := s.loadTxns(ctx)
	if err != nil {
		s.Logger.Warn("приложения: журнал транзакций не прочитан", "err", err)
		return
	}
	for _, tx := range l {
		if err := s.lockApp(tx.App); err != nil {
			continue
		}
		switch tx.Op {
		case txnRollback:
			if _, err := s.finishRollback(ctx, tx); err != nil {
				s.Logger.Warn("приложения: прерванный откат не завершён", "app", tx.App, "err", err)
			}
		default:
			if errs := s.undoUpdate(ctx, tx, "обновление прервано (перезапуск gamayund или сбой питания)"); len(errs) > 0 {
				s.Logger.Warn("приложения: откат прерванного обновления не полный", "app", tx.App, "err", strings.Join(errs, "; "))
			}
		}
		s.unlockApp(tx.App)
	}
}
