package snapshots

import (
	"fmt"
	"sort"
	"time"
)

// Policy — расписание и хранение снапшотов папки. Снапшот по расписанию
// создаётся с шагом самого частого непустого уровня (каждый час, если
// Hourly > 0; иначе раз в день, в неделю, в месяц). Ротация (как restic
// forget --keep-*): из снапшотов по расписанию сохраняется самый новый в
// каждом из последних Hourly часов, Daily дней, Weekly недель и Monthly
// месяцев (объединение); остальные по расписанию удаляются. Ручные не
// удаляются никогда.
type Policy struct {
	Enabled bool `json:"enabled"`
	Hourly  int  `json:"hourly"`
	Daily   int  `json:"daily"`
	Weekly  int  `json:"weekly"`
	Monthly int  `json:"monthly"`
}

// DefaultPolicy — по умолчанию включено: 24 часовых, 7 дневных, 4 недельных,
// 6 месячных.
func DefaultPolicy() Policy {
	return Policy{Enabled: true, Hourly: 24, Daily: 7, Weekly: 4, Monthly: 6}
}

// Пределы уровней хранения.
var policyMax = [4]int{24 * 14, 366, 260, 120}

// Validate — пределы значений.
func (p Policy) Validate() error {
	vals := [4]int{p.Hourly, p.Daily, p.Weekly, p.Monthly}
	names := [4]string{"hourly", "daily", "weekly", "monthly"}
	sum := 0
	for i, v := range vals {
		if v < 0 || v > policyMax[i] {
			return fmt.Errorf("%s — от 0 до %d", names[i], policyMax[i])
		}
		sum += v
	}
	if p.Enabled && sum == 0 {
		return fmt.Errorf("расписание включено, но не задан ни один уровень хранения")
	}
	return nil
}

// Шаги расписания.
const (
	periodHour = iota
	periodDay
	periodWeek
	periodMonth
)

// period — самый частый уровень с ненулевым хранением; -1 — нет.
func (p Policy) period() int {
	for i, v := range [4]int{p.Hourly, p.Daily, p.Weekly, p.Monthly} {
		if v > 0 {
			return i
		}
	}
	return -1
}

// bucket — интервал времени t уровня period в часовом поясе loc.
func bucket(t time.Time, period int, loc *time.Location) string {
	t = t.In(loc)
	switch period {
	case periodHour:
		return t.Format("2006-01-02T15")
	case periodDay:
		return t.Format("2006-01-02")
	case periodWeek:
		y, w := t.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	default:
		return t.Format("2006-01")
	}
}

// Due — пора ли снапшоту по расписанию: снапшотов по расписанию ещё не было
// или последний — в прошлом интервале шага.
func (p Policy) Due(now time.Time, last time.Time, hasLast bool, loc *time.Location) bool {
	per := p.period()
	if !p.Enabled || per < 0 {
		return false
	}
	if !hasLast {
		return true
	}
	return now.After(last) && bucket(now, per, loc) != bucket(last, per, loc)
}

// Expired — имена снапшотов по расписанию (Auto), которые ротация удаляет.
// Ручные в расчёт не входят и не удаляются. При выключенном расписании —
// ничего (имеющиеся снапшоты остаются, удалить их можно вручную).
func (p Policy) Expired(snaps []Snapshot, loc *time.Location) []string {
	if !p.Enabled || p.period() < 0 {
		return nil
	}
	var auto []Snapshot
	for _, s := range snaps {
		if s.Auto {
			auto = append(auto, s)
		}
	}
	sort.Slice(auto, func(i, j int) bool { return auto[i].Time.After(auto[j].Time) })
	keep := make([]bool, len(auto))
	for per, n := range [4]int{p.Hourly, p.Daily, p.Weekly, p.Monthly} {
		last, kept := "", 0
		for i, s := range auto {
			if kept >= n {
				break
			}
			if b := bucket(s.Time, per, loc); b != last {
				keep[i], last = true, b
				kept++
			}
		}
	}
	var out []string
	for i, s := range auto {
		if !keep[i] {
			out = append(out, s.Name)
		}
	}
	return out
}
