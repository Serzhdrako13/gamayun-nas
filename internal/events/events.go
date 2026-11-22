// Package events — журнал событий в SQLite и шина для живых подписчиков (SSE).
//
// Log.Emit записывает событие в журнал и рассылает подписчикам под одной
// блокировкой, поэтому подписчики получают события в порядке возрастания ID.
// Эфемерные события (Ephemeral, например прогресс) не журналируются и имеют
// ID 0. Подписчик, не успевающий читать, отключается (канал закрывается):
// клиент переподключается с Last-Event-ID и догоняет по журналу.
//
// Журнал задач и событий оборудования: у события есть
// уровень важности (Severity) и предмет (Subject — диск, массив или пул).
package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// Уровни важности.
const (
	SevInfo     = "info"
	SevWarning  = "warning"
	SevCritical = "critical"
)

// Типы событий оборудования. Subject: "disk:<id>", "array:<имя>", "pool:<имя>".
const (
	DiskAdded       = "disk.added"       // новый диск появился в системе
	DiskRemoved     = "disk.removed"     // диск исчез (извлечён или отказал на шине)
	DiskReturned    = "disk.returned"    // ранее известный диск снова виден
	DiskFailed      = "disk.failed"      // участник массива помечен сбойным
	ArrayDegraded   = "array.degraded"   // массив потерял избыточность
	ArrayRecovering = "array.recovering" // идёт восстановление/синхронизация
	ArrayRecovered  = "array.recovered"  // избыточность восстановлена
	IntegrityErrors = "integrity.errors" // выросли счётчики ошибок (btrfs/dm-integrity)
	// ArrayReaddFailed — вернувшийся диск не удалось вернуть в массив ни
	// re-add, ни полным add: нужен администратор.
	ArrayReaddFailed = "array.readd_failed"
	// PoolEmergencyRO — Btrfs пула аварийно перешёл в read-only после ошибки
	// записи; PoolEmergencyROCleared — пул снова записываемый.
	PoolEmergencyRO        = "pool.emergency_ro"
	PoolEmergencyROCleared = "pool.emergency_ro_cleared"
	// PoolProtectionBypassed — запас избыточности пула исчерпан, а защита
	// read-only выключена администратором: пул остаётся на запись,
	// отказ ещё одного диска приведёт к потере данных. Cleared — запас
	// восстановлен или защита снова включена.
	PoolProtectionBypassed        = "pool.protection_bypassed"
	PoolProtectionBypassedCleared = "pool.protection_bypassed_cleared"
)

