// Package watch — наблюдатель за долгим процессом.
//
// Принцип (VISION 2026-11-13): неизвестное состояние недопустимо и не
// трактуется ни как «работает», ни как «всё хорошо». Процесс считается
// живым, только пока доказано продвижение к цели: растёт счётчик прогресса
// элемента, устройство под которым живо. Если за окно продвижения нет или
// состояние неизвестно, наблюдатель перепроверяет (подтверждающий опрос,
// одна попытка восстановления, второе окно) и только потом возвращает
// исход с причиной и последним наблюдением.
package watch

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// State — состояние элемента процесса.
type State int

const (
	Unknown State = iota // прочитать не удалось или вывод непонятен
	Working              // процесс идёт
	Done                 // элемент достиг цели
)

func (s State) String() string {
	switch s {
	case Working:
		return "работает"
	case Done:
		return "готово"
	}
	return "неизвестно"
}

// Element — наблюдение за одной частью процесса (массив md, отображение
// dm-integrity, устройство).
type Element struct {
	ID    string
	State State
	// Pos — счётчик прогресса (сектор, байт); растёт при продвижении.
	// Отрицательный — неизвестен.
	Pos int64
	// Alive — устройство под элементом есть и исправно. Работа на
	// мёртвом устройстве не продвигает процесс.
	Alive  bool
	Detail string
}

// Observation — результат одного опроса.
type Observation struct {
	Elements []Element
	// Target — цель процесса достигнута (решает вызывающий: например, все
	// массивы без действия и без недостающих участников).
	Target bool
	// Err — опрос не удался целиком: состояние неизвестно.
	Err error
}

// known — наблюдение полностью известно.
func (o Observation) known() bool {
	if o.Err != nil {
		return false
	}
	for _, e := range o.Elements {
		if e.State == Unknown {
			return false
		}
	}
	return true
}

