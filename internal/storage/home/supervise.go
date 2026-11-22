package home

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gamayun-nas/internal/tasks"
)

// Надзор на стороне gamayund — вызовы helper под сторожем
// задач, короткий срок опроса состояния, остановка без новых
// изменяющих действий, сторож цикла защиты.

// Сроки вызовов helper по умолчанию.
const (
	// DefaultStatusTimeout — опрос состояния (helper: 60 с на операцию):
	// зависший опрос — «неизвестно», а не ожидание срока клиента (35 мин).
	DefaultStatusTimeout = 90 * time.Second
	// DefaultHelperTimeout — изменяющая операция (клиент gamayund — 35 мин).
	DefaultHelperTimeout = 35 * time.Minute
)

// ErrStopping — gamayund останавливается: новые изменяющие действия не
// начинаются, откат выполнит следующий запуск.
var ErrStopping = errors.New("gamayund останавливается: изменяющие действия не начинаются, незавершённое разберёт следующий запуск")

// readOnlyOps — операции helper, которые ничего не меняют.
var readOnlyOps = map[string]bool{OpStatus: true, OpScrubStatus: true, "disks.observe": true, "system.info": true}

// stopSafeOps — изменяющие операции, допустимые при остановке gamayund:
// остановка проверки пула только прекращает чтение и нужна, чтобы
// проверка не шла в ядре без наблюдения после выхода gamayund.
var stopSafeOps = map[string]bool{OpScrubCancel: true}

// guardedHelper — вызовы helper из Service: отказ изменяющих при
// остановке, срок опроса состояния, признак жизни задачи на время вызова.
type guardedHelper struct {
	h HelperClient
	s *Service
}

func (g guardedHelper) Call(ctx context.Context, op string, params, result any) error {
	limit := g.s.HelperTimeout
	if limit <= 0 {
		limit = DefaultHelperTimeout
	}
	if readOnlyOps[op] {
		limit = g.s.StatusTimeout
		if limit <= 0 {
			limit = DefaultStatusTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	} else if g.s.stopping.Load() && !stopSafeOps[op] {
		return fmt.Errorf("%s: %w", op, ErrStopping)
	}
	// Вызов может молчать до своего срока — сторож задач ждёт его конца
	// с запасом; зависание дольше — уже отказ helper/клиента.
	tasks.Expect(ctx, limit+time.Minute, "helper: "+op)
	err := g.h.Call(ctx, op, params, result)
	if err == nil {
		tasks.Alive(ctx, "helper: "+op+" — ответ получен")
	}
	return err
}

// Stop — gamayund останавливается (вызывать до tasks.Manager.Shutdown):
// изменяющие вызовы helper больше не начинаются; прерванное создание пула
// остаётся записью «создаётся» и разбирается при старте (resolveCreating) —
// у пула один владелец (иначе старый процесс откатывал параллельно новому).
// Уже идущая операция helper доводится им самим: изменяющие операции helper
// выполняет по одной (Executor.mu), следующий запуск дождётся её.
func (s *Service) Stop() { s.stopping.Store(true) }

// Stopping — вызван Stop.
func (s *Service) Stopping() bool { return s.stopping.Load() }

// ---------- Сторож цикла защиты ----------

// protWatch — отметки цикла защиты.
type protWatch struct {
	mu       sync.Mutex
	started  bool
	lastPass time.Time // конец последнего прохода
	staleFor time.Duration
	stale    string // непусто — проход просрочен
	reported bool
}

// protStaleAfter — просрочка прохода: три интервала, не меньше 2 мин
// (проход с повторной сборкой деградированного пула идёт до минут).
func protStaleAfter(interval time.Duration) time.Duration {
	return max(3*interval, 2*time.Minute)
}

func (s *Service) protStart(interval time.Duration) {
	w := &s.protW
	w.mu.Lock()
	defer w.mu.Unlock()
	w.started, w.lastPass, w.staleFor = true, s.now(), protStaleAfter(interval)
	if s.ProtectionStaleAfter > 0 {
		w.staleFor = s.ProtectionStaleAfter
	}
}

func (s *Service) protPassed() {
	w := &s.protW
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastPass, w.stale, w.reported = s.now(), "", false
}

// protCheck — проверка сторожа; возвращает непустую причину один раз на
// каждый эпизод просрочки (для события).
func (s *Service) protCheck() string {
	w := &s.protW
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.started {
		return ""
	}
	if late := s.now().Sub(w.lastPass); late > w.staleFor {
		w.stale = fmt.Sprintf("проход защиты пулов не завершён %s (предел %s): состояние пулов не проверяется, read-only при отказе может не включиться",
			late.Round(time.Second), w.staleFor)
		if !w.reported {
			w.reported = true
			return w.stale
		}
	}
	return ""
}

// ProtectionStale — непусто, если цикл защиты просрочен.
func (s *Service) ProtectionStale() string {
	s.protCheck()
	w := &s.protW
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stale
}

// markUnknown — состояние пула неизвестно: health unknown, запас
// избыточности не определён.
func markUnknown(v *PoolView, why string) {
	if v.Status != nil {
		v.Status.Health = HealthUnknown
		v.Status.Unknown = append(v.Status.Unknown, why)
		v.Status.Issues = append(v.Status.Issues, why)
	}
	if v.Protection.RedundancyExhausted != nil && !*v.Protection.RedundancyExhausted {
		v.Protection.RedundancyExhausted = nil
	}
}

func unknownReason(st PoolStatus) string {
	return "состояние не определено: " + strings.Join(st.Unknown, "; ")
}
