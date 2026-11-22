package apps

// Доступ приложений в gamayund: реестр UID учёток приложений, выдачи доступа к общим
// папкам (задания ACL в helper с ходом выполнения), сверка ACL.
//
// Долгая правка ACL идёт без s.mu (блокировки действий Docker и сверки
// подсистемы): s.mu берётся только на шаги Docker. Одновременные задачи
// одного приложения исключает projState.busy.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gamayun-nas/internal/apps/compose"
	"gamayun-nas/internal/events"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/tasks"
)

const (
	uidsKey = "apps.uids"
	// ACLStall — задание ACL без продвижения дольше — сбой (helper завис).
	ACLStall = 10 * time.Minute
	// ACLMax — предел одного задания (папка в миллионы файлов).
	ACLMax           = 6 * time.Hour
	aclPoll          = time.Second
	EvACLDrift       = "apps.acl_drift"
	EvAccessChanged  = "apps.access_changed"
	EvAppMigrated    = "apps.app_migrated"
	TaskProjectGrant = "apps.project.grants"
	aclCheckEvery    = 10 * time.Minute
)

// Grant — выдача доступа приложению к общей папке.
type Grant struct {
	Share string    `json:"share"`
	Pool  string    `json:"pool"`
	Mode  string    `json:"mode"` // ro | rw
	By    string    `json:"by,omitempty"`
	At    time.Time `json:"at"`
}

// uidRegistry — UID учёток приложений: id → UID; Retired — UID удалённых
// приложений (не выдаются повторно: на пулах могут остаться их файлы).
type uidRegistry struct {
	ByID    map[string]int `json:"by_id"`
	Retired []int          `json:"retired"`
}

func (s *Service) loadUIDs(ctx context.Context) (uidRegistry, error) {
	r := uidRegistry{ByID: map[string]int{}}
	var v string
	err := s.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", uidsKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(v), &r); err != nil {
		return r, err
	}
	if r.ByID == nil {
		r.ByID = map[string]int{}
	}
	return r, nil
}

func (s *Service) saveUIDs(ctx context.Context, r uidRegistry) error {
	b, _ := json.Marshal(r)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, uidsKey, string(b))
	return err
}

// systemIDs — занятые UID/GID ОС (passwd, group).
func (s *Service) systemIDs() map[int]bool {
	used := map[int]bool{}
	for _, f := range []string{or(s.Passwd, "/etc/passwd"), or(s.Group, "/etc/group")} {
		l, _ := readIDs(f)
		for _, e := range l {
			if !strings.HasPrefix(e.name, nas.AppUserPrefix) {
				used[e.id] = true
			}
		}
	}
	return used
}

// peekUID — UID приложения: закреплённый или первый свободный (без записи).
func (s *Service) peekUID(ctx context.Context, id string) (int, error) {
	s.uidMu.Lock()
	defer s.uidMu.Unlock()
	r, err := s.loadUIDs(ctx)
	if err != nil {
		return 0, err
	}
	return s.nextUID(r, id)
}

func (s *Service) nextUID(r uidRegistry, id string) (int, error) {
	if u, ok := r.ByID[id]; ok {
		return u, nil
	}
	taken := s.systemIDs()
	for _, u := range r.ByID {
		taken[u] = true
	}
	for _, u := range r.Retired {
		taken[u] = true
	}
	for u := nas.AppUIDMin; u <= nas.AppUIDMax; u++ {
		if !taken[u] {
			return u, nil
		}
	}
	return 0, wrapf(ErrConflictSvc, "нет свободных UID для учёток приложений (%d–%d)", nas.AppUIDMin, nas.AppUIDMax)
}

// reserveUID закрепляет UID за приложением; fresh — закреплён сейчас.
func (s *Service) reserveUID(ctx context.Context, id string) (uid int, fresh bool, err error) {
	s.uidMu.Lock()
	defer s.uidMu.Unlock()
	r, err := s.loadUIDs(ctx)
	if err != nil {
		return 0, false, err
	}
	if u, ok := r.ByID[id]; ok {
		return u, false, nil
	}
	if uid, err = s.nextUID(r, id); err != nil {
		return 0, false, err
	}
	r.ByID[id] = uid
	return uid, true, s.saveUIDs(ctx, r)
}

