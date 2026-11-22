package notify

// Тишина (временное отключение внешних каналов на время работ) и
// группировка серий одинаковых событий.
//
// Тишина — запись settings 'notify.quiet'. Пока она действует, события
// пишутся в журнал и центр как обычно, но строки outbox (SMTP, webhook) не
// создаются — кроме critical, если не задан явный флаг mute_critical;
// подавленные считаются в той же транзакции, что и запись в центр.
// Окончание (срок истёк или отмена) — событие журнала notify.quiet_ended:
// запись тишины удаляется при его обработке отбором (Process), поэтому все
// события, записанные в журнал раньше, уже посчитаны, а более поздние —
// нет. Уведомление об окончании попадает в центр и, если что-то было
// подавлено, во внешние каналы — это и есть сводка «за время тишины: N».
// Повторное событие окончания (сбой между отметкой и записью в журнал)
// безвредно: запись с другим ID не трогается, дубль в центр не попадает.
//
// Группировка: warning/info с одинаковыми типом и предметом в окне
// GroupWindow от первого отправленного — первое уходит сразу, остальные
// только считаются (в центре — все); по окончании окна одна строка outbox
// на последнее уведомление серии с числом повторов. critical и события
// тишины не группируются.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gamayun-nas/internal/events"
)

// Типы событий тишины.
const (
	TypeQuietStarted = "notify.quiet_started"
	TypeQuietEnded   = "notify.quiet_ended"
)

const quietKey = "notify.quiet"

// Пределы тишины.
const (
	MaxQuiet       = 7 * 24 * time.Hour // дольше — забытая тишина
	MaxQuietReason = 200                // символов
	// quietRetry — повторная запись события окончания, если запись тишины
	// осталась после отметки «завершается» (сбой до записи в журнал).
	quietRetry = time.Minute
)

// Ошибки тишины (API → 400/409).
var (
	ErrQuietInvalid = errors.New("неверные параметры тишины")
	ErrQuietEnding  = errors.New("тишина завершается, повторите через несколько секунд")
)

// Quiet — действующая тишина.
type Quiet struct {
	ID                 string     `json:"id"`
	Started            time.Time  `json:"started"`
	Until              time.Time  `json:"until"`
	Reason             string     `json:"reason"`
	MuteCritical       bool       `json:"mute_critical"`
	By                 string     `json:"by,omitempty"`
	Suppressed         int        `json:"suppressed"`          // не отправлено во внешние каналы
	SuppressedCritical int        `json:"suppressed_critical"` // из них critical (только при mute_critical)
	Ending             bool       `json:"ending,omitempty"`    // окончание записано в журнал, ждёт отбора
	EndingAt           *time.Time `json:"ending_at,omitempty"`
	EndReason          string     `json:"end_reason,omitempty"`
}

// covers — событие внешнего уровня подавляется этой тишиной.
func (q *Quiet) covers(ev events.Event) bool {
	if q == nil || !ev.Time.Before(q.Until) {
		return false
	}
	return ev.Severity != events.SevCritical || q.MuteCritical
}

type queryer interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

func loadQuiet(ctx context.Context, db queryer) (*Quiet, error) {
	var v string
	err := db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", quietKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var q Quiet
	if err := json.Unmarshal([]byte(v), &q); err != nil || q.ID == "" {
		// Испорченная запись не должна глушить уведомления навсегда.
		return nil, nil
	}
	return &q, nil
}

func saveQuiet(ctx context.Context, db queryer, q *Quiet) error {
	v, err := json.Marshal(q)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		"INSERT INTO settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		quietKey, string(v))
	return err
}

// Quiet — действующая тишина (nil — нет).
func (s *Service) Quiet(ctx context.Context) (*Quiet, error) {
	return loadQuiet(ctx, s.db)
}

