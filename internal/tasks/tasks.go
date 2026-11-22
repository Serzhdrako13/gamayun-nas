// Package tasks — долгие операции демона с состоянием в SQLite.
//
// Состояния: queued → running → succeeded | failed; interrupted — задача
// была queued/running, когда демон остановился. При старте (Recover) такие
// задачи переводятся в interrupted и НЕ возобновляются: операции с дисками
// нельзя повторять вслепую, решение принимает администратор после проверки.
// Отмена — через context (Cancel): задача завершается как failed с кодом
// cancelled. Остановка демона (Shutdown) отменяет контексты и переводит
// незавершённые задачи в interrupted.
//
// Виды задач регистрируются кодом (Register) до приёма запросов; задачи
// произвольных видов создать нельзя.
//
// Сторож задач: выполняющаяся задача обязана подавать признаки
// жизни — Alive (наблюдение, прогресс), Expect (впереди долгий шаг с
// известным пределом). Признак хранится в записи задачи (alive_at,
// alive_note) и переживает перезапуск: причина interrupted называет
// последнее наблюдение. Нет признака дольше окна — задача отменяется с
// причиной stalled; не завершилась за второе окно после отмены — переводится
// в failed принудительно (функция задачи, вернувшись позже, состояние уже
// не меняет). Задача не может висеть в running бесконечно.
package tasks

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"gamayun-nas/internal/events"
)

// State — состояние задачи.
type State string

const (
	Queued      State = "queued"
	Running     State = "running"
	Succeeded   State = "succeeded"
	Failed      State = "failed"
	Interrupted State = "interrupted"
)

// Коды ошибок задач.
const (
	CodeInterrupted = "interrupted"
	CodeCancelled   = "cancelled"
	CodeFailed      = "task_failed"
	CodePanic       = "panic"
	CodeStalled     = "stalled" // нет признаков жизни
)

// DefaultStallWindow — окно без признаков жизни по умолчанию.
const DefaultStallWindow = 5 * time.Minute

// Error — причина failed/interrupted.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Task — снимок задачи.
type Task struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	State      State           `json:"state"`
	Progress   int             `json:"progress"` // 0–100
	Params     json.RawMessage `json:"params"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      *Error          `json:"error,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	// AliveAt, AliveNote — последний признак жизни.
	AliveAt   *time.Time `json:"alive_at,omitempty"`
	AliveNote string     `json:"alive_note,omitempty"`
}

// RunFunc выполняет задачу. Обязана завершаться при отмене ctx.
// progress принимает процент 0–100.
type RunFunc func(ctx context.Context, params json.RawMessage, progress func(int)) (result any, err error)

// Типы событий.
const (
	EventState    = "task.state"    // смена состояния, журналируется
	EventProgress = "task.progress" // прогресс, только живым подписчикам
)

var (
	ErrNotFound    = errors.New("задача не найдена")
	ErrUnknownKind = errors.New("неизвестный вид задачи")
	ErrNotActive   = errors.New("задача уже завершена")
	ErrClosed      = errors.New("менеджер задач остановлен")
	ErrBadParams   = errors.New("параметры задачи должны быть JSON")

	errCancelled = errors.New("отменена пользователем")
	errShutdown  = errors.New("демон остановлен во время выполнения задачи")
)

// Options — настройки менеджера.
type Options struct {
	MaxParallel int // одновременно выполняемых задач; по умолчанию 2
	Logger      *slog.Logger
	// StallWindow — окно без признаков жизни (Alive/Expect/progress), после
	// которого задача отменяется; второе такое же окно — до принудительного
	// failed. 0 — DefaultStallWindow.
	StallWindow time.Duration
	// WatchPoll — период проверки сторожа; 0 — StallWindow/10 (не реже 1 с
	// и не чаще 10 мс).
	WatchPoll time.Duration
}

