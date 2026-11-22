package notify

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/store"
)

func open(t *testing.T, path string) (*sql.DB, *events.Log) {
	t.Helper()
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, events.NewLog(db)
}

func emit(t *testing.T, l *events.Log, typ, sev string) events.Event {
	t.Helper()
	ev, err := l.Emit(context.Background(), events.Event{Type: typ, Severity: sev, Subject: "disk:x"}, true)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func count(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func both() []string { return []string{ChannelSMTP, ChannelWebhook} }

func TestSelection(t *testing.T) {
	ctx := context.Background()
	db, l := open(t, filepath.Join(t.TempDir(), "d.db"))
	s := New(db, l, Options{Channels: both})
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	emit(t, l, "task.progress", events.SevInfo)          // не в центр
	emit(t, l, events.DiskReturned, events.SevInfo)      // центр, без outbox
	emit(t, l, events.DiskRemoved, events.SevWarning)    // центр + 2 канала
	emit(t, l, events.ArrayDegraded, events.SevCritical) // центр + 2 канала
	emit(t, l, "auth.locked", events.SevWarning)         // не оборудование, но warning
	if err := s.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "SELECT COUNT(*) FROM notifications"); n != 4 {
		t.Fatalf("центр: %d, ждали 4", n)
	}
	if n := count(t, db, "SELECT COUNT(*) FROM outbox"); n != 6 {
		t.Fatalf("outbox: %d, ждали 6", n)
	}
	// Повтор — без дублей.
	evs, _ := l.Since(ctx, 0, 100)
	for _, ev := range evs {
		if err := s.Process(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, db, "SELECT COUNT(*) FROM notifications"); n != 4 {
		t.Fatalf("дубли в центре: %d", n)
	}
	if n, _ := s.Unread(ctx); n != 4 {
		t.Fatalf("непрочитанных %d", n)
	}
	list, _ := s.List(ctx, 2, 0, false)
	if len(list) != 2 || list[0].Type != "auth.locked" {
		t.Fatalf("список: %+v", list)
	}
	if _, err := s.MarkRead(ctx, list[1].ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Unread(ctx); n != 1 {
		t.Fatalf("после MarkRead непрочитанных %d", n)
	}
}

func TestFirstRunSkipsOldAndRestartCatchesUp(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "d.db")
	db, l := open(t, path)
	emit(t, l, events.DiskRemoved, events.SevCritical) // до появления уведомлений
	s := New(db, l, Options{Channels: both})
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "SELECT COUNT(*) FROM notifications"); n != 0 {
		t.Fatalf("старое событие попало в центр: %d", n)
	}
	// «Простой»: события пишутся без работающего отбора, затем перезапуск.
	emit(t, l, events.DiskRemoved, events.SevCritical)
	emit(t, l, events.ArrayDegraded, events.SevCritical)
	db.Close()
	db2, l2 := open(t, path)
	s2 := New(db2, l2, Options{Channels: both})
	if err := s2.Init(ctx); err != nil { // курсор уже есть — не сдвигается
		t.Fatal(err)
	}
	if err := s2.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db2, "SELECT COUNT(*) FROM notifications"); n != 2 {
		t.Fatalf("после перезапуска в центре %d, ждали 2", n)
	}
	if n := count(t, db2, "SELECT COUNT(*) FROM outbox"); n != 4 {
		t.Fatalf("outbox %d", n)
	}
}

func TestRunLiveAndResubscribe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, l := open(t, filepath.Join(t.TempDir(), "d.db"))
	s := New(db, l, Options{})
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	waitFor(t, func() bool { return count(t, db, "SELECT COUNT(*) FROM settings WHERE key='notify.cursor'") == 1 })
	// Поток больше буфера подписчика — отставший догоняет по журналу.
	for i := 0; i < 600; i++ {
		emit(t, l, events.DiskRemoved, events.SevWarning)
	}
	waitFor(t, func() bool { return count(t, db, "SELECT COUNT(*) FROM notifications") == 600 })
	if n := count(t, db, "SELECT COUNT(*) FROM outbox"); n != 0 {
		t.Fatalf("без каналов outbox %d", n)
	}
	cancel()
	<-done
}

func TestPruneCascades(t *testing.T) {
	ctx := context.Background()
	db, l := open(t, filepath.Join(t.TempDir(), "d.db"))
	s := New(db, l, Options{Channels: both, GroupWindow: -1})
	_ = s.Init(ctx)
	for i := 0; i < 5; i++ {
		emit(t, l, events.DiskRemoved, events.SevWarning)
	}
	_ = s.CatchUp(ctx)
	if n, err := s.Prune(ctx, 2); err != nil || n != 3 {
		t.Fatalf("prune %d %v", n, err)
	}
	if n := count(t, db, "SELECT COUNT(*) FROM outbox"); n != 4 {
		t.Fatalf("outbox после prune %d", n)
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("не дождались")
}

func TestPruneAge(t *testing.T) {
	ctx := context.Background()
	db, l := open(t, filepath.Join(t.TempDir(), "d.db"))
	s := New(db, l, Options{Channels: both})
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ago := func(d int) time.Time { return now.Add(-time.Duration(d) * 24 * time.Hour) }
	for _, x := range []struct {
		sev  string
		days int
	}{{events.SevWarning, 91}, {events.SevWarning, 10}, {events.SevCritical, 200}, {events.SevCritical, 366}} {
		if _, err := l.Emit(ctx, events.Event{Type: events.DiskRemoved, Severity: x.sev, Subject: "disk:x", Time: ago(x.days)}, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "SELECT COUNT(*) FROM notifications"); n != 4 {
		t.Fatalf("уведомлений %d", n)
	}
	if n := count(t, db, "SELECT COUNT(*) FROM outbox"); n != 8 {
		t.Fatalf("outbox %d", n)
	}
	n, err := s.PruneAge(ctx, events.DefaultRetention(), now)
	if err != nil || n != 2 {
		t.Fatalf("удалено %d %v", n, err)
	}
	if n := count(t, db, "SELECT COUNT(*) FROM notifications WHERE ts < "+strconv.FormatInt(ago(90).UnixMilli(), 10)+" AND severity = 'warning'"); n != 0 {
		t.Errorf("старое warning осталось")
	}
	if n := count(t, db, "SELECT COUNT(*) FROM notifications WHERE severity = 'critical'"); n != 1 {
		t.Errorf("critical: %d", n)
	}
	if n := count(t, db, "SELECT COUNT(*) FROM outbox"); n != 4 {
		t.Errorf("outbox после обрезки %d (каскад)", n)
	}
}