// releaseUID — снять закрепление; retire — UID больше не выдавать.
func (s *Service) releaseUID(ctx context.Context, id string, retire bool) error {
	s.uidMu.Lock()
	defer s.uidMu.Unlock()
	r, err := s.loadUIDs(ctx)
	if err != nil {
		return err
	}
	u, ok := r.ByID[id]
	if !ok {
		return nil
	}
	delete(r.ByID, id)
	if retire {
		r.Retired = append(r.Retired, u)
		sort.Ints(r.Retired)
	}
	return s.saveUIDs(ctx, r)
}

// busy — приложение занято задачей (одна задача на приложение).
func (s *Service) lockApp(id string) error {
	s.proj.mu.Lock()
	defer s.proj.mu.Unlock()
	if s.proj.busy[id] {
		return wrapf(ErrConflictSvc, "с приложением %s уже выполняется задача", id)
	}
	s.proj.busy[id] = true
	return nil
}

func (s *Service) unlockApp(id string) {
	s.proj.mu.Lock()
	delete(s.proj.busy, id)
	s.proj.mu.Unlock()
}

// grantMap — выдачи как share → mode.
func grantMap(l []Grant) map[string]string {
	m := map[string]string{}
	for _, g := range l {
		m[g.Share] = g.Mode
	}
	return m
}

// buildGrants — выдачи запроса по общим папкам (пул — из реестра папок).
func buildGrants(req map[string]string, prev []Grant, pools map[string]string, actor string, now time.Time) ([]Grant, error) {
	old := map[string]Grant{}
	for _, g := range prev {
		old[g.Share] = g
	}
	out := []Grant{}
	for _, sh := range sortedKeys(req) {
		mode := req[sh]
		if mode != compose.GrantRO && mode != compose.GrantRW {
			return nil, wrapf(ErrInvalidSvc, "доступ к папке %s: %q (ro или rw)", sh, mode)
		}
		pool, ok := pools[sh]
		if !ok {
			return nil, wrapf(ErrInvalidSvc, "общей папки %s нет", sh)
		}
		if o, ok := old[sh]; ok && o.Mode == mode && o.Pool == pool {
			out = append(out, o)
			continue
		}
		out = append(out, Grant{Share: sh, Pool: pool, Mode: mode, By: actor, At: now.UTC()})
	}
	return out, nil
}

// aclStep — одно изменение ACL папки.
type aclStep struct {
	Grant
	revoke bool
}

// grantDiff — что сделать: выдать/изменить (до запуска) и отозвать (после).
func grantDiff(old, new []Grant) (apply, revoke []aclStep) {
	om, nm := map[string]Grant{}, map[string]Grant{}
	for _, g := range old {
		om[g.Share] = g
	}
	for _, g := range new {
		nm[g.Share] = g
		if o, ok := om[g.Share]; !ok || o.Mode != g.Mode || o.Pool != g.Pool {
			apply = append(apply, aclStep{Grant: g})
		}
	}
	for _, g := range old {
		if n, ok := nm[g.Share]; !ok || n.Pool != g.Pool {
			revoke = append(revoke, aclStep{Grant: g, revoke: true})
		}
	}
	return
}

// otherWriters — есть ли у других приложений запись в папку (тогда d:g не снимается).
func (s *Service) otherWriters(ctx context.Context, id, pool, share string) bool {
	m, err := s.loadProjects(ctx)
	if err != nil {
		return true
	}
	for _, p := range m {
		if p.ID == id || p.Removed {
			continue
		}
		for _, g := range p.Grants {
			if g.Share == share && g.Pool == pool && g.Mode == compose.GrantRW {
				return true
			}
		}
	}
	return false
}

