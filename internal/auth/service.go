// Package auth — аутентификация веб-интерфейса и API gamayund:
// учётные записи администраторов, серверные сессии в cookie, CSRF,
// ограничение перебора, первичная настройка без пароля по умолчанию.
//
// Сессия: 32 случайных байта (crypto/rand) в cookie __Host-dn_session
// (HttpOnly, Secure, SameSite=Strict, Path=/); в базе — только SHA-256.
// Простой (idle) и абсолютный тайм-ауты проверяются на сервере. Каждый вход
// создаёт новую сессию (ротация), предъявленная старая удаляется.
//
// CSRF для POST/PUT/PATCH/DELETE: Sec-Fetch-Site (если есть) — same-origin
// или none; Origin (если есть) совпадает со схемой и Host запроса; заголовок
// X-CSRF-Token равен токену сессии (GET /api/v1/auth/session). Для входа и
// первичной настройки (сессии ещё нет) — только проверка источника. Кроме
// того, API принимает тела только с Content-Type: application/json.
package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gamayun-nas/internal/events"
)

// Имена cookie и заголовка CSRF.
const (
	SessionCookie = "__Host-dn_session"
	DeviceCookie  = "__Host-dn_device"
	CSRFHeader    = "X-CSRF-Token"
)

// Тайм-ауты сессии по умолчанию.
const (
	DefaultIdleTimeout     = 30 * time.Minute
	DefaultAbsoluteTimeout = 12 * time.Hour
	// touchInterval — как часто обновлять last_seen_at (не на каждый запрос).
	touchInterval = time.Minute
)

// Типы событий журнала (internal/events).
const (
	EventLogin           = "auth.login"            // data.result: success | failure
	EventLockout         = "auth.lockout"          // временная блокировка: scope ip | account | device
	EventUnlock          = "auth.unlock"           // ручное снятие блокировки администратором
	EventLogout          = "auth.logout"           //
	EventPasswordChanged = "auth.password_changed" // data.revoked_sessions
	EventSetup           = "auth.setup"            // первый администратор создан по токену установки
	EventUserCreated     = "auth.user_created"     // через CLI
)

// Options — параметры сервиса.
type Options struct {
	Logger *slog.Logger
	Events *events.Log // nil — события не пишутся
	Now    func() time.Time
	// IdleTimeout — простой сессии; AbsoluteTimeout — предельный срок.
	IdleTimeout     time.Duration
	AbsoluteTimeout time.Duration
	// Params — параметры argon2id; нулевые — DefaultParams.
	Params Params
	// SetupTokenFile — куда записать одноразовый токен установки, пока нет
	// ни одного администратора. Пусто — вход по токену отключён (только CLI).
	SetupTokenFile string
	// Политики ограничения попыток; нулевые — значения по умолчанию.
	IPPolicy, AccountPolicy, DevicePolicy LimitPolicy
	// OnPassword — вызывается при смене/установке пароля пользователя веба.
	// Ошибка смены пароля прерывает её.
	OnPassword func(ctx context.Context, name, password string) error
	// OnPasswordMismatch — OnPassword прошёл (ОС и Samba новые), а хэш веба
	// не сохранён: пометить несогласованность.
	OnPasswordMismatch func(ctx context.Context, name, reason string)
}

// Service — аутентификация.
type Service struct {
	st      Store
	o       Options
	now     func() time.Time
	ipLim   *limiter
	acctLim *limiter
	devLim  *limiter
	hashSem chan struct{}

	dummyOnce sync.Once
	dummy     string

	setupMu   sync.Mutex
	setupHash []byte

	gate appGate // вход в приложения (appauth.go)
}

