package notify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gamayun-nas/internal/events"
	"path/filepath"
)

type fakeSender struct {
	mu   sync.Mutex
	fail error
	got  []Notification
}

func (f *fakeSender) Send(_ context.Context, n Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.got = append(f.got, n)
	return nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func setup(t *testing.T, opt DeliverOptions) (*Service, *Deliverer, *events.Log, *clock) {
	t.Helper()
	db, l := open(t, filepath.Join(t.TempDir(), "d.db"))
	c := &clock{t: time.Now().Truncate(time.Millisecond)}
	opt.Now = c.now
	d := NewDeliverer(db, opt)
	// Серии проверяются в quiet_test.go; здесь — механика очереди.
	s := New(db, l, Options{Channels: d.Channels, OnQueued: d.Kick, Now: c.now, GroupWindow: -1})
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, d, l, c
}

func TestDeliverRetryBackoffAndSuccess(t *testing.T) {
	ctx := context.Background()
	s, d, l, c := setup(t, DeliverOptions{})
	f := &fakeSender{fail: errors.New("connection refused\nвторая строка")}
	d.SetSender(ChannelSMTP, f)
	emit(t, l, events.DiskRemoved, events.SevCritical)
	if err := s.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := d.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if w := next.Sub(c.t); w != 30*time.Second {
		t.Fatalf("первая пауза %v", w)
	}
	st, _ := d.Status(ctx)
	if len(st) != 1 || st[0].Pending != 1 || st[0].LastError != "connection refused вторая строка" || st[0].LastSuccess != nil {
		t.Fatalf("статус после ошибки: %+v", st)
	}
	// Раньше срока — без попытки.
	c.add(10 * time.Second)
	d.Step(ctx)
	c.add(25 * time.Second)
	next, _ = d.Step(ctx) // вторая ошибка → 60 с
	if w := next.Sub(c.t); w != 60*time.Second {
		t.Fatalf("вторая пауза %v", w)
	}
	f.fail = nil
	c.add(time.Minute)
	if next, _ = d.Step(ctx); !next.IsZero() {
		t.Fatalf("очередь не пуста: %v", next)
	}
	if len(f.got) != 1 || f.got[0].Type != events.DiskRemoved {
		t.Fatalf("доставлено: %+v", f.got)
	}
	st, _ = d.Status(ctx)
	if st[0].Pending != 0 || st[0].LastSuccess == nil || st[0].LastError != "" {
		t.Fatalf("статус после успеха: %+v", st)
	}
}

func TestBackoffCapAndFailedByAge(t *testing.T) {
	ctx := context.Background()
	s, d, l, c := setup(t, DeliverOptions{MaxAge: 3 * time.Hour})
	if b := d.backoff(20); b != time.Hour {
		t.Fatalf("потолок %v", b)
	}
	d.SetSender(ChannelWebhook, &fakeSender{fail: errors.New("503")})
	emit(t, l, events.ArrayDegraded, events.SevCritical)
	s.CatchUp(ctx)
	for i := 0; i < 20; i++ {
		d.Step(ctx)
		c.add(time.Hour)
	}
	st, _ := d.Status(ctx)
	if st[0].Failed != 1 || st[0].Pending != 0 {
		t.Fatalf("ждали failed: %+v", st)
	}
}

func TestQueueBoundAndDisabledChannel(t *testing.T) {
	ctx := context.Background()
	s, d, l, _ := setup(t, DeliverOptions{MaxPending: 3})
	d.SetSender(ChannelSMTP, &fakeSender{fail: errors.New("down")})
	d.SetSender(ChannelWebhook, &fakeSender{fail: errors.New("down")})
	for i := 0; i < 5; i++ {
		emit(t, l, events.DiskRemoved, events.SevWarning)
	}
	s.CatchUp(ctx)
	d.SetSender(ChannelWebhook, nil) // отключён после постановки
	d.Step(ctx)
	st, _ := d.Status(ctx)
	if len(st) != 2 {
		t.Fatalf("каналы: %+v", st)
	}
	smtp, wh := st[0], st[1]
	if !smtp.Enabled || smtp.Pending != 3 || smtp.Dropped != 2 {
		t.Fatalf("smtp: %+v", smtp)
	}
	if wh.Enabled || wh.Dropped != 5 || wh.LastError != "канал отключён" {
		t.Fatalf("webhook: %+v", wh)
	}
}

func TestRunDeliversLive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, l := open(t, filepath.Join(t.TempDir(), "d.db"))
	d := NewDeliverer(db, DeliverOptions{})
	f := &fakeSender{}
	d.SetSender(ChannelSMTP, f)
	s := New(db, l, Options{Channels: d.Channels, OnQueued: d.Kick})
	go s.Run(ctx)
	go d.Run(ctx, func(err error) { t.Error(err) })
	waitFor(t, func() bool { return count(t, db, "SELECT COUNT(*) FROM settings WHERE key='notify.cursor'") == 1 })
	emit(t, l, events.DiskRemoved, events.SevCritical)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.got) == 1 })
}

func TestErrText(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "ж"
	}
	if s := errText(errors.New(long)); len(s) > 304 || s[len(s)-3:] != "…" {
		t.Fatalf("обрезка: %q", s)
	}
}
