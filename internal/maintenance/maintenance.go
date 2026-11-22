// Package maintenance — плановое обслуживание: проверка пулов
// (scrub) и SMART-самотесты дисков по расписанию, опрос SMART и события
// об ухудшении. Расписание и отметки последних запусков — в таблице
// settings; планировщик — внутри gamayund (минутный такт).
package maintenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"gamayun-nas/internal/disks"
	"gamayun-nas/internal/events"
	"gamayun-nas/internal/smart"
	"gamayun-nas/internal/storage/home"
	"gamayun-nas/internal/tasks"
)

// Типы событий (Subject: "pool:<имя>", "disk:<id>").
const (
	EvScrubFinished = "pool.scrub_finished"    // проверка завершена (уровень — по найденному)
	EvScrubFailed   = "pool.scrub_failed"      // проверка не завершена
	EvSmartFailed   = "disk.smart_failed"      // диск сообщает о неисправности (SMART FAILED)
	EvSmartSectors  = "disk.smart_sectors"     // выросли переназначенные/ожидающие/нечитаемые секторы
	EvSmartTestFail = "disk.smart_test_failed" // самотест завершился с ошибкой
	EvSmartTestPass = "disk.smart_test_passed" // самотест завершён без ошибок
	// SMART в диске включён (автоматически или оператором) —
	// info; выключен (не удалось включить — warning, оператором — info).
	EvSmartEnabled  = "disk.smart_enabled"
	EvSmartDisabled = "disk.smart_disabled"
)

// Every — периодичность в днях.
type Every struct {
	Enabled      bool `json:"enabled"`
	IntervalDays int  `json:"interval_days"`
}

// Schedule — расписание обслуживания.
type Schedule struct {
	Scrub      Every `json:"scrub"`
	SmartShort Every `json:"smart_short"`
	SmartLong  Every `json:"smart_long"`
	// Hour — час начала (местное время сервера), 0–23.
	Hour int `json:"hour"`
}

// DefaultSchedule — по умолчанию включено:
// проверка пулов раз в месяц, короткий SMART-тест раз в неделю, длинный —
// раз в месяц, в 3 часа ночи.
func DefaultSchedule() Schedule {
	return Schedule{
		Scrub:      Every{Enabled: true, IntervalDays: 30},
		SmartShort: Every{Enabled: true, IntervalDays: 7},
		SmartLong:  Every{Enabled: true, IntervalDays: 30},
		Hour:       3,
	}
}

// ErrInvalid — неверное расписание.
var ErrInvalid = errors.New("неверное расписание")

// Validate — пределы значений.
func (s Schedule) Validate() error {
	for name, e := range map[string]Every{"scrub": s.Scrub, "smart_short": s.SmartShort, "smart_long": s.SmartLong} {
		if e.IntervalDays < 1 || e.IntervalDays > 365 {
			return fmt.Errorf("%w: %s.interval_days — от 1 до 365", ErrInvalid, name)
		}
	}
	if s.Hour < 0 || s.Hour > 23 {
		return fmt.Errorf("%w: hour — от 0 до 23", ErrInvalid)
	}
	return nil
}

const (
	keySchedule   = "maintenance.schedule"
	keySmartState = "maintenance.smart.state"
	// ID дисков, где SMART выключен оператором (JSON-массив).
	keySmartManualOff = "maintenance.smart.manual_off"
	keyLastPrefix     = "maintenance.last."
)

// Виды запусков для отметок.
const (
	RunSmartShort = "smart_short"
	RunSmartLong  = "smart_long"
)

func scrubKey(pool string) string { return "scrub:" + pool }

// Helper — вызов gamayun-helper.
type Helper interface {
	Call(ctx context.Context, op string, params, result any) error
}

// Storage — пулы (home.Service).
type Storage interface {
	Names(ctx context.Context) ([]string, error)
	Scrub(ctx context.Context, pool, trigger string) (tasks.Task, error)
	ScrubRunning() bool
}

