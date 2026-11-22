package watch

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) spec(s Spec) Spec {
	s.Now = func() time.Time { return c.t }
	s.Sleep = func(ctx context.Context, d time.Duration) error { c.t = c.t.Add(d); return ctx.Err() }
	return s
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_000_000, 0)} }

func el(id string, st State, pos int64, alive bool) Element {
	return Element{ID: id, State: st, Pos: pos, Alive: alive}
}

func TestDoneWhenTargetKnown(t *testing.T) {
	c := newClock()
	n := 0
	out := Run(context.Background(), c.spec(Spec{Name: "t", Probe: func(context.Context) Observation {
		n++
		if n < 5 {
			return Observation{Elements: []Element{el("md0", Working, int64(n), true)}}
		}
		return Observation{Elements: []Element{el("md0", Done, 0, true)}, Target: true}
	}}))
	if !out.OK() {
		t.Fatalf("ожидалось готово: %+v", out)
	}
}

// Пересчёт integrity на исчезнувшем устройстве «идёт», счётчик стоит
// или даже растёт — это не продвижение.
func TestWorkingOnDeadDeviceStalls(t *testing.T) {
	c := newClock()
	recovered := 0
	pos := int64(0)
	start := c.t
	out := Run(context.Background(), c.spec(Spec{Name: "create", StallWindow: time.Minute,
		Recover: func(context.Context) { recovered++ },
		Probe: func(context.Context) Observation {
			pos++
			return Observation{Elements: []Element{el("md0", Done, 0, true), el("dni_a2_1", Working, pos, false)}}
		}}))
	if out.Result != ResultStalled || recovered != 1 {
		t.Fatalf("ожидался застой после одной попытки восстановления: %v, recover=%d", out, recovered)
	}
	if d := c.t.Sub(start); d < 2*time.Minute || d > 2*time.Minute+5*time.Second {
		t.Fatalf("срок исхода %s, ожидалось ~2 мин (окно + второе окно)", d)
	}
	if !strings.Contains(out.Reason, "устройство недоступно") || !strings.Contains(out.Reason, "перепроверка") {
		t.Fatalf("причина без подробностей: %s", out.Reason)
	}
}

func TestRecoverResumesProgress(t *testing.T) {
	c := newClock()
	alive := false
	pos := int64(0)
	out := Run(context.Background(), c.spec(Spec{Name: "t", StallWindow: time.Minute,
		Recover: func(context.Context) { alive = true },
		Probe: func(context.Context) Observation {
			if alive {
				pos += 10
			}
			if pos > 100 {
				return Observation{Elements: []Element{el("md0", Done, 0, true)}, Target: true}
			}
			return Observation{Elements: []Element{el("md0", Working, pos, alive)}}
		}}))
	if !out.OK() {
		t.Fatalf("после восстановления процесс должен дойти до цели: %v", out)
	}
}

func TestUnknownNeverDone(t *testing.T) {
	c := newClock()
	out := Run(context.Background(), c.spec(Spec{Name: "t", StallWindow: time.Minute, Probe: func(context.Context) Observation {
		// цель «достигнута», но часть стека прочитать не удалось
		return Observation{Elements: []Element{el("md0", Done, 0, true), el("lv", Unknown, -1, true)}, Target: true}
	}}))
	if out.Result != ResultUnknown {
		t.Fatalf("ожидалось «неизвестно»: %v", out)
	}
	out = Run(context.Background(), c.spec(Spec{Name: "t", StallWindow: time.Minute, Probe: func(context.Context) Observation {
		return Observation{Err: errors.New("mdadm завис"), Target: true}
	}}))
	if out.Result != ResultUnknown || !strings.Contains(out.Reason, "mdadm завис") {
		t.Fatalf("ожидалось «неизвестно» с причиной: %v", out)
	}
}

// Колебание счётчика (вниз-вверх) не продлевает жизнь процессу.
func TestOscillationIsNotProgress(t *testing.T) {
	c := newClock()
	n := 0
	out := Run(context.Background(), c.spec(Spec{Name: "t", StallWindow: time.Minute, Probe: func(context.Context) Observation {
		n++
		return Observation{Elements: []Element{el("md0", Working, int64(n%2*10), true)}}
	}}))
	if out.Result != ResultStalled {
		t.Fatalf("ожидался застой: %v", out)
	}
}

func TestDeadline(t *testing.T) {
	c := newClock()
	pos := int64(0)
	out := Run(context.Background(), c.spec(Spec{Name: "t", Deadline: 10 * time.Minute, Probe: func(context.Context) Observation {
		pos++
		return Observation{Elements: []Element{el("md0", Working, pos, true)}}
	}}))
	if out.Result != ResultDeadline {
		t.Fatalf("ожидался общий срок: %v", out)
	}
}

func TestCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := Run(ctx, Spec{Name: "t", Probe: func(context.Context) Observation { return Observation{} }})
	if out.Result != ResultCancelled {
		t.Fatalf("ожидалась отмена: %v", out)
	}
}

// Генеративный тест: случайные последовательности наблюдений (прогресс,
// застой, колебания, ошибки опроса, пропадание и возврат устройств,
// завершение элементов). Инварианты:
//  1. наблюдатель завершается не позже Deadline+Poll;
//  2. не позже 2×max(окно)+2×Poll после последнего продвижения;
//  3. «готово» — только если последнее наблюдение известно и цель достигнута;
//  4. у неуспеха есть причина с последним состоянием.
func TestPropertyRandomSequences(t *testing.T) {
	rng := rand.New(rand.NewSource(20261113))
	for run := 0; run < 5000; run++ {
		c := newClock()
		start := c.t
		poll := time.Duration(1+rng.Intn(5)) * time.Second
		stall := time.Duration(10+rng.Intn(120)) * time.Second
		unknown := time.Duration(10+rng.Intn(120)) * time.Second
		deadline := time.Duration(5+rng.Intn(60)) * time.Minute
		ids := []string{"md0", "md1", "dni0", "dni1"}[:1+rng.Intn(4)]
		pos := map[string]int64{}
		alive := map[string]bool{}
		state := map[string]State{}
		for _, id := range ids {
			alive[id] = true
			state[id] = Working
		}
		mode := rng.Intn(4) // 0 нормальный, 1 «плохой» (много сбоев), 2 колебания, 3 вечный прогресс
		probe := func(context.Context) Observation {
			if mode == 1 && rng.Intn(3) == 0 || rng.Intn(40) == 0 {
				return Observation{Err: errors.New("сбой опроса")}
			}
			o := Observation{}
			all := true
			for _, id := range ids {
				if rng.Intn(60) == 0 {
					alive[id] = !alive[id]
				}
				switch {
				case state[id] == Done:
				case mode == 2:
					pos[id] = int64(rng.Intn(3))
				case mode == 3 || rng.Intn(3) > 0:
					if alive[id] {
						pos[id] += int64(rng.Intn(5))
					}
				}
				if state[id] == Working && mode == 0 && pos[id] > 200 && rng.Intn(5) == 0 {
					state[id] = Done
				}
				st := state[id]
				if mode == 1 && rng.Intn(10) == 0 {
					st = Unknown
				}
				if st != Done {
					all = false
				}
				o.Elements = append(o.Elements, Element{ID: id, State: st, Pos: pos[id], Alive: alive[id]})
			}
			o.Target = all
			return o
		}
		var trace []string
		var lastProgress time.Time = start
		prevMax := map[string]int64{}
		prevState := map[string]State{}
		spec := c.spec(Spec{Name: "p", Poll: poll, StallWindow: stall, UnknownWindow: unknown, Deadline: deadline, Probe: probe,
			OnObserve: func(o Observation, _ bool) {
				if len(trace) >= 80 {
					trace = trace[1:]
				}
				trace = append(trace, c.t.Sub(start).String()+" "+o.String())
				// независимая модель продвижения (для инварианта 2)
				if !o.known() {
					return
				}
				for _, e := range o.Elements {
					if e.State == Done && prevState[e.ID] == Working {
						lastProgress = c.t
					}
					if e.State == Working && e.Alive && prevState[e.ID] == Working && e.Pos > prevMax[e.ID] {
						lastProgress = c.t
					}
					if e.State != prevState[e.ID] {
						prevMax[e.ID] = e.Pos
					} else if e.Pos > prevMax[e.ID] {
						prevMax[e.ID] = e.Pos
					}
					prevState[e.ID] = e.State
				}
			}})
		out := Run(context.Background(), spec)
		el := c.t.Sub(start)
		if el > deadline+poll {
			t.Fatalf("run %d: завершился за %s > срок %s", run, el, deadline)
		}
		w := stall
		if unknown > w {
			w = unknown
		}
		if !out.OK() && out.Result != ResultDeadline && c.t.Sub(lastProgress) > 2*w+2*poll {
			t.Log(strings.Join(trace, "\n"))
			t.Fatalf("run %d: исход через %s после продвижения > 2×%s+2×%s (%v)", run, c.t.Sub(lastProgress), w, poll, out.Result)
		}
		if out.OK() && (!out.Last.known() || !out.Last.Target) {
			t.Fatalf("run %d: «готово» при %v", run, out.Last)
		}
		if !out.OK() && (out.Reason == "" || !strings.Contains(out.Reason, "последнее состояние")) {
			t.Fatalf("run %d: нет причины: %q", run, out.Reason)
		}
	}
}
