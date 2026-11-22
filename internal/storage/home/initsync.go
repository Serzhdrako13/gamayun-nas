package home

import (
	"fmt"
	"sync"
	"time"
)

// Начальная синхронизация нового пула — состояние пула, а не задача.
// Создание завершается, как только пул смонтирован; дальше ядро в фоне
// пересчитывает теги dm-integrity и синхронизирует md. Здесь — этап,
// общий процент и прогноз для карточки пула и текстов отказов.

// Этапы начальной синхронизации.
const (
	SyncStageIntegrity = "integrity" // пересчёт тегов dm-integrity (и md в фоне)
	SyncStageMirror    = "mirror"    // синхронизация md
)

// SyncProgress — ход начальной синхронизации пула.
type SyncProgress struct {
	Stage   string  `json:"stage"`
	Percent float64 `json:"percent"`
	// EtaMin — прогноз до полной защиты, мин; nil — ещё не оценить
	// (нужны два замера скорости пересчёта).
	EtaMin *float64 `json:"eta_min,omitempty"`
	// NoRedundancy — массив собирается достройкой диска (raid5/6: recovery
	// при создании): до окончания отказ любого полного диска — потеря данных
	// (стенд 2026-11-19). У raid1/10 записанное уже есть на всех копиях.
	NoRedundancy bool `json:"no_redundancy,omitempty"`
}

// syncSample — замер пересчёта для скорости.
type syncSample struct {
	at    time.Time
	done  int64   // сумма пересчитанного по всем отображениям, байт
	speed float64 // байт/с на одно отображение; 0 — неизвестна
}

// syncSampleMin — минимальный интервал между замерами скорости.
const syncSampleMin = 30 * time.Second

type syncMeter struct {
	mu sync.Mutex
	m  map[string]syncSample
}

// initialSyncProgress заполняет st.Sync для пула в начальной синхронизации.
//
// Модель прогноза (по замеру 2026-11-19):
// пока идёт пересчёт тегов, md уступает ему ввод-вывод (speed_limit_min), и
// оценка mdstat (finish=…) многократно завышена. Поэтому, пока пересчёт не
// закончен, остаток считается как «пересчёт + оставшаяся часть md» на
// скорости пересчёта (оба прохода — последовательное чтение того же диска).
// После пересчёта — оценка mdstat, она тогда честная.
func (s *Service) initialSyncProgress(st *PoolStatus) {
	if !st.InitialSync {
		s.meter.mu.Lock()
		delete(s.meter.m, st.Pool)
		s.meter.mu.Unlock()
		return
	}
	var done, total int64
	var n int
	for _, m := range st.Integrity {
		if m.RecalcBytes == nil {
			continue
		}
		n++
		total += m.ProvidedBytes
		if !m.Recalculating {
			done += m.ProvidedBytes
		} else {
			done += min(*m.RecalcBytes, m.ProvidedBytes)
		}
	}
	recalc := 1.0
	if total > 0 {
		recalc = float64(done) / float64(total)
	}
	var mdSum, mdRem float64
	var arrays int
	var finish *float64
	noRed := false
	for _, a := range st.Arrays {
		arrays++
		f := 1.0
		if initialSyncArray(a) {
			if a.Action == "recovery" {
				noRed = true
			}
			f = 0
			if a.Progress != nil {
				f = *a.Progress / 100
			}
			mdRem = max(mdRem, (1-f)*float64(a.DevSizeBytes))
			if a.FinishMin != nil && (finish == nil || *a.FinishMin > *finish) {
				finish = a.FinishMin
			}
		}
		mdSum += f
	}
	md := 1.0
	if arrays > 0 {
		md = mdSum / float64(arrays)
	}
	p := &SyncProgress{Stage: SyncStageMirror, Percent: 100 * md, NoRedundancy: noRed}
	if n > 0 {
		p.Percent = 100 * (recalc + md) / 2
	}
	if recalc < 1 {
		p.Stage = SyncStageIntegrity
		now := s.now()
		s.meter.mu.Lock()
		if s.meter.m == nil {
			s.meter.m = map[string]syncSample{}
		}
		prev, ok := s.meter.m[st.Pool]
		switch {
		case !ok || done < prev.done:
			s.meter.m[st.Pool] = syncSample{at: now, done: done}
		case now.Sub(prev.at) >= syncSampleMin:
			v := float64(done-prev.done) / now.Sub(prev.at).Seconds() / float64(n)
			s.meter.m[st.Pool] = syncSample{at: now, done: done, speed: v}
			prev.speed = v
		}
		s.meter.mu.Unlock()
		if prev.speed > 0 {
			rem := float64(total-done)/float64(n) + mdRem
			eta := rem / prev.speed / 60
			p.EtaMin = &eta
		}
	} else {
		p.EtaMin = finish
	}
	st.Sync = p
}

// syncBusy — текст отказа операции во время начальной синхронизации;
// пусто — пул не в начальной синхронизации.
func syncBusy(pool string, st PoolStatus, op string) string {
	if !st.InitialSync {
		return ""
	}
	msg := fmt.Sprintf("пул %s: идёт начальная синхронизация", pool)
	if st.Sync != nil {
		msg += fmt.Sprintf(" (%.0f %%", st.Sync.Percent)
		if st.Sync.EtaMin != nil {
			msg += ", осталось " + etaText(*st.Sync.EtaMin)
		}
		msg += ")"
	}
	return msg + "; " + op + " — после её окончания"
}

// etaText — «~3 ч 20 мин» / «~15 мин».
func etaText(min float64) string {
	m := int(min + 0.5)
	if m >= 60 {
		return fmt.Sprintf("~%d ч %d мин", m/60, m%60)
	}
	return fmt.Sprintf("~%d мин", max(m, 1))
}
