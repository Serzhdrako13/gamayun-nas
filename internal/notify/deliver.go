package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Состояния строки outbox.
const (
	StatePending = "pending" // ждёт попытки (next_at)
	StateSent    = "sent"    // принято сервером/получателем (не значит «прочитано»)
	StateFailed  = "failed"  // попытки исчерпаны по сроку MaxAge
	StateDropped = "dropped" // вытеснено ограничением очереди или канал отключён
)

// Sender — внешний канал. Ошибка — повод повторить позже.
type Sender interface {
	Send(ctx context.Context, n Notification) error
}

// DeliverOptions — параметры доставщика (нули → значения по умолчанию).
type DeliverOptions struct {
	MinBackoff  time.Duration // первая пауза после ошибки (30 с)
	MaxBackoff  time.Duration // потолок паузы (1 ч)
	MaxAge      time.Duration // после — failed (24 ч)
	MaxPending  int           // ограничение очереди на канал (1000), старые → dropped
	SendTimeout time.Duration // таймаут одной попытки (30 с)
	Now         func() time.Time
}

// Deliverer — доставка outbox по каналам.
type Deliverer struct {
	db   *sql.DB
	opt  DeliverOptions
	mu   sync.Mutex
	snd  map[string]Sender
	kick chan struct{}
}

