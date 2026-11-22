package notify

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/events"
)

// Тишина и серии.

func quietSetup(t *testing.T) (*Service, *Deliverer, *events.Log, *clock) {
	t.Helper()
	db, l := open(t, filepath.Join(t.TempDir(), "d.db"))
	c := &clock{t: time.Now().Truncate(time.Millisecond)}
	d := NewDeliverer(db, DeliverOptions{Now: c.now})
	d.SetSender(ChannelSMTP, &fakeSender{})
	d.SetSender(ChannelWebhook, &fakeSender{})
	s := New(db, l, Options{Channels: d.Channels, OnQueued: d.Kick, Now: c.now})
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, d, l, c
}

func emitAt(t *testing.T, l *events.Log, c *clock, typ, sev, subj string) {
	t.Helper()
	if _, err := l.Emit(context.Background(), events.Event{Type: typ, Severity: sev, Subject: subj, Time: c.t.UTC()}, true); err != nil {
		t.Fatal(err)
	}
}

func catchUp(t *testing.T, s *Service) {
	t.Helper()
	if err := s.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func centerOf(t *testing.T, s *Service, typ string) []Notification {
	t.Helper()
	list, err := s.List(context.Background(), 500, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	var out []Notification
	for _, n := range list {
		if n.Type == typ {
			out = append(out, n)
		}
	}
	return out
}

func TestQuietSuppressesExternalAndSummarizes(t *testing.T) {
	ctx := context.Background()
	s, _, l, c := quietSetup(t)
	db := s.db
	q, err := s.SetQuiet(ctx, c.t.Add(time.Hour), "  замена   диска ", false, "admin")
	if err != nil || q.Reason != "замена диска" || q.MuteCritical {
		t.Fatalf("SetQuiet: %+v %v", q, err)
	}
	emitAt(t, l, c, events.DiskRemoved, events.SevWarning, "disk:a")
	emitAt(t, l, c, "auth.lockout", events.SevWarning, "")
	emitAt(t, l, c, events.ArrayDegraded, events.SevCritical, "array:md0") // critical проходит
	catchUp(t, s)
	if n := count(t, db, "SELECT COUNT(*) FROM outbox"); n != 2 {
		t.Fatalf("outbox в тишине: %d, ждали 2 (только critical)", n)
	}
	if n := len(centerOf(t, s, TypeQuietStarted)); n != 1 {
		t.Fatalf("начало тишины в центре: %d", n)
	}
	q, _ = s.Quiet(ctx)
	if q == nil || q.Suppressed != 2 || q.SuppressedCritical != 0 {
		t.Fatalf("счётчик: %+v", q)
	}
	// Досрочная отмена: событие окончания, сводка во внешние каналы.
	if ok, err := s.EndQuiet(ctx, "отменена", "admin"); !ok || err != nil {
		t.Fatalf("EndQuiet %v %v", ok, err)
	}
	if q, _ := s.Quiet(ctx); q == nil || !q.Ending {
		t.Fatalf("до отбора тишина должна быть «завершается»: %+v", q)
	}
	if _, err := s.SetQuiet(ctx, c.t.Add(time.Hour), "x", false, ""); !errors.Is(err, ErrQuietEnding) {
		t.Fatalf("SetQuiet при завершении: %v", err)
	}
	catchUp(t, s)
	if q, _ := s.Quiet(ctx); q != nil {
		t.Fatalf("тишина не снята: %+v", q)
	}
	ended := centerOf(t, s, TypeQuietEnded)
	if len(ended) != 1 {
		t.Fatalf("окончание в центре: %d", len(ended))
	}
	var d map[string]any
	_ = json.Unmarshal(ended[0].Data, &d)
	if d["suppressed"] != float64(2) || d["end_reason"] != "отменена" || d["reason"] != "замена диска" {
		t.Fatalf("данные окончания: %v", d)
	}
	if n := count(t, db, "SELECT COUNT(*) FROM outbox o JOIN notifications n ON n.id = o.notif_id WHERE n.type = 'notify.quiet_ended'"); n != 2 {
		t.Fatalf("сводка тишины во внешние каналы: %d", n)
	}
	subj, _ := Format(ended[0], "nas")
	if !strings.Contains(subj, "не отправлено событий — 2") {
		t.Fatalf("тема сводки: %s", subj)
	}
	// После тишины — обычная отправка.
	emitAt(t, l, c, "auth.lockout", events.SevWarning, "u2")
	catchUp(t, s)
	if n := count(t, db, "SELECT COUNT(*) FROM outbox"); n != 6 {
		t.Fatalf("outbox после тишины: %d", n)
	}
}

func TestQuietMuteCriticalAndEmptySummary(t *testing.T) {
	ctx := context.Background()
	s, _, l, c := quietSetup(t)
	if _, err := s.SetQuiet(ctx, c.t.Add(time.Hour), "работы", true, ""); err != nil {
		t.Fatal(err)
	}
	emitAt(t, l, c, events.ArrayDegraded, events.SevCritical, "array:md0")
	catchUp(t, s)
	q, _ := s.Quiet(ctx)
	if q.Suppressed != 1 || q.SuppressedCritical != 1 || count(t, s.db, "SELECT COUNT(*) FROM outbox") != 0 {
		t.Fatalf("critical не заглушён: %+v", q)
	}
	s.EndQuiet(ctx, "", "")
	catchUp(t, s)
	// Тишина без подавленных — окончание только в центр.
	if _, err := s.SetQuiet(ctx, c.t.Add(time.Hour), "работы 2", false, ""); err != nil {
		t.Fatal(err)
	}
	s.EndQuiet(ctx, "", "")
	catchUp(t, s)
	if n := len(centerOf(t, s, TypeQuietEnded)); n != 2 {
		t.Fatalf("окончаний в центре: %d", n)
	}
	if n := count(t, s.db, "SELECT COUNT(*) FROM outbox"); n != 2 {
		t.Fatalf("outbox: %d, ждали 2 (сводка первой тишины)", n)
	}
}

func TestQuietExpiresByTickAndRetry(t *testing.T) {
	ctx := context.Background()
	s, _, l, c := quietSetup(t)
	q, err := s.SetQuiet(ctx, c.t.Add(time.Hour), "работы", false, "")
	if err != nil {
		t.Fatal(err)
	}
	// Продление: тот же ID, счётчики сохраняются.
	emitAt(t, l, c, events.DiskRemoved, events.SevWarning, "disk:a")
	catchUp(t, s)
	q2, err := s.SetQuiet(ctx, c.t.Add(2*time.Hour), "работы дольше", false, "")
	if err != nil || q2.ID != q.ID || q2.Suppressed != 1 || !q2.Until.Equal(c.t.Add(2*time.Hour).UTC()) {
		t.Fatalf("продление: %+v %v", q2, err)
	}
	if err := s.QuietTick(ctx); err != nil {
		t.Fatal(err)
	}
	if q, _ := s.Quiet(ctx); q == nil || q.Ending {
		t.Fatalf("до срока тишина не должна заканчиваться: %+v", q)
	}
	c.add(2*time.Hour + time.Second)
	// Событие после срока, но до таймера — уже не глушится.
	emitAt(t, l, c, events.DiskRemoved, events.SevWarning, "disk:b")
	catchUp(t, s)
	if n := count(t, s.db, "SELECT COUNT(*) FROM outbox"); n != 2 {
		t.Fatalf("outbox после срока: %d", n)
	}
	// Сбой: отметка «завершается» есть, события окончания в журнале нет.
	q, _ = s.Quiet(ctx)
	old := c.t.Add(-2 * time.Minute)
	q.Ending, q.EndingAt = true, &old
	if err := saveQuiet(ctx, s.db, q); err != nil {
		t.Fatal(err)
	}
	if err := s.QuietTick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.QuietTick(ctx); err != nil { // второй раз сразу — без дубля
		t.Fatal(err)
	}
	evs, _ := l.Since(ctx, 0, 100)
	ends := 0
	for _, ev := range evs {
		if ev.Type == TypeQuietEnded {
			ends++
		}
	}
	if ends != 1 {
		t.Fatalf("событий окончания: %d", ends)
	}
	// Дубль события окончания (сбой между журналом и отметкой) безвреден.
	b, _ := json.Marshal(map[string]any{"id": q.ID})
	l.Emit(ctx, events.Event{Type: TypeQuietEnded, Data: b}, true)
	catchUp(t, s)
	if q, _ := s.Quiet(ctx); q != nil {
		t.Fatalf("тишина не снята: %+v", q)
	}
	if n := len(centerOf(t, s, TypeQuietEnded)); n != 1 {
		t.Fatalf("дубль окончания в центре: %d", n)
	}
	if n := count(t, s.db, "SELECT COUNT(*) FROM outbox o JOIN notifications n ON n.id = o.notif_id WHERE n.type = 'notify.quiet_ended'"); n != 2 {
		t.Fatalf("сводка: %d", n)
	}
}

func TestQuietValidation(t *testing.T) {
	ctx := context.Background()
	s, _, _, c := quietSetup(t)
	for name, f := range map[string]func() error{
		"без причины":    func() error { _, err := s.SetQuiet(ctx, c.t.Add(time.Hour), " ", false, ""); return err },
		"прошлое":        func() error { _, err := s.SetQuiet(ctx, c.t, "x", false, ""); return err },
		"дольше 7 суток": func() error { _, err := s.SetQuiet(ctx, c.t.Add(MaxQuiet+time.Minute), "x", false, ""); return err },
		"длинная причина": func() error {
			_, err := s.SetQuiet(ctx, c.t.Add(time.Hour), strings.Repeat("я", 201), false, "")
			return err
		},
	} {
		if err := f(); !errors.Is(err, ErrQuietInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if ok, err := s.EndQuiet(ctx, "", ""); ok || err != nil {
		t.Fatalf("EndQuiet без тишины: %v %v", ok, err)
	}
	// Испорченная запись не глушит уведомления.
	s.db.Exec("INSERT INTO settings(key, value) VALUES ('notify.quiet', '{broken')")
	if q, err := s.Quiet(ctx); q != nil || err != nil {
		t.Fatalf("испорченная запись: %+v %v", q, err)
	}
}

func TestGroupSeries(t *testing.T) {
	ctx := context.Background()
	s, d, l, c := quietSetup(t)
	smtp := &fakeSender{}
	d.SetSender(ChannelSMTP, smtp)
	for i := 0; i < 4; i++ {
		emitAt(t, l, c, events.DiskRemoved, events.SevWarning, "disk:a")
		c.add(30 * time.Second)
	}
	emitAt(t, l, c, events.DiskRemoved, events.SevWarning, "disk:b")     // другой предмет — сразу
	emitAt(t, l, c, events.DiskRemoved, events.SevCritical, "disk:a")    // critical — сразу
	emitAt(t, l, c, events.ArrayDegraded, events.SevCritical, "array:m") // critical — сразу
	emitAt(t, l, c, events.ArrayDegraded, events.SevCritical, "array:m") // critical не группируется
	catchUp(t, s)
	if n := len(centerOf(t, s, events.DiskRemoved)); n != 6 {
		t.Fatalf("центр: %d, ждали все 6", n)
	}
	if n := count(t, s.db, "SELECT COUNT(*) FROM outbox"); n != 10 {
		t.Fatalf("outbox: %d, ждали 10 (5 событий × 2 канала)", n)
	}
	if err := s.FlushGroups(ctx, c.t); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s.db, "SELECT COUNT(*) FROM outbox WHERE repeats > 0"); n != 0 {
		t.Fatalf("сводка до конца окна: %d", n)
	}
	c.add(5 * time.Minute)
	if err := s.FlushGroups(ctx, c.t); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s.db, "SELECT COUNT(*) FROM outbox WHERE repeats = 3"); n != 2 {
		t.Fatalf("сводка серии: %d", n)
	}
	if n := count(t, s.db, "SELECT COUNT(*) FROM notify_groups"); n != 0 {
		t.Fatalf("окна не удалены: %d", n)
	}
	if _, err := d.Step(ctx); err != nil {
		t.Fatal(err)
	}
	var sum *Notification
	for i := range smtp.got {
		if smtp.got[i].Repeats > 0 {
			sum = &smtp.got[i]
		}
	}
	if len(smtp.got) != 6 || sum == nil || sum.Repeats != 3 || sum.RepeatsSince == nil || sum.Subject != "disk:a" {
		t.Fatalf("доставлено %d, сводка %+v", len(smtp.got), sum)
	}
	subj, body := Format(*sum, "nas")
	if !strings.Contains(subj, "повторов: 3") || !strings.Contains(body, "повторилось ещё 3 раз") {
		t.Fatalf("формат сводки: %s / %s", subj, body)
	}
	// Новая серия после окна — первое снова сразу.
	emitAt(t, l, c, events.DiskRemoved, events.SevWarning, "disk:a")
	catchUp(t, s)
	if n := count(t, s.db, "SELECT COUNT(*) FROM outbox"); n != 14 {
		t.Fatalf("outbox после окна: %d", n)
	}
}

func TestGroupCatchUpAndQuiet(t *testing.T) {
	ctx := context.Background()
	s, _, l, c := quietSetup(t)
	// Догон после простоя: повтор за окном приходит раньше сводки.
	emitAt(t, l, c, events.DiskRemoved, events.SevWarning, "disk:a")
	emitAt(t, l, c, events.DiskRemoved, events.SevWarning, "disk:a")
	c.add(6 * time.Minute)
	emitAt(t, l, c, events.DiskRemoved, events.SevWarning, "disk:a")
	catchUp(t, s)
	if n := count(t, s.db, "SELECT COUNT(*) FROM outbox WHERE repeats = 1"); n != 2 {
		t.Fatalf("сводка при догоне: %d", n)
	}
	if n := count(t, s.db, "SELECT COUNT(*) FROM outbox WHERE repeats = 0"); n != 4 {
		t.Fatalf("сразу: %d", n)
	}
	// Повторы в окне, затем тишина: сводка не уходит, идёт в подавленные.
	emitAt(t, l, c, events.DiskRemoved, events.SevWarning, "disk:a")
	emitAt(t, l, c, events.DiskRemoved, events.SevWarning, "disk:a")
	catchUp(t, s)
	if _, err := s.SetQuiet(ctx, c.t.Add(time.Hour), "работы", false, ""); err != nil {
		t.Fatal(err)
	}
	c.add(6 * time.Minute)
	if err := s.FlushGroups(ctx, c.t); err != nil {
		t.Fatal(err)
	}
	q, _ := s.Quiet(ctx)
	if q.Suppressed != 2 {
		t.Fatalf("повторы серии в тишине: %+v", q)
	}
	if n := count(t, s.db, "SELECT COUNT(*) FROM outbox WHERE repeats > 0"); n != 2 {
		t.Fatalf("сводка в тишине ушла: %d", n)
	}
}
