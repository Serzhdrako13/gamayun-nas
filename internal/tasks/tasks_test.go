package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/store"
)

type env struct {
	path string
	db   *sql.DB
	log  *events.Log
	m    *Manager
}

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newEnv(t *testing.T, maxParallel int) *env {
	t.Helper()
	e := &env{path: filepath.Join(t.TempDir(), "t.db")}
	e.db = openDB(t, e.path)
	e.log = events.NewLog(e.db)
	e.m = New(e.db, e.log, Options{MaxParallel: maxParallel})
	if err := e.m.Register(DemoSleepKind, DemoSleep); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		e.m.Shutdown(ctx)
	})
	return e
}

func waitState(t *testing.T, m *Manager, id string, want State) Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		task, err := m.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if task.State == want {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("задача %s: состояние %s, ожидалось %s", id, task.State, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func submitSleep(t *testing.T, m *Manager, ms int, fail bool) Task {
	t.Helper()
	p, _ := json.Marshal(DemoSleepParams{DurationMS: ms, Fail: fail})
	task, err := m.Submit(context.Background(), DemoSleepKind, p)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestSucceeded(t *testing.T) {
	e := newEnv(t, 2)
	sub := e.log.Subscribe(64)
	defer sub.Close()

	task := submitSleep(t, e.m, 30, false)
	if task.State != Queued {
		t.Fatalf("начальное состояние %s", task.State)
	}
	done := waitState(t, e.m, task.ID, Succeeded)
	if done.Progress != 100 || string(done.Result) != `{"slept_ms":30}` || done.Error != nil {
		t.Errorf("итог: %+v", done)
	}
	if done.StartedAt == nil || done.FinishedAt == nil {
		t.Error("нет started_at/finished_at")
	}

	var states []State
	timeout := time.After(2 * time.Second)
	for len(states) < 3 {
		select {
		case ev := <-sub.C:
			if ev.Type == EventState {
				var s Task
				json.Unmarshal(ev.Data, &s)
				states = append(states, s.State)
				if ev.ID == 0 {
					t.Error("событие состояния не журналировано")
				}
			} else if ev.Type == EventProgress && ev.ID != 0 {
				t.Error("событие прогресса не должно журналироваться")
			}
		case <-timeout:
			t.Fatalf("получены состояния %v", states)
		}
	}
	if states[0] != Queued || states[1] != Running || states[2] != Succeeded {
		t.Errorf("переходы %v", states)
	}
}

func TestFailed(t *testing.T) {
	e := newEnv(t, 1)
	task := submitSleep(t, e.m, 10, true)
	done := waitState(t, e.m, task.ID, Failed)
	if done.Error == nil || done.Error.Code != CodeFailed {
		t.Errorf("ошибка: %+v", done.Error)
	}
}

func TestBadParamsFailTask(t *testing.T) {
	e := newEnv(t, 1)
	task, err := e.m.Submit(context.Background(), DemoSleepKind, json.RawMessage(`{"duration_ms":10,"extra":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if done := waitState(t, e.m, task.ID, Failed); done.Error.Code != CodeFailed {
		t.Errorf("ошибка: %+v", done.Error)
	}
	if _, err := e.m.Submit(context.Background(), DemoSleepKind, json.RawMessage(`{`)); !errors.Is(err, ErrBadParams) {
		t.Errorf("невалидный JSON: %v", err)
	}
}

func TestUnknownKind(t *testing.T) {
	e := newEnv(t, 1)
	if _, err := e.m.Submit(context.Background(), "rm.rf", nil); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("ожидалась ErrUnknownKind, получено %v", err)
	}
	if err := e.m.Register(DemoSleepKind, DemoSleep); err == nil {
		t.Error("повторная регистрация вида должна отклоняться")
	}
}

func TestCancelRunning(t *testing.T) {
	e := newEnv(t, 1)
	task := submitSleep(t, e.m, 60_000, false)
	waitState(t, e.m, task.ID, Running)
	if err := e.m.Cancel(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	done := waitState(t, e.m, task.ID, Failed)
	if done.Error == nil || done.Error.Code != CodeCancelled {
		t.Errorf("ошибка: %+v", done.Error)
	}
	if err := e.m.Cancel(context.Background(), task.ID); !errors.Is(err, ErrNotActive) {
		t.Errorf("повторная отмена: %v", err)
	}
	if err := e.m.Cancel(context.Background(), "нет"); !errors.Is(err, ErrNotFound) {
		t.Errorf("отмена несуществующей: %v", err)
	}
}

func TestCancelQueued(t *testing.T) {
	e := newEnv(t, 1)
	first := submitSleep(t, e.m, 60_000, false)
	waitState(t, e.m, first.ID, Running)
	second := submitSleep(t, e.m, 10, false)
	time.Sleep(20 * time.Millisecond)
	if s, _ := e.m.Get(context.Background(), second.ID); s.State != Queued {
		t.Fatalf("вторая задача должна ждать слота, состояние %s", s.State)
	}
	e.m.Cancel(context.Background(), second.ID)
	done := waitState(t, e.m, second.ID, Failed)
	if done.Error.Code != CodeCancelled || done.StartedAt != nil {
		t.Errorf("отменённая в очереди: %+v", done)
	}
}

func TestPanicBecomesFailed(t *testing.T) {
	e := newEnv(t, 1)
	e.m.Register("test.panic", func(context.Context, json.RawMessage, func(int)) (any, error) {
		panic("бум")
	})
	task, _ := e.m.Submit(context.Background(), "test.panic", nil)
	if done := waitState(t, e.m, task.ID, Failed); done.Error.Code != CodePanic {
		t.Errorf("ошибка: %+v", done.Error)
	}
}

func TestShutdownInterrupts(t *testing.T) {
	e := newEnv(t, 1)
	run := submitSleep(t, e.m, 60_000, false)
	waitState(t, e.m, run.ID, Running)
	queued := submitSleep(t, e.m, 60_000, false)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{run.ID, queued.ID} {
		task, _ := e.m.Get(context.Background(), id)
		if task.State != Interrupted || task.Error.Code != CodeInterrupted {
			t.Errorf("%s: %+v", id, task)
		}
	}
	if _, err := e.m.Submit(context.Background(), DemoSleepKind, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Submit после Shutdown: %v", err)
	}
}

// TestRecoverAfterCrash: первый процесс «падает» с задачей в running,
// второй открывает ту же базу — задача становится interrupted и не
// перезапускается; запоздалое завершение первого не перезаписывает состояние.
func TestRecoverAfterCrash(t *testing.T) {
	e := newEnv(t, 2)
	release := make(chan struct{})
	e.m.Register("test.block", func(ctx context.Context, _ json.RawMessage, _ func(int)) (any, error) {
		<-release
		return "готово", nil
	})
	running, _ := e.m.Submit(context.Background(), "test.block", nil)
	waitState(t, e.m, running.ID, Running)
	// Задача в queued без исполнителя — как если бы процесс упал до запуска.
	now := time.Now().UnixMilli()
	if _, err := e.db.Exec("INSERT INTO tasks(id, kind, state, created_at, updated_at) VALUES ('q1', 'test.block', 'queued', ?, ?)", now, now); err != nil {
		t.Fatal(err)
	}

	db2 := openDB(t, e.path)
	m2 := New(db2, events.NewLog(db2), Options{})
	n, err := m2.Recover(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("Recover: %d, %v", n, err)
	}
	for _, id := range []string{running.ID, "q1"} {
		task, _ := m2.Get(context.Background(), id)
		if task.State != Interrupted || task.Error == nil || task.Error.Code != CodeInterrupted || task.FinishedAt == nil {
			t.Errorf("%s после перезапуска: %+v", id, task)
		}
	}
	if n, _ := m2.Recover(context.Background()); n != 0 {
		t.Errorf("повторный Recover нашёл %d задач", n)
	}

	close(release) // старый исполнитель завершился поздно
	time.Sleep(50 * time.Millisecond)
	if task, _ := m2.Get(context.Background(), running.ID); task.State != Interrupted {
		t.Errorf("состояние перезаписано запоздалым исполнителем: %s", task.State)
	}
}

func TestListNewestFirst(t *testing.T) {
	e := newEnv(t, 2)
	a := submitSleep(t, e.m, 5, false)
	time.Sleep(3 * time.Millisecond)
	b := submitSleep(t, e.m, 5, false)
	list, err := e.m.List(context.Background(), 10)
	if err != nil || len(list) != 2 || list[0].ID != b.ID || list[1].ID != a.ID {
		t.Fatalf("список: %+v, %v", list, err)
	}
	if list, _ := e.m.List(context.Background(), 1); len(list) != 1 {
		t.Errorf("limit не применён: %d", len(list))
	}
	if _, err := e.m.Get(context.Background(), "нет"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get несуществующей: %v", err)
	}
}