// New создаёт сервис поверх открытой базы (store.Open).
func New(db *sql.DB, o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = DefaultIdleTimeout
	}
	if o.AbsoluteTimeout <= 0 {
		o.AbsoluteTimeout = DefaultAbsoluteTimeout
	}
	if o.Params == (Params{}) {
		o.Params = DefaultParams
	}
	if o.IPPolicy == (LimitPolicy{}) {
		o.IPPolicy = IPPolicy
	}
	if o.AccountPolicy == (LimitPolicy{}) {
		o.AccountPolicy = AccountPolicy
	}
	if o.DevicePolicy == (LimitPolicy{}) {
		o.DevicePolicy = DevicePolicy
	}
	return &Service{
		st: Store{DB: db}, o: o, now: o.Now,
		ipLim:   newLimiter(o.IPPolicy, o.Now),
		acctLim: newLimiter(o.AccountPolicy, o.Now),
		devLim:  newLimiter(o.DevicePolicy, o.Now),
		hashSem: make(chan struct{}, MaxConcurrentHashes),
	}
}

// Init удаляет истёкшие сессии и, если администратора нет, создаёт
// одноразовый токен установки в SetupTokenFile (права 0600). Сам токен в
// журнал не пишется — только путь к файлу.
func (s *Service) Init(ctx context.Context) error {
	if n, err := s.st.PurgeSessions(ctx, s.now(), s.o.IdleTimeout); err != nil {
		return fmt.Errorf("сессии: %w", err)
	} else if n > 0 {
		s.o.Logger.Info("удалены истёкшие сессии", "count", n)
	}
	n, err := s.st.AdminCount(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		if s.o.SetupTokenFile != "" {
			if err := os.Remove(s.o.SetupTokenFile); err == nil {
				s.o.Logger.Info("удалён устаревший токен установки", "file", s.o.SetupTokenFile)
			}
		}
		return nil
	}
	if s.o.SetupTokenFile == "" {
		s.o.Logger.Warn("администратор не создан; создайте его: gamayund admin create -name <имя>")
		return nil
	}
	tok, h, err := newToken()
	if err != nil {
		return err
	}
	if err := writeSecretFile(s.o.SetupTokenFile, []byte(tok+"\n")); err != nil {
		return fmt.Errorf("токен установки: %w", err)
	}
	s.setupMu.Lock()
	s.setupHash = h
	s.setupMu.Unlock()
	s.o.Logger.Warn("администратор не создан: одноразовый токен установки записан в файл (0600); "+
		"используйте его в POST /api/v1/auth/setup или создайте администратора: gamayund admin create -name <имя>",
		"file", s.o.SetupTokenFile)
	return nil
}

// writeSecretFile атомарно записывает файл с правами 0600.
func writeSecretFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// --- контекст запроса ---

type ctxKey struct{}

type authInfo struct {
	user User
	sess *Session
}

// UserFrom — пользователь аутентифицированного запроса.
func UserFrom(ctx context.Context) (User, bool) {
	ai, ok := ctx.Value(ctxKey{}).(*authInfo)
	if !ok {
		return User{}, false
	}
	return ai.user, true
}

// --- проверка сессии ---

type authError struct {
	status  int
	code    string
	message string
}

var (
	errUnauthorized = &authError{http.StatusUnauthorized, "unauthorized", "требуется вход"}
	errExpired      = &authError{http.StatusUnauthorized, "session_expired", "сессия истекла, войдите снова"}
	errSetup        = &authError{http.StatusUnauthorized, "setup_required", "администратор не создан: выполните первичную настройку"}
	errCSRF         = &authError{http.StatusForbidden, "csrf_failed", "запрос отклонён защитой от CSRF"}
	errForbidden    = &authError{http.StatusForbidden, "forbidden", "недостаточно прав"}
)