// Options — настройки сервиса.
type Options struct {
	Logger *slog.Logger
	// SmartPoll — период опроса SMART; 0 — 30 мин (температура и
	// история; как smartd по умолчанию, спящий диск не будится).
	SmartPoll time.Duration
	// Tick — такт планировщика; 0 — 1 мин.
	Tick time.Duration
	// Location — часовой пояс расписания; nil — time.Local.
	Location *time.Location
}

// Service — планировщик обслуживания.
type Service struct {
	db      *sql.DB
	helper  Helper
	storage Storage
	log     *events.Log
	opt     Options
	now     func() time.Time

	mu    sync.Mutex // опрос SMART и состояние
	state map[string]smart.Info
	// Объявленный уровень температуры, вид дисков (HDD/SSD) по
	// последнему снимку, время последней точки истории, прореживание.
	temp      map[string]TempState
	tempDirty bool
	kinds     map[string]string
	histAt    map[string]time.Time
	prunedAt  time.Time
	// Автовключение SMART пробовалось (одна попытка, пока диск
	// не увидят включённым).
	autoTried map[string]bool
}

// New создаёт сервис.
func New(db *sql.DB, h Helper, st Storage, log *events.Log, opt Options) *Service {
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.SmartPoll <= 0 {
		opt.SmartPoll = 30 * time.Minute
	}
	if opt.Tick <= 0 {
		opt.Tick = time.Minute
	}
	if opt.Location == nil {
		opt.Location = time.Local
	}
	return &Service{db: db, helper: h, storage: st, log: log, opt: opt, now: time.Now,
		kinds: map[string]string{}, histAt: map[string]time.Time{}, autoTried: map[string]bool{}}
}

// SetClock — подмена времени в тестах.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func (s *Service) get(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Service) set(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Schedule — текущее расписание (по умолчанию, если не задано).
func (s *Service) Schedule(ctx context.Context) (Schedule, error) {
	v, ok, err := s.get(ctx, keySchedule)
	if err != nil || !ok {
		return DefaultSchedule(), err
	}
	sc := DefaultSchedule()
	if err := json.Unmarshal([]byte(v), &sc); err != nil {
		return DefaultSchedule(), fmt.Errorf("расписание в базе: %w", err)
	}
	return sc, nil
}

// SetSchedule сохраняет расписание.
func (s *Service) SetSchedule(ctx context.Context, sc Schedule) (Schedule, error) {
	if err := sc.Validate(); err != nil {
		return Schedule{}, err
	}
	b, _ := json.Marshal(sc)
	return sc, s.set(ctx, keySchedule, string(b))
}

// Last — отметки последних плановых запусков (вид → время).
func (s *Service) Last(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM settings WHERE key LIKE ?", keyLastPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			out[k[len(keyLastPrefix):]] = t
		}
	}
	return out, rows.Err()
}

func (s *Service) markRun(ctx context.Context, kind string, t time.Time) error {
	return s.set(ctx, keyLastPrefix+kind, t.UTC().Format(time.RFC3339))
}

// due — пора ли запуску kind: в час расписания и не раньше интервала
// (минус час — запас на сдвиг такта) после прошлого. Первое появление —
// отметка без запуска (новая установка не начинает с полной проверки).
// Пропущенный за простой запуск выполняется один раз в ближайший час
// расписания, без серии.
func (s *Service) due(ctx context.Context, kind string, e Every, hour int, last map[string]time.Time) (bool, error) {
	if !e.Enabled {
		return false, nil
	}
	now := s.now()
	t, ok := last[kind]
	if !ok {
		return false, s.markRun(ctx, kind, now)
	}
	if now.In(s.opt.Location).Hour() != hour {
		return false, nil
	}
	return now.Sub(t) >= time.Duration(e.IntervalDays)*24*time.Hour-time.Hour, nil
}

