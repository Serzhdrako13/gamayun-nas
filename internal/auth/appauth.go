package auth

// Вход в веб-приложения на поддоменах (Caddy forward_auth).
//
// Cookie сессии NAS (__Host-dn_session) поддоменам не передаётся: префикс
// __Host- запрещает Domain, и так задумано — контейнер на поддомене не должен
// получать сессию администратора. Вход в приложение:
//
//  1. Caddy спрашивает gamayund (Verify, unix-сокет). Нет cookie приложения —
//     302 на портал NAS: /api/v1/auth/app-login?host=<поддомен>&rd=<путь>.
//  2. app-login: есть действующая сессия NAS — одноразовый код (60 с, привязан
//     к поддомену и хэшу сессии NAS), 302 на https://<поддомен>/.gamayun/callback.
//     Нет сессии — на страницу входа с возвратом (?app_login=…).
//  3. Callback (тоже через Verify): код погашен — своя сессия приложения в
//     cookie __Host-dn_app (только этот поддомен), 302 на исходный путь.
//
// Сессия приложения — в памяти gamayund (после перезапуска — прозрачный повторный
// вход через шаги 1–3, если сессия NAS жива). Она действует, пока жива
// родительская сессия NAS: выход, смена пароля, отключение учётки закрывают и
// доступ к приложениям. Пользование приложением продлевает простой сессии NAS.

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Имена и сроки входа в приложения.
const (
	AppCookie       = "__Host-dn_app"
	AppCallbackPath = "/.gamayun/callback"
	AppUserHeader   = "X-Gamayun-User"
	appCodeTTL      = time.Minute
	appMaxCodes     = 1000
	appMaxSessions  = 10000
	EventAppLogin   = "auth.app_login" // data: user, host
)

// AppGateOptions — связь с прокси (internal/proxy).
type AppGateOptions struct {
	// Allowed — поддомен опубликован через прокси.
	Allowed func(host string) bool
	// Portal — адрес веб-интерфейса NAS для поддомена (https://nas.local:8780).
	Portal func(appHost string) string
}

type appCode struct {
	host   string
	parent []byte
	exp    time.Time
}

type appSession struct {
	host     string
	parent   []byte
	user     string
	created  time.Time
	lastSeen time.Time
}

type appGate struct {
	mu    sync.Mutex
	o     AppGateOptions
	codes map[string]appCode     // ключ — хэш кода
	sess  map[string]*appSession // ключ — хэш токена
}

// SetAppGate включает вход в приложения (вызывается при наличии прокси).
func (s *Service) SetAppGate(o AppGateOptions) {
	s.gate.mu.Lock()
	defer s.gate.mu.Unlock()
	s.gate.o = o
	if s.gate.codes == nil {
		s.gate.codes = map[string]appCode{}
		s.gate.sess = map[string]*appSession{}
	}
}

func (g *appGate) allowed(host string) bool {
	g.mu.Lock()
	f := g.o.Allowed
	g.mu.Unlock()
	return f != nil && host != "" && f(host)
}

