// Package notify — уведомления: отбор событий журнала в центр
// уведомлений и постановка в очередь внешней доставки (outbox) по каналам.
//
// Отбор идёт по курсору (settings 'notify.cursor' — ID последнего
// обработанного события). Запись уведомления, строк outbox и сдвиг курсора —
// одна транзакция, поэтому после перезапуска события, пропущенные за простой,
// подбираются из журнала без потерь и без дублей (UNIQUE event_id). Повторная
// отправка во внешний канал возможна (сбой между отправкой и отметкой) —
// гарантии «ровно один раз» нет.
package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gamayun-nas/internal/events"
)

// Каналы внешней доставки.
const (
	ChannelSMTP    = "smtp"
	ChannelWebhook = "webhook"
)

const cursorKey = "notify.cursor"

// Notification — запись центра уведомлений.
type Notification struct {
	ID       int64           `json:"id"`
	EventID  int64           `json:"event_id"`
	Time     time.Time       `json:"time"`
	Type     string          `json:"type"`
	Severity string          `json:"severity"`
	Subject  string          `json:"subject,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	ReadAt   *time.Time      `json:"read_at,omitempty"`
	// Сводка серии во внешнем канале — событие повторилось ещё
	// Repeats раз начиная с RepeatsSince (Notification — последнее).
	Repeats      int        `json:"repeats,omitempty"`
	RepeatsSince *time.Time `json:"repeats_since,omitempty"`
}

// Options — настройки отбора.
type Options struct {
	// Channels — включённые внешние каналы на момент отбора (nil — нет).
	Channels func() []string
	// MinExternal — минимальный уровень для внешних каналов (по умолчанию warning).
	MinExternal string
	// OnQueued — после фиксации новых строк outbox (Deliverer.Kick).
	OnQueued func()
	Logger   *slog.Logger
	Now      func() time.Time
	// GroupWindow — окно серии одинаковых warning/info: 0 — 5 мин,
	// отрицательное — без группировки.
	GroupWindow time.Duration
	// Tick — период проверки окончания тишины и окон серий (0 — 15 с).
	Tick time.Duration
}

// DefaultGroupWindow — окно серии по умолчанию.
const DefaultGroupWindow = 5 * time.Minute

// Service — отбор и центр уведомлений.
type Service struct {
	db  *sql.DB
	log *events.Log
	opt Options
}

// New создаёт сервис поверх открытой базы и журнала событий.
func New(db *sql.DB, log *events.Log, opt Options) *Service {
	if opt.MinExternal == "" {
		opt.MinExternal = events.SevWarning
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.GroupWindow == 0 {
		opt.GroupWindow = DefaultGroupWindow
	}
	if opt.Tick <= 0 {
		opt.Tick = 15 * time.Second
	}
	return &Service{db: db, log: log, opt: opt}
}

var sevRank = map[string]int{events.SevInfo: 0, events.SevWarning: 1, events.SevCritical: 2}

// hardware — события оборудования и итоги проверки пулов
// попадают в центр при любом уровне.
func hardware(t string) bool {
	for _, p := range []string{"disk.", "array.", "integrity.", "pool.scrub_", "pool.emergency_ro", "pool.protection_"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// ForCenter — событие попадает в центр: оборудование, тишина уведомлений
// или уровень ≥ warning.
func ForCenter(ev events.Event) bool {
	return ev.ID > 0 && (hardware(ev.Type) || strings.HasPrefix(ev.Type, "notify.quiet_") || sevRank[ev.Severity] >= 1)
}

func (s *Service) external(ev events.Event) bool {
	return sevRank[ev.Severity] >= sevRank[s.opt.MinExternal]
}

// Cursor — ID последнего обработанного события.
func (s *Service) Cursor(ctx context.Context) (int64, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", cursorKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// Init при первом запуске (курсора нет) ставит курсор на конец журнала:
// старые события до появления уведомлений не рассылаются.
func (s *Service) Init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT OR IGNORE INTO settings(key, value) SELECT ?, CAST(COALESCE(MAX(id), 0) AS TEXT) FROM events", cursorKey)
	return err
}

// Process обрабатывает одно журнальное событие; события с ID ≤ курсора
// пропускаются (идемпотентно).
func (s *Service) Process(ctx context.Context, ev events.Event) error {
	if ev.ID <= 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cur int64
	var v string
	if err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", cursorKey).Scan(&v); err == nil {
		cur, _ = strconv.ParseInt(v, 10, 64)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if ev.ID <= cur {
		return nil
	}
	queued := false
	center, external, force := ForCenter(ev), s.external(ev), false
	data := string(ev.Data)
	switch ev.Type {
	case TypeQuietEnded:
		// Окончание тишины — сводка во внешние каналы, если что-то
		// было подавлено; дубль окончания в центр не попадает.
		d, n, ok, err := processQuietEnded(ctx, tx, ev)
		if err != nil {
			return err
		}
		center, external, force = ok, ok && n > 0, true
		data = string(d)
	case TypeQuietStarted:
		external = false
	}
	if center {
		if data == "" {
			data = "null"
		}
		res, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO notifications(event_id, ts, type, severity, subject, data) VALUES (?, ?, ?, ?, ?, ?)",
			ev.ID, ev.Time.UnixMilli(), ev.Type, ev.Severity, nullable(ev.Subject), data)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 && external {
			nid, err := res.LastInsertId()
			if err != nil {
				return err
			}
			if queued, err = s.outboxFor(ctx, tx, ev, nid, force); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		cursorKey, strconv.FormatInt(ev.ID, 10)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if queued && s.opt.OnQueued != nil {
		s.opt.OnQueued()
	}
	return nil
}

// CatchUp обрабатывает журнал после курсора (пропущенное за простой или
// отставание подписчика).
func (s *Service) CatchUp(ctx context.Context) error {
	for {
		cur, err := s.Cursor(ctx)
		if err != nil {
			return err
		}
		list, err := s.log.Since(ctx, cur, 500)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return nil
		}
		for _, ev := range list {
			if err := s.Process(ctx, ev); err != nil {
				return err
			}
		}
	}
}

// Run — отбор до отмены ctx: подписка, догон по журналу, затем живые события.
// Отключённый (отстающий) подписчик переподписывается и догоняет журнал.
func (s *Service) Run(ctx context.Context) {
	if err := s.Init(ctx); err != nil {
		s.opt.Logger.Error("уведомления: курсор не создан", "err", err)
	}
	go s.housekeep(ctx)
	for ctx.Err() == nil {
		sub := s.log.Subscribe(256)
		if err := s.CatchUp(ctx); err != nil && ctx.Err() == nil {
			s.opt.Logger.Error("уведомления: догон журнала", "err", err)
		}
		s.consume(ctx, sub)
		sub.Close()
		if ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

// housekeep — окончание тишины по сроку и сводки серий.
func (s *Service) housekeep(ctx context.Context) {
	t := time.NewTicker(s.opt.Tick)
	defer t.Stop()
	for {
		if err := s.QuietTick(ctx); err != nil && ctx.Err() == nil {
			s.opt.Logger.Error("уведомления: окончание тишины", "err", err)
		}
		if err := s.FlushGroups(ctx, s.opt.Now()); err != nil && ctx.Err() == nil {
			s.opt.Logger.Error("уведомления: сводка серий", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) consume(ctx context.Context, sub *events.Subscription) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				return
			}
			if err := s.Process(ctx, ev); err != nil {
				if ctx.Err() == nil {
					s.opt.Logger.Error("уведомления: событие не обработано", "id", ev.ID, "err", err)
				}
				return // переподписка и догон по журналу
			}
		}
	}
}

// List — уведомления центра, новые первыми; unread — только непрочитанные.
func (s *Service) List(ctx context.Context, limit int, before int64, unread bool) ([]Notification, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := "SELECT id, event_id, ts, type, severity, COALESCE(subject, ''), data, read_at FROM notifications WHERE 1=1"
	var args []any
	if before > 0 {
		q += " AND id < ?"
		args = append(args, before)
	}
	if unread {
		q += " AND read_at IS NULL"
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var ts int64
		var data string
		var read sql.NullInt64
		if err := rows.Scan(&n.ID, &n.EventID, &ts, &n.Type, &n.Severity, &n.Subject, &data, &read); err != nil {
			return nil, err
		}
		n.Time = time.UnixMilli(ts).UTC()
		n.Data = json.RawMessage(data)
		if read.Valid {
			t := time.UnixMilli(read.Int64).UTC()
			n.ReadAt = &t
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Unread — число непрочитанных.
func (s *Service) Unread(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notifications WHERE read_at IS NULL").Scan(&n)
	return n, err
}

// MarkRead отмечает прочитанными уведомления с ID ≤ upTo (0 — все).
func (s *Service) MarkRead(ctx context.Context, upTo int64) (int64, error) {
	q := "UPDATE notifications SET read_at = ? WHERE read_at IS NULL"
	args := []any{s.opt.Now().UnixMilli()}
	if upTo > 0 {
		q += " AND id <= ?"
		args = append(args, upTo)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Prune оставляет в центре не более keep последних уведомлений
// (строки outbox удаляются каскадом).
func (s *Service) Prune(ctx context.Context, keep int) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM notifications WHERE id <= (SELECT id FROM notifications ORDER BY id DESC LIMIT 1 OFFSET ?)", keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneAge удаляет уведомления старше срока своего уровня (те же
// сроки, что у журнала событий), прочитанные и непрочитанные;
// строки outbox удаляются каскадом.
func (s *Service) PruneAge(ctx context.Context, r events.Retention, now time.Time) (int64, error) {
	cond, args := r.AgeCondition(now)
	res, err := s.db.ExecContext(ctx, "DELETE FROM notifications WHERE "+cond, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