// SetQuiet включает тишину до until или меняет действующую (срок, причина,
// флаг critical; счётчики и начало сохраняются). Событие
// notify.quiet_started пишется в журнал.
func (s *Service) SetQuiet(ctx context.Context, until time.Time, reason string, muteCritical bool, by string) (*Quiet, error) {
	now := s.opt.Now()
	reason = strings.Join(strings.Fields(reason), " ")
	switch {
	case reason == "":
		return nil, fmt.Errorf("%w: %s", ErrQuietInvalid, "укажите причину")
	case utf8.RuneCountInString(reason) > MaxQuietReason:
		return nil, fmt.Errorf("%w: %s", ErrQuietInvalid, "причина длиннее 200 символов")
	case !until.After(now):
		return nil, fmt.Errorf("%w: %s", ErrQuietInvalid, "срок уже прошёл")
	case until.Sub(now) > MaxQuiet:
		return nil, fmt.Errorf("%w: %s", ErrQuietInvalid, "тишина не дольше 7 суток")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q, err := loadQuiet(ctx, tx)
	if err != nil {
		return nil, err
	}
	changed := q != nil
	if q != nil && q.Ending {
		return nil, ErrQuietEnding
	}
	if q == nil {
		q = &Quiet{ID: strconv.FormatInt(now.UnixNano(), 36), Started: now.UTC()}
	}
	q.Until, q.Reason, q.MuteCritical, q.By = until.UTC(), reason, muteCritical, by
	if err := saveQuiet(ctx, tx, q); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.emitQuiet(ctx, TypeQuietStarted, map[string]any{"id": q.ID, "until": q.Until, "reason": q.Reason,
		"mute_critical": q.MuteCritical, "user": by, "changed": changed})
	return q, nil
}

// EndQuiet завершает тишину досрочно (false — тишины нет).
func (s *Service) EndQuiet(ctx context.Context, reason, by string) (bool, error) {
	return s.endQuiet(ctx, reason, by, false)
}

// endQuiet отмечает тишину «завершается» и пишет событие окончания;
// expired — только если срок истёк (таймер).
func (s *Service) endQuiet(ctx context.Context, reason, by string, expired bool) (bool, error) {
	now := s.opt.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	q, err := loadQuiet(ctx, tx)
	if err != nil || q == nil {
		return false, err
	}
	if q.Ending {
		// Окончание уже записано; повтор — только если запись тишины
		// подзадержалась (сбой между отметкой и журналом).
		if q.EndingAt != nil && now.Sub(*q.EndingAt) < quietRetry {
			return true, nil
		}
	} else {
		if expired && now.Before(q.Until) {
			return false, nil
		}
		q.Ending, q.EndReason = true, reason
	}
	t := now.UTC()
	q.EndingAt = &t
	if err := saveQuiet(ctx, tx, q); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.emitQuiet(ctx, TypeQuietEnded, map[string]any{"id": q.ID, "started": q.Started, "until": q.Until,
		"reason": q.Reason, "end_reason": q.EndReason, "user": by})
	return true, nil
}

func (s *Service) emitQuiet(ctx context.Context, typ string, data map[string]any) {
	b, _ := json.Marshal(data)
	if _, err := s.log.Emit(ctx, events.Event{Type: typ, Severity: events.SevInfo, Data: b}, true); err != nil {
		s.opt.Logger.Error("уведомления: событие тишины не записано", "type", typ, "err", err)
	}
}

// processQuietEnded — обработка события окончания в транзакции отбора:
// удаляет запись тишины и возвращает данные для центра (с числом
// подавленных); ok=false — дубль или чужая тишина (в центр не попадает).
func processQuietEnded(ctx context.Context, tx *sql.Tx, ev events.Event) (data []byte, suppressed int, ok bool, err error) {
	var d map[string]any
	if json.Unmarshal(ev.Data, &d) != nil || d == nil {
		return nil, 0, false, nil
	}
	q, err := loadQuiet(ctx, tx)
	if err != nil || q == nil || d["id"] != q.ID {
		return nil, 0, false, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", quietKey); err != nil {
		return nil, 0, false, err
	}
	d["suppressed"], d["suppressed_critical"], d["mute_critical"] = q.Suppressed, q.SuppressedCritical, q.MuteCritical
	data, err = json.Marshal(d)
	return data, q.Suppressed, true, err
}

// QuietTick — окончание тишины по сроку и повтор записи окончания после
// сбоя; вызывается периодически (Run).
func (s *Service) QuietTick(ctx context.Context) error {
	_, err := s.endQuiet(ctx, "срок истёк", "", true)
	return err
}

// groupKey — ключ серии: тип и предмет.
func groupKey(ev events.Event) string { return ev.Type + "\x00" + ev.Subject }

// groupable — серии собираются только для warning/info не служебных событий.
func groupable(ev events.Event) bool {
	return ev.Severity != events.SevCritical && !strings.HasPrefix(ev.Type, "notify.")
}

type group struct {
	key       string
	windowEnd int64
	repeats   int
	firstTS   sql.NullInt64
	lastNotif sql.NullInt64
}

// flushGroup ставит в outbox сводку серии (если были повторы) и удаляет
// окно. В тишине сводка не уходит — повторы добавляются к подавленным.
func (s *Service) flushGroup(ctx context.Context, tx *sql.Tx, g group, now time.Time) (queued bool, err error) {
	if _, err := tx.ExecContext(ctx, "DELETE FROM notify_groups WHERE key = ?", g.key); err != nil {
		return false, err
	}
	if g.repeats == 0 || !g.lastNotif.Valid || s.opt.Channels == nil {
		return false, nil
	}
	q, err := loadQuiet(ctx, tx)
	if err != nil {
		return false, err
	}
	if q != nil && now.Before(q.Until) {
		q.Suppressed += g.repeats
		return false, saveQuiet(ctx, tx, q)
	}
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM notifications WHERE id = ?", g.lastNotif.Int64).Scan(&exists); err != nil || exists == 0 {
		return false, err // уведомление уже удалено обрезкой
	}
	for _, ch := range s.opt.Channels() {
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO outbox(notif_id, channel, next_at, repeats, repeats_since) VALUES (?, ?, ?, ?, ?)",
			g.lastNotif.Int64, ch, now.UnixMilli(), g.repeats, g.firstTS); err != nil {
			return false, err
		}
		queued = true
	}
	return queued, nil
}