// runACL — задание helper с ожиданием и ходом выполнения (from..to %).
func (s *Service) runACL(ctx context.Context, id string, uid int, st aclStep, progress func(int), from, to int) (ACLJob, error) {
	p := ACLParams{ID: id, UID: uid, Pool: st.Pool, Share: st.Share, Mode: st.Mode}
	if st.revoke {
		p.Mode = GrantModeRevoke
		p.DropGroupDefault = !s.otherWriters(ctx, id, st.Pool, st.Share)
	}
	var j ACLJob
	if err := s.Helper.Call(ctx, OpACLStart, p, &j); err != nil {
		return j, err
	}
	start, last, lastDone := s.now(), s.now(), int64(-1)
	what := "выдача доступа к папке " + st.Share
	if st.revoke {
		what = "отзыв доступа к папке " + st.Share
	}
	for {
		var cur ACLJob
		if err := s.Helper.Call(ctx, OpACLStatus, JobParams{Job: j.Job}, &cur); err != nil {
			return j, err
		}
		j = cur
		if j.State != JobRunning {
			break
		}
		if j.Done != lastDone {
			lastDone, last = j.Done, s.now()
		}
		if s.now().Sub(last) > ACLStall || s.now().Sub(start) > ACLMax {
			return j, fmt.Errorf("%s: нет продвижения (обработано %d из %d)", what, j.Done, j.Total)
		}
		if j.Total > 0 && progress != nil {
			progress(from + int(int64(to-from)*j.Done/j.Total))
		}
		tasks.Expect(ctx, ACLStall+time.Minute, fmt.Sprintf("%s: %d из %d", what, j.Done, j.Total))
		select {
		case <-ctx.Done():
			return j, ctx.Err()
		case <-time.After(aclPoll):
		}
	}
	if j.State == JobFailed {
		return j, fmt.Errorf("%s: %s (обработано %d из %d)", what, j.Error, j.Done, j.Total)
	}
	s.Logger.Info("приложения: ACL", "app", id, "share", st.Share, "mode", p.Mode, "objects", j.Done, "changed", j.Changed,
		"skipped", j.Skipped, "seconds", j.Seconds)
	return j, nil
}

// applySteps — шаги по порядку; при сбое — откат выполненных (undo).
func (s *Service) applySteps(ctx context.Context, id string, uid int, steps []aclStep, prev map[string]Grant,
	progress func(int), from, to int) error {
	var done []aclStep
	for i, st := range steps {
		a := from + (to-from)*i/len(steps)
		b := from + (to-from)*(i+1)/len(steps)
		if _, err := s.runACL(ctx, id, uid, st, progress, a, b); err != nil {
			// откат: текущий шаг тоже (мог примениться частично)
			if e := s.undoSteps(context.WithoutCancel(ctx), id, uid, append(done, st), prev); e != nil {
				return fmt.Errorf("%v; откат доступа не полный: %v", err, e)
			}
			return err
		}
		done = append(done, st)
	}
	return nil
}