// normHost — имя без порта, в нижнем регистре.
func normHost(h string) string {
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// safeReturn — только путь на том же хосте: «/…», не «//…» и не «/\…».
func safeReturn(rd string) string {
	if !strings.HasPrefix(rd, "/") || strings.HasPrefix(rd, "//") || strings.HasPrefix(rd, "/\\") ||
		strings.ContainsAny(rd, "\r\n") {
		return "/"
	}
	return rd
}

// AppLogin — GET /api/v1/auth/app-login (открытый маршрут: сессию проверяет сам).
func (s *Service) appLogin(w http.ResponseWriter, r *http.Request) {
	host := normHost(r.URL.Query().Get("host"))
	rd := safeReturn(r.URL.Query().Get("rd"))
	if !s.gate.allowed(host) {
		writeError(w, http.StatusBadRequest, "app_host", "поддомен не опубликован через прокси")
		return
	}
	sess, u, ae, err := s.authenticate(r, true)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if ae != nil || !u.IsAdmin {
		// Вход в веб-интерфейс NAS, затем возврат сюда (web: App.svelte, app_login).
		http.Redirect(w, r, "/?app_login="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	code, h, err := newToken()
	if err != nil {
		s.internal(w, r, err)
		return
	}
	now := s.now()
	g := &s.gate
	g.mu.Lock()
	g.purge(now, s.o.IdleTimeout, s.o.AbsoluteTimeout)
	if len(g.codes) >= appMaxCodes {
		g.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "app_codes", "слишком много незавершённых входов, повторите позже")
		return
	}
	g.codes[string(h)] = appCode{host: host, parent: sess.idHash, exp: now.Add(appCodeTTL)}
	g.mu.Unlock()
	q := url.Values{"code": {code}, "rd": {rd}}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "https://"+host+AppCallbackPath+"?"+q.Encode(), http.StatusFound)
}

// purge — истёкшие коды и сессии (под g.mu).
func (g *appGate) purge(now time.Time, idle, abs time.Duration) {
	for k, c := range g.codes {
		if !now.Before(c.exp) {
			delete(g.codes, k)
		}
	}
	for k, a := range g.sess {
		if now.Sub(a.lastSeen) >= idle || now.Sub(a.created) >= abs {
			delete(g.sess, k)
		}
	}
}

// parentValid — родительская сессия NAS жива; продлевает её простой.
func (s *Service) parentValid(ctx context.Context, idHash []byte, now time.Time) (User, bool) {
	sess, u, err := s.st.lookupSessionHash(ctx, idHash)
	if err != nil || !now.Before(sess.ExpiresAt) || now.Sub(sess.LastSeen) >= s.o.IdleTimeout ||
		u.Disabled || !u.hash.Valid || !u.IsAdmin {
		return User{}, false
	}
	if now.Sub(sess.LastSeen) >= touchInterval {
		_ = s.st.touchSession(ctx, sess, now)
	}
	return u, true
}

// AppVerify — обработчик forward_auth Caddy (слушается на unix-сокете gamayund,
// не на сетевом адресе). 2xx — пропустить (заголовок X-Gamayun-User),
// иначе ответ уходит клиенту как есть (302 на вход / Set-Cookie).
func (s *Service) AppVerify() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Header.Get("X-Forwarded-Host")
		if host == "" {
			host = r.Host
		}
		host = normHost(host)
		uri := r.Header.Get("X-Forwarded-Uri")
		if uri == "" {
			uri = "/"
		}
		w.Header().Set("Cache-Control", "no-store")
		if !s.gate.allowed(host) {
			http.Error(w, "приложение не опубликовано", http.StatusForbidden)
			return
		}
		now := s.now()
		u, _ := url.ParseRequestURI(uri)
		if u != nil && u.Path == AppCallbackPath {
			s.appCallback(w, r, host, u.Query(), now)
			return
		}
		if c, err := r.Cookie(AppCookie); err == nil && c.Value != "" && len(c.Value) <= 128 {
			key := string(tokenHash(c.Value))
			g := &s.gate
			g.mu.Lock()
			a := g.sess[key]
			ok := a != nil && subtle.ConstantTimeCompare([]byte(a.host), []byte(host)) == 1 &&
				now.Sub(a.lastSeen) < s.o.IdleTimeout && now.Sub(a.created) < s.o.AbsoluteTimeout
			var parent []byte
			if ok {
				parent = a.parent
			}
			g.mu.Unlock()
			if ok {
				if usr, live := s.parentValid(r.Context(), parent, now); live {
					g.mu.Lock()
					a.lastSeen = now
					g.mu.Unlock()
					w.Header().Set(AppUserHeader, usr.Name)
					w.WriteHeader(http.StatusOK)
					return
				}
				g.mu.Lock()
				delete(g.sess, key)
				g.mu.Unlock()
			}
		}
		s.toPortal(w, r, host, uri)
	})
}

func (s *Service) toPortal(w http.ResponseWriter, r *http.Request, host, uri string) {
	s.gate.mu.Lock()
	portal := s.gate.o.Portal
	s.gate.mu.Unlock()
	if portal == nil {
		http.Error(w, "вход в приложения не настроен", http.StatusServiceUnavailable)
		return
	}
	q := url.Values{"host": {host}, "rd": {safeReturn(uri)}}
	http.Redirect(w, r, portal(host)+"/api/v1/auth/app-login?"+q.Encode(), http.StatusFound)
}

func (s *Service) appCallback(w http.ResponseWriter, r *http.Request, host string, q url.Values, now time.Time) {
	rd := safeReturn(q.Get("rd"))
	code := q.Get("code")
	g := &s.gate
	g.mu.Lock()
	h := string(tokenHash(code))
	c, ok := g.codes[h]
	delete(g.codes, h) // одноразовый в любом случае
	g.mu.Unlock()
	if code == "" || !ok || !now.Before(c.exp) || c.host != host {
		s.toPortal(w, r, host, rd)
		return
	}
	usr, live := s.parentValid(r.Context(), c.parent, now)
	if !live {
		s.toPortal(w, r, host, rd)
		return
	}
	tok, th, err := newToken()
	if err != nil {
		s.internal(w, r, err)
		return
	}
	g.mu.Lock()
	g.purge(now, s.o.IdleTimeout, s.o.AbsoluteTimeout)
	if len(g.sess) >= appMaxSessions {
		g.mu.Unlock()
		http.Error(w, "слишком много сессий приложений", http.StatusTooManyRequests)
		return
	}
	g.sess[string(th)] = &appSession{host: host, parent: c.parent, user: usr.Name, created: now, lastSeen: now}
	g.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: AppCookie, Value: tok, Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(s.o.AbsoluteTimeout / time.Second)})
	s.o.Logger.Info("вход в приложение", "user", usr.Name, "host", host)
	s.emit(EventAppLogin, map[string]any{"user": usr.Name, "host": host})
	http.Redirect(w, r, rd, http.StatusFound)
}