// Run — такт планировщика и опрос SMART до отмены ctx.
func (s *Service) Run(ctx context.Context) {
	tick := time.NewTicker(s.opt.Tick)
	defer tick.Stop()
	nextPoll := s.now().Add(time.Minute) // первый опрос — вскоре после старта
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.opt.Logger.Warn("обслуживание: такт планировщика", "err", err)
		}
		if !s.now().Before(nextPoll) {
			nextPoll = s.now().Add(s.opt.SmartPoll)
			if _, err := s.PollSmart(ctx); err != nil && ctx.Err() == nil {
				s.opt.Logger.Warn("обслуживание: опрос SMART", "err", err)
			}
		}
	}
}

// Tick — один такт: плановая проверка пулов (по одному за раз) и
// SMART-самотесты.
func (s *Service) Tick(ctx context.Context) error {
	sc, err := s.Schedule(ctx)
	if err != nil {
		s.opt.Logger.Warn("обслуживание: расписание повреждено, действует по умолчанию", "err", err)
	}
	last, err := s.Last(ctx)
	if err != nil {
		return err
	}
	var errs []error
	if s.storage != nil {
		names, err := s.storage.Names(ctx)
		if err != nil {
			errs = append(errs, err)
		}
		for _, p := range names {
			ok, err := s.due(ctx, scrubKey(p), sc.Scrub, sc.Hour, last)
			if err != nil {
				errs = append(errs, err)
			}
			// Одна проверка за раз: долгое чтение всех дисков двух пулов
			// одновременно делит диски и слоты задач.
			if !ok || s.storage.ScrubRunning() {
				continue
			}
			t, err := s.storage.Scrub(ctx, p, home.ScrubSchedule)
			if err != nil {
				// Пул занят/не исправен — повтор следующим тактом в этот
				// же час, иначе — на следующий день.
				s.opt.Logger.Info("обслуживание: плановая проверка пула отложена", "pool", p, "err", err)
				continue
			}
			s.opt.Logger.Info("обслуживание: плановая проверка пула", "pool", p, "task", t.ID)
			errs = append(errs, s.markRun(ctx, scrubKey(p), s.now()))
			break
		}
	}
	// Длинный тест включает короткий: в день длинного короткий не нужен.
	long, err := s.due(ctx, RunSmartLong, sc.SmartLong, sc.Hour, last)
	errs = append(errs, err)
	short, err := s.due(ctx, RunSmartShort, sc.SmartShort, sc.Hour, last)
	errs = append(errs, err)
	switch {
	case long:
		errs = append(errs, s.startTests(ctx, smart.TestLong))
		errs = append(errs, s.markRun(ctx, RunSmartLong, s.now()), s.markRun(ctx, RunSmartShort, s.now()))
	case short:
		errs = append(errs, s.startTests(ctx, smart.TestShort))
		errs = append(errs, s.markRun(ctx, RunSmartShort, s.now()))
	}
	return errors.Join(errs...)
}

