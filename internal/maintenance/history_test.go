package maintenance

import (
	"context"
	"errors"
	"testing"
	"time"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/smart"
)

func (e *env) setTemp(id string, c int64) {
	e.h.mu.Lock()
	in := e.h.info[id]
	in.TemperatureC = p64(c)
	e.h.info[id] = in
	e.h.mu.Unlock()
}

func heat(evs []events.Event) []string {
	var out []string
	for i := len(evs) - 1; i >= 0; i-- { // Recent — новые первыми
		if evs[i].Type == EvOverheat || evs[i].Type == EvTempNormal {
			out = append(out, evs[i].Type+"/"+evs[i].Severity)
		}
	}
	return out
}

func TestOverheatEvents(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.h.ids = []string{"d1"}
	e.h.info["d1"] = smart.Info{Supported: true, Passed: pb(true), TemperatureC: p64(40)}
	step := func(c int64, d time.Duration) {
		t.Helper()
		e.now = e.now.Add(d)
		e.setTemp("d1", c)
		if _, err := e.s.PollSmart(ctx); err != nil {
			t.Fatal(err)
		}
	}
	step(40, 0)
	step(56, 30*time.Minute) // warning
	step(57, 10*time.Minute) // тот же уровень — без события
	step(52, 10*time.Minute) // гистерезис: 52 ≥ 55-3 — ещё warning
	step(51, 10*time.Minute) // норма
	step(56, 10*time.Minute) // перегрев через 40 мин после прошлого — подавлен
	step(66, 10*time.Minute) // но рост до critical — сразу
	step(62, 10*time.Minute) // 62 ≥ 65-3 — ещё critical
	step(61, 10*time.Minute) // critical → warning без события
	step(66, 10*time.Minute) // снова critical в течение часа — подавлено
	step(66, 60*time.Minute) // через час — событие
	step(40, 30*time.Minute) // норма
	want := []string{"disk.overheat/warning", "disk.temp_normal/info", "disk.overheat/critical", "disk.overheat/critical", "disk.temp_normal/info"}
	if got := heat(e.events(t)); len(got) != len(want) || func() bool {
		for i := range got {
			if got[i] != want[i] {
				return true
			}
		}
		return false
	}() {
		t.Fatalf("события: %v, ожидалось %v", got, want)
	}

	// Состояние переживает перезапуск: перегрев объявлен — повтора нет.
	step(58, 2*time.Hour)
	s2 := New(e.s.db, e.h, nil, e.log, Options{Location: time.UTC})
	s2.SetClock(func() time.Time { return e.now })
	e.now = e.now.Add(2 * time.Hour)
	if _, err := s2.PollSmart(ctx); err != nil {
		t.Fatal(err)
	}
	if got := heat(e.events(t)); len(got) != 6 || got[5] != "disk.overheat/warning" {
		t.Fatalf("после перезапуска: %v", got)
	}

	// Спящий диск и диск без датчика — без событий.
	e.h.info["d1"] = smart.Info{Supported: true, Standby: true}
	e.now = e.now.Add(2 * time.Hour)
	s2.PollSmart(ctx)
	if got := heat(e.events(t)); len(got) != 6 {
		t.Fatalf("спящий: %v", got)
	}
}

