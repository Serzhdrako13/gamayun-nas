package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Список действующих блокировок веб-входа и ручная разблокировка
// администратором. Счётчики — в памяти (см. limiter): перезапуск gamayund
// по-прежнему снимает все блокировки.

// Lockout — действующая блокировка.
type Lockout struct {
	// ID — непрозрачный идентификатор (хэш ключа) для разблокировки.
	ID string `json:"id"`
	// Scope — ip | account | device.
	Scope string `json:"scope"`
	// Address — адрес или сеть /64 (только для ip).
	Address string `json:"address,omitempty"`
	// User — имя существующей учётки (account, device). Пусто для
	// несуществующего имени — его текст не показывается (может быть паролем).
	User        string    `json:"user,omitempty"`
	UnknownUser bool      `json:"unknown_user,omitempty"`
	Fails       int       `json:"fails"`
	LastFailure time.Time `json:"last_failure"`
	Until       time.Time `json:"until"`
}

func lockoutID(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:8])
}

type limScope struct {
	scope string
	lim   *limiter
}

func (s *Service) limiters() []limScope {
	return []limScope{{"ip", s.ipLim}, {"account", s.acctLim}, {"device", s.devLim}}
}

// Lockouts — действующие блокировки, раньше истекающие — последними.
func (s *Service) Lockouts() []Lockout {
	out := []Lockout{}
	for _, ls := range s.limiters() {
		for _, e := range ls.lim.locked() {
			l := Lockout{ID: lockoutID(e.key), Scope: ls.scope, User: e.user, Fails: e.fails,
				LastFailure: e.last.UTC(), Until: e.until.UTC()}
			if ls.scope == "ip" {
				l.Address = strings.TrimPrefix(e.key, "ip:")
			}
			l.UnknownUser = ls.scope == "account" && e.user == ""
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Until.After(out[j].Until) })
	return out
}

// Unlock снимает блокировку по ID (сбрасывает счётчик целиком). Не найдено
// (истекла или неверный ID) — false.
func (s *Service) Unlock(id string) (Lockout, bool) {
	for _, l := range s.Lockouts() {
		if l.ID != id {
			continue
		}
		for _, ls := range s.limiters() {
			if ls.scope != l.Scope {
				continue
			}
			for _, e := range ls.lim.locked() {
				if lockoutID(e.key) == id {
					ls.lim.Reset(e.key)
				}
			}
		}
		return l, true
	}
	return Lockout{}, false
}

func (s *Service) listLockouts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"lockouts": s.Lockouts(), "persistent": false})
}

func (s *Service) unlock(w http.ResponseWriter, r *http.Request) {
	l, ok := s.Unlock(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "блокировки нет (истекла или уже снята)")
		return
	}
	s.unlocked(r, l)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) unlockAll(w http.ResponseWriter, r *http.Request) {
	ls := s.Lockouts()
	for _, l := range ls {
		if l, ok := s.Unlock(l.ID); ok {
			s.unlocked(r, l)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"unlocked": len(ls)})
}

// unlocked — журнал gamayund и событие auth.unlock.
func (s *Service) unlocked(r *http.Request, l Lockout) {
	actor := authFrom(r).user.Name
	data := map[string]any{"scope": l.Scope, "actor": actor, "fails": l.Fails}
	if l.Address != "" {
		data["remote"] = l.Address
	}
	if l.User != "" {
		data["user"] = l.User
	}
	s.o.Logger.Info("блокировка входа снята вручную", "scope", l.Scope, "remote", l.Address, "user", l.User, "actor", actor)
	s.emit(EventUnlock, data)
}