// FlushGroups — сводки по сериям, окно которых закончилось к now.
func (s *Service) FlushGroups(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx,
		"SELECT key, window_end, repeats, first_ts, last_notif FROM notify_groups WHERE window_end <= ?", now.UnixMilli())
	if err != nil {
		return err
	}
	var list []group
	for rows.Next() {
		var g group
		if err := rows.Scan(&g.key, &g.windowEnd, &g.repeats, &g.firstTS, &g.lastNotif); err != nil {
			rows.Close()
			return err
		}
		list = append(list, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	queued := false
	for _, g := range list {
		q, err := s.flushGroup(ctx, tx, g, now)
		if err != nil {
			return err
		}
		queued = queued || q
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if queued && s.opt.OnQueued != nil {
		s.opt.OnQueued()
	}
	return nil
}

// outboxFor — постановка уведомления во внешние каналы с учётом тишины и
// серий (в транзакции отбора). Возвращает, поставлено ли что-то в очередь.
func (s *Service) outboxFor(ctx context.Context, tx *sql.Tx, ev events.Event, nid int64, force bool) (bool, error) {
	if s.opt.Channels == nil {
		return false, nil
	}
	if !force {
		q, err := loadQuiet(ctx, tx)
		if err != nil {
			return false, err
		}
		if q.covers(ev) {
			q.Suppressed++
			if ev.Severity == events.SevCritical {
				q.SuppressedCritical++
			}
			return false, saveQuiet(ctx, tx, q)
		}
	}
	queued := false
	if !force && groupable(ev) && s.opt.GroupWindow > 0 {
		g := group{key: groupKey(ev)}
		err := tx.QueryRowContext(ctx, "SELECT window_end, repeats, first_ts, last_notif FROM notify_groups WHERE key = ?", g.key).
			Scan(&g.windowEnd, &g.repeats, &g.firstTS, &g.lastNotif)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return false, err
		case ev.Time.UnixMilli() < g.windowEnd:
			// Повтор в окне — только счётчик.
			_, err := tx.ExecContext(ctx, `UPDATE notify_groups SET repeats = repeats + 1,
				first_ts = COALESCE(first_ts, ?), last_notif = ? WHERE key = ?`, ev.Time.UnixMilli(), nid, g.key)
			return false, err
		default:
			// Окно прошло, но сводка ещё не ушла (догон после простоя).
			if queued, err = s.flushGroup(ctx, tx, g, s.opt.Now()); err != nil {
				return false, err
			}
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO notify_groups(key, window_end) VALUES (?, ?)",
			g.key, ev.Time.Add(s.opt.GroupWindow).UnixMilli()); err != nil {
			return false, err
		}
	}
	now := s.opt.Now().UnixMilli()
	for _, ch := range s.opt.Channels() {
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO outbox(notif_id, channel, next_at) VALUES (?, ?, ?)", nid, ch, now); err != nil {
			return false, err
		}
		queued = true
	}
	return queued, nil
}