// undoSteps — вернуть ACL к prev (выдачи до изменения) по выполненным шагам.
func (s *Service) undoSteps(ctx context.Context, id string, uid int, done []aclStep, prev map[string]Grant) error {
	var errs []string
	for i := len(done) - 1; i >= 0; i-- {
		st := done[i]
		var back aclStep
		if o, ok := prev[st.Share]; ok && o.Pool == st.Pool {
			back = aclStep{Grant: o}
		} else if !st.revoke {
			back = aclStep{Grant: st.Grant, revoke: true}
		} else {
			continue
		}
		if _, err := s.runACL(ctx, id, uid, back, nil, 0, 0); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func byShare(l []Grant) map[string]Grant {
	m := map[string]Grant{}
	for _, g := range l {
		m[g.Share] = g
	}
	return m
}

// revokeAll — отзыв всех выдач (удаление приложения, откат создания).
func (s *Service) revokeAll(ctx context.Context, id string, uid int, l []Grant) error {
	var errs []string
	for _, g := range l {
		if _, err := s.runACL(ctx, id, uid, aclStep{Grant: g, revoke: true}, nil, 0, 0); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// ---------- сверка ACL ----------

// checkAccess — сверка выдач с ACL корней папок; расхождение → повторное
// применение задачей (откат папки из снапшота, ручная правка) и событие.
func (s *Service) checkAccess(ctx context.Context) {
	if s.proj == nil || s.Tasks == nil {
		return
	}
	s.proj.mu.Lock()
	due := s.now().Sub(s.proj.aclAt) >= aclCheckEvery
	if due {
		s.proj.aclAt = s.now()
	}
	s.proj.mu.Unlock()
	if !due {
		return
	}
	m, err := s.loadProjects(ctx)
	if err != nil {
		return
	}
	for _, p := range m {
		if p.Removed || p.UID == 0 || len(p.Grants) == 0 {
			continue
		}
		drift := s.accessDrift(ctx, p)
		if strings.Join(drift, ";") != strings.Join(p.AccessDrift, ";") {
			s.mu.Lock()
			if cur, err := s.loadProjects(ctx); err == nil {
				if q, ok := cur[p.ID]; ok {
					q.AccessDrift = drift
					s.putProject(ctx, q)
				}
			}
			s.mu.Unlock()
		}
		if len(drift) == 0 {
			continue
		}
		s.emit(ctx, EvACLDrift, events.SevWarning, "app:"+p.ID, map[string]any{"app": p.ID, "shares": drift})
		raw, _ := json.Marshal(projTask{ID: p.ID, Actor: "gamayund"})
		if _, err := s.Tasks.Submit(ctx, TaskProjectGrant, raw); err != nil {
			s.Logger.Warn("приложения: повторная выдача доступа не поставлена", "app", p.ID, "err", err)
		}
	}
}

// accessDrift — папки, где ACL корня не соответствует выдаче.
func (s *Service) accessDrift(ctx context.Context, p Project) []string {
	out := []string{}
	for _, g := range p.Grants {
		var a ShareACL
		if err := s.Helper.Call(ctx, OpACLCheck, ShareParams{Pool: g.Pool, Share: g.Share}, &a); err != nil {
			out = append(out, g.Share)
			continue
		}
		u := strconv.Itoa(p.UID)
		if a.Users[u] != g.Mode || a.Default[u] != g.Mode {
			out = append(out, g.Share)
		}
	}
	return out
}

// runProjectGrants — повторное применение выдач (после расхождения).
func (s *Service) runProjectGrants(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	t, _, err := s.taskParams(raw)
	if err != nil {
		return nil, err
	}
	if err := s.lockApp(t.ID); err != nil {
		return nil, err
	}
	defer s.unlockApp(t.ID)
	m, err := s.loadProjects(ctx)
	if err != nil {
		return nil, err
	}
	p, ok := m[t.ID]
	if !ok || p.Removed || p.UID == 0 {
		return nil, wrapf(ErrNotFound, "приложение %s", t.ID)
	}
	var errs []string
	for i, g := range p.Grants {
		a, b := 100*i/len(p.Grants), 100*(i+1)/len(p.Grants)
		if _, err := s.runACL(ctx, p.ID, p.UID, aclStep{Grant: g}, progress, a, b); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	s.mu.Lock()
	if cur, err := s.loadProjects(ctx); err == nil {
		if q, ok := cur[p.ID]; ok {
			q.AccessDrift = nil
			s.putProject(ctx, q)
		}
	}
	s.mu.Unlock()
	return map[string]any{"id": p.ID, "grants": len(p.Grants)}, nil
}

// AppAccess — приложения с доступом к папке (для экрана общих папок).
type AppAccess struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Mode string `json:"mode"`
}

// ShareApps — какие приложения имеют доступ к общей папке.
func (s *Service) ShareApps(ctx context.Context, share string) ([]AppAccess, error) {
	m, err := s.loadProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := []AppAccess{}
	for _, p := range m {
		if p.Removed {
			continue
		}
		for _, g := range p.Grants {
			if g.Share == share {
				out = append(out, AppAccess{ID: p.ID, Name: p.Name, Mode: g.Mode})
			}
		}
		if p.UID == 0 {
			for _, r := range p.Risks {
				if r.Code == compose.RShareBind && strings.HasSuffix(r.Key, ":"+share) {
					out = append(out, AppAccess{ID: p.ID, Name: p.Name, Mode: "root"})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
