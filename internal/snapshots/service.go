package snapshots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/tasks"
)

// Типы событий (Subject: "share:<папка>").
const (
	EvCreated       = "snapshot.created"        // ручной снапшот создан
	EvDeleted       = "snapshot.deleted"        // снапшот удалён вручную
	EvRestored      = "snapshot.restored"       // копия из снапшота восстановлена
	EvRestoreFailed = "snapshot.restore_failed" // восстановление не выполнено
	EvSkipped       = "snapshot.skipped"        // снапшот по расписанию пропущен (мало места, только чтение)
	EvFailed        = "snapshot.failed"         // снапшот по расписанию или ротация не выполнены
)

// TaskRestore — вид задачи восстановления (долгое копирование большой папки
// не укладывается в предел HTTP-ответа).
const TaskRestore = "nas.snapshot.restore"

// RestoreTimeout — предел операции helper snapshot.restore.
const RestoreTimeout = 30 * time.Minute

// DefaultMinFreePct — снапшот по расписанию пропускается, если на пуле
// свободно меньше 10 %.
const DefaultMinFreePct = 10

const (
	keyPolicy = "snapshots.policy."
	keyLast   = "snapshots.last."
)

// Helper — вызов gamayun-helper.
type Helper interface {
	Call(ctx context.Context, op string, params, result any) error
}

// Target — папка со снапшотами: общая папка или служба домашних папок.
type Target struct {
	Share string
	Pool  string
}

// Service — снапшоты в gamayund: расписание, ротация, ручные операции.
type Service struct {
	DB     *sql.DB
	Helper Helper
	Events *events.Log
	Logger *slog.Logger
	// Targets — текущие папки и их пулы.
	Targets func(ctx context.Context) ([]Target, error)
	// Tasks — менеджер задач (восстановление); nil — восстановление недоступно.
	Tasks *tasks.Manager
	// Location — часовой пояс расписания и ротации; nil — time.Local.
	Location *time.Location
	// Tick — такт планировщика; 0 — 1 мин.
	Tick time.Duration
	// MinFreePct — порог свободного места для снапшота по расписанию; 0 — DefaultMinFreePct.
	MinFreePct int
	// Now — часы (тесты); nil — time.Now.
	Now func() time.Time

	mu   sync.Mutex        // создание, удаление, ротация
	smu  sync.Mutex        // busy, skip
	busy map[string]int    // "папка/снапшот" → восстановлений в работе
	skip map[string]string // папка → причина последнего пропуска (одно событие на смену)
}

// Init регистрирует вид задачи восстановления.
func (s *Service) Init() error {
	if s.Tasks == nil {
		return nil
	}
	return s.Tasks.Register(TaskRestore, s.runRestore)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) loc() *time.Location {
	if s.Location != nil {
		return s.Location
	}
	return time.Local
}

func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Service) minFree() int {
	if s.MinFreePct > 0 {
		return s.MinFreePct
	}
	return DefaultMinFreePct
}