// Manager — менеджер задач.
type Manager struct {
	db     *sql.DB
	log    *events.Log
	logger *slog.Logger
	kinds  map[string]RunFunc
	sem    chan struct{}
	now    func() time.Time

	base context.Context
	stop context.CancelCauseFunc
	wg   sync.WaitGroup

	mu     sync.Mutex
	active map[string]context.CancelCauseFunc
	alive  map[string]*aliveState // выполняющиеся задачи (running)
	closed bool

	stall time.Duration
	poll  time.Duration
}

// aliveState — признак жизни выполняющейся задачи (под Manager.mu).
type aliveState struct {
	until    time.Time // следующий признак ожидается до
	last     time.Time // последний признак
	note     string
	saved    time.Time // когда признак записан в базу
	savedFor string
	stalled  bool      // отменена сторожем, ждём завершения
	forceAt  time.Time // после — failed принудительно
}

// New создаёт менеджер. db — открытая store.Open база, log — журнал событий.
func New(db *sql.DB, log *events.Log, opt Options) *Manager {
	if opt.MaxParallel < 1 {
		opt.MaxParallel = 2
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.StallWindow <= 0 {
		opt.StallWindow = DefaultStallWindow
	}
	if opt.WatchPoll <= 0 {
		opt.WatchPoll = min(max(opt.StallWindow/10, 10*time.Millisecond), time.Second)
	}
	base, stop := context.WithCancelCause(context.Background())
	m := &Manager{
		db: db, log: log, logger: opt.Logger,
		kinds:  map[string]RunFunc{},
		sem:    make(chan struct{}, opt.MaxParallel),
		now:    time.Now,
		base:   base,
		stop:   stop,
		active: map[string]context.CancelCauseFunc{},
		alive:  map[string]*aliveState{},
		stall:  opt.StallWindow,
		poll:   opt.WatchPoll,
	}
	go m.watchdog()
	return m
}

// aliveKey — ключ контекста задачи для Alive/Expect.
type aliveKey struct{}

type aliveRef struct {
	m  *Manager
	id string
}

// Alive — признак жизни задачи с описанием текущего наблюдения: следующий
// ожидается в пределах окна сторожа. Вне задачи — ничего не делает.
func Alive(ctx context.Context, note string) { Expect(ctx, 0, note) }

// Expect — впереди шаг, который может молчать до d (вызов helper с
// известным пределом): следующий признак ожидается до max(d, окно).
func Expect(ctx context.Context, d time.Duration, note string) {
	if r, ok := ctx.Value(aliveKey{}).(aliveRef); ok {
		r.m.beat(r.id, d, note)
	}
}

func (m *Manager) beat(id string, d time.Duration, note string) {
	now := m.now()
	m.mu.Lock()
	a, ok := m.alive[id]
	if !ok || a.stalled {
		m.mu.Unlock()
		return
	}
	a.until, a.last = now.Add(max(d, m.stall)), now
	if note != "" {
		a.note = note
	}
	save := a.note != a.savedFor || now.Sub(a.saved) >= time.Second
	if save {
		a.saved, a.savedFor = now, a.note
	}
	note = a.note
	m.mu.Unlock()
	if save {
		if _, err := m.db.ExecContext(context.Background(),
			"UPDATE tasks SET alive_at = ?, alive_note = ? WHERE id = ? AND state = 'running'",
			now.UnixMilli(), note, id); err != nil {
			m.logger.Error("задача: признак жизни", "task", id, "err", err)
		}
	}
}

// stalledError — причина отмены сторожем.
type stalledError struct{ msg string }

func (e stalledError) Error() string { return e.msg }

// watchdog — сторож задач до остановки менеджера.
func (m *Manager) watchdog() {
	t := time.NewTicker(m.poll)
	defer t.Stop()
	for {
		select {
		case <-m.base.Done():
			return
		case <-t.C:
		}
		m.checkAlive()
	}
}

// checkAlive — одна проверка сторожа: просроченные задачи отменяются,
// не завершившиеся после отмены за второе окно — failed принудительно.
func (m *Manager) checkAlive() {
	now := m.now()
	type act struct {
		id     string
		cancel context.CancelCauseFunc
		err    error
		force  bool
	}
	var acts []act
	m.mu.Lock()
	for id, a := range m.alive {
		switch {
		case !a.stalled && now.After(a.until):
			why := fmt.Sprintf("нет признаков жизни %s (последнее: %s, %s)", now.Sub(a.last).Round(time.Second), noteOr(a.note), a.last.UTC().Format(time.RFC3339))
			a.stalled, a.forceAt = true, now.Add(m.stall)
			acts = append(acts, act{id: id, cancel: m.active[id], err: stalledError{why}})
		case a.stalled && now.After(a.forceAt):
			delete(m.alive, id)
			acts = append(acts, act{id: id, force: true, err: stalledError{fmt.Sprintf(
				"задача не завершилась за %s после отмены сторожем (последнее: %s) — состояние операции не определено, проверьте перед повтором", m.stall, noteOr(a.note))}})
		}
	}
	m.mu.Unlock()
	for _, a := range acts {
		if a.force {
			m.logger.Error("задача не завершилась после отмены сторожем", "task", a.id)
			if err := m.transition(context.Background(), a.id, Running, Failed, &Error{Code: CodeStalled, Message: a.err.Error()}, nil); err != nil {
				m.logger.Error("сторож задач", "task", a.id, "err", err)
			}
			continue
		}
		m.logger.Warn("задача без признаков жизни — отмена", "task", a.id, "reason", a.err)
		if a.cancel != nil {
			a.cancel(a.err)
		}
	}
}

func noteOr(s string) string {
	if s == "" {
		return "нет"
	}
	return s
}

// Register добавляет вид задачи. Вызывать до Submit.
func (m *Manager) Register(kind string, fn RunFunc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if kind == "" || fn == nil {
		return errors.New("пустой вид задачи или функция")
	}
	if _, dup := m.kinds[kind]; dup {
		return fmt.Errorf("вид задачи %q уже зарегистрирован", kind)
	}
	m.kinds[kind] = fn
	return nil
}

// Kinds — зарегистрированные виды по алфавиту.
func (m *Manager) Kinds() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.kinds))
	for k := range m.kinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Recover переводит задачи queued/running, оставшиеся от прошлого запуска,