// authenticate проверяет cookie сессии и сроки; touch — продлить простой.
func (s *Service) authenticate(r *http.Request, touch bool) (*Session, User, *authError, error) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" || len(c.Value) > 128 {
		return nil, User{}, errUnauthorized, nil
	}
	ctx := r.Context()
	sess, u, err := s.st.lookupSession(ctx, c.Value)
	if errors.Is(err, errNoSession) {
		return nil, User{}, errUnauthorized, nil
	}
	if err != nil {
		return nil, User{}, nil, err
	}
	now := s.now()
	if !now.Before(sess.ExpiresAt) || now.Sub(sess.LastSeen) >= s.o.IdleTimeout {
		if err := s.st.deleteSession(ctx, sess.idHash); err != nil {
			return nil, User{}, nil, err
		}
		return nil, User{}, errExpired, nil
	}
	if u.Disabled || !u.hash.Valid {
		if err := s.st.deleteSession(ctx, sess.idHash); err != nil {
			return nil, User{}, nil, err
		}
		return nil, User{}, errUnauthorized, nil
	}
	if touch && now.Sub(sess.LastSeen) >= touchInterval {
		if err := s.st.touchSession(ctx, sess, now); err != nil {
			return nil, User{}, nil, err
		}
	}
	return sess, u, nil, nil
}

func (s *Service) deny(w http.ResponseWriter, r *http.Request, e *authError) {
	if e.status == http.StatusUnauthorized {
		if n, err := s.st.AdminCount(r.Context()); err == nil && n == 0 {
			e = errSetup
		}
		if _, err := r.Cookie(SessionCookie); err == nil {
			clearCookie(w, SessionCookie)
		}
	}
	writeError(w, e.status, e.code, e.message)
}

// Middleware требует действующую сессию администратора; для изменяющих
// методов — ещё и проверку CSRF. Сведения о пользователе — UserFrom.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, u, ae, err := s.authenticate(r, true)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		if ae != nil {
			s.deny(w, r, ae)
			return
		}
		if unsafeMethod(r.Method) && !csrfOK(r, sess) {
			s.o.Logger.Warn("отклонён запрос без CSRF-защиты", "path", r.URL.Path, "user", u.Name,
				"remote", r.RemoteAddr, "origin", r.Header.Get("Origin"), "sec_fetch_site", r.Header.Get("Sec-Fetch-Site"))
			s.deny(w, r, errCSRF)
			return
		}
		// Пока есть только администраторы; права обычных пользователей не предусмотрены.
		if !u.IsAdmin {
			s.deny(w, r, errForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, &authInfo{user: u, sess: sess})))
	})
}

// StillValid — сессия запроса ещё действует (без продления простоя).
// Для долгих потоков SSE: проверяется на каждом пинге.
func (s *Service) StillValid(r *http.Request) bool {
	_, u, ae, err := s.authenticate(r, false)
	return err == nil && ae == nil && u.IsAdmin
}

func unsafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// sameOrigin — запрос не межсайтовый: по Sec-Fetch-Site и Origin, если
// браузер их прислал. Клиенты без этих заголовков (curl, скрипты)
// проходят — для них защитой служит токен.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false // в том числе "null"
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return u.Scheme == scheme && strings.EqualFold(u.Host, r.Host)
}

func csrfOK(r *http.Request, sess *Session) bool {
	if !sameOrigin(r) {
		return false
	}
	got := r.Header.Get(CSRFHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRFToken())) == 1
}

// --- маршруты ---

// Routes регистрирует /api/v1/auth/*. login и setup — без сессии
// (с проверкой источника и ограничением попыток), остальные — через Middleware.
func (s *Service) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("POST /api/v1/auth/setup", s.setup)
	mux.HandleFunc("GET /api/v1/auth/app-login", s.appLogin)
	mux.Handle("POST /api/v1/auth/logout", s.Middleware(http.HandlerFunc(s.logout)))
	mux.Handle("GET /api/v1/auth/session", s.Middleware(http.HandlerFunc(s.session)))
	mux.Handle("POST /api/v1/auth/password", s.Middleware(http.HandlerFunc(s.password)))
	mux.Handle("GET /api/v1/auth/lockouts", s.Middleware(http.HandlerFunc(s.listLockouts)))
	mux.Handle("DELETE /api/v1/auth/lockouts", s.Middleware(http.HandlerFunc(s.unlockAll)))
	mux.Handle("DELETE /api/v1/auth/lockouts/{id}", s.Middleware(http.HandlerFunc(s.unlock)))
}