// Event — запись журнала / сообщение шины.
type Event struct {
	ID       int64           `json:"id,omitempty"` // 0 — эфемерное, не в журнале
	Type     string          `json:"type"`
	Time     time.Time       `json:"time"`
	Severity string          `json:"severity"` // пусто при Emit → info
	Subject  string          `json:"subject,omitempty"`
	TaskID   string          `json:"task_id,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// Log — журнал + шина.
type Log struct {
	db  *sql.DB
	mu  sync.Mutex // порядок Emit; защищает subs
	sub map[*Subscription]struct{}
	now func() time.Time
}

// NewLog создаёт журнал поверх открытой базы (store.Open).
func NewLog(db *sql.DB) *Log {
	return &Log{db: db, sub: map[*Subscription]struct{}{}, now: time.Now}
}

// Subscription — живой подписчик. C закрывается при Close или переполнении.
type Subscription struct {
	C      <-chan Event
	ch     chan Event
	log    *Log
	closed bool
}

// Subscribe регистрирует подписчика с буфером buf событий.
func (l *Log) Subscribe(buf int) *Subscription {
	if buf < 1 {
		buf = 1
	}
	ch := make(chan Event, buf)
	s := &Subscription{C: ch, ch: ch, log: l}
	l.mu.Lock()
	l.sub[s] = struct{}{}
	l.mu.Unlock()
	return s
}

// Close отписывает; повторный вызов безопасен.
func (s *Subscription) Close() {
	s.log.mu.Lock()
	defer s.log.mu.Unlock()
	s.closeLocked()
}

func (s *Subscription) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	delete(s.log.sub, s)
	close(s.ch)
}

// Emit журналирует событие (если persist) и рассылает подписчикам.
// Возвращает событие с присвоенными ID и временем.
func (l *Log) Emit(ctx context.Context, ev Event, persist bool) (Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ev.Time.IsZero() {
		ev.Time = l.now().UTC()
	}
	if len(ev.Data) == 0 {
		ev.Data = json.RawMessage("null")
	}
	if ev.Severity == "" {
		ev.Severity = SevInfo
	}
	ev.ID = 0
	if persist {
		res, err := l.db.ExecContext(ctx,
			"INSERT INTO events(ts, type, severity, subject, task_id, data) VALUES (?, ?, ?, ?, ?, ?)",
			ev.Time.UnixMilli(), ev.Type, ev.Severity, nullable(ev.Subject), nullable(ev.TaskID), string(ev.Data))
		if err != nil {
			return ev, err
		}
		if ev.ID, err = res.LastInsertId(); err != nil {
			return ev, err
		}
	}
	for s := range l.sub {
		select {
		case s.ch <- ev:
		default:
			s.closeLocked() // отстающий подписчик догонит по журналу
		}
	}
	return ev, nil
}

// Since возвращает журнальные события с ID > after по возрастанию (не более limit).
func (l *Log) Since(ctx context.Context, after int64, limit int) ([]Event, error) {
	rows, err := l.db.QueryContext(ctx,
		"SELECT "+cols+" FROM events WHERE id > ? ORDER BY id LIMIT ?",
		after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scan(rows, nil)
}

// Recent — последние limit событий журнала, новые первыми (виджет «События»).
func (l *Log) Recent(ctx context.Context, limit int) ([]Event, error) {
	return l.Query(ctx, Filter{Limit: limit})
}

// Filter — выборка журнала, новые первыми. Пустые поля не фильтруют.
type Filter struct {
	Limit    int
	Before   int64  // только ID < Before (страницы)
	Type     string // точный тип или префикс с точкой на конце ("disk.")
	Severity string // не ниже: warning → warning и critical
	Subject  string // точный предмет ("disk:<id>") или префикс с двоеточием ("disk:")
}

var sevRank = map[string]int{SevInfo: 0, SevWarning: 1, SevCritical: 2}

// ValidSeverity — известный уровень важности.
func ValidSeverity(s string) bool { _, ok := sevRank[s]; return ok }

// Query — события по фильтру.
func (l *Log) Query(ctx context.Context, f Filter) ([]Event, error) {
	q := "SELECT " + cols + " FROM events WHERE 1=1"
	var args []any
	if f.Before > 0 {
		q += " AND id < ?"
		args = append(args, f.Before)
	}
	like := func(col, v, sep string) {
		if strings.HasSuffix(v, sep) {
			q += " AND substr(" + col + ", 1, ?) = ?"
			args = append(args, len(v), v)
		} else {
			q += " AND " + col + " = ?"
			args = append(args, v)
		}
	}
	if f.Type != "" {
		like("type", f.Type, ".")
	}
	if f.Subject != "" {
		like("subject", f.Subject, ":")
	}
	if r := sevRank[f.Severity]; r > 0 {
		var in []string
		for s, x := range sevRank {
			if x >= r {
				in = append(in, "'"+s+"'")
			}
		}
		q += " AND severity IN (" + strings.Join(in, ",") + ")"
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, f.Limit)
	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scan(rows, []Event{})
}

// Prune оставляет в журнале не более keep последних событий (по числу
// строк, а не по диапазону ID: после обрезки по сроку в ID есть пропуски).
func (l *Log) Prune(ctx context.Context, keep int) (int64, error) {
	res, err := l.db.ExecContext(ctx,
		"DELETE FROM events WHERE id <= (SELECT id FROM events ORDER BY id DESC LIMIT 1 OFFSET ?)", keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const cols = "id, ts, type, severity, COALESCE(subject, ''), COALESCE(task_id, ''), data"

func scan(rows *sql.Rows, out []Event) ([]Event, error) {
	for rows.Next() {
		var ev Event
		var ts int64
		var data string
		if err := rows.Scan(&ev.ID, &ts, &ev.Type, &ev.Severity, &ev.Subject, &ev.TaskID, &data); err != nil {
			return nil, err
		}
		ev.Time = time.UnixMilli(ts).UTC()
		ev.Data = json.RawMessage(data)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// LastPerSubject — последнее событие каждого предмета с префиксом ("disk:").
func (l *Log) LastPerSubject(ctx context.Context, prefix string) (map[string]Event, error) {
	rows, err := l.db.QueryContext(ctx, "SELECT "+cols+" FROM events WHERE id IN "+
		"(SELECT MAX(id) FROM events WHERE substr(subject, 1, ?) = ? GROUP BY subject)", len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scan(rows, nil)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Event, len(list))
	for _, e := range list {
		out[e.Subject] = e
	}
	return out, nil
}

// LastBySubject — последнее журнальное событие по предмету (ok=false, если нет).
func (l *Log) LastBySubject(ctx context.Context, subject string) (Event, bool, error) {
	rows, err := l.db.QueryContext(ctx,
		"SELECT "+cols+" FROM events WHERE subject = ? ORDER BY id DESC LIMIT 1", subject)
	if err != nil {
		return Event{}, false, err
	}
	defer rows.Close()
	out, err := scan(rows, nil)
	if err != nil || len(out) == 0 {
		return Event{}, false, err
	}
	return out[0], true, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