// String — краткое описание для причины и журнала.
func (o Observation) String() string {
	if o.Err != nil {
		return "опрос не удался: " + o.Err.Error()
	}
	parts := make([]string, 0, len(o.Elements))
	for _, e := range o.Elements {
		p := fmt.Sprintf("%s: %s", e.ID, e.State)
		if e.Pos >= 0 && e.State == Working {
			p += fmt.Sprintf(" @%d", e.Pos)
		}
		if !e.Alive {
			p += ", устройство недоступно"
		}
		if e.Detail != "" {
			p += " (" + e.Detail + ")"
		}
		parts = append(parts, p)
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// Result — вид исхода.
type Result int

const (
	ResultDone      Result = iota // цель достигнута
	ResultStalled                 // нет продвижения (подтверждено перепроверкой)
	ResultUnknown                 // состояние неизвестно (подтверждено перепроверкой)
	ResultDeadline                // истёк общий срок
	ResultCancelled               // отмена контекста
)

func (r Result) String() string {
	return [...]string{"готово", "нет продвижения", "состояние неизвестно", "истёк срок", "отменено"}[r]
}

// Outcome — исход наблюдения.
type Outcome struct {
	Result Result
	Reason string // подробная причина для failed и журнала
	Last   Observation
}

// OK — цель достигнута.
func (o Outcome) OK() bool { return o.Result == ResultDone }

// Err — nil при успехе, иначе ошибка с причиной.
func (o Outcome) Err() error {
	if o.OK() {
		return nil
	}
	return fmt.Errorf("%s", o.Reason)
}

// DefaultDeadline — общий срок, если вызывающий его не задал: колеблющийся
// счётчик может долго изображать продвижение, бесконечного наблюдения нет.
const DefaultDeadline = 24 * time.Hour

// Spec — что и как наблюдать.
type Spec struct {
	Name  string
	Probe func(ctx context.Context) Observation
	// Recover — одна попытка восстановления перед вторым окном
	// (например, повторная сборка пула). Необязательна.
	Recover func(ctx context.Context)
	// OnObserve вызывается после каждого опроса (прогресс задачи,
	// признак жизни). changed — наблюдение отличается от предыдущего
	// (для журнала).
	OnObserve func(o Observation, changed bool)

	Poll          time.Duration // период опроса (по умолчанию 2 с)
	StallWindow   time.Duration // окно без продвижения (по умолчанию 60 с)
	UnknownWindow time.Duration // окно неизвестного состояния (по умолчанию = StallWindow)
	Deadline      time.Duration // общий срок; 0 — DefaultDeadline (бесконечного наблюдения нет)

	// Часы; nil — реальные. Для тестов.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

func (s *Spec) defaults() {
	if s.Poll <= 0 {
		s.Poll = 2 * time.Second
	}
	if s.StallWindow <= 0 {
		s.StallWindow = 60 * time.Second
	}
	if s.UnknownWindow <= 0 {
		s.UnknownWindow = s.StallWindow
	}
	if s.Deadline <= 0 {
		s.Deadline = DefaultDeadline
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Sleep == nil {
		s.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
}

// Run наблюдает процесс до исхода. Гарантии (проверяются генеративным
// тестом): завершается не позже 2×max(окно)+2×Poll после последнего
// продвижения и не позже Deadline+Poll; ResultDone — только при известном
// наблюдении с Target.
func Run(ctx context.Context, s Spec) Outcome {
	s.defaults()
	start := s.Now()
	lastPos := map[string]int64{}
	lastState := map[string]State{}
	doneOnce := map[string]bool{}
	lastProgress := start
	var unknownSince time.Time
	var prev string
	var escalatedAt time.Time // начало второго окна (после перепроверки)
	second := max(s.StallWindow, s.UnknownWindow)
	recovered := false // попытка восстановления сделана (одна на наблюдение)
	var last Observation

	fail := func(r Result, why string) Outcome {
		return Outcome{Result: r, Reason: fmt.Sprintf("%s: %s; последнее состояние: %s", s.Name, why, last), Last: last}
	}

	for {
		if err := ctx.Err(); err != nil {
			return fail(ResultCancelled, err.Error())
		}
		last = s.Probe(ctx)
		now := s.Now()
		str := last.String()
		if s.OnObserve != nil {
			s.OnObserve(last, str != prev)
		}
		prev = str

		known := last.known()
		if known && last.Target {
			return Outcome{Result: ResultDone, Last: last}
		}

		progressed := false
		if known {
			unknownSince = time.Time{}
			for _, e := range last.Elements {
				ps, seen := lastState[e.ID]
				lastState[e.ID] = e.State
				if e.State == Done {
					if seen && ps != Done && !doneOnce[e.ID] {
						progressed = true // элемент дошёл до цели (один раз)
					}
					doneOnce[e.ID] = true
					continue
				}
				if !seen || ps != Working {
					// новый или заново запущенный процесс элемента —
					// точка отсчёта (resync после сборки идёт с начала)
					delete(lastPos, e.ID)
				}
				if e.State != Working || e.Pos < 0 {
					continue
				}
				// Продвижение — только превышение достигнутого максимума и
				// только на живом устройстве: колебания счётчика и рост на
				// мёртвом устройстве не продлевают жизнь процессу.
				// Максимум учитывается и на мёртвом — «отложенного» роста нет.
				if p, ok := lastPos[e.ID]; !ok {
					lastPos[e.ID] = e.Pos
				} else if e.Pos > p {
					lastPos[e.ID] = e.Pos
					progressed = progressed || e.Alive
				}
			}
		} else if unknownSince.IsZero() {
			unknownSince = now
		}
		if progressed {
			lastProgress = now
			escalatedAt = time.Time{}
		}

		if now.Sub(start) >= s.Deadline {
			return fail(ResultDeadline, fmt.Sprintf("истёк общий срок %s", s.Deadline))
		}

		// Часы застоя идут всегда (и при неизвестном состоянии): срок исхода
		// после последнего продвижения ограничен окном + вторым окном.
		res, why := ResultStalled, fmt.Sprintf("нет продвижения дольше %s", s.StallWindow)
		if !known {
			res, why = ResultUnknown, fmt.Sprintf("состояние неизвестно (с %s)", unknownSince.Format(time.TimeOnly))
		}
		if !escalatedAt.IsZero() {
			if now.Sub(escalatedAt) >= second {
				return fail(res, why+"; перепроверка и попытка восстановления не помогли")
			}
		} else if now.Sub(lastProgress) >= s.StallWindow || !known && now.Sub(unknownSince) >= s.UnknownWindow {
			// Перепроверка: попытка восстановления (одна), сразу
			// подтверждающий опрос, затем второе окно.
			escalatedAt = now
			if s.Recover != nil && !recovered {
				recovered = true
				s.Recover(ctx)
			}
			continue
		}

		if err := s.Sleep(ctx, s.Poll); err != nil {
			return fail(ResultCancelled, err.Error())
		}
	}
}