// SessionInfo — ответ входа и GET /api/v1/auth/session.
type SessionInfo struct {
	User               User      `json:"user"`
	CSRFToken          string    `json:"csrf_token"`
	CreatedAt          time.Time `json:"created_at"`
	ExpiresAt          time.Time `json:"expires_at"`
	IdleTimeoutSeconds int       `json:"idle_timeout_seconds"`
}

func (s *Service) info(u User, sess *Session) SessionInfo {
	return SessionInfo{User: u, CSRFToken: sess.CSRFToken(), CreatedAt: sess.CreatedAt, ExpiresAt: sess.ExpiresAt,
		IdleTimeoutSeconds: int(s.o.IdleTimeout / time.Second)}
}

// LoginRequest — тело POST /api/v1/auth/login.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

const invalidCredentials = "неверное имя пользователя или пароль"

func (s *Service) tooMany(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.Seconds()))))
	writeError(w, http.StatusTooManyRequests, "too_many_attempts", "слишком много неудачных попыток, повторите позже")
}

// login. Ограничения попыток (в памяти; перезапуск демона их сбрасывает):
//   - по адресу клиента — всегда;
//   - по имени учётной записи — если браузер не предъявил cookie доверенного
//     устройства этого пользователя (выдаётся при успешном входе, 180 дней);
//   - по такой cookie — вместо ограничения по имени.
//
// Поэтому перебор извне блокирует имя лишь временно (до 15 мин, после
// неудачи снова), а администратор со своего браузера входит и во время атаки.
// Попытка во время блокировки неудачей не считается.
func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, errCSRF.status, errCSRF.code, errCSRF.message)
		return
	}
	ipk := "ip:" + ipKey(r.RemoteAddr)
	if d := s.ipLim.Blocked(ipk); d > 0 {
		s.tooMany(w, d)
		return
	}
	var req LoginRequest
	if err := decodeBody(w, r, 8<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()
	name := strings.ToLower(strings.TrimSpace(req.Username))
	if len(name) > 64 {
		name = name[:64]
	}
	u, err := s.st.UserByName(ctx, name)
	known := err == nil
	if err != nil && !errors.Is(err, ErrUserNotFound) {
		s.internal(w, r, err)
		return
	}

	lim, key, devTok := s.acctLim, "user:"+name, ""
	if c, err := r.Cookie(DeviceCookie); err == nil && known && len(c.Value) <= 128 {
		uid, err := s.st.deviceUser(ctx, c.Value, s.now())
		if err != nil {
			s.internal(w, r, err)
			return
		}
		if uid == u.ID {
			devTok = c.Value
			lim, key = s.devLim, "dev:"+hex.EncodeToString(tokenHash(devTok))
		}
	}
	if d := lim.Blocked(key); d > 0 {
		s.tooMany(w, d)
		return
	}

	ok, rehash, err := s.checkPassword(ctx, u, known, req.Password)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !ok {
		s.failure(r, ipk, lim, key, u, known, devTok != "")
		writeError(w, http.StatusUnauthorized, "invalid_credentials", invalidCredentials)
		return
	}
	s.ipLim.Reset(ipk)
	s.acctLim.Reset("user:" + name)
	if devTok != "" {
		s.devLim.Reset(key)
	}
	if rehash {
		if h, err := s.hash(ctx, req.Password); err == nil {
			if err := s.st.rehash(ctx, u.ID, u.hash.String, h); err != nil {
				s.o.Logger.Warn("пересчёт хэша пароля", "user", u.Name, "err", err)
			}
		}
	}
	sess, err := s.startSession(w, r, u, devTok)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.o.Logger.Info("вход выполнен", "user", u.Name, "remote", r.RemoteAddr)
	s.emit(EventLogin, map[string]any{"result": "success", "user": u.Name, "remote": ipKey(r.RemoteAddr)})
	writeJSON(w, http.StatusOK, s.info(u, sess))
}