// startTests запускает самотест на всех дисках с SMART (по последнему
// опросу). Итог читает опрос SMART (журнал самотестов диска).
func (s *Service) startTests(ctx context.Context, typ string) error {
	if _, err := s.PollSmart(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	var ids []string
	for id, in := range s.state {
		if in.Supported && !in.Standby && !in.TestRunning {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	sort.Strings(ids)
	var errs []error
	for _, id := range ids {
		if _, err := s.StartTest(ctx, id, typ); err != nil {
			errs = append(errs, fmt.Errorf("диск %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// StartTest запускает самотест диска.
func (s *Service) StartTest(ctx context.Context, id, typ string) (smart.TestResult, error) {
	var r smart.TestResult
	err := s.helper.Call(ctx, smart.OpTest, smart.TestParams{DiskID: id, Type: typ}, &r)
	if err == nil {
		s.opt.Logger.Info("обслуживание: SMART-самотест", "disk", id, "type", typ, "started", r.Started, "message", r.Message)
	}
	return r, err
}

// Smart — сведения SMART последнего опроса (по ID дисков).
func (s *Service) Smart(ctx context.Context) (map[string]smart.Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(ctx); err != nil {
		return nil, err
	}
	out := make(map[string]smart.Info, len(s.state))
	for k, v := range s.state {
		out[k] = v
	}
	return out, nil
}

func (s *Service) loadLocked(ctx context.Context) error {
	if err := s.loadTempLocked(ctx); err != nil {
		return err
	}
	if s.state != nil {
		return nil
	}
	s.state = map[string]smart.Info{}
	v, ok, err := s.get(ctx, keySmartState)
	if err != nil || !ok {
		return err
	}
	if err := json.Unmarshal([]byte(v), &s.state); err != nil {
		s.state = map[string]smart.Info{}
		s.opt.Logger.Warn("обслуживание: состояние SMART в базе повреждено, начато заново", "err", err)
	}
	return nil
}

// ReadDisk — свежий опрос одного диска (с событиями об изменениях).
func (s *Service) ReadDisk(ctx context.Context, id string) (smart.Info, error) {
	var in smart.Info
	if err := s.helper.Call(ctx, smart.OpRead, smart.DiskParams{DiskID: id}, &in); err != nil {
		return in, err
	}
	man, err := s.manualOff(ctx)
	if err != nil {
		return in, err
	}
	in = s.autoEnable(ctx, id, in, man)
	th := s.thresholds(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(ctx); err != nil {
		return in, err
	}
	s.compareLocked(ctx, id, in, th)
	return in, s.persistLocked(ctx, map[string]*Point{id: s.histPointLocked(id, in)})
}

// thresholds — пороги температуры; испорченная запись — по умолчанию.
func (s *Service) thresholds(ctx context.Context) Thresholds {
	th, err := s.Thresholds(ctx)
	if err != nil {
		s.opt.Logger.Warn("обслуживание: пороги температуры, действуют по умолчанию", "err", err)
	}
	return th
}

// PollSmart опрашивает SMART всех дисков с ID и сообщает об ухудшениях.
// Диски без SMART (виртуальные, мосты) запоминаются как неподдерживаемые.
func (s *Service) PollSmart(ctx context.Context) (map[string]smart.Info, error) {
	var snap disks.Snapshot
	if err := s.helper.Call(ctx, "disks.observe", nil, &snap); err != nil {
		return nil, err
	}
	got := map[string]smart.Info{}
	kinds := map[string]string{}
	var errs []error
	for _, d := range snap.Disks {
		if d.ID == "" {
			continue
		}
		kinds[d.ID] = d.Kind
		var in smart.Info
		if err := s.helper.Call(ctx, smart.OpRead, smart.DiskParams{DiskID: d.ID}, &in); err != nil {
			errs = append(errs, fmt.Errorf("диск %s: %w", d.ID, err))
			continue
		}
		got[d.ID] = in
	}
	man, err := s.manualOff(ctx)
	if err != nil {
		return nil, err
	}
	for id, in := range got {
		got[id] = s.autoEnable(ctx, id, in, man)
	}
	th := s.thresholds(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(ctx); err != nil {
		return nil, err
	}
	for id, k := range kinds {
		s.kinds[id] = k
	}
	pts := map[string]*Point{}
	for id, in := range got {
		s.compareLocked(ctx, id, in, th)
		pts[id] = s.histPointLocked(id, in)
	}
	// Пропавшие диски не удаляются: их возвращение сравнится с прошлым.
	if err := s.persistLocked(ctx, pts); err != nil {
		errs = append(errs, err)
	}
	return got, errors.Join(errs...)
}

func val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// compareLocked — события по изменению сведений диска id и запоминание.
// Спящий диск (Standby) — без изменений, прошлое значение сохраняется.
func (s *Service) compareLocked(ctx context.Context, id string, in smart.Info, th Thresholds) {
	prev, had := s.state[id]
	if in.Standby {
		if !had {
			s.state[id] = in
		}
		return
	}
	s.state[id] = in
	emit := func(typ, sev string, data map[string]any) { s.emitDisk(ctx, id, in.Model, typ, sev, data) }
	// Выключен и не оператором (автовключение не помогло) —
	// предупреждение один раз, до включения.
	if in.Disabled && !in.ManualOff && (!had || !prev.Disabled || prev.ManualOff) {
		emit(EvSmartDisabled, events.SevWarning, map[string]any{"reason": "не удалось включить, состояние диска не отслеживается"})
	}
	if !in.Supported {
		return
	}
	s.checkTempLocked(ctx, id, in, th, emit)
	if in.Passed != nil && !*in.Passed && (!had || prev.Passed == nil || *prev.Passed) {
		emit(EvSmartFailed, events.SevCritical, map[string]any{"messages": in.Messages})
	}
	type ctr struct {
		name      string
		cur, prev *int64
	}
	// Одно событие на изменение (не по письму на каждый счётчик).
	grown := map[string]any{}
	for _, c := range []ctr{{"reallocated", in.Reallocated, prev.Reallocated}, {"pending", in.Pending, prev.Pending},
		{"uncorrectable", in.Uncorrectable, prev.Uncorrectable}} {
		if c.cur != nil && val(c.cur) > val(c.prev) {
			grown[c.name] = map[string]int64{"value": val(c.cur), "previous": val(c.prev)}
		}
	}
	if len(grown) > 0 {
		emit(EvSmartSectors, events.SevWarning, map[string]any{"grown": grown})
	}
	if t := in.LastTest; t != nil && had && !sameTest(t, prev.LastTest) {
		if t.Passed {
			emit(EvSmartTestPass, events.SevInfo, map[string]any{"test": t.Type, "status": t.Status})
		} else {
			emit(EvSmartTestFail, events.SevCritical, map[string]any{"test": t.Type, "status": t.Status})
		}
	}
}

// emitDisk — событие по диску id.
func (s *Service) emitDisk(ctx context.Context, id, model, typ, sev string, data map[string]any) {
	data["disk_id"] = id
	if model != "" {
		data["model"] = model
	}
	raw, _ := json.Marshal(data)
	if _, err := s.log.Emit(ctx, events.Event{Type: typ, Severity: sev, Subject: "disk:" + id, Data: raw}, true); err != nil {
		s.opt.Logger.Error("обслуживание: событие", "type", typ, "err", err)
	}
}

// manualOff — диски, где SMART выключен оператором.
func (s *Service) manualOff(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	v, ok, err := s.get(ctx, keySmartManualOff)
	if err != nil || !ok {
		return out, err
	}
	var ids []string
	if err := json.Unmarshal([]byte(v), &ids); err != nil {
		s.opt.Logger.Warn("обслуживание: список ручного выключения SMART повреждён, не учитывается", "err", err)
		return out, nil
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

func (s *Service) setManualOff(ctx context.Context, id string, off bool) error {
	m, err := s.manualOff(ctx)
	if err != nil {
		return err
	}
	if off {
		m[id] = true
	} else {
		delete(m, id)
	}
	ids := make([]string, 0, len(m))
	for k := range m {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	b, _ := json.Marshal(ids)
	return s.set(ctx, keySmartManualOff, string(b))
}

// autoEnable — SMART выключен в диске не оператором — одна
// попытка включить (smartctl -s on), событие info при успехе. Неудача —
// в журнал службы; предупреждение даёт compareLocked.
func (s *Service) autoEnable(ctx context.Context, id string, in smart.Info, man map[string]bool) smart.Info {
	in.ManualOff = man[id] && in.Disabled
	s.mu.Lock()
	if !in.Disabled {
		delete(s.autoTried, id)
	}
	try := in.Disabled && !man[id] && !s.autoTried[id]
	if try {
		s.autoTried[id] = true
	}
	s.mu.Unlock()
	if !try {
		return in
	}
	var got smart.Info
	if err := s.helper.Call(ctx, smart.OpSet, smart.SetParams{DiskID: id, On: true}, &got); err != nil || got.Disabled {
		s.opt.Logger.Warn("обслуживание: SMART не включён автоматически", "disk", id, "err", err)
		return in
	}
	s.mu.Lock()
	delete(s.autoTried, id) // включён: если выключат снаружи снова — новая попытка
	s.mu.Unlock()
	s.opt.Logger.Info("обслуживание: SMART включён автоматически", "disk", id)
	s.emitDisk(ctx, id, got.Model, EvSmartEnabled, events.SevInfo, map[string]any{"by": "auto"})
	return got
}

// SetSmart — включить/выключить SMART в диске вручную. Выключение
// запоминается: автоматика такой диск не включает.
func (s *Service) SetSmart(ctx context.Context, id string, on bool, actor string) (smart.Info, error) {
	var in smart.Info
	if err := s.helper.Call(ctx, smart.OpSet, smart.SetParams{DiskID: id, On: on}, &in); err != nil {
		return in, err
	}
	if err := s.setManualOff(ctx, id, !on); err != nil {
		return in, err
	}
	typ := EvSmartEnabled
	if !on {
		typ = EvSmartDisabled
		in.ManualOff = in.Disabled
	}
	s.emitDisk(ctx, id, in.Model, typ, events.SevInfo, map[string]any{"by": "operator", "actor": actor})
	th := s.thresholds(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(ctx); err != nil {
		return in, err
	}
	delete(s.autoTried, id)
	s.compareLocked(ctx, id, in, th)
	return in, s.persistLocked(ctx, map[string]*Point{id: s.histPointLocked(id, in)})
}

func sameTest(a, b *smart.SelfTest) bool {
	if b == nil {
		return false
	}
	return a.Type == b.Type && a.Status == b.Status && val(a.LifetimeHours) == val(b.LifetimeHours)
}

// OnScrub — событие по итогу проверки пула (home.Service.OnScrub).
func (s *Service) OnScrub(r home.ScrubResult, err error) {
	ctx := context.Background()
	data := map[string]any{"pool": r.Pool, "trigger": r.Trigger}
	typ, sev := EvScrubFinished, r.Severity()
	if err != nil {
		typ, sev = EvScrubFailed, events.SevWarning
		data["error"] = err.Error()
	} else {
		data["cancelled"] = r.Cancelled
		data["mismatch_cnt"] = r.MismatchCnt
		data["integrity_errors_delta"] = r.IntegrityErrorsDelta
		if r.Btrfs != nil {
			data["btrfs_errors"] = r.Btrfs.Errors()
			data["btrfs_uncorrectable"] = r.Btrfs.UncorrectableErrors
			data["btrfs_corrected"] = r.Btrfs.CorrectedErrors
			data["bytes_scrubbed"] = r.Btrfs.DataBytesScrubbed + r.Btrfs.TreeBytesScrubbed
		}
		if r.Cancelled {
			sev = events.SevInfo
		}
	}
	raw, _ := json.Marshal(data)
	if _, err := s.log.Emit(ctx, events.Event{Type: typ, Severity: sev, Subject: "pool:" + r.Pool, Data: raw}, true); err != nil {
		s.opt.Logger.Error("обслуживание: событие проверки", "err", err)
	}
	if err == nil && !r.Cancelled {
		// Ручная проверка тоже сдвигает плановую.
		if e := s.markRun(ctx, scrubKey(r.Pool), s.now()); e != nil {
			s.opt.Logger.Warn("обслуживание: отметка проверки", "err", e)
		}
	}
}