// в interrupted. Вызывать при старте до Submit. Возвращает число задач.
func (m *Manager) Recover(ctx context.Context) (int, error) {
	rows, err := m.db.QueryContext(ctx, "SELECT id, state, alive_at, alive_note FROM tasks WHERE state IN ('queued','running')")
	if err != nil {
		return 0, err
	}
	type left struct {
		id    string
		state State
		at    sql.NullInt64
		note  sql.NullString
	}
	var found []left
	for rows.Next() {
		var l left
		if err := rows.Scan(&l.id, &l.state, &l.at, &l.note); err != nil {
			rows.Close()
			return 0, err
		}
		found = append(found, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, l := range found {
		msg := "демон перезапущен во время выполнения; операция не возобновлялась — проверьте состояние перед повтором"
		if l.state == Queued {
			msg = "демон перезапущен до запуска задачи; задача не выполнялась"
		} else if l.at.Valid {
			msg += fmt.Sprintf("; последний признак жизни %s: %s",
				time.UnixMilli(l.at.Int64).UTC().Format(time.RFC3339), noteOr(l.note.String))
		}
		if err := m.transition(ctx, l.id, l.state, Interrupted, &Error{Code: CodeInterrupted, Message: msg}, nil); err != nil {
			return 0, err
		}
		m.logger.Warn("задача прервана перезапуском", "task", l.id, "was", l.state)
	}
	return len(found), nil
}

// Submit создаёт задачу в состоянии queued и запускает её, когда освободится слот.
func (m *Manager) Submit(ctx context.Context, kind string, params json.RawMessage) (Task, error) {
	if len(params) == 0 {
		params = json.RawMessage("null")
	}
	if !json.Valid(params) {
		return Task{}, ErrBadParams
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Task{}, ErrClosed
	}
	fn, ok := m.kinds[kind]
	if !ok {
		return Task{}, fmt.Errorf("%w: %s", ErrUnknownKind, kind)
	}
	id, err := newID()
	if err != nil {
		return Task{}, err
	}
	now := m.now().UnixMilli()
	if _, err := m.db.ExecContext(ctx,
		"INSERT INTO tasks(id, kind, state, progress, params, created_at, updated_at) VALUES (?, ?, 'queued', 0, ?, ?, ?)",
		id, kind, string(params), now, now); err != nil {
		return Task{}, err
	}
	t, err := m.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	m.emitState(t)

	tctx, cancel := context.WithCancelCause(m.base)
	m.active[id] = cancel
	m.wg.Add(1)
	go m.run(tctx, id, fn, params)
	return t, nil
}

// Cancel отменяет queued/running задачу.
func (m *Manager) Cancel(ctx context.Context, id string) error {
	m.mu.Lock()
	cancel, ok := m.active[id]
	m.mu.Unlock()
	if ok {
		cancel(errCancelled)
		return nil
	}
	if _, err := m.Get(ctx, id); err != nil {
		return err
	}
	return ErrNotActive
}

// Shutdown прекращает приём задач, отменяет выполняющиеся (они станут
// interrupted) и ждёт их завершения не дольше ctx.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.stop(errShutdown)
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) run(ctx context.Context, id string, fn RunFunc, params json.RawMessage) {
	defer m.wg.Done()
	defer func() {
		m.mu.Lock()
		if c, ok := m.active[id]; ok {
			c(nil)
			delete(m.active, id)
		}
		m.mu.Unlock()
	}()

	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		m.finishCancelled(id, Queued, context.Cause(ctx))
		return
	}
	defer func() { <-m.sem }()

	m.mu.Lock()
	m.alive[id] = &aliveState{until: m.now().Add(m.stall), last: m.now(), note: "запущена"}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.alive, id)
		m.mu.Unlock()
	}()
	if err := m.transition(context.Background(), id, Queued, Running, nil, nil); err != nil {
		m.logger.Error("задача: не удалось перейти в running", "task", id, "err", err)
		return
	}
	ctx = context.WithValue(ctx, aliveKey{}, aliveRef{m, id})
	progress := func(p int) {
		m.beat(id, 0, "")
		p = min(max(p, 0), 100)
		res, err := m.db.ExecContext(context.Background(),
			"UPDATE tasks SET progress = ?, updated_at = ? WHERE id = ? AND state = 'running'",
			p, m.now().UnixMilli(), id)
		if err != nil {
			m.logger.Error("задача: прогресс", "task", id, "err", err)
			return
		}
		if n, _ := res.RowsAffected(); n == 1 {
			data, _ := json.Marshal(map[string]int{"progress": p})
			m.log.Emit(context.Background(), events.Event{Type: EventProgress, TaskID: id, Data: data}, false)
		}
	}

	result, err := safeRun(ctx, fn, params, progress)
	if err == nil {
		data, merr := json.Marshal(result)
		if merr != nil {
			m.transition(context.Background(), id, Running, Failed,
				&Error{Code: CodeFailed, Message: "результат не сериализуется: " + merr.Error()}, nil)
			return
		}
		m.transition(context.Background(), id, Running, Succeeded, nil, data)
		return
	}
	if ctx.Err() != nil {
		m.finishCancelled(id, Running, context.Cause(ctx))
		return
	}
	var pe panicError
	code := CodeFailed
	if errors.As(err, &pe) {
		code = CodePanic
	}
	m.transition(context.Background(), id, Running, Failed, &Error{Code: code, Message: err.Error()}, nil)
}

