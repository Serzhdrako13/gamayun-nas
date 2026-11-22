package tasks

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/events"
)

// Сторож задач.

func newWatchEnv(t *testing.T, stall time.Duration) *env {
	t.Helper()
	e := &env{path: filepath.Join(t.TempDir(), "t.db")}
	e.db = openDB(t, e.path)
	e.log = events.NewLog(e.db)
	e.m = New(e.db, e.log, Options{StallWindow: stall, WatchPoll: 5 * time.Millisecond})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		e.m.Shutdown(ctx)
	})
	return e
}

func submitKind(t *testing.T, m *Manager, kind string) Task {
	t.Helper()
	task, err := m.Submit(context.Background(), kind, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// Задача молчит, но завершается при отмене — failed stalled с последним наблюдением.
func TestWatchdogCancelsSilentTask(t *testing.T) {
	e := newWatchEnv(t, 50*time.Millisecond)
	e.m.Register("silent", func(ctx context.Context, _ json.RawMessage, _ func(int)) (any, error) {
		Alive(ctx, "массив p-0 resync 10%")
		<-ctx.Done()
		return nil, ctx.Err()
	})
	task := submitKind(t, e.m, "silent")
	got := waitState(t, e.m, task.ID, Failed)
	if got.Error == nil || got.Error.Code != CodeStalled || !strings.Contains(got.Error.Message, "массив p-0 resync 10%") {
		t.Fatalf("ошибка %+v", got.Error)
	}
	if got.AliveNote != "массив p-0 resync 10%" || got.AliveAt == nil {
		t.Errorf("признак жизни в записи: %q %v", got.AliveNote, got.AliveAt)
	}
}

// Регулярные признаки жизни (и прогресс) держат задачу дольше окна.
func TestWatchdogAliveKeepsTask(t *testing.T) {
	e := newWatchEnv(t, 60*time.Millisecond)
	e.m.Register("beating", func(ctx context.Context, _ json.RawMessage, progress func(int)) (any, error) {
		for i := 0; i < 15; i++ {
			if i%2 == 0 {
				Alive(ctx, "опрос")
			} else {
				progress(i)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
		return "ok", nil
	})
	task := submitKind(t, e.m, "beating")
	waitState(t, e.m, task.ID, Succeeded)
}

// Expect продлевает ожидание на долгий шаг (вызов helper).
func TestWatchdogExpectLongStep(t *testing.T) {
	e := newWatchEnv(t, 40*time.Millisecond)
	e.m.Register("long", func(ctx context.Context, _ json.RawMessage, _ func(int)) (any, error) {
		Expect(ctx, 300*time.Millisecond, "helper: home.pool.create")
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
		return "ok", nil
	})
	task := submitKind(t, e.m, "long")
	waitState(t, e.m, task.ID, Succeeded)
}

// Задача не реагирует на отмену — через второе окно failed принудительно;
// поздний возврат функции состояние не меняет.
func TestWatchdogForcesHungTask(t *testing.T) {
	e := newWatchEnv(t, 40*time.Millisecond)
	release := make(chan struct{})
	e.m.Register("hung", func(ctx context.Context, _ json.RawMessage, _ func(int)) (any, error) {
		<-release
		return "поздно", nil
	})
	task := submitKind(t, e.m, "hung")
	got := waitState(t, e.m, task.ID, Failed)
	if got.Error == nil || got.Error.Code != CodeStalled || !strings.Contains(got.Error.Message, "не завершилась") {
		t.Fatalf("ошибка %+v", got.Error)
	}
	close(release)
	time.Sleep(50 * time.Millisecond)
	if again, _ := e.m.Get(context.Background(), task.ID); again.State != Failed {
		t.Fatalf("после позднего возврата состояние %s", again.State)
	}
}

// Признак жизни переживает перезапуск: причина interrupted его называет.
func TestRecoverReportsLastAlive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	db := openDB(t, path)
	m := New(db, events.NewLog(db), Options{StallWindow: time.Hour})
	started := make(chan struct{})
	m.Register("work", func(ctx context.Context, _ json.RawMessage, _ func(int)) (any, error) {
		Alive(ctx, "пересчёт тегов 42%")
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	task := submitKind(t, m, "work")
	<-started
	// «Сброс»: запись остаётся running, менеджер не завершал задачу.
	db.Close()

	db2 := openDB(t, path)
	m2 := New(db2, events.NewLog(db2), Options{})
	defer m2.Shutdown(context.Background())
	if n, err := m2.Recover(context.Background()); err != nil || n != 1 {
		t.Fatalf("Recover: %d %v", n, err)
	}
	got, _ := m2.Get(context.Background(), task.ID)
	if got.State != Interrupted || got.Error == nil || !strings.Contains(got.Error.Message, "пересчёт тегов 42%") {
		t.Fatalf("задача %s %+v", got.State, got.Error)
	}
}