func TestThresholds(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	th, err := e.s.Thresholds(ctx)
	if err != nil || th != DefaultThresholds() || th.HDD != (Limits{55, 65}) {
		t.Fatalf("по умолчанию: %+v %v", th, err)
	}
	for _, bad := range []Thresholds{{HDD: Limits{60, 60}, SSD: Limits{70, 80}}, {HDD: Limits{20, 60}, SSD: Limits{70, 80}},
		{HDD: Limits{50, 60}, SSD: Limits{70, 120}}} {
		if _, err := e.s.SetThresholds(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	want := Thresholds{HDD: Limits{45, 50}, SSD: Limits{60, 75}}
	if _, err := e.s.SetThresholds(ctx, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.Thresholds(ctx); got != want {
		t.Fatalf("сохранено: %+v", got)
	}
	// Новые пороги действуют: 46 °C на HDD — перегрев; NVMe 46 — нет.
	e.h.ids = []string{"h", "n"}
	e.h.info["h"] = smart.Info{Supported: true, TemperatureC: p64(46)}
	e.h.info["n"] = smart.Info{Supported: true, Protocol: "NVMe", TemperatureC: p64(46)}
	e.s.PollSmart(ctx)
	if got := heat(e.events(t)); len(got) != 1 {
		t.Fatalf("события: %v", got)
	}
	// Испорченная запись — пороги по умолчанию и ошибка.
	e.s.set(ctx, keyTemp, "{")
	if got, err := e.s.Thresholds(ctx); err == nil || got != DefaultThresholds() {
		t.Fatalf("испорчено: %+v %v", got, err)
	}
}

func TestHistoryRecordAndPrune(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.h.ids = []string{"d1", "q"}
	e.h.info["d1"] = smart.Info{Supported: true, TemperatureC: p64(30), Reallocated: p64(0)}
	e.h.info["q"] = smart.Info{} // без SMART — точек нет
	start := e.now
	// 100 дней опросов раз в 30 мин.
	for i := 0; i < 100*48; i++ {
		e.setTemp("d1", 30+int64(i%10))
		if _, err := e.s.PollSmart(ctx); err != nil {
			t.Fatal(err)
		}
		e.now = e.now.Add(30 * time.Minute)
	}
	// Свежий опрос карточки сразу после планового — точка не нужна.
	e.now = e.now.Add(-25 * time.Minute)
	if _, err := e.s.ReadDisk(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.PruneHistory(ctx); err != nil {
		t.Fatal(err)
	}
	all, err := e.s.History(ctx, "d1", start)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 || all[0].Time.Before(e.now.Add(-HistoryKeep)) {
		t.Fatalf("старше срока осталось: %v", all[0].Time)
	}
	week := 0
	for _, p := range all {
		if !p.Time.Before(e.now.Add(-HistoryFull)) {
			week++
		}
	}
	// Неделя — все точки (~336), до 90 дней — по одной на 6 ч (~332).
	if week < 330 || week > 340 || len(all)-week < 320 || len(all)-week > 340 {
		t.Fatalf("точек: всего %d, за неделю %d", len(all), week)
	}
	// Прореживание оставляет самую горячую точку корзины.
	for _, p := range all[:10] {
		if *p.TemperatureC != 39 {
			t.Fatalf("в корзине не максимум: %d", *p.TemperatureC)
		}
	}
	if q, _ := e.s.History(ctx, "q", start); len(q) != 0 {
		t.Fatalf("диск без SMART: %d точек", len(q))
	}
	n, _ := e.s.PruneHistory(ctx)
	if n != 0 {
		t.Fatalf("повтор удалил %d", n)
	}
}

func TestSummarize(t *testing.T) {
	th := DefaultThresholds()
	cases := []struct {
		in    smart.Info
		kind  string
		level string
	}{
		{smart.Info{}, "HDD", "unknown"},
		{smart.Info{Supported: true, Standby: true}, "HDD", "unknown"},
		{smart.Info{Supported: true, Passed: pb(true), TemperatureC: p64(40), Reallocated: p64(0)}, "HDD", "ok"},
		{smart.Info{Supported: true, Passed: pb(true), TemperatureC: p64(56)}, "HDD", "warning"},
		{smart.Info{Supported: true, Passed: pb(true), TemperatureC: p64(56)}, "SSD", "ok"},
		{smart.Info{Supported: true, Passed: pb(true), Protocol: "NVMe", TemperatureC: p64(81)}, "", "critical"},
		{smart.Info{Supported: true, Passed: pb(true), Pending: p64(2)}, "HDD", "warning"},
		{smart.Info{Supported: true, Passed: pb(false), Pending: p64(2)}, "HDD", "critical"},
		{smart.Info{Supported: true, Passed: pb(true), LastTest: &smart.SelfTest{Type: "short", Status: "read failure"}}, "HDD", "critical"},
	}
	for i, c := range cases {
		if got := Summarize(c.in, c.kind, th); got.Level != c.level {
			t.Errorf("%d: %s, ожидалось %s (%v)", i, got.Level, c.level, got.Reasons)
		}
	}
	if got := Summarize(cases[7].in, "HDD", th); len(got.Reasons) != 2 {
		t.Errorf("причины: %v", got.Reasons)
	}
}