func (m *Manager) finishCancelled(id string, from State, cause error) {
	var se stalledError
	if errors.As(cause, &se) {
		m.transition(context.Background(), id, from, Failed, &Error{Code: CodeStalled, Message: se.Error()}, nil)
		return
	}
	if errors.Is(cause, errShutdown) {
		m.transition(context.Background(), id, from, Interrupted, &Error{Code: CodeInterrupted, Message: cause.Error()}, nil)
		return
	}
	m.transition(context.Background(), id, from, Failed, &Error{Code: CodeCancelled, Message: errCancelled.Error()}, nil)
}

type panicError struct{ v any }

func (p panicError) Error() string { return fmt.Sprintf("паника в задаче: %v", p.v) }

func safeRun(ctx context.Context, fn RunFunc, params json.RawMessage, progress func(int)) (res any, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = panicError{v}
		}
	}()
	return fn(ctx, params, progress)
}

// transition меняет состояние, только если задача в состоянии from
// (сравнение с заменой), и журналирует событие.
func (m *Manager) transition(ctx context.Context, id string, from, to State, terr *Error, result json.RawMessage) error {
	now := m.now().UnixMilli()
	var code, msg, res any
	if terr != nil {
		code, msg = terr.Code, terr.Message
	}
	if result != nil {
		res = string(result)
	}
	q := "UPDATE tasks SET state = ?, updated_at = ?, error_code = ?, error_msg = ?, result = ?"
	args := []any{to, now, code, msg, res}
	switch to {
	case Running:
		q += ", started_at = ?"
		args = append(args, now)
	case Succeeded:
		q += ", progress = 100, finished_at = ?"
		args = append(args, now)
	default:
		q += ", finished_at = ?"
		args = append(args, now)
	}
	q += " WHERE id = ? AND state = ?"
	args = append(args, id, from)
	r, err := m.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return fmt.Errorf("задача %s: ожидалось состояние %s", id, from)
	}
	t, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	m.emitState(t)
	return nil
}