// NewDeliverer создаёт доставщика без каналов (SetSender включает канал).
func NewDeliverer(db *sql.DB, opt DeliverOptions) *Deliverer {
	if opt.MinBackoff <= 0 {
		opt.MinBackoff = 30 * time.Second
	}
	if opt.MaxBackoff <= 0 {
		opt.MaxBackoff = time.Hour
	}
	if opt.MaxAge <= 0 {
		opt.MaxAge = 24 * time.Hour
	}
	if opt.MaxPending <= 0 {
		opt.MaxPending = 1000
	}
	if opt.SendTimeout <= 0 {
		opt.SendTimeout = 30 * time.Second
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Deliverer{db: db, opt: opt, snd: map[string]Sender{}, kick: make(chan struct{}, 1)}
}

// SetSender включает (s != nil) или отключает (nil) канал.
func (d *Deliverer) SetSender(channel string, s Sender) {
	d.mu.Lock()
	if s == nil {
		delete(d.snd, channel)
	} else {
		d.snd[channel] = s
	}
	d.mu.Unlock()
	d.Kick()
}

// Channels — включённые каналы (для Options.Channels отбора).
func (d *Deliverer) Channels() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.snd))
	for c := range d.snd {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func (d *Deliverer) sender(ch string) Sender {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snd[ch]
}

// Kick — внеочередной проход (новая строка outbox, включён канал).
func (d *Deliverer) Kick() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

func (d *Deliverer) backoff(attempts int) time.Duration {
	b := d.opt.MinBackoff
	for i := 1; i < attempts && b < d.opt.MaxBackoff; i++ {
		b *= 2
	}
	if b > d.opt.MaxBackoff {
		b = d.opt.MaxBackoff
	}
	return b
}

type job struct {
	id       int64
	channel  string
	attempts int
	n        Notification
}

// Step — один проход: ограничение очереди, затем все созревшие попытки.
// Возвращает время ближайшей следующей попытки (ноль — ждать нечего).
func (d *Deliverer) Step(ctx context.Context) (time.Time, error) {
	now := d.opt.Now()
	if err := d.trim(ctx); err != nil {
		return time.Time{}, err
	}
	rows, err := d.db.QueryContext(ctx, `SELECT o.id, o.channel, o.attempts, o.repeats, o.repeats_since,
		n.id, n.event_id, n.ts, n.type, n.severity, COALESCE(n.subject, ''), n.data
		FROM outbox o JOIN notifications n ON n.id = o.notif_id
		WHERE o.state = ? AND o.next_at <= ? ORDER BY o.id LIMIT 100`, StatePending, now.UnixMilli())
	if err != nil {
		return time.Time{}, err
	}
	var jobs []job
	for rows.Next() {
		var j job
		var ts int64
		var data string
		var since sql.NullInt64
		if err := rows.Scan(&j.id, &j.channel, &j.attempts, &j.n.Repeats, &since, &j.n.ID, &j.n.EventID, &ts,
			&j.n.Type, &j.n.Severity, &j.n.Subject, &data); err != nil {
			rows.Close()
			return time.Time{}, err
		}
		j.n.Time = time.UnixMilli(ts).UTC()
		j.n.Data = json.RawMessage(data)
		if since.Valid {
			t := time.UnixMilli(since.Int64).UTC()
			j.n.RepeatsSince = &t
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return time.Time{}, err
	}
	for _, j := range jobs {
		if err := d.attempt(ctx, j); err != nil {
			return time.Time{}, err
		}
	}
	var next sql.NullInt64
	if err := d.db.QueryRowContext(ctx, "SELECT MIN(next_at) FROM outbox WHERE state = ?", StatePending).Scan(&next); err != nil {
		return time.Time{}, err
	}
	if !next.Valid {
		return time.Time{}, nil
	}
	return time.UnixMilli(next.Int64), nil
}

func (d *Deliverer) attempt(ctx context.Context, j job) error {
	s := d.sender(j.channel)
	if s == nil {
		_, err := d.db.ExecContext(ctx, "UPDATE outbox SET state = ?, last_error = ? WHERE id = ?",
			StateDropped, "канал отключён", j.id)
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, d.opt.SendTimeout)
	serr := s.Send(sctx, j.n)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err() // остановка: попытку не засчитываем
	}
	now := d.opt.Now()
	if serr == nil {
		_, err := d.db.ExecContext(ctx,
			"UPDATE outbox SET state = ?, attempts = attempts + 1, sent_at = ?, last_error = NULL WHERE id = ?",
			StateSent, now.UnixMilli(), j.id)
		return err
	}
	attempts := j.attempts + 1
	state := StatePending
	if now.Sub(j.n.Time) >= d.opt.MaxAge {
		state = StateFailed
	}
	_, err := d.db.ExecContext(ctx,
		"UPDATE outbox SET state = ?, attempts = ?, next_at = ?, last_error = ? WHERE id = ?",
		state, attempts, now.Add(d.backoff(attempts)).UnixMilli(), errText(serr), j.id)
	return err
}

// trim — не более MaxPending ожидающих на канал; старые → dropped.
func (d *Deliverer) trim(ctx context.Context) error {
	_, err := d.db.ExecContext(ctx, `UPDATE outbox SET state = ?, last_error = 'очередь переполнена'
		WHERE state = ? AND id IN (
			SELECT id FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY channel ORDER BY id DESC) AS rn
				FROM outbox WHERE state = ?) WHERE rn > ?)`,
		StateDropped, StatePending, StatePending, d.opt.MaxPending)
	return err
}

// Run — доставка до отмены ctx; onErr — ошибки базы (не каналов).
func (d *Deliverer) Run(ctx context.Context, onErr func(error)) {
	for {
		next, err := d.Step(ctx)
		if ctx.Err() != nil {
			return
		}
		wait := time.Minute
		if err != nil {
			if onErr != nil {
				onErr(err)
			}
		} else if !next.IsZero() {
			if w := next.Sub(d.opt.Now()); w < wait {
				wait = max(w, 100*time.Millisecond)
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-d.kick:
		case <-t.C:
		}
		t.Stop()
	}
}

// ChannelStatus — состояние канала для API/веба.
type ChannelStatus struct {
	Channel     string     `json:"channel"`
	Enabled     bool       `json:"enabled"`
	Pending     int        `json:"pending"`
	Failed      int        `json:"failed"`
	Dropped     int        `json:"dropped"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

// Status — по каждому известному каналу (включённые и встречавшиеся в outbox).
func (d *Deliverer) Status(ctx context.Context) ([]ChannelStatus, error) {
	m := map[string]*ChannelStatus{}
	get := func(c string) *ChannelStatus {
		if m[c] == nil {
			m[c] = &ChannelStatus{Channel: c}
		}
		return m[c]
	}
	for _, c := range d.Channels() {
		get(c).Enabled = true
	}
	rows, err := d.db.QueryContext(ctx, "SELECT channel, state, COUNT(*), MAX(sent_at) FROM outbox GROUP BY channel, state")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c, st string
		var n int
		var sent sql.NullInt64
		if err := rows.Scan(&c, &st, &n, &sent); err != nil {
			rows.Close()
			return nil, err
		}
		s := get(c)
		switch st {
		case StatePending:
			s.Pending = n
		case StateFailed:
			s.Failed = n
		case StateDropped:
			s.Dropped = n
		case StateSent:
			if sent.Valid {
				t := time.UnixMilli(sent.Int64).UTC()
				s.LastSuccess = &t
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Последняя ошибка — у самой новой строки канала с ошибкой.
	rows, err = d.db.QueryContext(ctx, `SELECT channel, last_error FROM outbox WHERE id IN
		(SELECT MAX(id) FROM outbox WHERE last_error IS NOT NULL AND state != ? GROUP BY channel)`, StateSent)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c, e string
		if err := rows.Scan(&c, &e); err != nil {
			rows.Close()
			return nil, err
		}
		get(c).LastError = e
	}
	rows.Close()
	out := make([]ChannelStatus, 0, len(m))
	for _, s := range m {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Channel < out[j].Channel })
	return out, rows.Err()
}

// errText — текст ошибки для базы: одна строка, не длиннее 300 символов.
// Секреты в ошибки каналов не попадают — отвечает Sender.
func errText(err error) string {
	if err == nil {
		return ""
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 300 {
		cut := 300
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}
