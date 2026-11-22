package home

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gamayun-nas/internal/disks"
	"gamayun-nas/internal/storage/planner"
	"gamayun-nas/internal/tasks"
)

// HelperClient — вызов операции gamayun-helper (helper.Client).
type HelperClient interface {
	Call(ctx context.Context, op string, params, result any) error
}

// Операции helper.
const (
	OpCreate  = "home.pool.create"
	OpAddDisk = "home.pool.add_disk"
	OpReplace = "home.pool.replace"
	// OpReplaceFinish — вывод и очистка старого диска после горячей замены.
	OpReplaceFinish = "home.pool.replace_finish"
	OpGrow          = "home.pool.grow"
	OpStatus        = "home.pool.status"
)

// Виды задач.
const (
	TaskCreate  = "storage.pool.create"
	TaskAddDisk = "storage.pool.add_disk"
	TaskReplace = "storage.pool.replace"
	// TaskGrow — дорастить LVM/Btrfs после reshape, прерванного
	// перезагрузкой или перезапуском gamayund.
	TaskGrow = "storage.pool.grow"
	// TaskResume — «продолжить замену»: подхватить прерванную операцию с
	// диском (замена, добавление, рост) без перезапуска gamayund.
	TaskResume = "storage.pool.resume"
)

// Операции плана.
const (
	PlanCreate  = "create"
	PlanAddDisk = "add_disk"
	// PlanExpand — рост пула за счёт свободного места диска пула после
	// замены на больший.
	PlanExpand  = "expand"
	PlanReplace = "replace"
)

// Ошибки сервиса (API сопоставляет их с кодами HTTP).
var (
	ErrInvalid      = errors.New("неверный запрос")
	ErrConflict     = errors.New("конфликт состояния")
	ErrUnsupported  = errors.New("не поддерживается")
	ErrNotFound     = errors.New("не найдено")
	ErrPlanNotFound = errors.New("план не найден или уже использован")
	ErrPlanExpired  = errors.New("срок подтверждения плана истёк")
	ErrBadToken     = errors.New("неверный токен подтверждения; план аннулирован")
	ErrPlanStale    = errors.New("состояние дисков или пула изменилось после расчёта плана; рассчитайте план заново")
	ErrTooMany      = errors.New("слишком много неподтверждённых планов")
)

func wrap(base error, format string, a ...any) error {
	return fmt.Errorf("%w: %s", base, fmt.Sprintf(format, a...))
}

// PlanRequest — запрос плана.
type PlanRequest struct {
	Op      string   `json:"op"`
	Pool    string   `json:"pool"`
	Type    string   `json:"type,omitempty"`     // create — тип; add_disk — целевой (пусто — текущий)
	DiskIDs []string `json:"disk_ids,omitempty"` // create
	DiskID  string   `json:"disk_id,omitempty"`  // add_disk, replace, expand
	// OldDiskID — replace: исправный диск пула, который заменяется на
	// DiskID (горячая замена); пусто — замена отказавшего.
	OldDiskID string `json:"old_disk_id,omitempty"`
	Integrity bool   `json:"integrity,omitempty"`
}

// PlanDisk — диск плана.
type PlanDisk struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	KernelName string `json:"kernel_name"`
	SizeBytes  int64  `json:"size_bytes"`
	Erase      bool   `json:"erase"` // данные на диске будут уничтожены
}

// Plan — рассчитанный план с токеном подтверждения.
type Plan struct {
	PlanID       string    `json:"plan_id"`
	ConfirmToken string    `json:"confirm_token,omitempty"` // только в ответе на расчёт
	ExpiresAt    time.Time `json:"expires_at"`
	Op           string    `json:"op"`
	Pool         string    `json:"pool"`
	Type         string    `json:"type"`
	FromType     string    `json:"from_type,omitempty"`
	Integrity    bool      `json:"integrity"`
	// IntegrityMode — режим dm-integrity (journal/bitmap), выбирается
	// сервером: DefaultIntegrityMode для новых пулов, режим пула — для
	// операций над ним.
	IntegrityMode string      `json:"integrity_mode,omitempty"`
	Disks         []PlanDisk  `json:"disks"`
	Arrays        []ArraySpec `json:"arrays"`
	// UsableBytes — ёмкость по planner (до накладных расходов md, dm-integrity,
	// LVM и Btrfs; см. фактические значения в состоянии пула).
	UsableBytes       int64    `json:"usable_bytes"`
	PrevUsableBytes   int64    `json:"prev_usable_bytes,omitempty"`
	Tolerance         int      `json:"tolerance"`
	TransitionChecked bool     `json:"transition_verified"`
	Warnings          []string `json:"warnings"`
}