func (m *Manager) emitState(t Task) {
	data, _ := json.Marshal(t)
	if _, err := m.log.Emit(context.Background(), events.Event{Type: EventState, TaskID: t.ID, Data: data}, true); err != nil {
		m.logger.Error("журнал событий", "task", t.ID, "err", err)
	}
}

const taskCols = "id, kind, state, progress, params, result, error_code, error_msg, created_at, updated_at, started_at, finished_at, alive_at, alive_note"

type scanner interface{ Scan(...any) error }

func scanTask(s scanner) (Task, error) {
	var t Task
	var params string
	var result, code, msg sql.NullString
	var created, updated int64
	var started, finished, aliveAt sql.NullInt64
	var aliveNote sql.NullString
	if err := s.Scan(&t.ID, &t.Kind, &t.State, &t.Progress, &params, &result, &code, &msg,
		&created, &updated, &started, &finished, &aliveAt, &aliveNote); err != nil {
		return Task{}, err
	}
	t.Params = json.RawMessage(params)
	if result.Valid {
		t.Result = json.RawMessage(result.String)
	}
	if code.Valid {
		t.Error = &Error{Code: code.String, Message: msg.String}
	}
	t.CreatedAt = time.UnixMilli(created).UTC()
	t.UpdatedAt = time.UnixMilli(updated).UTC()
	if started.Valid {
		v := time.UnixMilli(started.Int64).UTC()
		t.StartedAt = &v
	}
	if finished.Valid {
		v := time.UnixMilli(finished.Int64).UTC()
		t.FinishedAt = &v
	}
	if aliveAt.Valid {
		v := time.UnixMilli(aliveAt.Int64).UTC()
		t.AliveAt = &v
	}
	t.AliveNote = aliveNote.String
	return t, nil
}

// Get возвращает задачу по ID.
func (m *Manager) Get(ctx context.Context, id string) (Task, error) {
	t, err := scanTask(m.db.QueryRowContext(ctx, "SELECT "+taskCols+" FROM tasks WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	return t, err
}

// List возвращает последние задачи, новые первыми.
func (m *Manager) List(ctx context.Context, limit int) ([]Task, error) {
	rows, err := m.db.QueryContext(ctx,
		"SELECT "+taskCols+" FROM tasks ORDER BY created_at DESC, rowid DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
