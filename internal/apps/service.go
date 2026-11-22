package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"gamayun-nas/internal/catalog"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/storage/home"
	"gamayun-nas/internal/tasks"
)

// Состояния подсистемы (коды для перевода в вебе).
const (
	StateDisabled     = "disabled"      // раздел не включён
	StateNotInstalled = "not_installed" // включён, но пакета gamayun-nas-apps нет
	StateUnavailable  = "unavailable"   // пул приложений не подключён
	StatePaused       = "paused"        // приостановлено: пул только для чтения
	StateStarting     = "starting"
	StateRunning      = "running"
	StateBusy         = "busy"    // Docker остановлен, но пул держит посторонний процесс
	StateError        = "error"   // запуск не удался; ждёт действия администратора
	StateUnknown      = "unknown" // состояние не прочитано (helper недоступен)
)

// Коды причин (reason_code).
const (
	ReasonNotInstalled = "not_installed"
	ReasonPoolMissing  = "pool_missing"
	ReasonReadOnly     = "read_only"  // пул ушёл в RO (ошибка записи или защита)
	ReasonProtection   = "protection" // плановый RO защиты
	ReasonResumeReady  = "resume_ready"
	ReasonStartFailed  = "start_failed"
	ReasonPoolBusy     = "pool_busy" // остановка/перемонтирование: пул занят процессом
	ReasonHelper       = "helper"
	ReasonConfig       = "config"
)

// Типы событий (Subject: "pool:<пул>" для подсистемы, "container:<имя>" для контейнеров).
const (
	EvEnabled            = "apps.enabled"
	EvDisabled           = "apps.disabled"
	EvStarted            = "apps.started"
	EvStartFailed        = "apps.start_failed"
	EvPaused             = "apps.paused"
	EvUnavailable        = "apps.unavailable"
	EvResumed            = "apps.resumed"
	EvPoolBusy           = "apps.pool_busy"
	EvContainerDied      = "apps.container_died"
	EvContainerOOM       = "apps.container_oom"
	EvContainerUnhealthy = "apps.container_unhealthy"
	EvContainerHealthy   = "apps.container_healthy"
	EvContainerRestart   = "apps.container_restart"
	EvAppWeb             = "apps.app_web" // публикация через прокси (data: app, name, port, actor)
)

// Виды задач.
const (
	TaskEnable  = "apps.enable"
	TaskDisable = "apps.disable"
	TaskResume  = "apps.resume"
)

const settingsKey = "apps.config"

// Ошибки сервиса (API: 404/409/400/503).
var (
	ErrNotFound    = errors.New("не найдено")
	ErrConflictSvc = errors.New("конфликт состояния")
	ErrInvalidSvc  = errors.New("недопустимый запрос")
	ErrUnavailable = errors.New("недоступно")
)

func wrapf(base error, format string, a ...any) error {
	return fmt.Errorf("%w: %s", base, fmt.Sprintf(format, a...))
}

// Config — настройка в settings.
type Config struct {
	Enabled bool   `json:"enabled"`
	Pool    string `json:"pool,omitempty"`
	// Paused — причина паузы (read_only, protection); снимается только
	// подтверждением администратора (resume).
	Paused string `json:"paused,omitempty"`
	// Error — причина последнего неудачного запуска; повтор — раз в
	// RetryAfter, сразу — после resume, возврата пула и перезапуска gamayund.
	Error   string    `json:"error,omitempty"`
	ErrorAt time.Time `json:"error_at,omitempty"`
}