// PoolRecord — пул в базе gamayund.
type PoolRecord struct {
	Name          string      `json:"name"`
	Type          string      `json:"type"`
	Integrity     bool        `json:"integrity"`
	IntegrityMode string      `json:"integrity_mode,omitempty"` // хранится в layout (без миграции)
	Disks         []DiskRef   `json:"disks"`
	Arrays        []ArraySpec `json:"arrays"`
	// InitialSync — пул создан, начальная синхронизация md ещё не
	// завершалась: это не отказ, read-only не включается.
	InitialSync bool `json:"initial_sync,omitempty"`
	// Pending — добавление диска, начатое и не подтверждённое:
	// пишется до вызова helper add_disk, при старте сверяется с суперблоками md.
	Pending *PoolPending `json:"pending,omitempty"`
	// GrowPending — состав массивов изменён добавлением диска, рост
	// LVM/Btrfs ещё не выполнен: при старте gamayund ставится задача роста
	// (сброс во время resync/reshape нового состава).
	GrowPending bool `json:"grow_pending,omitempty"`
	// Creating — пул создаётся: запись пишется до вызова
	// helper create, снимается после его успеха. При старте gamayund: полный
	// стек — флаг снимается; без тома данных — откат create_rollback.
	Creating  bool      `json:"creating,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type pending struct {
	plan        Plan
	tokenHash   [32]byte
	fingerprint string
	task        taskParams
}

// taskParams — параметры задачи (хранятся в tasks.params).
type taskParams struct {
	PlanID  string         `json:"plan_id"`
	Op      string         `json:"op"`
	Create  *CreateParams  `json:"create,omitempty"`
	AddDisk *AddDiskParams `json:"add_disk,omitempty"`
	Replace *ReplaceParams `json:"replace,omitempty"`
	Record  PoolRecord     `json:"record"` // запись пула после успешной операции
	Usable  int64          `json:"planned_usable_bytes"`
}

// PoolPending — ожидаемый состав после add_disk.
type PoolPending struct {
	Disk   DiskRef       `json:"disk"`
	Type   string        `json:"type"`
	Arrays []ArraySpec   `json:"arrays"` // состав всех массивов после добавления
	Expand []ExpandArray `json:"expand"` // целевые уровень/число участников
	// NewArrays — новые верхние слои.
	NewArrays []NewArray `json:"new_arrays,omitempty"`
	// Grow — рост после замены (AddDiskParams.Grow): Disk уже в пуле.
	Grow bool `json:"grow,omitempty"`
	// Replace — ожидание замены: Disk ставится в массивы
	// Expand (число участников прежнее) вместо Old (горячая замена) или
	// отказавшего; Disks — диски пула после замены. Принят хоть в один
	// массив — дозавершение задачей replace (Resume); ни в один — откат.
	Replace bool      `json:"replace,omitempty"`
	Old     *DiskRef  `json:"old,omitempty"`
	Disks   []DiskRef `json:"disks,omitempty"`
}

func (r PoolRecord) assembleParams() AssembleParams {
	p := AssembleParams{Pool: r.Name, Integrity: r.Integrity, IntegrityMode: r.IntegrityMode, Arrays: r.Arrays, Type: r.Type, Disks: r.Disks}
	if r.Pending != nil {
		p.Pending = &PendingAdd{DiskID: r.Pending.Disk.ID, Arrays: r.Pending.Expand, New: r.Pending.NewArrays}
	}
	return p
}

// resolve — запись после сверки ожидания с суперблоками md: в принятых
// массивах — состав из ожидания, в остальных — прежний; ожидание снято.
//
// Если диск принят не во все ожидаемые массивы (SHR из нескольких
// массивов, прерванный между ними), ожидание остаётся только для
// непринятых массивов — их дозавершает задача add_disk с Resume
// (resumeTask). Диск в записи один раз.
//
// Новые слои принимаются по порядку снизу вверх: слой
// добавляется в запись, только если приняты все новые слои под ним.
func (r PoolRecord) resolve(accepted []int) PoolRecord {
	pd := r.Pending
	r.Pending = nil
	if pd == nil || len(accepted) == 0 {
		if pd != nil && r.hasDisk(pd.Disk.ID) {
			// Диск уже в пуле (частичное принятие раньше): ожидание
			// остаётся до дозавершения.
			r.Pending = pd
		}
		return r
	}
	acc := map[int]bool{}
	for _, i := range accepted {
		acc[i] = true
	}
	r.Arrays = append([]ArraySpec{}, r.Arrays...)
	for i := range r.Arrays {
		if acc[i] && i < len(pd.Arrays) {
			r.Arrays[i] = pd.Arrays[i]
		}
	}
	var restNew []NewArray
	for _, na := range pd.NewArrays {
		if acc[na.Index] && na.Index == len(r.Arrays) && na.Index < len(pd.Arrays) && len(restNew) == 0 {
			r.Arrays = append(r.Arrays, pd.Arrays[na.Index])
			continue
		}
		acc[na.Index] = false
		restNew = append(restNew, na)
	}
	var rest []ExpandArray
	for _, x := range pd.Expand {
		if !acc[x.Index] {
			rest = append(rest, x)
		}
	}
	if len(rest) == 0 && len(restNew) == 0 {
		r.Type = pd.Type
	} else {
		left := *pd
		left.Expand = rest
		left.NewArrays = restNew
		r.Pending = &left
	}
	r.GrowPending = true
	if !r.hasDisk(pd.Disk.ID) {
		r.Disks = append(append([]DiskRef{}, r.Disks...), pd.Disk)
	}
	return r
}

func (r PoolRecord) hasDisk(id string) bool {
	for _, d := range r.Disks {
		if d.ID == id {
			return true
		}
	}
	return false
}

// resumeTask — параметры задачи дозавершения добавления для
// записи с частично принятым диском; ok=false — дозавершать нечего.
func (r PoolRecord) resumeTask() (taskParams, bool) {
	pd := r.Pending
	if pd == nil || len(pd.Expand)+len(pd.NewArrays) == 0 || !r.hasDisk(pd.Disk.ID) {
		return taskParams{}, false
	}
	final := r
	final.Pending = nil
	final.Type = pd.Type
	final.Arrays = pd.Arrays
	final.GrowPending = true
	ap := AddDiskParams{Pool: r.Name, Integrity: r.Integrity, IntegrityMode: r.IntegrityMode, Disk: pd.Disk, Arrays: pd.Expand,
		NewArrays: pd.NewArrays, Resume: true, Grow: pd.Grow}
	return taskParams{Op: PlanAddDisk, AddDisk: &ap, Record: final}, true
}

// Service — пулы Home на стороне gamayund.
type Service struct {
	helper HelperClient
	db     *sql.DB
	tasks  *tasks.Manager
	now    func() time.Time
	meter  syncMeter // скорость пересчёта для прогноза
	// PlanTTL — срок действия токена подтверждения.
	PlanTTL time.Duration
	// OnReassemble — журнал повторной сборки деградированного пула
	// (возврат диска); nil — не журналировать.
	OnReassemble func(pool string, r AssembleResult, err error)
	// BootFile — файл состава пулов для раннего открытия dm-integrity
	// (gamayun-helper -open-integrity); пусто — не писать.
	BootFile string
	// Poll — интервал опроса состояния во время reshape/recovery.
	Poll time.Duration
	// DegradedWait — окно без продвижения при ожидании перестройки
	// (выпавший участник, пересчёт на пропавшем диске,
	// зависший процесс); после него — попытка сборки и второе окно; 0 — 60 с.
	DegradedWait time.Duration
	// OnWatch — журнал смены наблюдений при ожидании процесса;
	// nil — не журналировать.
	OnWatch func(name, observation string)
	// OnMdstatChange — состав/состояние массивов в /proc/mdstat изменились
	// (опрос RunProtection): запуск сверки коллектора. nil — нет.
	OnMdstatChange func()
	// MdstatPath, MdstatPoll — источник и период быстрой реакции защиты
	// на изменения массивов (RunProtection); пусто/0 — /proc/mdstat, 2 с.
	MdstatPath string
	MdstatPoll time.Duration

	// OnScrub — итог проверки пула: событие и уведомление; err —
	// проверка не завершена. nil — не сообщать.
	OnScrub func(r ScrubResult, err error)

	// Shares — применение read-only к общим папкам; nil — только Btrfs.
	Shares ShareGuard
	// BeforeReadOnly — до перевода пула защитой в read-only
	// (остановить Docker, иначе remount,ro — «занято»); ошибка — RO не
	// применяется в этом проходе. nil — нет.
	BeforeReadOnly func(ctx context.Context, pool string) error
	// InUse — чем ещё занят пул; непусто — удаление
	// пула запрещено. nil — нет.
	InUse func(ctx context.Context, pool string) string
	// Unavailable — ошибка «helper недоступен» (повтор сборки при старте,
	// nil — без повторов.
	Unavailable func(error) bool
	// StatusTimeout — срок опроса состояния; 0 — DefaultStatusTimeout.
	StatusTimeout time.Duration
	// HelperTimeout — срок изменяющего вызова helper для сторожа задач;
	// 0 — DefaultHelperTimeout.
	HelperTimeout time.Duration
	// ProtectionStaleAfter — просрочка прохода защиты; 0 — три
	// интервала, не меньше 2 мин.
	ProtectionStaleAfter time.Duration

	stopping atomic.Bool
	protW    protWatch

	mu    sync.Mutex
	plans map[string]*pending
	busy  map[string]string // пул → вид задачи (поставлена или выполняется)
	// handed — задача пула поставила дозавершение того же пула (handoff):
	// освобождение по её окончании пропускается один раз.
	handed map[string]bool
	// scrubStop — пользователь остановил проверку пула.
	scrubStop map[string]bool
	protMu    sync.Mutex // проход защиты и удаление пула
}

// NewService создаёт сервис и регистрирует виды задач.
func NewService(h HelperClient, db *sql.DB, m *tasks.Manager) (*Service, error) {
	s := &Service{db: db, tasks: m, now: time.Now, PlanTTL: 10 * time.Minute,
		Poll: 2 * time.Second, plans: map[string]*pending{}}
	s.helper = guardedHelper{h: h, s: s}
	for kind, fn := range map[string]tasks.RunFunc{TaskCreate: s.runCreate, TaskAddDisk: s.runAddDisk, TaskReplace: s.runReplace, TaskDestroy: s.runDestroy, TaskGrow: s.runGrow, TaskResume: s.runResume, TaskScrub: s.runScrub, TaskImport: s.runImport} {
		if err := m.Register(kind, s.releasing(fn)); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// SetClock — подмена времени в тестах.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

const maxPending = 64

// Plan рассчитывает операцию и выдаёт одноразовый токен подтверждения.
func (s *Service) Plan(ctx context.Context, req PlanRequest) (Plan, error) {
	if !ValidPoolName(req.Pool) {
		return Plan{}, wrap(ErrInvalid, "имя пула: латиница в нижнем регистре и цифры, 1–16 символов, первая — буква")
	}
	var p *pending
	var err error
	switch req.Op {
	case PlanCreate:
		p, err = s.planCreate(ctx, req)
	case PlanAddDisk:
		p, err = s.planAddDisk(ctx, req)
	case PlanReplace:
		p, err = s.planReplace(ctx, req)
	case PlanExpand:
		p, err = s.planExpand(ctx, req)
	default:
		return Plan{}, wrap(ErrInvalid, "op: create, add_disk, replace или expand")
	}
	if err != nil {
		return Plan{}, err
	}
	id, err := randHex(16)
	if err != nil {
		return Plan{}, err
	}
	token, err := randHex(32)
	if err != nil {
		return Plan{}, err
	}
	p.plan.PlanID = id
	p.plan.ExpiresAt = s.now().Add(s.PlanTTL).UTC()
	p.tokenHash = sha256.Sum256([]byte(token))
	p.task.PlanID = id
	if p.plan.Warnings == nil {
		p.plan.Warnings = []string{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, v := range s.plans {
		if !now.Before(v.plan.ExpiresAt) {
			delete(s.plans, k)
		}
	}
	if len(s.plans) >= maxPending {
		return Plan{}, ErrTooMany
	}
	s.plans[id] = p
	out := p.plan
	out.ConfirmToken = token
	return out, nil
}

// Confirm проверяет токен (одноразовый, с истечением), сверяет состояние с
// моментом расчёта и ставит задачу.
func (s *Service) Confirm(ctx context.Context, planID, token string) (tasks.Task, error) {
	s.mu.Lock()
	p, ok := s.plans[planID]
	delete(s.plans, planID) // одноразовый: любая попытка расходует план
	s.mu.Unlock()
	if !ok {
		return tasks.Task{}, ErrPlanNotFound
	}
	if !s.now().Before(p.plan.ExpiresAt) {
		return tasks.Task{}, ErrPlanExpired
	}
	h := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(h[:], p.tokenHash[:]) != 1 {
		return tasks.Task{}, ErrBadToken
	}
	fp, err := s.fingerprint(ctx, p.plan.Op, p.plan.Pool, planDiskIDs(p.plan))
	if err != nil {
		return tasks.Task{}, err
	}
	if fp != p.fingerprint {
		return tasks.Task{}, ErrPlanStale
	}
	raw, err := json.Marshal(p.task)
	if err != nil {
		return tasks.Task{}, err
	}
	kind := map[string]string{PlanCreate: TaskCreate, PlanAddDisk: TaskAddDisk, PlanReplace: TaskReplace, PlanExpand: TaskAddDisk}[p.plan.Op]
	return s.submit(ctx, p.plan.Pool, kind, raw)
}

// Названия задач для сообщения «пул занят».
// BusyDestroying — PoolView.Busy во время удаления пула (сборщик событий
// не считает разборку массива отказом).
const BusyDestroying = "удаление"

var taskTitle = map[string]string{TaskCreate: "создание", TaskAddDisk: "добавление диска", TaskReplace: "замена диска",
	TaskDestroy: BusyDestroying, TaskGrow: "рост после перестройки", TaskResume: "продолжение замены", TaskScrub: "проверка данных", TaskImport: "подхват пула"}

// submit ставит задачу над пулом: над одним пулом — одна задача за раз
// Пул занят от постановки до конца выполнения
// (releasing); после перезапуска gamayund прерванные задачи пул не держат.
func (s *Service) submit(ctx context.Context, pool, kind string, raw json.RawMessage) (tasks.Task, error) {
	s.mu.Lock()
	if s.busy == nil {
		s.busy = map[string]string{}
	}
	// Постановка из задачи того же пула (дозавершение после сбоя): пул
	// переходит к новой задаче без освобождения, окончание текущей его не
	// освобождает — иначе дозавершение шло бы без блокировки.
	handoff := ctx.Value(taskPoolKey{}) == pool
	if k, ok := s.busy[pool]; ok && !handoff {
		s.mu.Unlock()
		return tasks.Task{}, wrap(ErrConflict, "пул %s занят: выполняется %s — дождитесь окончания", pool, taskTitle[k])
	}
	// Защита не отвечает или gamayund останавливается — операции
	// над пулами не начинаются.
	if s.Stopping() {
		s.mu.Unlock()
		return tasks.Task{}, wrap(ErrConflict, "%v", ErrStopping)
	}
	if why := s.ProtectionStale(); why != "" && !handoff {
		s.mu.Unlock()
		return tasks.Task{}, wrap(ErrConflict, "операции над пулами недоступны: %s", why)
	}
	s.busy[pool] = kind
	if handoff {
		if s.handed == nil {
			s.handed = map[string]bool{}
		}
		s.handed[pool] = true
	}
	s.mu.Unlock()
	t, err := s.tasks.Submit(ctx, kind, raw)
	if err != nil {
		s.mu.Lock()
		if handoff {
			delete(s.handed, pool) // пул освободит окончание текущей задачи
		} else {
			delete(s.busy, pool)
		}
		s.mu.Unlock()
	}
	return t, err
}

// taskPoolKey — ключ контекста: пул выполняющейся задачи (releasing).
type taskPoolKey struct{}

func (s *Service) release(pool string) {
	s.mu.Lock()
	delete(s.busy, pool)
	s.mu.Unlock()
}

// finish — освобождение пула по окончании задачи: если задача передала
// пул дозавершению (handoff), пул остаётся занятым им.
func (s *Service) finish(pool string) {
	s.mu.Lock()
	if s.handed[pool] {
		delete(s.handed, pool)
	} else {
		delete(s.busy, pool)
	}
	s.mu.Unlock()
}

// Busy — какая задача занимает пул (пусто — свободен).
func (s *Service) Busy(pool string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return taskTitle[s.busy[pool]]
}

// releasing освобождает пул по окончании задачи (успех, ошибка, отмена).
func (s *Service) releasing(fn tasks.RunFunc) tasks.RunFunc {
	return func(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
		pool := taskPool(raw)
		defer s.finish(pool)
		return fn(context.WithValue(ctx, taskPoolKey{}, pool), raw, progress)
	}
}

// taskPool — имя пула из параметров любой задачи хранилища.
func taskPool(raw json.RawMessage) string {
	var v struct {
		Pool    string                 `json:"pool"`
		Create  *struct{ Pool string } `json:"create"`
		AddDisk *struct{ Pool string } `json:"add_disk"`
		Replace *struct{ Pool string } `json:"replace"`
		Record  struct{ Name string }  `json:"record"`
	}
	json.Unmarshal(raw, &v)
	switch {
	case v.Pool != "":
		return v.Pool
	case v.Create != nil:
		return v.Create.Pool
	case v.AddDisk != nil:
		return v.AddDisk.Pool
	case v.Replace != nil:
		return v.Replace.Pool
	}
	return v.Record.Name
}

func planDiskIDs(p Plan) []string {
	var ids []string
	for _, d := range p.Disks {
		ids = append(ids, d.ID)
	}
	return ids
}

// ---------- наблюдение ----------

func (s *Service) observe(ctx context.Context) (disks.Snapshot, error) {
	var snap disks.Snapshot
	if err := s.helper.Call(ctx, "disks.observe", nil, &snap); err != nil {
		return snap, err
	}
	return snap, nil
}

// diskOwner — пул из базы, в записи которого есть диск id (в т. ч.
// «создаётся»: такой диск может быть ещё без разметки).
func (s *Service) diskOwner(ctx context.Context, id string) (string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM pools ORDER BY name")
	if err != nil {
		return "", err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return "", err
		}
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		rec, err := s.getRecord(ctx, n)
		if err != nil {
			return "", err
		}
		if rec.hasDisk(id) {
			return n, nil
		}
	}
	return "", nil
}

// freeDiskOf — freeDisk и диск не в записи другого пула.
func (s *Service) freeDiskOf(ctx context.Context, snap disks.Snapshot, id string) (disks.Disk, error) {
	if n, err := s.diskOwner(ctx, id); err != nil {
		return disks.Disk{}, err
	} else if n != "" {
		return disks.Disk{}, wrap(ErrConflict, "диск %s входит в запись пула %s", id, n)
	}
	if n := s.foundPoolOf(ctx, id); n != "" {
		return disks.Disk{}, wrap(ErrConflict, "на диске %s найден пул GamayunNas %s — подключите его (Хранилище → Найденные пулы) или освободите диск", id, n)
	}
	return freeDisk(snap, id)
}

// freeDisk — диск по ID из снимка; должен быть unknown в полном снимке.
func freeDisk(snap disks.Snapshot, id string) (disks.Disk, error) {
	if !snap.Complete {
		return disks.Disk{}, wrap(ErrConflict, "снимок дисков неполный: занятость дисков не определена")
	}
	var found []disks.Disk
	for _, d := range snap.Disks {
		if d.ID == id {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		return disks.Disk{}, wrap(ErrInvalid, "диск %s найден %d раз", id, len(found))
	}
	d := found[0]
	if d.SystemRole != disks.SummaryUnknown {
		return disks.Disk{}, wrap(ErrConflict, "диск %s (%s) занят: %s", id, d.Name, d.SystemRole)
	}
	if d.SizeBytes == nil {
		return disks.Disk{}, wrap(ErrConflict, "размер диска %s не определён", id)
	}
	return d, nil
}

func (s *Service) status(ctx context.Context, pool string) (PoolStatus, error) {
	var st PoolStatus
	err := s.helper.Call(ctx, OpStatus, PoolParams{Pool: pool}, &st)
	if err == nil {
		if rec, rerr := s.getRecord(ctx, pool); rerr == nil {
			checkExpectedArrays(&st, rec)
			if rec.Creating && s.Busy(pool) == "" {
				st.Issues = append(st.Issues, "создание пула прервано — удалите пул")
				if st.Health != HealthUnknown { // неизвестное не становится «известно плохим»
					st.Health = HealthFailed
				}
			}
			if rec.InitialSync {
				markInitialSync(&st)
			}
			s.initialSyncProgress(&st)
		}
	}
	return st, err
}

// checkExpectedArrays: массив md из записи, которого нет в системе, —
// пул не собран полностью, health не может быть ok (несобранный
// raid1 показывался ok, пока VG была видна).
func checkExpectedArrays(st *PoolStatus, rec PoolRecord) {
	if st.Health == HealthMissing {
		return
	}
	have := map[int]bool{}
	for _, a := range st.Arrays {
		have[a.Index] = true
	}
	for i, a := range rec.Arrays {
		if a.Level != LevelSingle && !have[i] {
			if st.Health == HealthUnknown {
				// Массивы не прочитаны — «не собран» не доказано.
				continue
			}
			st.Issues = append(st.Issues, fmt.Sprintf("массив %d не собран", i))
			st.Health = HealthFailed
		}
	}
}

// endGrowPending снимает признак незавершённого роста после home.pool.grow.
func (s *Service) endGrowPending(ctx context.Context, pool string) error {
	rec, err := s.getRecord(ctx, pool)
	if err != nil || !rec.GrowPending {
		return err
	}
	rec.GrowPending = false
	return s.saveRecord(ctx, rec, false)
}

// endInitialSync снимает признак начальной синхронизации.
func (s *Service) endInitialSync(ctx context.Context, pool string) error {
	rec, err := s.getRecord(ctx, pool)
	if err != nil || !rec.InitialSync {
		return err
	}
	// Идёт add_disk/рост: пока helper собирает новый raid5/6-слой,
	// пул ещё ok, но признак нужен сразу после возврата helper — иначе
	// новый слой в recovery считается деградацией и включается read-only.
	if rec.Pending != nil || rec.GrowPending {
		return nil
	}
	rec.InitialSync = false
	return s.saveRecord(ctx, rec, false)
}

// fingerprint — снимок того, от чего зависит план: диски плана (ID,
// размер, роль) и, для операций над пулом, состояние его массивов.
func (s *Service) fingerprint(ctx context.Context, op, pool string, ids []string) (string, error) {
	snap, err := s.observe(ctx)
	if err != nil {
		return "", err
	}
	type dfp struct {
		ID   string
		Size int64
		Role string
	}
	var fp struct {
		Complete bool
		Disks    []dfp
		Arrays   []string
		PoolIDs  []string
		Health   string
	}
	fp.Complete = snap.Complete
	sorted := append([]string{}, ids...)
	sort.Strings(sorted)
	for _, id := range sorted {
		x := dfp{ID: id, Role: "absent"}
		for _, d := range snap.Disks {
			if d.ID == id {
				x.Role = d.SystemRole + ":" + strings.Join(d.SystemRoles, ",")
				if d.SizeBytes != nil {
					x.Size = *d.SizeBytes
				}
			}
		}
		fp.Disks = append(fp.Disks, x)
	}
	if op != PlanCreate {
		st, err := s.status(ctx, pool)
		if err != nil {
			return "", err
		}
		fp.Health = st.Health
		fp.PoolIDs = st.DiskIDs
		for _, a := range st.Arrays {
			fp.Arrays = append(fp.Arrays, fmt.Sprintf("%d:%s:%d:%d:%s", a.Index, a.Level, a.RaidDevices, a.Missing, a.Action))
		}
	}
	b, _ := json.Marshal(fp)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// ---------- расчёт ----------

func planDisk(d disks.Disk, erase bool) PlanDisk {
	return PlanDisk{ID: d.ID, Name: d.Name, KernelName: d.KernelName, SizeBytes: *d.SizeBytes, Erase: erase}
}

func eraseWarning(ds []PlanDisk) string {
	var names []string
	for _, d := range ds {
		if d.Erase {
			names = append(names, fmt.Sprintf("%s (%s)", d.Name, d.ID))
		}
	}
	return "Все данные на дисках будут уничтожены: " + strings.Join(names, ", ")
}

// toSpecs переводит массивы planner в разделы helper.
func toSpecs(p planner.Plan, sizes map[string]int64) ([]ArraySpec, error) {
	var out []ArraySpec
	for _, a := range p.Arrays {
		switch a.Level {
		case "linear":
			for _, m := range a.Members {
				out = append(out, ArraySpec{Level: LevelSingle, SliceBytes: (sizes[m] - PartitionReserve) / Align * Align, Members: []string{m}})
			}
		case LevelSingle, LevelRAID0, LevelRAID1, LevelRAID5, LevelRAID6, LevelRAID10:
			out = append(out, ArraySpec{Level: a.Level, SliceBytes: a.SliceSize, Members: append([]string{}, a.Members...)})
		default:
			return nil, wrap(ErrUnsupported, "уровень %s", a.Level)
		}
	}
	return out, nil
}

// storedLayout — столбец pools.layout.
type storedLayout struct {
	Disks         []DiskRef    `json:"disks"`
	Arrays        []ArraySpec  `json:"arrays"`
	IntegrityMode string       `json:"integrity_mode,omitempty"`
	InitialSync   bool         `json:"initial_sync,omitempty"`
	Pending       *PoolPending `json:"pending,omitempty"`
	GrowPending   bool         `json:"grow_pending,omitempty"`
	Creating      bool         `json:"creating,omitempty"`
}

func (s *Service) getRecord(ctx context.Context, name string) (PoolRecord, error) {
	var r PoolRecord
	var integ int
	var layout string
	var created, updated int64
	err := s.db.QueryRowContext(ctx, "SELECT name, type, integrity, layout, created_at, updated_at FROM pools WHERE name = ?", name).
		Scan(&r.Name, &r.Type, &integ, &layout, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return r, wrap(ErrNotFound, "пул %s", name)
	}
	if err != nil {
		return r, err
	}
	var l storedLayout
	if err := json.Unmarshal([]byte(layout), &l); err != nil {
		return r, fmt.Errorf("пул %s: разметка в базе: %w", name, err)
	}
	r.Integrity = integ != 0
	r.Disks, r.Arrays, r.InitialSync, r.Pending, r.GrowPending, r.Creating = l.Disks, l.Arrays, l.InitialSync, l.Pending, l.GrowPending, l.Creating
	if r.Integrity {
		r.IntegrityMode = l.IntegrityMode
		if r.IntegrityMode == "" {
			r.IntegrityMode = LegacyIntegrityMode
		}
	}
	r.CreatedAt = time.UnixMilli(created).UTC()
	r.UpdatedAt = time.UnixMilli(updated).UTC()
	return r, nil
}

func (s *Service) planCreate(ctx context.Context, req PlanRequest) (*pending, error) {
	if len(req.DiskIDs) == 0 || len(req.DiskIDs) > MaxDisks {
		return nil, wrap(ErrInvalid, "disk_ids: 1..%d дисков", MaxDisks)
	}
	if _, err := s.getRecord(ctx, req.Pool); err == nil {
		return nil, wrap(ErrConflict, "пул %s уже существует", req.Pool)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	snap, err := s.observe(ctx)
	if err != nil {
		return nil, err
	}
	pr := planner.Request{Type: planner.PoolType(req.Type), SystemReserve: PartitionReserve}
	var pd []PlanDisk
	var refs []DiskRef
	sizes := map[string]int64{}
	seen := map[string]bool{}
	for _, id := range req.DiskIDs {
		if !ValidDiskID(id) || seen[id] {
			return nil, wrap(ErrInvalid, "недопустимый или повторный диск %q", id)
		}
		seen[id] = true
		d, err := s.freeDiskOf(ctx, snap, id)
		if err != nil {
			return nil, err
		}
		pd = append(pd, planDisk(d, true))
		refs = append(refs, DiskRef{ID: id, SizeBytes: *d.SizeBytes})
		sizes[id] = *d.SizeBytes
		pr.Disks = append(pr.Disks, planner.Disk{ID: id, Size: *d.SizeBytes})
	}
	plan, err := planner.Compute(pr)
	if err != nil {
		return nil, wrap(ErrInvalid, "%v", err)
	}
	specs, err := toSpecs(plan, sizes)
	if err != nil {
		return nil, err
	}
	mode := ""
	if req.Integrity {
		mode = DefaultIntegrityMode
	}
	cp := CreateParams{Pool: req.Pool, Integrity: req.Integrity, IntegrityMode: mode, Disks: refs, Arrays: specs}
	if err := cp.Validate(); err != nil {
		return nil, wrap(ErrInvalid, "%v", err)
	}
	warn := append([]string{eraseWarning(pd)}, plan.Warnings...)
	if req.Integrity && plan.Tolerance == 0 {
		warn = append(warn, "Без избыточности dm-integrity только обнаруживает повреждения, исправить их нечем")
	}
	if !req.Integrity {
		warn = append(warn, "Без dm-integrity повреждение данных обнаруживается Btrfs, но не исправляется автоматически")
	}
	if mode == IntegrityBitmap {
		warn = append(warn, "dm-integrity в режиме bitmap: после сбоя питания контрольные суммы областей, в которые шла запись, пересчитываются, и порча в них dm-integrity не обнаруживается (Btrfs обнаружит)")
	}
	if req.Integrity || plan.Tolerance > 0 {
		warn = append(warn, "Пул доступен сразу; начальная синхронизация массива и пересчёт контрольных сумм идут в фоне, до их завершения защита неполная")
	}
	fp, err := s.fingerprint(ctx, PlanCreate, req.Pool, req.DiskIDs)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	return &pending{
		plan: Plan{Op: PlanCreate, Pool: req.Pool, Type: string(plan.Type), Integrity: req.Integrity, IntegrityMode: mode,
			Disks: pd, Arrays: specs, UsableBytes: plan.Usable, Tolerance: plan.Tolerance, Warnings: warn},
		fingerprint: fp,
		task: taskParams{Op: PlanCreate, Create: &cp, Usable: plan.Usable,
			Record: PoolRecord{Name: req.Pool, Type: string(plan.Type), Integrity: req.Integrity, IntegrityMode: mode, Disks: refs, Arrays: specs, InitialSync: true, CreatedAt: now, UpdatedAt: now}},
	}, nil
}

func (s *Service) planAddDisk(ctx context.Context, req PlanRequest) (*pending, error) {
	rec, err := s.getRecord(ctx, req.Pool)
	if err != nil {
		return nil, err
	}
	st, err := s.status(ctx, req.Pool)
	if err != nil {
		return nil, err
	}
	if rec.GrowPending {
		return nil, wrap(ErrConflict, "пул %s заблокирован: не завершено добавление диска — верните или замените диск и нажмите «Продолжить» либо удалите пул", req.Pool)
	}
	if rec.Pending != nil {
		return nil, wrap(ErrConflict, "пул %s: не завершено добавление диска %s", req.Pool, rec.Pending.Disk.ID)
	}
	if m := syncBusy(req.Pool, st, "добавление диска"); m != "" {
		return nil, wrap(ErrConflict, "%s", m)
	}
	if st.Health != HealthOK {
		return nil, wrap(ErrConflict, "пул %s в состоянии %s; добавление диска возможно только в исправном покое", req.Pool, st.Health)
	}
	target := rec.Type
	if req.Type != "" {
		target = req.Type
	}
	var verified, allowed bool
	for _, t := range planner.Transitions(planner.PoolType(rec.Type), len(rec.Disks)) {
		if string(t.Target) == target && (t.Op == planner.OpAddDisk || t.Op == planner.OpConvert || t.Op == planner.OpAddMirror) && t.AddDisks <= 1 {
			allowed, verified = true, t.Verified
		}
	}
	if !allowed {
		return nil, wrap(ErrUnsupported, "переход %s → %s добавлением одного диска", rec.Type, target)
	}
	snap, err := s.observe(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range rec.Disks {
		if d.ID == req.DiskID {
			return nil, wrap(ErrConflict, "диск %s уже в пуле", req.DiskID)
		}
	}
	nd, err := s.freeDiskOf(ctx, snap, req.DiskID)
	if err != nil {
		return nil, err
	}
	pr := planner.Request{Type: planner.PoolType(target), SystemReserve: PartitionReserve}
	sizes := map[string]int64{}
	for _, d := range rec.Disks {
		pr.Disks = append(pr.Disks, planner.Disk{ID: d.ID, Size: d.SizeBytes})
		sizes[d.ID] = d.SizeBytes
	}
	pr.Disks = append(pr.Disks, planner.Disk{ID: nd.ID, Size: *nd.SizeBytes})
	sizes[nd.ID] = *nd.SizeBytes
	plan, err := planner.Compute(pr)
	if err != nil {
		return nil, wrap(ErrInvalid, "%v", err)
	}
	var specs []ArraySpec
	if target == string(planner.JBOD) {
		// JBOD: каждый диск — свой слой single в порядке добавления;
		// новый диск — новый верхний слой (порядок planner по размеру
		// сдвинул бы индексы, а с ними номера разделов и имена объектов).
		specs = append(append([]ArraySpec{}, rec.Arrays...),
			ArraySpec{Level: LevelSingle, SliceBytes: (*nd.SizeBytes - PartitionReserve) / Align * Align, Members: []string{nd.ID}})
	} else if specs, err = toSpecs(plan, sizes); err != nil {
		return nil, err
	}
	// Каждый прежний слой либо не меняется, либо растёт на новый диск с
	// тем же срезом; слои сверх прежних — новые верхние
	// (SHR/SHR-2 диском большего размера, JBOD): раздел на хвосте
	// прежних дисков, которые входят во все нижние слои, и на новом диске.
	if len(specs) < len(rec.Arrays) {
		return nil, wrap(ErrUnsupported, "добавление диска уменьшает число слоёв (%d → %d)", len(rec.Arrays), len(specs))
	}
	ap := AddDiskParams{Pool: req.Pool, Integrity: rec.Integrity, IntegrityMode: rec.IntegrityMode, Disk: DiskRef{ID: nd.ID, SizeBytes: *nd.SizeBytes}}
	var newWarn []string
	for i, sp := range specs {
		if i >= len(rec.Arrays) {
			if !hasMember(sp.Members, nd.ID) || (sp.Level != LevelSingle && sp.Level != LevelRAID1) {
				return nil, wrap(ErrUnsupported, "новый слой %d (%s) без нового диска или неподдерживаемого уровня", i, sp.Level)
			}
			na := NewArray{Index: i, Level: sp.Level, SliceBytes: sp.SliceBytes}
			for _, m := range sp.Members {
				for k := 0; k < i && m != nd.ID; k++ {
					if !hasMember(specs[k].Members, m) {
						return nil, wrap(ErrUnsupported, "новый слой %d: диск %s не входит в слой %d", i, m, k)
					}
				}
				na.Members = append(na.Members, DiskRef{ID: m, SizeBytes: sizes[m]})
			}
			ap.NewArrays = append(ap.NewArrays, na)
			if sp.Level == LevelSingle {
				newWarn = append(newWarn, fmt.Sprintf("Новый слой %d: диск целиком добавляется в пул без защиты", i))
			} else {
				newWarn = append(newWarn, fmt.Sprintf("Новый слой %d (%s из %d): разделы на свободном месте больших дисков пула; начальная синхронизация идёт в фоне", i, sp.Level, len(sp.Members)))
			}
			continue
		}
		old := rec.Arrays[i]
		switch {
		case sp.SliceBytes == old.SliceBytes && sp.Level == old.Level && sameMembers(sp.Members, old.Members):
			continue // слой не меняется
		case sp.SliceBytes == old.SliceBytes && len(sp.Members) == len(old.Members)+1 && hasMember(sp.Members, nd.ID):
		default:
			return nil, wrap(ErrUnsupported, "слой %d меняет размер раздела: добавьте диск размером одного из дисков пула или не меньше верхней границы его слоёв", i)
		}
		ap.Arrays = append(ap.Arrays, ExpandArray{Index: i, Level: sp.Level, RaidDevices: len(sp.Members), SliceBytes: sp.SliceBytes})
		if old.Level == LevelRAID5 && sp.Level == LevelRAID6 {
			// --layout=preserve (см. growCmds).
			newWarn = append(newWarn, fmt.Sprintf("Слой %d: RAID 5 → RAID 6 без перестройки данных — второй паритет целиком на новом диске (восстанавливается только он), ёмкость слоя не растёт; запись медленнее, чем в RAID 6 с обычной раскладкой", i))
		}
	}
	if len(ap.Arrays)+len(ap.NewArrays) == 0 {
		return nil, wrap(ErrUnsupported, "диск %s не входит ни в один слой пула", nd.ID)
	}
	if err := ap.Validate(); err != nil {
		return nil, wrap(ErrUnsupported, "%v", err)
	}
	prev, _ := planner.Compute(planner.Request{Type: planner.PoolType(rec.Type), Disks: pr.Disks[:len(pr.Disks)-1], SystemReserve: PartitionReserve})
	pd := []PlanDisk{planDisk(nd, true)}
	warn := []string{eraseWarning(pd),
		"Перестройка массива идёт в фоне; до её завершения отказ ещё одного диска опасен, производительность снижена"}
	warn = append(warn, newWarn...)
	warn = append(warn, plan.Warnings...)
	if !verified {
		warn = append(warn, "Переход не проверен на стенде")
	}
	ids := []string{nd.ID}
	fp, err := s.fingerprint(ctx, PlanAddDisk, req.Pool, ids)
	if err != nil {
		return nil, err
	}
	refs := append(append([]DiskRef{}, rec.Disks...), ap.Disk)
	rec2 := rec
	rec2.Type, rec2.Disks, rec2.Arrays, rec2.GrowPending = target, refs, specs, true
	return &pending{
		plan: Plan{Op: PlanAddDisk, Pool: req.Pool, Type: target, FromType: rec.Type, Integrity: rec.Integrity, IntegrityMode: rec.IntegrityMode,
			Disks: pd, Arrays: specs, UsableBytes: plan.Usable, PrevUsableBytes: prev.Usable, Tolerance: plan.Tolerance,
			TransitionChecked: verified, Warnings: warn},
		fingerprint: fp,
		task:        taskParams{Op: PlanAddDisk, AddDisk: &ap, Record: rec2, Usable: plan.Usable},
	}, nil
}

// sameMembers — одинаковый набор участников (без учёта порядка).
func sameMembers(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, m := range a {
		if !hasMember(b, m) {
			return false
		}
	}
	return true
}

func hasMember(ms []string, id string) bool {
	for _, m := range ms {
		if m == id {
			return true
		}
	}
	return false
}

func (s *Service) planReplace(ctx context.Context, req PlanRequest) (*pending, error) {
	rec, err := s.getRecord(ctx, req.Pool)
	if err != nil {
		return nil, err
	}
	if rec.Pending != nil {
		return nil, wrap(ErrConflict, "пул %s: не завершена операция с диском %s", req.Pool, rec.Pending.Disk.ID)
	}
	st, err := s.status(ctx, req.Pool)
	if err != nil {
		return nil, err
	}
	hot := req.OldDiskID != ""
	if m := syncBusy(req.Pool, st, "замена диска"); m != "" && (hot || st.Health != HealthDegraded) {
		return nil, wrap(ErrConflict, "%s", m)
	}
	switch {
	case hot && st.Health != HealthOK:
		return nil, wrap(ErrConflict, "пул %s в состоянии %s; замена исправного диска — только в исправном покое (отказавший диск заменяется без old_disk_id)", req.Pool, st.Health)
	case !hot && st.Health != HealthDegraded:
		return nil, wrap(ErrConflict, "пул %s в состоянии %s; замена нужна только неполному пулу, в котором не идёт восстановление", req.Pool, st.Health)
	}
	for _, id := range st.DiskIDs {
		if id == req.DiskID {
			return nil, wrap(ErrConflict, "диск %s сейчас входит в пул", req.DiskID)
		}
	}
	if rec.hasDisk(req.DiskID) {
		return nil, wrap(ErrConflict, "диск %s уже в пуле", req.DiskID)
	}
	snap, err := s.observe(ctx)
	if err != nil {
		return nil, err
	}
	nd, err := s.freeDiskOf(ctx, snap, req.DiskID)
	if err != nil {
		return nil, err
	}
	y := DiskRef{ID: nd.ID, SizeBytes: *nd.SizeBytes}
	rp := ReplaceParams{Pool: req.Pool, Integrity: rec.Integrity, IntegrityMode: rec.IntegrityMode, Disk: y}
	present := map[string]bool{}
	var old DiskRef
	if hot {
		for _, d := range rec.Disks {
			if d.ID == req.OldDiskID {
				old = d
			}
		}
		if old.ID == "" {
			return nil, wrap(ErrNotFound, "диск %s не входит в пул %s", req.OldDiskID, req.Pool)
		}
		rp.Old = &old
		for i, a := range rec.Arrays {
			if !hasMember(a.Members, old.ID) {
				continue
			}
			if a.Level == LevelSingle || a.Level == LevelRAID0 {
				return nil, wrap(ErrUnsupported, "слой %d (%s) без избыточности: замена без остановки невозможна", i, a.Level)
			}
			rp.Arrays = append(rp.Arrays, ReplaceArray{Index: i, SliceBytes: a.SliceBytes})
		}
		for _, d := range rec.Disks {
			present[d.ID] = d.ID != old.ID
		}
	} else {
		for _, a := range st.Arrays {
			if a.Missing == 0 && a.FailedDevices == 0 {
				continue
			}
			if a.Index < 0 || a.Index >= len(rec.Arrays) {
				return nil, wrap(ErrConflict, "массив %s отсутствует в записи пула", a.Name)
			}
			rp.Arrays = append(rp.Arrays, ReplaceArray{Index: a.Index, SliceBytes: rec.Arrays[a.Index].SliceBytes})
		}
		for _, id := range st.DiskIDs {
			present[id] = true
		}
	}
	if len(rp.Arrays) == 0 {
		return nil, wrap(ErrConflict, "нет массивов для замены")
	}
	for n, a := range rp.Arrays {
		if a.Index != n {
			return nil, wrap(ErrUnsupported, "заменяемый диск входит в слои не подряд снизу (%d)", a.Index)
		}
	}
	if err := rp.Validate(); err != nil {
		// Новый диск меньше места, занятого заменяемым.
		return nil, wrap(ErrUnsupported, "диск %s не вмещает разделы заменяемого: %v", y.ID, err)
	}
	// Запись пула после замены: отсутствующий (или старый) диск — новый.
	rec2 := rec
	rec2.Pending, rec2.Disks, rec2.Arrays = nil, nil, append([]ArraySpec{}, rec.Arrays...)
	var gone []string
	for _, d := range rec.Disks {
		if present[d.ID] {
			rec2.Disks = append(rec2.Disks, d)
		} else {
			gone = append(gone, d.ID)
		}
	}
	rec2.Disks = append(rec2.Disks, y)
	for i, a := range rec2.Arrays {
		var ms []string
		for _, m := range a.Members {
			if present[m] {
				ms = append(ms, m)
			}
		}
		for _, r := range rp.Arrays {
			if r.Index == i {
				ms = append(ms, y.ID)
			}
		}
		rec2.Arrays[i].Members = ms
	}
	verified := false
	for _, t := range planner.Transitions(planner.PoolType(rec.Type), len(rec.Disks)) {
		if t.Op == planner.OpReplaceLarger {
			verified = t.Verified
		}
	}
	pd := []PlanDisk{planDisk(nd, true)}
	warn := []string{eraseWarning(pd)}
	if hot {
		warn = append(warn, "Копирование на новый диск идёт в фоне, избыточность пула при этом сохраняется; после него старый диск "+old.ID+" выводится из пула и его разметка стирается")
	} else {
		warn = append(warn, "Восстановление идёт в фоне; до его завершения пул не защищён от следующего отказа")
	}
	if len(gone) > 0 && !hot {
		warn = append(warn, "Отсутствующие диски пула удаляются из записи: "+strings.Join(gone, ", "))
	}
	used := int64(0)
	for _, a := range rp.Arrays {
		used += a.SliceBytes
	}
	if hot && y.SizeBytes < old.SizeBytes {
		warn = append(warn, "Новый диск меньше заменяемого, но вмещает его разделы: ёмкость не меняется")
	}
	final := rec2.Arrays
	if g, err := growthPlan(rec2, y); err == nil {
		if len(g.Expand)+len(g.New) > 0 {
			final = g.Final
			warn = append(warn, "После замены пул растёт за счёт свободного места нового диска (рост выполняется в той же задаче)")
		} else if availBytes(y.SizeBytes) > used {
			warn = append(warn, "Свободное место нового диска пока не используется: ёмкость растёт после замены остальных дисков")
		}
		warn = append(warn, g.Notes...)
	}
	if !verified {
		warn = append(warn, "Переход не проверен на стенде")
	}
	fp, err := s.fingerprint(ctx, PlanReplace, req.Pool, []string{nd.ID})
	if err != nil {
		return nil, err
	}
	return &pending{
		plan: Plan{Op: PlanReplace, Pool: req.Pool, Type: rec.Type, FromType: rec.Type, Integrity: rec.Integrity, IntegrityMode: rec.IntegrityMode,
			Disks: pd, Arrays: final, UsableBytes: usableOf(final), PrevUsableBytes: usableOf(rec.Arrays), TransitionChecked: verified, Warnings: warn},
		fingerprint: fp,
		task:        taskParams{Op: PlanReplace, Replace: &rp, Record: rec2},
	}, nil
}

// planExpand — рост пула за счёт свободного места диска пула (повтор роста
// после замены, если он прерван или не выполнялся).
func (s *Service) planExpand(ctx context.Context, req PlanRequest) (*pending, error) {
	rec, err := s.getRecord(ctx, req.Pool)
	if err != nil {
		return nil, err
	}
	if rec.GrowPending {
		return nil, wrap(ErrConflict, "пул %s заблокирован: не завершено добавление диска — верните или замените диск и нажмите «Продолжить» либо удалите пул", req.Pool)
	}
	if rec.Pending != nil {
		return nil, wrap(ErrConflict, "пул %s: не завершена операция с диском %s", req.Pool, rec.Pending.Disk.ID)
	}
	st, err := s.status(ctx, req.Pool)
	if err != nil {
		return nil, err
	}
	if m := syncBusy(req.Pool, st, "рост"); m != "" {
		return nil, wrap(ErrConflict, "%s", m)
	}
	if st.Health != HealthOK {
		return nil, wrap(ErrConflict, "пул %s в состоянии %s; рост возможен только в исправном покое", req.Pool, st.Health)
	}
	var y DiskRef
	for _, d := range rec.Disks {
		if d.ID == req.DiskID {
			y = d
		}
	}
	if y.ID == "" {
		return nil, wrap(ErrNotFound, "диск %s не входит в пул %s", req.DiskID, req.Pool)
	}
	g, err := growthPlan(rec, y)
	if err != nil {
		return nil, wrap(ErrUnsupported, "%v", err)
	}
	if len(g.Expand)+len(g.New) == 0 {
		return nil, wrap(ErrConflict, "пул %s: расти нечему (%s)", req.Pool, strings.Join(append([]string{"свободного места на дисках не хватает для защищённого слоя"}, g.Notes...), "; "))
	}
	ap := AddDiskParams{Pool: req.Pool, Integrity: rec.Integrity, IntegrityMode: rec.IntegrityMode, Disk: y, Arrays: g.Expand, NewArrays: g.New, Grow: true}
	if err := ap.Validate(); err != nil {
		return nil, wrap(ErrUnsupported, "%v", err)
	}
	fp, err := s.fingerprint(ctx, PlanExpand, req.Pool, nil)
	if err != nil {
		return nil, err
	}
	rec2 := rec
	rec2.Arrays, rec2.GrowPending = g.Final, true
	warn := append([]string{"Разделы создаются на свободном месте дисков пула, данные не затрагиваются; перестройка и синхронизация идут в фоне"}, g.Notes...)
	return &pending{
		plan: Plan{Op: PlanExpand, Pool: req.Pool, Type: rec.Type, FromType: rec.Type, Integrity: rec.Integrity, IntegrityMode: rec.IntegrityMode,
			Disks: []PlanDisk{}, Arrays: g.Final, UsableBytes: usableOf(g.Final), PrevUsableBytes: usableOf(rec.Arrays), TransitionChecked: true, Warnings: warn},
		fingerprint: fp,
		task:        taskParams{Op: PlanAddDisk, AddDisk: &ap, Record: rec2, Usable: usableOf(g.Final)},
	}, nil
}

// usableOf — ёмкость слоёв до накладных расходов (как planner).
func usableOf(arrs []ArraySpec) int64 {
	u := int64(0)
	for _, a := range arrs {
		n := int64(len(a.Members))
		switch a.Level {
		case LevelSingle, LevelRAID1:
			u += a.SliceBytes
		case LevelRAID0:
			u += n * a.SliceBytes
		case LevelRAID5:
			u += (n - 1) * a.SliceBytes
		case LevelRAID6:
			u += (n - 2) * a.SliceBytes
		case LevelRAID10:
			u += n / 2 * a.SliceBytes
		}
	}
	return u
}

// ---------- задачи ----------

// TaskResult — итог задачи пула.
type TaskResult struct {
	Pool               string     `json:"pool"`
	PlannedUsableBytes int64      `json:"planned_usable_bytes,omitempty"`
	Helper             []Result   `json:"helper"`
	Status             PoolStatus `json:"status"`
}

func decodeTask(raw json.RawMessage) (taskParams, error) {
	var tp taskParams
	err := json.Unmarshal(raw, &tp)
	return tp, err
}

func (s *Service) runCreate(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	tp, err := decodeTask(raw)
	if err != nil || tp.Create == nil {
		return nil, fmt.Errorf("параметры задачи: %v", err)
	}
	progress(5)
	// Запись «создаётся» — до helper: сброс или перезапуск
	// gamayund внутри create не оставляет остатков на дисках без записи.
	rec := tp.Record
	rec.Creating = true
	if err := s.saveRecord(ctx, rec, true); err != nil {
		return nil, fmt.Errorf("запись пула %s: %w", tp.Create.Pool, err)
	}
	var r Result
	if err := s.helper.Call(ctx, OpCreate, tp.Create, &r); err != nil {
		if refusedBeforeWrite(err) {
			// helper отказал до записи — диски не тронуты, откат
			// по дискам не нужен; запись «создаётся» убрать сразу.
			if derr := s.dropRecord(context.WithoutCancel(ctx), tp.Create.Pool); derr != nil {
				return nil, fmt.Errorf("создание пула %s: %w; диски не тронуты, но запись не удалена: %v — удалите пул", tp.Create.Pool, err, derr)
			}
			return nil, fmt.Errorf("создание пула %s отклонено, диски не тронуты: %w", tp.Create.Pool, err)
		}
		if s.Stopping() {
			return nil, deferRollback(tp.Create.Pool, err)
		}
		done, rerr := s.rollbackCreate(context.WithoutCancel(ctx), rec)
		if rerr != nil {
			return nil, fmt.Errorf("создание пула %s: %w; откат не выполнен: %v — удалите пул", tp.Create.Pool, err, rerr)
		}
		return nil, fmt.Errorf("создание пула %s: %w; %s", tp.Create.Pool, err, done)
	}
	rec.Creating = false
	if err := s.saveRecord(ctx, rec, false); err != nil {
		return nil, fmt.Errorf("пул создан, но не записан в базу: %w", err)
	}
	// Пул смонтирован и доступен —
	// создание завершено. Начальная синхронизация (пересчёт тегов
	// dm-integrity, resync md) идёт в ядре часами и видна как состояние
	// пула (resyncing, Sync); признак InitialSync снимает проверка защиты
	// при health ok. Сбой во время синхронизации — обычная деградация
	// с событием и защитой, без отката: на пуле уже могут быть данные.
	// Откат остаётся, только если цель не достигнута сразу после создания
	// (пул не поднят или уже неисправен) — данных на нём ещё нет.
	// Неисправен сразу — одна попытка повторной сборки (участник мог
	// появиться позже), затем решение.
	up := func(st PoolStatus) bool {
		return st.Mounted && (st.Health == HealthOK || st.Health == HealthResyncing)
	}
	st, err := s.status(ctx, tp.Create.Pool)
	if err == nil && !up(st) && s.reassemble(ctx, tp.Create.Pool) {
		st, err = s.status(ctx, tp.Create.Pool)
	}
	if err == nil && !up(st) {
		why := "участник не вернулся"
		if st.Health == HealthOK || st.Health == HealthResyncing {
			why = "файловая система не смонтирована"
		}
		err = fmt.Errorf("пул %s после создания в состоянии %s, повторная сборка не помогла — %s: %s", tp.Create.Pool, st.Health, why, strings.Join(st.Issues, "; "))
	}
	if err != nil {
		if s.Stopping() {
			return nil, deferRollback(tp.Create.Pool, err)
		}
		done, rerr := s.rollbackCreate(context.WithoutCancel(ctx), rec)
		if rerr != nil {
			return nil, fmt.Errorf("создание пула %s не завершено: %w; откат не выполнен: %v — удалите пул вручную", tp.Create.Pool, err, rerr)
		}
		return nil, fmt.Errorf("создание пула %s не завершено: %w; %s", tp.Create.Pool, err, done)
	}
	if st.Health == HealthOK {
		if err := s.endInitialSync(ctx, tp.Create.Pool); err != nil {
			return nil, err
		}
	}
	return TaskResult{Pool: tp.Create.Pool, PlannedUsableBytes: tp.Usable, Helper: []Result{r}, Status: st}, nil
}

// deferRollback — создание прервано остановкой gamayund: запись остаётся
// «создаётся», откат выполнит следующий запуск (resolveCreating).
func deferRollback(pool string, err error) error {
	return fmt.Errorf("создание пула %s прервано остановкой gamayund (%v): откат выполнит следующий запуск", pool, err)
}

func (s *Service) runAddDisk(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	tp, err := decodeTask(raw)
	if err != nil || tp.AddDisk == nil {
		return nil, fmt.Errorf("параметры задачи: %v", err)
	}
	return s.addDisk(ctx, tp.AddDisk, tp.Record, tp.Usable, progress)
}

// addDisk — добавление диска / рост пула (Grow) с ожиданием в записи
// helper add_disk → новая запись → конец reshape → рост LVM/Btrfs.
func (s *Service) addDisk(ctx context.Context, ap *AddDiskParams, record PoolRecord, usable int64, progress func(int)) (any, error) {
	pool := ap.Pool
	progress(2)
	// Ожидаемый состав — в базу до вызова helper: сброс внутри
	// add_disk разрешит AssembleAll по суперблокам md.
	cur, err := s.getRecord(ctx, pool)
	if err != nil {
		return nil, err
	}
	cur.Pending = &PoolPending{Disk: ap.Disk, Type: record.Type, Arrays: record.Arrays, Expand: ap.Arrays,
		NewArrays: ap.NewArrays, Grow: ap.Grow}
	if newParityLayer(ap) {
		// Новый слой raid5/raid6 (рост RAID 5/6 после замены)
		// строится как «деградирован + recovery»: это начальная
		// синхронизация, не отказ — иначе защита
		// включала read-only на всё время сборки слоя (найдено на стенде).
		// Признак снимает проход защиты, когда пул снова ok.
		cur.InitialSync, record.InitialSync = true, true
	}
	if err := s.saveRecord(ctx, cur, false); err != nil {
		return nil, err
	}
	var r1 Result
	if err := s.helper.Call(ctx, OpAddDisk, ap, &r1); err != nil {
		// Часть шагов могла выполниться: сверка с массивами сейчас.
		// Дозавершение получает пул от этой задачи (handoff, submit).
		// Сбой дозавершения (Resume) новую задачу не ставит — иначе цикл;
		// следующая попытка — «продолжить замену» (Resume) или перезапуск gamayund.
		if rerr := s.resolvePending(ctx, cur, !ap.Resume); rerr != nil {
			return nil, errors.Join(err, rerr)
		}
		return nil, err
	}
	// Состав массивов уже изменён: запись обновляется сразу, чтобы база не
	// расходилась с дисками, даже если ожидание прервут.
	if err := s.saveRecord(ctx, record, false); err != nil {
		return nil, err
	}
	progress(5)
	if _, err := s.waitIdle(ctx, pool, progress, 5, 90, expanded(ap.Arrays)); err != nil {
		// Состав массивов уже изменён (запись с GrowPending): откат
		// рискует данными — пул заблокирован до «Продолжить» или удаления.
		return nil, fmt.Errorf("добавление диска в пул %s не завершено: %w; пул заблокирован — верните или замените диск и нажмите «Продолжить» либо удалите пул", pool, err)
	}
	var r2 Result
	if err := s.helper.Call(ctx, OpGrow, PoolParams{Pool: pool}, &r2); err != nil {
		return nil, err
	}
	if err := s.endGrowPending(ctx, pool); err != nil {
		return nil, err
	}
	progress(95)
	st, err := s.status(ctx, pool)
	if err != nil {
		return nil, err
	}
	return TaskResult{Pool: pool, PlannedUsableBytes: usable, Helper: []Result{r1, r2}, Status: st}, nil
}

// newParityLayer — добавление создаёт слой raid5/raid6.
func newParityLayer(ap *AddDiskParams) bool {
	for _, na := range ap.NewArrays {
		if na.Level == LevelRAID5 || na.Level == LevelRAID6 {
			return true
		}
	}
	return false
}

// replaceTargets — ожидаемые массивы замены: число участников прежнее.
func replaceTargets(rec PoolRecord, rp *ReplaceParams) []ExpandArray {
	var out []ExpandArray
	for _, a := range rp.Arrays {
		sp := rec.Arrays[a.Index]
		out = append(out, ExpandArray{Index: a.Index, Level: sp.Level, RaidDevices: len(sp.Members), SliceBytes: a.SliceBytes})
	}
	return out
}

func (s *Service) runReplace(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	tp, err := decodeTask(raw)
	if err != nil || tp.Replace == nil {
		return nil, fmt.Errorf("параметры задачи: %v", err)
	}
	rp := tp.Replace
	pool := rp.Pool
	progress(2)
	cur, err := s.getRecord(ctx, pool)
	if err != nil {
		return nil, err
	}
	if !rp.Resume {
		// Ожидание — до вызова helper: сброс после --add
		// иначе оставлял диск в md, но не в записи (сборка и ранний юнит
		// integrity его не знали).
		cur.Pending = &PoolPending{Replace: true, Disk: rp.Disk, Old: rp.Old, Type: cur.Type, Arrays: tp.Record.Arrays,
			Expand: replaceTargets(cur, rp), Disks: tp.Record.Disks}
		if err := s.saveRecord(ctx, cur, false); err != nil {
			return nil, err
		}
	}
	var r1 Result
	if err := s.helper.Call(ctx, OpReplace, rp, &r1); err != nil {
		if rerr := s.resolveReplace(ctx, cur, !rp.Resume); rerr != nil {
			return nil, errors.Join(err, rerr)
		}
		return nil, err
	}
	progress(5)
	// Ждать не просто покоя: сразу после --replace (особенно при
	// дозавершении) recovery может ещё не начаться — найдено на стенде.
	if _, err := s.waitIdle(ctx, pool, progress, 5, 80, replaced(rp)); err != nil {
		return nil, err
	}
	helper := []Result{r1}
	if rp.Old != nil {
		var r2 Result
		if err := s.helper.Call(ctx, OpReplaceFinish, rp, &r2); err != nil {
			return nil, err
		}
		helper = append(helper, r2)
	}
	final := tp.Record
	final.Pending = nil
	if err := s.saveRecord(ctx, final, false); err != nil {
		return nil, err
	}
	progress(82)
	// Рост за счёт свободного места нового диска (после замены последнего
	// малого диска или в SHR — сразу).
	if g, err := growthPlan(final, rp.Disk); err == nil && len(g.Expand)+len(g.New) > 0 {
		ap := &AddDiskParams{Pool: pool, Integrity: final.Integrity, IntegrityMode: final.IntegrityMode, Disk: rp.Disk,
			Arrays: g.Expand, NewArrays: g.New, Grow: true}
		if err := ap.Validate(); err != nil {
			return nil, fmt.Errorf("замена выполнена, рост не запущен: %w", err)
		}
		rec2 := final
		rec2.Arrays, rec2.GrowPending = g.Final, true
		out, err := s.addDisk(ctx, ap, rec2, usableOf(g.Final), func(p int) { progress(82 + p*17/100) })
		if err != nil {
			return nil, fmt.Errorf("замена выполнена, рост прерван: %w", err)
		}
		tr := out.(TaskResult)
		tr.Helper = append(helper, tr.Helper...)
		return tr, nil
	}
	st, err := s.status(ctx, pool)
	if err != nil {
		return nil, err
	}
	return TaskResult{Pool: pool, Helper: helper, Status: st}, nil
}

// replaced — во всех массивах замены новый диск — исправный участник, а
// старый (горячая замена) — не участник (отказавший вне слотов или удалён).
func replaced(rp *ReplaceParams) func(PoolStatus) bool {
	return func(st PoolStatus) bool {
		for _, x := range rp.Arrays {
			okNew := false
			for _, a := range st.Arrays {
				if a.Index != x.Index {
					continue
				}
				for _, m := range a.Members {
					switch {
					case m.DiskID == rp.Disk.ID:
						okNew = okNew || (strings.Contains(m.State, "active sync") && !strings.Contains(m.State, "rebuilding"))
					case rp.Old != nil && m.DiskID == rp.Old.ID && !strings.Contains(m.State, "faulty"):
						return false
					}
				}
			}
			if !okNew {
				return false
			}
		}
		return true
	}
}

// resolveReplace сверяет ожидаемую замену с массивами: новый диск не принят
// ни в один массив — откат (сборка стёрла его разметку), принят — ожидание
// остаётся и (resume) ставится дозавершение.
func (s *Service) resolveReplace(ctx context.Context, rec PoolRecord, resume bool) error {
	var r AssembleResult
	if err := s.helper.Call(ctx, OpAssemble, rec.assembleParams(), &r); err != nil {
		return err
	}
	return s.afterReplaceAssemble(ctx, rec, r.PendingAccepted, resume)
}

func (s *Service) afterReplaceAssemble(ctx context.Context, rec PoolRecord, accepted []int, resume bool) error {
	if len(accepted) == 0 {
		rec.Pending = nil
		return s.saveRecord(ctx, rec, false)
	}
	if !resume {
		return nil
	}
	raw, err := json.Marshal(replaceResumeTask(rec))
	if err != nil {
		return err
	}
	if _, err := s.submit(ctx, rec.Name, TaskReplace, raw); err != nil {
		return fmt.Errorf("дозавершение замены диска: %w", err)
	}
	return nil
}

// replaceResumeTask — параметры дозавершения замены по ожиданию в записи.
func replaceResumeTask(rec PoolRecord) taskParams {
	pd := rec.Pending
	rp := ReplaceParams{Pool: rec.Name, Integrity: rec.Integrity, IntegrityMode: rec.IntegrityMode, Disk: pd.Disk, Old: pd.Old, Resume: true}
	for _, x := range pd.Expand {
		rp.Arrays = append(rp.Arrays, ReplaceArray{Index: x.Index, SliceBytes: x.SliceBytes})
	}
	final := rec
	final.Pending, final.Arrays, final.Disks = nil, pd.Arrays, pd.Disks
	return taskParams{Op: PlanReplace, Replace: &rp, Record: final}
}

// Resume — «продолжить замену». Сбой задачи
// дозавершения оставляет ожидание (pending/grow_pending) в записи, и раньше
// его подхватывала только сборка при старте gamayund, а новые операции
// получали 409. Resume ставит задачу, которая делает то же, что старт,
// для одного пула: сверка ожидания с массивами (сборка живого пула
// идемпотентна) и дозавершение в той же задаче — пул занят до конца.
// Нечего продолжать или пул занят — ErrConflict (повтор безопасен).
func (s *Service) Resume(ctx context.Context, pool string) (tasks.Task, error) {
	rec, err := s.getRecord(ctx, pool)
	if err != nil {
		return tasks.Task{}, err
	}
	if rec.Pending == nil && !rec.GrowPending {
		return tasks.Task{}, wrap(ErrConflict, "пул %s: незавершённой операции с диском нет", pool)
	}
	raw, err := json.Marshal(growTask{Pool: pool})
	if err != nil {
		return tasks.Task{}, err
	}
	return s.submit(ctx, pool, TaskResume, raw)
}

func (s *Service) runResume(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	var gt growTask
	if err := json.Unmarshal(raw, &gt); err != nil || gt.Pool == "" {
		return nil, fmt.Errorf("параметры задачи: %v", err)
	}
	progress(1)
	rec, err := s.getRecord(ctx, gt.Pool)
	if err != nil {
		return nil, err
	}
	if pd := rec.Pending; pd != nil {
		var r AssembleResult
		if err := s.helper.Call(ctx, OpAssemble, rec.assembleParams(), &r); err != nil {
			return nil, err
		}
		switch {
		case pd.Replace && len(r.PendingAccepted) == 0:
			// Новый диск не принят ни в один массив — откат (сборка
			// стёрла его разметку), как при старте.
			rec.Pending = nil
			if err := s.saveRecord(ctx, rec, false); err != nil {
				return nil, err
			}
		case pd.Replace:
			raw, err := json.Marshal(replaceResumeTask(rec))
			if err != nil {
				return nil, err
			}
			return s.runReplace(ctx, raw, progress)
		default:
			res := rec.resolve(r.PendingAccepted)
			if err := s.saveRecord(ctx, res, false); err != nil {
				return nil, err
			}
			if tp, ok := res.resumeTask(); ok {
				return s.addDisk(ctx, tp.AddDisk, tp.Record, tp.Usable, progress)
			}
			rec = res
		}
	}
	if rec.GrowPending {
		return s.runGrow(ctx, raw, progress)
	}
	st, err := s.status(ctx, gt.Pool)
	if err != nil {
		return nil, err
	}
	return TaskResult{Pool: gt.Pool, Status: st}, nil
}

// expanded — массивы достигли целевого уровня, числа участников и размера
// (reshape может начаться не мгновенно после mdadm --grow).
func expanded(want []ExpandArray) func(PoolStatus) bool {
	return func(st PoolStatus) bool {
		for _, w := range want {
			ok := false
			for _, a := range st.Arrays {
				if a.Index != w.Index {
					continue
				}
				data := int64(1)
				switch w.Level {
				case LevelRAID5:
					data = int64(w.RaidDevices - 1)
				case LevelRAID6:
					data = int64(w.RaidDevices - 2)
				}
				ok = a.Level == w.Level && a.RaidDevices == w.RaidDevices && a.DevSizeBytes > 0 &&
					a.ArraySizeBytes >= data*a.DevSizeBytes
			}
			if !ok {
				return false
			}
		}
		return true
	}
}

func (s *Service) saveRecord(ctx context.Context, r PoolRecord, insert bool) error {
	layout, err := json.Marshal(storedLayout{Disks: r.Disks, Arrays: r.Arrays, IntegrityMode: r.IntegrityMode, InitialSync: r.InitialSync, Pending: r.Pending, GrowPending: r.GrowPending, Creating: r.Creating})
	if err != nil {
		return err
	}
	now := s.now().UnixMilli()
	integ := 0
	if r.Integrity {
		integ = 1
	}
	defer s.writeBootFile(ctx)
	if insert {
		_, err = s.db.ExecContext(ctx, "INSERT INTO pools(name, type, integrity, layout, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
			r.Name, r.Type, integ, string(layout), now, now)
		return err
	}
	res, err := s.db.ExecContext(ctx, "UPDATE pools SET type = ?, integrity = ?, layout = ?, updated_at = ? WHERE name = ?",
		r.Type, integ, string(layout), now, r.Name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("пул %s отсутствует в базе", r.Name)
	}
	return nil
}

// ---------- чтение ----------

// PoolView — запись пула и его фактическое состояние.
type PoolView struct {
	PoolRecord
	Status      *PoolStatus `json:"status"`
	StatusError string      `json:"status_error,omitempty"`
	Protection  Protection  `json:"protection"`
	// Busy — выполняющаяся над пулом задача (пусто — свободен): веб
	// показывает «продолжить замену» только у свободного пула.
	Busy string `json:"busy,omitempty"`
}

// List — пулы из базы с состоянием.
func (s *Service) List(ctx context.Context) ([]PoolView, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM pools ORDER BY name")
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	out := []PoolView{}
	for _, n := range names {
		v, err := s.Get(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Get — пул по имени с состоянием; ошибка helper не скрывает запись.
func (s *Service) Get(ctx context.Context, name string) (PoolView, error) {
	if !ValidPoolName(name) {
		return PoolView{}, wrap(ErrNotFound, "пул %s", name)
	}
	rec, err := s.getRecord(ctx, name)
	if err != nil {
		return PoolView{}, err
	}
	v := PoolView{PoolRecord: rec, Busy: s.Busy(name)}
	if st, err := s.status(ctx, name); err != nil {
		v.StatusError = err.Error()
	} else {
		v.Status = &st
	}
	if v.Protection, err = s.protection(ctx, name, v.Status); err != nil {
		return PoolView{}, err
	}
	// Цикл защиты завис — показанное состояние не защищено проверкой.
	if why := s.ProtectionStale(); why != "" {
		markUnknown(&v, why)
	}
	return v, nil
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