// failure учитывает неудачную попытку и пишет события.
func (s *Service) failure(r *http.Request, ipk string, lim *limiter, key string, u User, known, device bool) {
	remote := ipKey(r.RemoteAddr)
	data := map[string]any{"result": "failure", "remote": remote}
	// Имя — только существующей учётной записи: в поле имени по ошибке
	// бывает введён пароль.
	if known {
		data["user"] = u.Name
	}
	s.o.Logger.Warn("неудачный вход", "user", data["user"], "remote", r.RemoteAddr)
	s.emit(EventLogin, data)
	if d := s.ipLim.Fail(ipk); d > 0 {
		s.lockout("ip", remote, "", d)
	}
	d := lim.Fail(key)
	if known {
		lim.setUser(key, u.Name)
	}
	if d > 0 {
		scope := "account"
		if device {
			scope = "device"
		}
		name := ""
		if known {
			name = u.Name
		}
		s.lockout(scope, remote, name, d)
	}
}

func (s *Service) lockout(scope, remote, user string, d time.Duration) {
	data := map[string]any{"scope": scope, "remote": remote, "seconds": int(d / time.Second)}
	if user != "" {
		data["user"] = user
	}
	s.o.Logger.Warn("временная блокировка входа", "scope", scope, "remote", remote, "user", user, "for", d)
	s.emit(EventLockout, data)
}

// checkPassword проверяет пароль; для несуществующего пользователя тоже
// вычисляет argon2id (по фиктивному хэшу), чтобы время ответа не выдавало,
// есть ли такое имя.
func (s *Service) checkPassword(ctx context.Context, u User, known bool, pw string) (ok, rehash bool, err error) {
	if len(pw) > MaxPasswordBytes {
		pw = pw[:MaxPasswordBytes]
	}
	s.dummyOnce.Do(func() { s.dummy, _ = HashPassword("dummy-password-for-timing", s.o.Params) })
	enc := s.dummy
	usable := known && u.hash.Valid && !u.Disabled && u.IsAdmin
	if usable {
		enc = u.hash.String
	}
	if err := s.acquire(ctx); err != nil {
		return false, false, err
	}
	ok, rehash, verr := VerifyPassword(pw, enc, s.o.Params)
	s.release()
	if verr != nil {
		s.o.Logger.Error("испорчен хэш пароля", "user", u.Name, "err", verr)
		return false, false, nil
	}
	return ok && usable, rehash, nil
}

func (s *Service) hash(ctx context.Context, pw string) (string, error) {
	if err := s.acquire(ctx); err != nil {
		return "", err
	}
	defer s.release()
	return HashPassword(pw, s.o.Params)
}