// View — состояние для API.
type View struct {
	Installed  bool      `json:"installed"`
	Enabled    bool      `json:"enabled"`
	Pool       string    `json:"pool,omitempty"`
	State      string    `json:"state"`
	ReasonCode string    `json:"reason_code,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Docker     string    `json:"docker,omitempty"`
	Version    string    `json:"version,omitempty"`
	Running    int       `json:"containers_running"`
	Total      int       `json:"containers_total"`
	CheckedAt  time.Time `json:"checked_at"`
}

// Helper — вызов gamayun-helper.
type Helper interface {
	Call(ctx context.Context, op string, params, result any) error
}

// Pools — пулы gamayund (home.Service).
type Pools interface {
	Get(ctx context.Context, name string) (home.PoolView, error)
}

// Service — подсистема приложений в gamayund.
type Service struct {
	DB     *sql.DB
	Helper Helper // короткие операции и запуск/остановка (таймаут ≥ 5 мин)
	Events *events.Log
	Tasks  *tasks.Manager
	Pools  Pools
	Logger *slog.Logger
	// Interval — период сверки; 0 — 15 с.
	Interval time.Duration
	// EventsHelper — долгий опрос apps.events (таймаут > MaxWait); nil — Helper.
	EventsHelper Helper
	// EventsWait — ожидание одного опроса событий; 0 — 50 с.
	EventsWait int
	// Shares — общие папки (bind в compose); nil — без общих папок.
	Shares Shares
	// ProcNet — /proc/net (занятые порты); пусто — /proc/net.
	ProcNet string
	// Passwd, Group — занятые UID/GID ОС; пусто — /etc/passwd, /etc/group.
	Passwd, Group string
	// OnRoutes — публикации через прокси изменились (proxy.Service.Trigger).
	OnRoutes func()
	// WebName — проверка имени публикации с именем NAS (proxy.Service.CheckName); nil — без проверки.
	WebName func(name string) error
	// Catalog — каталог приложений; nil — установка из каталога недоступна.
	Catalog CatalogSource
	// SysInfo — сведения о NAS для совместимости; nil — версия, GOARCH, /proc/meminfo, statfs пула.
	SysInfo func(ctx context.Context) (catalog.System, error)
	// HealthPoll — период опроса готовности после установки; 0 — 2 с.
	HealthPoll time.Duration
	uidMu      sync.Mutex
	proj       *projState
	now        func() time.Time

	mu      sync.Mutex // действия над Docker — по одному
	viewMu  sync.Mutex
	view    View
	trigger chan struct{}

	evMu      sync.Mutex
	lastEv    map[string]time.Time
	suppr     map[string]int
	unhealthy map[string]bool
	retryNow  bool      // повторить запуск, не дожидаясь RetryAfter (под s.mu)
	stopFrom  time.Time // окно остановки Docker самим gamayund: die 0/137/143 — не сбой
	stopTo    time.Time

	// busy — последняя остановка (busyStop) или перемонтирование
	// (busyRemount) отказала: пул занят посторонним процессом; под s.mu.
	busy       string
	busyReason string
}

const (
	busyStop    = "stop"
	busyRemount = "remount"
)

// isPoolBusy — ошибка helper «пул … занят» (apps.stop, apps.pool_remount).
func isPoolBusy(err error) bool {
	return err != nil && poolBusyRe.MatchString(err.Error())
}

var poolBusyRe = regexp.MustCompile(`пул( \S+)? занят`)

// setBusy запоминает отказ «пул занят» (from — busyStop/busyRemount).
func (s *Service) setBusy(from string, err error) {
	s.busy, s.busyReason = from, err.Error()
}

// checkBusy — пул освободился? Тогда состояние «пул занят» сбрасывается.
func (s *Service) checkBusy(ctx context.Context, pool string) {
	if s.busy == "" {
		return
	}
	var r PoolBusyResult
	if err := s.Helper.Call(ctx, OpPoolBusy, PoolParams{Pool: pool}, &r); err != nil {
		s.Logger.Warn("приложения: занятость пула не проверена", "pool", pool, "err", err)
		return
	}
	list := r.Apps
	if s.busy == busyRemount {
		list = append(append([]string(nil), r.Apps...), r.Holders...)
	}
	if len(list) == 0 {
		s.Logger.Info("приложения: пул освободился", "pool", pool)
		s.busy, s.busyReason = "", ""
		return
	}
	s.busyReason = fmt.Sprintf("пул %s занят: %s", pool, strings.Join(list, "; "))
}

// Init регистрирует задачи; вызывать до Run.
func (s *Service) Init() error {
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.trigger = make(chan struct{}, 1)
	s.lastEv, s.suppr, s.unhealthy = map[string]time.Time{}, map[string]int{}, map[string]bool{}
	s.view = View{State: StateUnknown}
	s.retryNow = true // после перезапуска gamayund (загрузки) — попытка запуска
	if err := s.initProjects(); err != nil {
		return err
	}
	if s.Tasks == nil {
		return nil
	}
	for k, fn := range map[string]tasks.RunFunc{TaskEnable: s.runEnable, TaskDisable: s.runDisable, TaskResume: s.runResume} {
		if err := s.Tasks.Register(k, fn); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) load(ctx context.Context) (Config, error) {
	var c Config
	var v string
	err := s.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", settingsKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal([]byte(v), &c)
}

func (s *Service) save(ctx context.Context, c Config) error {
	b, _ := json.Marshal(c)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, settingsKey, string(b))
	return err
}

func (s *Service) emit(ctx context.Context, typ, sev, subject string, data any) {
	b, _ := json.Marshal(data)
	if _, err := s.Events.Emit(ctx, events.Event{Type: typ, Severity: sev, Subject: subject, Data: b}, true); err != nil {
		s.Logger.Warn("приложения: событие не записано", "type", typ, "err", err)
	}
}

// View — последнее состояние (обновляется сверкой).
func (s *Service) View() View {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	return s.view
}

func (s *Service) setView(v View) View {
	v.CheckedAt = s.now().UTC()
	s.viewMu.Lock()
	prev := s.view
	s.view = v
	s.viewMu.Unlock()
	return prev
}

// Trigger — внеочередная сверка (изменение пулов, действия администратора).
func (s *Service) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

func (s *Service) status(ctx context.Context) (Status, error) {
	var st Status
	err := s.Helper.Call(ctx, OpStatus, nil, &st)
	return st, err
}

// Refresh — сверка сейчас; возвращает состояние.
func (s *Service) Refresh(ctx context.Context) View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconcile(ctx)
}

// RefreshIfIdle — сверка, если не идёт действие (иначе последнее состояние):
// GET состояния не ждёт минутной задачи.
func (s *Service) RefreshIfIdle(ctx context.Context) View {
	if !s.mu.TryLock() {
		return s.View()
	}
	defer s.mu.Unlock()
	return s.reconcile(ctx)
}

// RetryAfter — повтор неудачного запуска Docker.
const RetryAfter = 5 * time.Minute

// DisabledRecheck — период опроса helper, пока приложения выключены.
const DisabledRecheck = 5 * time.Minute

// reconcile приводит Docker в соответствие с настройкой и пулом (под s.mu).
func (s *Service) reconcile(ctx context.Context) View { return s.reconcileOpt(ctx, false) }

// reconcileOpt — сверка; результат — сохранённое состояние (с checked_at).
func (s *Service) reconcileOpt(ctx context.Context, lazy bool) View {
	s.reconcileStep(ctx, lazy)
	return s.View()
}

func (s *Service) reconcileStep(ctx context.Context, lazy bool) View {
	c, err := s.load(ctx)
	if err != nil {
		s.setView(View{State: StateUnknown, ReasonCode: ReasonConfig, Reason: "настройка не прочитана: " + err.Error()})
		return s.View()
	}
	v := View{Enabled: c.Enabled, Pool: c.Pool}
	if prev := s.View(); lazy && !c.Enabled && prev.State == StateDisabled && s.now().Sub(prev.CheckedAt) < DisabledRecheck {
		return prev // выключено: helper не опрашивается каждые 15 с
	}
	st, err := s.status(ctx)
	if err != nil {
		v.State, v.ReasonCode, v.Reason = StateUnknown, ReasonHelper, "состояние не прочитано: "+err.Error()
		s.setView(v)
		return v
	}
	v.Installed, v.Docker, v.Version, v.Running, v.Total = st.Installed, st.Docker, st.Version, st.Running, st.Total
	if !c.Enabled {
		v.State = StateDisabled
		s.setView(v)
		return v
	}
	if !st.Installed {
		v.State, v.ReasonCode, v.Reason = StateNotInstalled, ReasonNotInstalled, "пакет gamayun-nas-apps не установлен"
		s.transition(ctx, v)
		return v
	}
	if st.Pool == "" {
		// Настройка helper снята (переустановка пакета) — восстановить, если пул доступен.
		var r ConfigureResult
		if err := s.Helper.Call(ctx, OpConfigure, PoolParams{Pool: c.Pool}, &r); err != nil {
			s.Logger.Warn("приложения: настройка Docker на пул не восстановлена", "pool", c.Pool, "err", err)
		}
		if st, err = s.status(ctx); err != nil {
			v.State, v.ReasonCode, v.Reason = StateUnknown, ReasonHelper, "состояние не прочитано: "+err.Error()
			s.setView(v)
			return v
		}
	}
	// Остатки контейнеров при остановленных службах держат пул —
	// останавливать их, как работающий Docker.
	running := st.Docker == "active" || st.Containerd == "active" || st.Docker == "activating" || st.Leftover > 0
	switch {
	case st.Pool != c.Pool:
		v.State, v.ReasonCode = StateUnavailable, ReasonPoolMissing
		v.Reason = fmt.Sprintf("Приложения недоступны: пул %s не подключён", c.Pool)
	case !st.PoolMounted:
		s.retryNow = true // пул вернётся — запуск сразу, прежняя ошибка не держит
		if running {
			s.stopDocker(ctx, "пул не подключён")
		}
		v.State, v.ReasonCode = StateUnavailable, ReasonPoolMissing
		v.Reason = fmt.Sprintf("Приложения недоступны: пул %s не подключён", c.Pool)
	case st.PoolReadOnly:
		if running {
			s.stopDocker(ctx, "пул только для чтения")
		}
		if c.Paused == "" {
			c.Paused = ReasonReadOnly
			if err := s.save(ctx, c); err != nil {
				s.Logger.Warn("приложения: пауза не сохранена", "err", err)
			}
		}
		v.State, v.ReasonCode = StatePaused, ReasonReadOnly
		v.Reason = fmt.Sprintf("Приложения приостановлены: пул %s только для чтения", c.Pool)
	case c.Paused != "":
		if running {
			s.stopDocker(ctx, "пауза")
		}
		v.State, v.ReasonCode = StatePaused, ReasonResumeReady
		v.Reason = fmt.Sprintf("Приложения приостановлены: пул %s снова доступен на запись — возобновите вручную", c.Pool)
	case c.Error != "" && s.now().Sub(c.ErrorAt) < RetryAfter && !s.retryNow:
		v.State, v.ReasonCode, v.Reason = StateError, ReasonStartFailed, c.Error
	case st.Docker == "active" && st.Containerd == "active":
		v.State = StateRunning
	default:
		v.State = StateStarting
		s.setView(v)
		s.retryNow = false
		r, err := s.start(ctx, c.Pool)
		if err != nil {
			c.Error, c.ErrorAt = "Docker не запущен: "+err.Error(), s.now()
			if serr := s.save(ctx, c); serr != nil {
				s.Logger.Warn("приложения: ошибка не сохранена", "err", serr)
			}
			s.emit(ctx, EvStartFailed, events.SevWarning, "pool:"+c.Pool, map[string]any{"pool": c.Pool, "reason": c.Error})
			v.State, v.ReasonCode, v.Reason = StateError, ReasonStartFailed, c.Error
		} else {
			if c.Error != "" {
				c.Error = ""
				s.save(ctx, c)
			}
			s.emit(ctx, EvStarted, events.SevInfo, "pool:"+c.Pool, map[string]any{"pool": c.Pool, "version": r.Version})
			v.State, v.Version = StateRunning, r.Version
		}
	}
	// Docker остановлен, но пул держит посторонний процесс —
	// не молчать «недоступно/приостановлено»: показать, кто держит.
	if s.busy != "" && st.Pool == c.Pool {
		s.checkBusy(ctx, c.Pool)
	}
	if s.busy != "" && (v.State == StateUnavailable || v.State == StatePaused || v.State == StateError) {
		v.State, v.ReasonCode = StateBusy, ReasonPoolBusy
		v.Reason = "Docker остановлен, но " + s.busyReason + " — закройте программу или сеанс, держащий пул, или перезагрузите NAS"
	}
	s.transition(ctx, v)
	return v
}

// transition — событие при входе в «недоступно»/«приостановлено».
func (s *Service) transition(ctx context.Context, v View) {
	prev := s.setView(v)
	if prev.State == v.State && prev.ReasonCode == v.ReasonCode {
		return
	}
	switch {
	case v.State == StateBusy:
		s.emit(ctx, EvPoolBusy, events.SevWarning, "pool:"+v.Pool, map[string]any{"pool": v.Pool, "reason_code": v.ReasonCode, "reason": v.Reason})
	case v.State == StateUnavailable:
		s.emit(ctx, EvUnavailable, events.SevWarning, "pool:"+v.Pool, map[string]any{"pool": v.Pool, "reason_code": v.ReasonCode, "reason": v.Reason})
	case v.State == StatePaused && v.ReasonCode == ReasonReadOnly && prev.State != StatePaused:
		s.emit(ctx, EvPaused, events.SevWarning, "pool:"+v.Pool, map[string]any{"pool": v.Pool, "reason_code": v.ReasonCode, "reason": v.Reason})
	}
}

func (s *Service) start(ctx context.Context, pool string) (StartResult, error) {
	var r StartResult
	err := s.Helper.Call(ctx, OpStart, PoolParams{Pool: pool}, &r)
	return r, err
}

// stopDocker останавливает Docker; die контейнеров в это время — не сбой.
func (s *Service) stopDocker(ctx context.Context, why string) error {
	s.evMu.Lock()
	s.stopFrom, s.stopTo = s.now(), s.now().Add(StopTimeout)
	s.evMu.Unlock()
	var r StopResult
	err := s.Helper.Call(ctx, OpStop, nil, &r)
	s.evMu.Lock()
	s.stopTo = s.now().Add(10 * time.Second)
	s.evMu.Unlock()
	if err != nil {
		if isPoolBusy(err) {
			s.setBusy(busyStop, err)
		}
		s.Logger.Warn("приложения: Docker не остановлен", "why", why, "err", err)
		return err
	}
	if s.busy == busyStop {
		s.busy, s.busyReason = "", ""
	}
	s.Logger.Info("приложения: Docker остановлен", "why", why)
	return nil
}

// Run — сверка по таймеру и подписка на события Docker; до отмены ctx.
func (s *Service) Run(ctx context.Context) {
	iv := s.Interval
	if iv <= 0 {
		iv = 15 * time.Second
	}
	go s.watchEvents(ctx)
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		s.mu.Lock()
		s.reconcileOpt(ctx, true)
		s.reconcileProjects(ctx)
		running := s.View().State == StateRunning
		s.mu.Unlock()
		if running {
			s.checkAccess(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.trigger:
		}
	}
}

// ---------- действия администратора (задачи) ----------

type poolTask struct {
	Pool string `json:"pool,omitempty"`
}

// checkPool — пул существует, смонтирован на запись и не под защитой.
func (s *Service) checkPool(ctx context.Context, pool string) error {
	pv, err := s.Pools.Get(ctx, pool)
	if err != nil {
		return wrapf(ErrNotFound, "пул %s: %v", pool, err)
	}
	if pv.Protection.WriteBlocked {
		return wrapf(ErrConflictSvc, "пул %s под защитой только для чтения (%s)", pool, pv.Protection.Reason)
	}
	if pv.Status == nil {
		return wrapf(ErrUnavailable, "состояние пула %s не прочитано: %s", pool, pv.StatusError)
	}
	if !pv.Status.Mounted {
		return wrapf(ErrConflictSvc, "пул %s не смонтирован", pool)
	}
	return nil
}

// Enable ставит задачу включения приложений на пуле.
func (s *Service) Enable(ctx context.Context, pool string) (tasks.Task, error) {
	if !ValidPool(pool) {
		return tasks.Task{}, wrapf(ErrInvalidSvc, "недопустимое имя пула")
	}
	c, err := s.load(ctx)
	if err != nil {
		return tasks.Task{}, err
	}
	if c.Enabled && c.Pool != pool {
		return tasks.Task{}, wrapf(ErrConflictSvc, "приложения уже включены на пуле %s; перенос на другой пул — отдельная операция (позже)", c.Pool)
	}
	st, err := s.status(ctx)
	if err != nil {
		return tasks.Task{}, wrapf(ErrUnavailable, "%v", err)
	}
	if !st.Installed {
		return tasks.Task{}, wrapf(ErrConflictSvc, "пакет gamayun-nas-apps не установлен")
	}
	if err := s.checkPool(ctx, pool); err != nil {
		return tasks.Task{}, err
	}
	raw, _ := json.Marshal(poolTask{Pool: pool})
	return s.Tasks.Submit(ctx, TaskEnable, raw)
}

// Disable ставит задачу выключения (данные на пуле сохраняются).
func (s *Service) Disable(ctx context.Context) (tasks.Task, error) {
	c, err := s.load(ctx)
	if err != nil {
		return tasks.Task{}, err
	}
	if !c.Enabled {
		return tasks.Task{}, wrapf(ErrConflictSvc, "приложения не включены")
	}
	return s.Tasks.Submit(ctx, TaskDisable, json.RawMessage("{}"))
}

// Resume ставит задачу возобновления после паузы или ошибки запуска.
func (s *Service) Resume(ctx context.Context) (tasks.Task, error) {
	c, err := s.load(ctx)
	if err != nil {
		return tasks.Task{}, err
	}
	if !c.Enabled {
		return tasks.Task{}, wrapf(ErrConflictSvc, "приложения не включены")
	}
	if c.Paused == "" && c.Error == "" {
		return tasks.Task{}, wrapf(ErrConflictSvc, "приложения не приостановлены")
	}
	if err := s.checkPool(ctx, c.Pool); err != nil {
		return tasks.Task{}, err
	}
	return s.Tasks.Submit(ctx, TaskResume, json.RawMessage("{}"))
}

// runEnable: настройка → запуск; сбой — откат в исходное (настройка снята,
// созданный .apps удалён) и ошибка с причиной.
func (s *Service) runEnable(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	var p poolTask
	if err := json.Unmarshal(raw, &p); err != nil || !ValidPool(p.Pool) {
		return nil, fmt.Errorf("параметры задачи: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkPool(ctx, p.Pool); err != nil {
		return nil, err
	}
	progress(10)
	var cr ConfigureResult
	if err := s.Helper.Call(ctx, OpConfigure, PoolParams{Pool: p.Pool}, &cr); err != nil {
		return nil, fmt.Errorf("настройка Docker на пул %s: %w", p.Pool, err)
	}
	progress(40)
	sr, err := s.start(ctx, p.Pool)
	if err != nil {
		var ur UnconfigureResult
		if uerr := s.Helper.Call(context.WithoutCancel(ctx), OpUnconfigure, UnconfigureParams{PurgeCreated: true}, &ur); uerr != nil {
			return nil, fmt.Errorf("Docker не запущен: %v; откат не выполнен: %v", err, uerr)
		}
		return nil, fmt.Errorf("Docker не запущен: %v; настройка снята, система в исходном состоянии", err)
	}
	progress(90)
	if err := s.save(ctx, Config{Enabled: true, Pool: p.Pool}); err != nil {
		return nil, err
	}
	s.emit(ctx, EvEnabled, events.SevInfo, "pool:"+p.Pool, map[string]any{"pool": p.Pool, "version": sr.Version})
	v := s.reconcile(ctx)
	return v, nil
}

func (s *Service) runDisable(ctx context.Context, _ json.RawMessage, progress func(int)) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.stopDocker(ctx, "выключение"); err != nil {
		return nil, fmt.Errorf("Docker не остановлен: %w", err)
	}
	progress(50)
	var ur UnconfigureResult
	if err := s.Helper.Call(ctx, OpUnconfigure, UnconfigureParams{}, &ur); err != nil {
		return nil, fmt.Errorf("настройка Docker не снята: %w", err)
	}
	if err := s.save(ctx, Config{}); err != nil {
		return nil, err
	}
	s.emit(ctx, EvDisabled, events.SevInfo, "pool:"+c.Pool, map[string]any{"pool": c.Pool})
	return s.reconcile(ctx), nil
}

// runResume: принудительный RO снимается только umount + mount, затем запуск.
func (s *Service) runResume(ctx context.Context, _ json.RawMessage, progress func(int)) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	if !c.Enabled {
		return nil, wrapf(ErrConflictSvc, "приложения не включены")
	}
	if err := s.checkPool(ctx, c.Pool); err != nil {
		return nil, err
	}
	if err := s.stopDocker(ctx, "возобновление"); err != nil {
		s.reconcile(ctx)
		return nil, fmt.Errorf("Docker не остановлен: %w", err)
	}
	st, err := s.status(ctx)
	if err != nil {
		return nil, err
	}
	progress(20)
	remounted := false
	if st.PoolReadOnly {
		var rr RemountResult
		if err := s.Helper.Call(ctx, OpRemount, PoolParams{Pool: c.Pool}, &rr); err != nil {
			if isPoolBusy(err) {
				s.setBusy(busyRemount, err)
				s.reconcile(ctx)
			}
			return nil, fmt.Errorf("пул %s не перемонтирован: %w", c.Pool, err)
		}
		remounted = true
		s.busy, s.busyReason = "", ""
	}
	progress(60)
	c.Paused, c.Error = "", ""
	if err := s.save(ctx, c); err != nil {
		return nil, err
	}
	sr, err := s.start(ctx, c.Pool)
	if err != nil {
		c.Error = "Docker не запущен: " + err.Error()
		s.save(ctx, c)
		s.emit(ctx, EvStartFailed, events.SevWarning, "pool:"+c.Pool, map[string]any{"pool": c.Pool, "reason": c.Error})
		s.reconcile(ctx)
		return nil, errors.New(c.Error)
	}
	s.emit(ctx, EvResumed, events.SevInfo, "pool:"+c.Pool, map[string]any{"pool": c.Pool, "remounted": remounted, "version": sr.Version})
	return s.reconcile(ctx), nil
}

// ---------- хуки пулов ----------

// BeforeReadOnly — защита переводит пул в RO: `remount,ro` при
// работающих контейнерах невозможен («занято») — сначала остановить Docker.
func (s *Service) BeforeReadOnly(ctx context.Context, pool string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load(ctx)
	if err != nil {
		return err
	}
	if !c.Enabled || c.Pool != pool {
		return nil
	}
	if err := s.stopDocker(ctx, "защита пула: только чтение"); err != nil {
		return fmt.Errorf("приложения: Docker не остановлен перед переводом пула в read-only: %w", err)
	}
	if c.Paused == "" {
		c.Paused = ReasonProtection
		if err := s.save(ctx, c); err != nil {
			return err
		}
		msg := fmt.Sprintf("Приложения приостановлены: защита перевела пул %s в режим только для чтения", pool)
		s.emit(ctx, EvPaused, events.SevWarning, "pool:"+pool, map[string]any{"pool": pool, "reason_code": ReasonProtection, "reason": msg})
	}
	return nil
}

// PoolInUse — пул занят приложениями (запрет удаления пула).
func (s *Service) PoolInUse(ctx context.Context, pool string) string {
	c, err := s.load(ctx)
	if err != nil || !c.Enabled || c.Pool != pool {
		return ""
	}
	return "приложения (раздел «Приложения»)"
}

// ---------- события Docker ----------

func (s *Service) watchEvents(ctx context.Context) {
	h := s.EventsHelper
	if h == nil {
		h = s.Helper
	}
	wait := s.EventsWait
	if wait <= 0 {
		wait = 50
	}
	cursor := s.now().UnixNano()
	failures := 0
	for ctx.Err() == nil {
		var r EventsResult
		err := h.Call(ctx, OpEvents, EventsParams{SinceNano: cursor, WaitS: wait}, &r)
		if err != nil {
			if failures%20 == 0 && !errors.Is(err, context.Canceled) {
				s.Logger.Warn("приложения: события Docker не прочитаны, повтор", "err", err, "failures", failures+1)
			}
			failures++
			sleepCtx(ctx, 10*time.Second)
			continue
		}
		failures = 0
		for _, ev := range r.Events {
			s.dockerEvent(ctx, ev)
		}
		now := r.UntilNano
		if now == 0 {
			now = s.now().UnixNano()
		}
		s.flushNoise(ctx, now)
		if r.UntilNano > cursor {
			cursor = r.UntilNano
		}
		if !r.Running {
			sleepCtx(ctx, 15*time.Second)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// SuppressWindow — одно событие одного вида на контейнер за окно; остальные
// считаются и попадают в следующее событие (защита от цикла падений).
const SuppressWindow = 5 * time.Minute

// dockerEvent — событие Docker → журнал gamayund. die 137 вне действий gamayund
// откладывается до restart (шум docker restart).
func (s *Service) dockerEvent(ctx context.Context, ev DockerEvent) {
	at := time.Unix(0, ev.TimeNano)
	s.evMu.Lock()
	inStop := !at.Before(s.stopFrom) && !at.After(s.stopTo)
	s.evMu.Unlock()
	if !inStop && s.holdNoise(ev) {
		return
	}
	s.dropNoise(ev)
	s.publishDocker(ctx, ev)
}

// publishDocker — запись события контейнера и детектор «падает».
func (s *Service) publishDocker(ctx context.Context, ev DockerEvent) {
	var typ, sev string
	at := time.Unix(0, ev.TimeNano)
	s.evMu.Lock()
	inStop := !at.Before(s.stopFrom) && !at.After(s.stopTo)
	s.evMu.Unlock()
	switch ev.Action {
	case "die":
		if ev.ExitCode == "0" || inStop && (ev.ExitCode == "137" || ev.ExitCode == "143") {
			return // штатная остановка
		}
		typ, sev = EvContainerDied, events.SevWarning
		if ev.OOM {
			typ = EvContainerOOM
		}
		if id := appOf(ev.Project); id != "" && s.proj != nil && s.countDie(id, at) {
			go s.markCrashing(context.WithoutCancel(ctx), id)
		}
	case "oom":
		typ, sev = EvContainerOOM, events.SevWarning
	case "restart":
		typ, sev = EvContainerRestart, events.SevInfo
	case "health_status":
		s.evMu.Lock()
		was := s.unhealthy[ev.Container]
		switch ev.Health {
		case "unhealthy":
			s.unhealthy[ev.Container] = true
		case "healthy":
			delete(s.unhealthy, ev.Container)
		}
		s.evMu.Unlock()
		switch {
		case ev.Health == "unhealthy" && !was:
			typ, sev = EvContainerUnhealthy, events.SevWarning
		case ev.Health == "healthy" && was:
			typ, sev = EvContainerHealthy, events.SevInfo
		default:
			return
		}
	default:
		return
	}
	key := typ + "\x00" + ev.Container
	s.evMu.Lock()
	if last, ok := s.lastEv[key]; ok && at.Sub(last) < SuppressWindow && at.Sub(last) >= 0 {
		s.suppr[key]++
		s.evMu.Unlock()
		return
	}
	n := s.suppr[key]
	delete(s.suppr, key)
	s.lastEv[key] = at
	s.evMu.Unlock()
	data := map[string]any{"container": ev.Container, "id": ev.ID, "image": ev.Image}
	if ev.Project != "" {
		data["project"] = ev.Project
	}
	if ev.ExitCode != "" {
		data["exit_code"] = ev.ExitCode
	}
	if n > 0 {
		data["suppressed"] = n
	}
	s.Events.Emit(ctx, events.Event{Type: typ, Severity: sev, Subject: "container:" + ev.Container, Time: at.UTC(), Data: mustJSON(data)}, true)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
