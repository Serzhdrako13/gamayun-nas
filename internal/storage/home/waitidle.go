package home

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gamayun-nas/internal/tasks"
	"gamayun-nas/internal/watch"
)

// MinSyncBytesPerSec — минимальная скорость перестройки для общего срока
// ожидания: срок = 1 ч + наибольший участник / скорость.
const MinSyncBytesPerSec = 5 << 20

// waitIdle ждёт окончания reshape/resync/recovery всех массивов пула,
// пересчёта тегов dm-integrity, полного состава и условия done (nil — без
// него); прогресс — от lo до hi по наименьшему проценту.
//
// Ожидание через internal/watch — процесс жив, только пока растёт
// позиция действия массива или граница пересчёта на живом устройстве;
// без продвижения за DegradedWait — попытка сборки, второе окно, ошибка с
// причиной. Неизвестное состояние (ошибка опроса, непрочитанный массив,
// непроверенная живость диска) — не «работает», а отдельный исход.
func (s *Service) waitIdle(ctx context.Context, pool string, progress func(int), lo, hi int, done func(PoolStatus) bool) (PoolStatus, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var fatal error
	var last PoolStatus
	var deadline time.Duration
	// Первый опрос — и для общего срока, и первым наблюдением.
	first, firstErr := s.status(ctx, pool)
	probed := false
	spec := watch.Spec{
		Name:        "пул " + pool,
		Poll:        s.Poll,
		StallWindow: s.DegradedWait,
		Recover:     func(ctx context.Context) { s.reassemble(ctx, pool) },
		Probe: func(ctx context.Context) watch.Observation {
			st, err := first, firstErr
			if probed {
				st, err = s.status(ctx, pool)
			}
			probed = true
			if err != nil {
				return watch.Observation{Err: err}
			}
			last = st
			if st.Health == HealthUnknown {
				// Неполные данные — «неизвестно», не «работает».
				return watch.Observation{Err: errors.New(unknownReason(st))}
			}
			if st.Health == HealthFailed || st.Health == HealthMissing {
				fatal = fmt.Errorf("пул %s в состоянии %s: %s", pool, st.Health, strings.Join(st.Issues, "; "))
				cancel(fatal)
				return watch.Observation{Err: fatal}
			}
			o := poolObservation(st, done)
			if o.Target && st.Health != HealthOK {
				fatal = fmt.Errorf("пул %s: перестройка завершена, но состояние %s", pool, st.Health)
				cancel(fatal)
				o.Target = false
			}
			return o
		},
		OnObserve: func(o watch.Observation, changed bool) {
			// Наблюдатель жив — признак жизни задачи с наблюдением
			// (застой и «неизвестно» судит сам наблюдатель по окнам).
			tasks.Alive(ctx, "пул "+pool+": "+o.String())
			if o.Err == nil {
				progress(lo + int(float64(hi-lo)*poolPercent(last)/100))
			}
			if changed && s.OnWatch != nil {
				s.OnWatch("пул "+pool, o.String())
			}
		},
	}
	// Общий срок — от объёма (бесконечного ожидания нет).
	var maxDev int64
	for _, a := range first.Arrays {
		maxDev = max(maxDev, a.DevSizeBytes)
	}
	for _, m := range first.Integrity {
		maxDev = max(maxDev, m.ProvidedBytes)
	}
	deadline = time.Hour + time.Duration(maxDev/MinSyncBytesPerSec)*time.Second
	spec.Deadline = max(deadline, watch.DefaultDeadline)
	out := watch.Run(ctx, spec)
	if fatal != nil {
		return last, fatal
	}
	if out.Result == watch.ResultCancelled && ctx.Err() != nil {
		if c := context.Cause(ctx); c != nil && !errors.Is(c, context.Canceled) {
			return last, c
		}
		return last, ctx.Err()
	}
	return last, out.Err()
}

// poolObservation — наблюдение за перестройкой пула: элемент на массив и
// на отображение dm-integrity; дополнительный элемент «цель», если массивы
// в покое, но целевой состав/размер не достигнут.
func poolObservation(st PoolStatus, done func(PoolStatus) bool) watch.Observation {
	var o watch.Observation
	idle := true
	for _, a := range st.Arrays {
		e := watch.Element{ID: "массив " + a.Name, State: watch.Done, Pos: -1, Alive: true}
		switch {
		case a.RaidDevices == 0 || a.Level == "":
			e.State, e.Detail = watch.Unknown, "состояние массива не прочитано"
		case a.Action != "":
			e.State, e.Detail = watch.Working, a.Action
			switch {
			case a.Delayed:
				e.Detail += " ожидает очереди"
			case a.SyncPos != nil:
				e.Pos = *a.SyncPos
			case a.Progress != nil:
				e.Pos = int64(*a.Progress * 1000)
			}
		case a.Missing > 0:
			// Участник выпал, синхронизации нет — сам не восстановится.
			e.State, e.Alive = watch.Working, false
			e.Detail = fmt.Sprintf("не хватает дисков: %d, синхронизация не идёт (диск не вернулся)", a.Missing)
		}
		if e.State != watch.Done {
			idle = false
		}
		o.Elements = append(o.Elements, e)
	}
	for _, m := range st.Integrity {
		if !m.Recalculating {
			continue
		}
		idle = false
		e := watch.Element{ID: "integrity " + m.Name, State: watch.Working, Pos: -1, Alive: !m.Lost, Detail: "пересчёт тегов"}
		if m.RecalcBytes != nil {
			e.Pos = *m.RecalcBytes
		}
		switch {
		case m.LiveErr != "":
			e.State, e.Detail = watch.Unknown, "живость устройства не проверена: "+m.LiveErr
		case m.Lost:
			e.Detail += ": " + m.LostWhy
		}
		o.Elements = append(o.Elements, e)
	}
	if idle && done != nil && !done(st) {
		// Массивы в покое, но цель не достигнута: reshape ещё не начался
		// или не запустился вовсе — без продвижения истечёт окно.
		o.Elements = append(o.Elements, watch.Element{ID: "цель", State: watch.Working, Pos: -1, Alive: true,
			Detail: "массивы в покое, но не достигли целевого состава и размера"})
		idle = false
	}
	o.Target = idle
	return o
}

// poolPercent — наименьший процент незавершённых процессов пула.
func poolPercent(st PoolStatus) float64 {
	pct := 100.0
	low := func(p *float64) {
		if p == nil {
			pct = 0
		} else if *p < pct {
			pct = *p
		}
	}
	for _, a := range st.Arrays {
		if a.Action != "" || a.Missing > 0 {
			low(a.Progress)
		}
	}
	for _, m := range st.Integrity {
		if m.Recalculating {
			low(m.RecalcProgress())
		}
	}
	return pct
}

// stalledDegraded — непустое описание, если у какого-то массива нет
// участников, а синхронизации/reshape и пересчёта тегов на живом
// устройстве нет (md ждёт отсутствующий диск; пересчёт на пропавшем
// диске не идёт).
func stalledDegraded(st PoolStatus) string {
	var lost []string
	for _, a := range st.Arrays {
		if a.Action != "" {
			return ""
		}
		if a.Missing > 0 {
			lost = append(lost, fmt.Sprintf("массив %s: не хватает дисков: %d", a.Name, a.Missing))
		}
	}
	for _, m := range st.Integrity {
		if m.Recalculating && !m.Lost {
			return ""
		}
	}
	return strings.Join(lost, "; ")
}