func (s *Service) acquire(ctx context.Context) error {
	select {
	case s.hashSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) release() { <-s.hashSem }

// startSession — новая сессия (ротация: предъявленная старая удаляется),
// cookie сессии и, если нужно, доверенного устройства.
func (s *Service) startSession(w http.ResponseWriter, r *http.Request, u User, devTok string) (*Session, error) {
	ctx := r.Context()
	now := s.now()
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" && len(c.Value) <= 128 {
		if err := s.st.deleteSession(ctx, tokenHash(c.Value)); err != nil {
			return nil, err
		}
	}
	if _, err := s.st.PurgeSessions(ctx, now, s.o.IdleTimeout); err != nil {
		return nil, err
	}
	sess, err := s.st.CreateSession(ctx, u.ID, now, s.o.AbsoluteTimeout, ipKey(r.RemoteAddr), r.UserAgent())
	if err != nil {
		return nil, err
	}
	if devTok != "" {
		if err := s.st.touchDevice(ctx, devTok, now); err != nil {
			return nil, err
		}
	} else {
		tok, err := s.st.createDevice(ctx, u.ID, now)
		if err != nil {
			return nil, err
		}
		http.SetCookie(w, &http.Cookie{Name: DeviceCookie, Value: tok, Path: "/", MaxAge: int(deviceTTL / time.Second),
			Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: sess.token, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return sess, nil
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func authFrom(r *http.Request) *authInfo { return r.Context().Value(ctxKey{}).(*authInfo) }

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	ai := authFrom(r)
	if err := s.st.deleteSession(r.Context(), ai.sess.idHash); err != nil {
		s.internal(w, r, err)
		return
	}
	clearCookie(w, SessionCookie)
	s.o.Logger.Info("выход", "user", ai.user.Name, "remote", r.RemoteAddr)
	s.emit(EventLogout, map[string]any{"user": ai.user.Name, "remote": ipKey(r.RemoteAddr)})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) session(w http.ResponseWriter, r *http.Request) {
	ai := authFrom(r)
	writeJSON(w, http.StatusOK, s.info(ai.user, ai.sess))
}

// PasswordRequest — тело POST /api/v1/auth/password.
type PasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// password меняет пароль текущего пользователя, отзывает все его сессии и
// выдаёт новую (ротация). Неверный текущий пароль учитывается как неудачный
// вход (защита от подбора с похищенной сессией).
func (s *Service) password(w http.ResponseWriter, r *http.Request) {
	ai := authFrom(r)
	ctx := r.Context()
	ipk, key := "ip:"+ipKey(r.RemoteAddr), "user:"+ai.user.Name
	if d := max(s.ipLim.Blocked(ipk), s.acctLim.Blocked(key)); d > 0 {
		s.tooMany(w, d)
		return
	}
	var req PasswordRequest
	if err := decodeBody(w, r, 8<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ok, _, err := s.checkPassword(ctx, ai.user, true, req.CurrentPassword)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !ok {
		s.failure(r, ipk, s.acctLim, key, ai.user, true, false)
		writeError(w, http.StatusForbidden, "invalid_credentials", "текущий пароль неверен")
		return
	}
	if err := ValidatePassword(req.NewPassword, ai.user.Name); err != nil {
		writeError(w, http.StatusBadRequest, "weak_password", err.Error())
		return
	}
	if req.NewPassword == req.CurrentPassword {
		writeError(w, http.StatusBadRequest, "weak_password", "новый пароль совпадает с текущим")
		return
	}
	if s.o.OnPassword != nil {
		if err := s.o.OnPassword(ctx, ai.user.Name, req.NewPassword); err != nil {
			s.internal(w, r, err)
			return
		}
	}
	h, err := s.hash(ctx, req.NewPassword)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if err := s.st.SetPassword(ctx, ai.user.ID, h, s.now()); err != nil {
		if s.o.OnPassword != nil && s.o.OnPasswordMismatch != nil {
			s.o.OnPasswordMismatch(ctx, ai.user.Name, "пароль ОС и Samba новый, веба — прежний: "+err.Error())
		}
		s.internal(w, r, err)
		return
	}
	n, err := s.st.RevokeSessions(ctx, ai.user.ID, nil)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	devTok := ""
	if c, err := r.Cookie(DeviceCookie); err == nil {
		if uid, err := s.st.deviceUser(ctx, c.Value, s.now()); err == nil && uid == ai.user.ID {
			devTok = c.Value
		}
	}
	sess, err := s.startSession(w, r, ai.user, devTok)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.acctLim.Reset(key)
	s.o.Logger.Info("пароль изменён, сессии отозваны", "user", ai.user.Name, "revoked", n)
	s.emit(EventPasswordChanged, map[string]any{"user": ai.user.Name, "remote": ipKey(r.RemoteAddr), "revoked_sessions": n})
	writeJSON(w, http.StatusOK, s.info(ai.user, sess))
}

// SetupRequest — тело POST /api/v1/auth/setup.
type SetupRequest struct {
	SetupToken string `json:"setup_token"`
	Username   string `json:"username"`
	Password   string `json:"password"`
}

// setup создаёт первого администратора по одноразовому токену установки
// и сразу открывает сессию. После создания администратора токен
// недействителен, файл удаляется.
func (s *Service) setup(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, errCSRF.status, errCSRF.code, errCSRF.message)
		return
	}
	ipk := "ip:" + ipKey(r.RemoteAddr)
	if d := s.ipLim.Blocked(ipk); d > 0 {
		s.tooMany(w, d)
		return
	}
	var req SetupRequest
	if err := decodeBody(w, r, 8<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.setupHash == nil {
		if n, err := s.st.AdminCount(ctx); err == nil && n > 0 {
			writeError(w, http.StatusConflict, "already_configured", ErrAlreadyConfigured.Error())
			return
		}
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(req.SetupToken)))
	if s.setupHash == nil || subtle.ConstantTimeCompare(got[:], s.setupHash) != 1 {
		if d := s.ipLim.Fail(ipk); d > 0 {
			s.lockout("ip", ipKey(r.RemoteAddr), "", d)
		}
		s.o.Logger.Warn("неверный токен установки", "remote", r.RemoteAddr)
		writeError(w, http.StatusForbidden, "invalid_setup_token", "неверный токен установки")
		return
	}
	name := strings.TrimSpace(req.Username)
	if err := ValidateName(name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := ValidatePassword(req.Password, name); err != nil {
		writeError(w, http.StatusBadRequest, "weak_password", err.Error())
		return
	}
	h, err := s.hash(ctx, req.Password)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	u, err := s.st.CreateUser(ctx, name, h, true, true, s.now())
	if errors.Is(err, ErrAlreadyConfigured) {
		s.closeSetup()
		writeError(w, http.StatusConflict, "already_configured", err.Error())
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.closeSetup()
	if s.o.OnPassword != nil {
		if err := s.o.OnPassword(ctx, u.Name, req.Password); err != nil {
			s.o.Logger.Warn("пароль Samba первого администратора не задан", "user", u.Name, "err", err)
		}
	}
	sess, err := s.startSession(w, r, u, "")
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.o.Logger.Info("первый администратор создан по токену установки", "user", u.Name, "remote", r.RemoteAddr)
	s.emit(EventSetup, map[string]any{"user": u.Name, "remote": ipKey(r.RemoteAddr)})
	writeJSON(w, http.StatusCreated, s.info(u, sess))
}

// closeSetup делает токен установки недействительным (вызывать под setupMu).
func (s *Service) closeSetup() {
	s.setupHash = nil
	if s.o.SetupTokenFile != "" {
		if err := os.Remove(s.o.SetupTokenFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.o.Logger.Warn("не удалён файл токена установки", "file", s.o.SetupTokenFile, "err", err)
		}
	}
}

// --- вспомогательное ---

func (s *Service) emit(typ string, data map[string]any) {
	if s.o.Events == nil {
		return
	}
	b, _ := json.Marshal(data)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.o.Events.Emit(ctx, events.Event{Type: typ, Data: b}, true); err != nil {
		s.o.Logger.Error("журнал событий", "type", typ, "err", err)
	}
}

func (s *Service) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.o.Logger.Error("аутентификация: внутренняя ошибка", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "внутренняя ошибка")
}

// Формат ошибок совпадает с internal/api: {"error": {"code", "message"}}.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// decodeBody — строгий разбор JSON (как в internal/api): только
// application/json, неизвестные поля и лишние данные — ошибка.
func decodeBody(w http.ResponseWriter, r *http.Request, max int64, v any) error {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return errors.New("ожидается Content-Type: application/json")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		return fmt.Errorf("тело запроса: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("тело запроса: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("тело запроса: лишние данные после объекта")
	}
	return nil
}
