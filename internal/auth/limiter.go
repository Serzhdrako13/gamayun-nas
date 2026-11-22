package auth

import (
	"net"
	"sync"
	"time"
)

// LimitPolicy — правило ограничения попыток: первые Free неудач без
// блокировки, затем после каждой неудачи блокировка Base·2^(n−Free−1), но не
// дольше Max. Счётчик сбрасывается успешным входом или через Window без
// неудач. Попытка во время блокировки отклоняется и счётчик не увеличивает:
// блокировка всегда временная и сама истекает.
type LimitPolicy struct {
	Free   int
	Base   time.Duration
	Max    time.Duration
	Window time.Duration
}

// Политики по умолчанию.
var (
	// IPPolicy — по адресу клиента (IPv6 — по префиксу /64).
	IPPolicy = LimitPolicy{Free: 10, Base: time.Second, Max: 15 * time.Minute, Window: time.Hour}
	// AccountPolicy — по имени учётной записи (в том числе несуществующей,
	// чтобы ответы не выдавали, какие имена есть).
	AccountPolicy = LimitPolicy{Free: 5, Base: time.Second, Max: 15 * time.Minute, Window: time.Hour}
	// DevicePolicy — по cookie доверенного устройства (см. Service.login).
	DevicePolicy = LimitPolicy{Free: 5, Base: time.Second, Max: 15 * time.Minute, Window: time.Hour}
)

// maxLimiterEntries — предел записей одного ограничителя (память).
const maxLimiterEntries = 10000

type limitEntry struct {
	fails int
	last  time.Time // последняя неудача
	until time.Time // блокировка до
	user  string    // имя существующей учётки (для списка в вебе), иначе пусто
}

// limiter — счётчики неудач в памяти. Перезапуск демона их сбрасывает
// (это и есть способ снять блокировку локально).
type limiter struct {
	mu     sync.Mutex
	policy LimitPolicy
	now    func() time.Time
	m      map[string]*limitEntry
}

func newLimiter(p LimitPolicy, now func() time.Time) *limiter {
	return &limiter{policy: p, now: now, m: map[string]*limitEntry{}}
}

func (l *limiter) get(key string) *limitEntry {
	e := l.m[key]
	if e != nil && l.now().Sub(e.last) > l.policy.Window && !l.now().Before(e.until) {
		delete(l.m, key)
		return nil
	}
	return e
}

// Blocked — сколько ещё длится блокировка ключа (0 — не заблокирован).
func (l *limiter) Blocked(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.get(key); e != nil {
		if d := e.until.Sub(l.now()); d > 0 {
			return d
		}
	}
	return 0
}

// Fail учитывает неудачу; возвращает длительность новой блокировки (0 — нет).
func (l *limiter) Fail(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.get(key)
	if e == nil {
		if len(l.m) >= maxLimiterEntries {
			l.purge(now)
		}
		if len(l.m) >= maxLimiterEntries {
			// Таблица заполнена живыми записями: новый ключ не учитываем
			// (остаётся ограничение по адресу). Существующие записи, в том
			// числе действующие блокировки, не вытесняются.
			return 0
		}
		e = &limitEntry{}
		l.m[key] = e
	}
	e.fails++
	e.last = now
	over := e.fails - l.policy.Free
	if over <= 0 {
		return 0
	}
	d := l.policy.Base
	for i := 1; i < over && d < l.policy.Max; i++ {
		d *= 2
	}
	if d > l.policy.Max {
		d = l.policy.Max
	}
	e.until = now.Add(d)
	return d
}

// setUser запоминает имя существующей учётной записи для ключа (список
// блокировок в вебе). Несуществующие имена не хранятся в открытом виде:
// в поле имени бывает введён пароль.
func (l *limiter) setUser(key, user string) {
	l.mu.Lock()
	if e := l.m[key]; e != nil {
		e.user = user
	}
	l.mu.Unlock()
}

// lockedEntry — действующая блокировка (для списка).
type lockedEntry struct {
	key   string
	fails int
	last  time.Time
	until time.Time
	user  string
}

// locked — ключи, заблокированные сейчас.
func (l *limiter) locked() []lockedEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var out []lockedEntry
	for k, e := range l.m {
		if now.Before(e.until) {
			out = append(out, lockedEntry{key: k, fails: e.fails, last: e.last, until: e.until, user: e.user})
		}
	}
	return out
}

// Reset сбрасывает счётчик ключа.
func (l *limiter) Reset(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}

func (l *limiter) purge(now time.Time) {
	for k, e := range l.m {
		if now.Sub(e.last) > l.policy.Window && !now.Before(e.until) {
			delete(l.m, k)
		}
	}
}

// ipKey — ключ адреса клиента: IPv4 целиком, IPv6 — префикс /64 (у одного
// клиента обычно вся подсеть /64).
func ipKey(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