func (s *Service) get(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Service) set(ctx context.Context, key, value string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// target — папка по имени.
func (s *Service) target(ctx context.Context, share string) (Target, error) {
	if !ValidShare(share) {
		return Target{}, fmt.Errorf("%w: имя папки", ErrInvalid)
	}
	ts, err := s.Targets(ctx)
	if err != nil {
		return Target{}, err
	}
	for _, t := range ts {
		if t.Share == share {
			return t, nil
		}
	}
	return Target{}, fmt.Errorf("%w: папка %s", ErrNotFound, share)
}

// Policy — расписание папки (по умолчанию, если не задано).
func (s *Service) Policy(ctx context.Context, share string) (Policy, error) {
	v, ok, err := s.get(ctx, keyPolicy+share)
	if err != nil || !ok {
		return DefaultPolicy(), err
	}
	p := DefaultPolicy()
	if err := json.Unmarshal([]byte(v), &p); err != nil {
		return DefaultPolicy(), fmt.Errorf("расписание снимков %s в базе: %w", share, err)
	}
	return p, nil
}

// SetPolicy сохраняет расписание папки. Ротация по новым пределам — при
// следующем снапшоте по расписанию.
func (s *Service) SetPolicy(ctx context.Context, share string, p Policy) (Policy, error) {
	if err := p.Validate(); err != nil {
		return Policy{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if _, err := s.target(ctx, share); err != nil {
		return Policy{}, err
	}
	b, _ := json.Marshal(p)
	return p, s.set(ctx, keyPolicy+share, string(b))
}

// ShareInfo — снапшоты папки для API.
type ShareInfo struct {
	Share     string     `json:"share"`
	Pool      string     `json:"pool"`
	Subvolume bool       `json:"subvolume"`
	ReadOnly  bool       `json:"read_only"`
	FreePct   float64    `json:"free_pct"`
	Policy    Policy     `json:"policy"`
	Snapshots []Snapshot `json:"snapshots"`
}

// Info — снапшоты, расписание и состояние папки.
func (s *Service) Info(ctx context.Context, share string) (ShareInfo, error) {
	t, err := s.target(ctx, share)
	if err != nil {
		return ShareInfo{}, err
	}
	var l ListResult
	if err := s.Helper.Call(ctx, OpList, ShareParams{Pool: t.Pool, Share: t.Share}, &l); err != nil {
		return ShareInfo{}, err
	}
	p, err := s.Policy(ctx, share)
	if err != nil {
		s.logger().Warn("снимки: расписание повреждено, действует по умолчанию", "share", share, "err", err)
	}
	info := ShareInfo{Share: t.Share, Pool: t.Pool, Subvolume: l.Subvolume, ReadOnly: l.ReadOnly, Policy: p, Snapshots: l.Snapshots}
	if l.Total > 0 {
		info.FreePct = float64(l.Avail) * 100 / float64(l.Total)
	}
	if info.Snapshots == nil {
		info.Snapshots = []Snapshot{}
	}
	return info, nil
}

func (s *Service) emit(ctx context.Context, typ, sev, share string, data map[string]any) {
	if s.Events == nil {
		return
	}
	b, _ := json.Marshal(data)
	if _, err := s.Events.Emit(ctx, events.Event{Type: typ, Severity: sev, Subject: "share:" + share, Data: b}, true); err != nil {
		s.logger().Warn("снимки: событие не записано", "type", typ, "err", err)
	}
}

// Create — ручной снапшот папки (не ротируется).
func (s *Service) Create(ctx context.Context, share string) (Snapshot, error) {
	t, err := s.target(ctx, share)
	if err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var r CreateResult
	if err := s.Helper.Call(ctx, OpCreate, CreateParams{Pool: t.Pool, Share: t.Share}, &r); err != nil {
		return Snapshot{}, err
	}
	tm, _ := NameTime(r.Name)
	s.emit(ctx, EvCreated, events.SevInfo, share, map[string]any{"share": share, "pool": t.Pool, "snapshot": r.Name})
	return Snapshot{Name: r.Name, Time: tm}, nil
}

func busyKey(share, name string) string { return share + "/" + name }

func (s *Service) isBusy(share, name string) bool {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.busy[busyKey(share, name)] > 0
}

func (s *Service) setBusy(share, name string, d int) {
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.busy == nil {
		s.busy = map[string]int{}
	}
	k := busyKey(share, name)
	s.busy[k] += d
	if s.busy[k] <= 0 {
		delete(s.busy, k)
	}
}

// Delete удаляет снапшот вручную (ручной или по расписанию). Снапшот, из
// которого идёт восстановление, не удаляется (ErrConflict).
func (s *Service) Delete(ctx context.Context, share, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("%w: имя снимка", ErrInvalid)
	}
	t, err := s.target(ctx, share)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isBusy(share, name) {
		return fmt.Errorf("%w: из снимка %s идёт восстановление", ErrConflict, name)
	}
	if err := s.Helper.Call(ctx, OpDelete, SnapParams{Pool: t.Pool, Share: t.Share, Name: name}, nil); err != nil {
		return err
	}
	s.emit(ctx, EvDeleted, events.SevInfo, share, map[string]any{"share": share, "pool": t.Pool, "snapshot": name})
	return nil
}

// Browse — содержимое каталога rel в снапшоте.
func (s *Service) Browse(ctx context.Context, share, name, rel string) (BrowseResult, error) {
	if !ValidName(name) {
		return BrowseResult{}, fmt.Errorf("%w: имя снимка", ErrInvalid)
	}
	if _, err := CleanRel(rel); err != nil {
		return BrowseResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	t, err := s.target(ctx, share)
	if err != nil {
		return BrowseResult{}, err
	}
	var r BrowseResult
	if err := s.Helper.Call(ctx, OpBrowse, PathParams{Pool: t.Pool, Share: t.Share, Name: name, Path: rel}, &r); err != nil {
		return BrowseResult{}, err
	}
	return r, nil
}

// restoreParams — параметры задачи восстановления.
type restoreParams struct {
	Share string `json:"share"`
	Pool  string `json:"pool"`
	Name  string `json:"name"`
	Path  string `json:"path"`
}

// Restore ставит задачу восстановления файла или папки rel из снапшота
// копией рядом с исходным местом.
func (s *Service) Restore(ctx context.Context, share, name, rel string) (tasks.Task, error) {
	if !ValidName(name) {
		return tasks.Task{}, fmt.Errorf("%w: имя снимка", ErrInvalid)
	}
	if r, err := CleanRel(rel); err != nil || r == "" {
		if err == nil {
			err = errors.New("укажите файл или папку внутри снимка")
		}
		return tasks.Task{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if s.Tasks == nil {
		return tasks.Task{}, errors.New("менеджер задач не настроен")
	}
	t, err := s.target(ctx, share)
	if err != nil {
		return tasks.Task{}, err
	}
	b, _ := json.Marshal(restoreParams{Share: t.Share, Pool: t.Pool, Name: name, Path: rel})
	return s.Tasks.Submit(ctx, TaskRestore, b)
}

func (s *Service) runRestore(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	var p restoreParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	s.setBusy(p.Share, p.Name, 1)
	defer s.setBusy(p.Share, p.Name, -1)
	tasks.Expect(ctx, RestoreTimeout, "копирование из снимка "+p.Name)
	var r RestoreResult
	err := s.Helper.Call(ctx, OpRestore, PathParams{Pool: p.Pool, Share: p.Share, Name: p.Name, Path: p.Path}, &r)
	data := map[string]any{"share": p.Share, "pool": p.Pool, "snapshot": p.Name, "path": p.Path}
	if err != nil {
		data["error"] = err.Error()
		s.emit(context.Background(), EvRestoreFailed, events.SevWarning, p.Share, data)
		return nil, err
	}
	data["restored_as"] = r.Path
	data["files"], data["dirs"], data["bytes"] = r.Files, r.Dirs, r.Bytes
	if r.Skipped > 0 {
		data["skipped"] = r.Skipped
	}
	s.emit(context.Background(), EvRestored, events.SevInfo, p.Share, data)
	progress(100)
	return r, nil
}

// Run — такт планировщика до отмены ctx.
func (s *Service) Run(ctx context.Context) {
	d := s.Tick
	if d <= 0 {
		d = time.Minute
	}
	tick := time.NewTicker(d)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			s.logger().Warn("снимки: такт планировщика", "err", err)
		}
	}
}

func (s *Service) lastAttempt(ctx context.Context, share string) (time.Time, bool) {
	v, ok, err := s.get(ctx, keyLast+share)
	if err != nil || !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	return t, err == nil
}

// RunOnce — один такт: снапшоты по расписанию и ротация для каждой папки,
// которой пора. Отметка попытки ставится и при пропуске/ошибке: повтор — в
// следующем интервале, без серии попыток каждую минуту.
func (s *Service) RunOnce(ctx context.Context) error {
	ts, err := s.Targets(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range ts {
		p, err := s.Policy(ctx, t.Share)
		if err != nil {
			s.logger().Warn("снимки: расписание повреждено, действует по умолчанию", "share", t.Share, "err", err)
		}
		last, ok := s.lastAttempt(ctx, t.Share)
		now := s.now()
		if !p.Due(now, last, ok, s.loc()) {
			continue
		}
		errs = append(errs, s.scheduled(ctx, t, p))
		errs = append(errs, s.set(ctx, keyLast+t.Share, now.UTC().Format(time.RFC3339)))
	}
	return errors.Join(errs...)
}

// scheduled — снапшот по расписанию и ротация одной папки.
func (s *Service) scheduled(ctx context.Context, t Target, p Policy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var l ListResult
	if err := s.Helper.Call(ctx, OpList, ShareParams{Pool: t.Pool, Share: t.Share}, &l); err != nil {
		s.failed(ctx, t, "список снимков", err)
		return nil
	}
	if !l.Subvolume {
		return nil // старая папка: снапшоты недоступны (видно в интерфейсе)
	}
	var r CreateResult
	if err := s.Helper.Call(ctx, OpCreate, CreateParams{Pool: t.Pool, Share: t.Share, Auto: true, MinFreePct: s.minFree()}, &r); err != nil {
		s.failed(ctx, t, "создание снимка", err)
		return nil
	}
	if r.Skipped != "" {
		s.skipped(ctx, t, r)
		return nil
	}
	s.smu.Lock()
	delete(s.skip, t.Share)
	s.smu.Unlock()
	for _, w := range r.Warnings {
		s.logger().Warn("снимки: предупреждение helper", "share", t.Share, "msg", w)
	}
	s.logger().Info("снимки: снимок по расписанию", "share", t.Share, "snapshot", r.Name)
	if tm, ok := NameTime(r.Name); ok && len(r.Warnings) == 0 {
		l.Snapshots = append(l.Snapshots, Snapshot{Name: r.Name, Time: tm, Auto: true})
	}
	for _, name := range p.Expired(l.Snapshots, s.loc()) {
		if s.isBusy(t.Share, name) {
			continue // идёт восстановление — удалится следующим проходом
		}
		if err := s.Helper.Call(ctx, OpDelete, SnapParams{Pool: t.Pool, Share: t.Share, Name: name}, nil); err != nil {
			s.failed(ctx, t, "ротация (удаление "+name+")", err)
			return nil
		}
		s.logger().Info("снимки: удалён ротацией", "share", t.Share, "snapshot", name)
	}
	return nil
}

func (s *Service) failed(ctx context.Context, t Target, what string, err error) {
	s.logger().Warn("снимки: "+what, "share", t.Share, "err", err)
	s.emit(ctx, EvFailed, events.SevWarning, t.Share, map[string]any{"share": t.Share, "pool": t.Pool, "step": what, "error": err.Error()})
}

// skipped — событие о пропуске только при смене причины (не каждый час).
func (s *Service) skipped(ctx context.Context, t Target, r CreateResult) {
	s.smu.Lock()
	if s.skip == nil {
		s.skip = map[string]string{}
	}
	same := s.skip[t.Share] == r.Skipped
	s.skip[t.Share] = r.Skipped
	s.smu.Unlock()
	s.logger().Warn("снимки: снимок по расписанию пропущен", "share", t.Share, "reason", r.Skipped, "free_pct", r.FreePct)
	if same {
		return
	}
	data := map[string]any{"share": t.Share, "pool": t.Pool, "reason": r.Skipped, "free_pct": int(r.FreePct)}
	if r.Skipped == SkipLowSpace {
		data["min_free_pct"] = s.minFree()
	}
	s.emit(ctx, EvSkipped, events.SevWarning, t.Share, data)
}
