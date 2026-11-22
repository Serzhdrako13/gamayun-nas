package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/store"
)

// fastParams — лёгкие параметры argon2id для тестов (стойкость не проверяется).
var fastParams = Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

const goodPW = "correct horse battery"

// clock — подменяемые часы.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type tenv struct {
	db    *sql.DB
	svc   *Service
	clk   *clock
	srv   *httptest.Server
	log   *events.Log
	token string // файл токена установки
}

func newTEnv(t *testing.T, mod func(*Options)) *tenv {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &tenv{db: db, clk: &clock{t: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}, log: events.NewLog(db),
		token: filepath.Join(dir, "setup-token")}
	o := Options{Logger: slog.New(slog.DiscardHandler), Events: e.log, Now: e.clk.Now, Params: fastParams,
		SetupTokenFile: e.token}
	if mod != nil {
		mod(&o)
	}
	e.svc = New(db, o)
	if err := e.svc.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	e.svc.Routes(mux)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFrom(r.Context())
		writeJSON(w, 200, map[string]string{"user": u.Name})
	})
	mux.Handle("GET /api/v1/x", e.svc.Middleware(ok))
	mux.Handle("POST /api/v1/x", e.svc.Middleware(ok))
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *tenv) addAdmin(t *testing.T, name, pw string) User {
	t.Helper()
	h, err := HashPassword(pw, fastParams)
	if err != nil {
		t.Fatal(err)
	}
	u, err := Store{DB: e.db}.CreateUser(context.Background(), name, h, true, false, e.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// client — браузер: хранит cookie из ответов (включая удаление).
type client struct {
	t       *testing.T
	e       *tenv
	cookies map[string]string
	csrf    string
	hdr     map[string]string
}

func (e *tenv) client(t *testing.T) *client {
	return &client{t: t, e: e, cookies: map[string]string{}, hdr: map[string]string{}}
}

type resp struct {
	status int
	h      http.Header
	body   map[string]any
	raw    string
	sc     []*http.Cookie
}

func (r resp) code() string {
	e, _ := r.body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (c *client) do(method, path, body string) resp {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.e.srv.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	if c.csrf != "" {
		req.Header.Set(CSRFHeader, c.csrf)
	}
	for k, v := range c.hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	r := resp{status: res.StatusCode, h: res.Header, raw: string(b), sc: res.Cookies()}
	json.Unmarshal(b, &r.body)
	for _, ck := range res.Cookies() {
		if ck.MaxAge < 0 {
			delete(c.cookies, ck.Name)
		} else {
			c.cookies[ck.Name] = ck.Value
		}
	}
	return r
}

func (c *client) login(name, pw string) resp {
	c.t.Helper()
	b, _ := json.Marshal(LoginRequest{Username: name, Password: pw})
	r := c.do("POST", "/api/v1/auth/login", string(b))
	if r.status == 200 {
		c.csrf, _ = r.body["csrf_token"].(string)
	}
	return r
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword(goodPW, DefaultParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("формат: %s", h)
	}
	h2, _ := HashPassword(goodPW, DefaultParams)
	if h == h2 {
		t.Error("соль не случайна")
	}
	if ok, rh, err := VerifyPassword(goodPW, h, DefaultParams); !ok || rh || err != nil {
		t.Errorf("верный пароль: %v %v %v", ok, rh, err)
	}
	if ok, _, _ := VerifyPassword(goodPW+"x", h, DefaultParams); ok {
		t.Error("неверный пароль принят")
	}
	if ok, rh, _ := VerifyPassword(goodPW, h, fastParams); !ok || !rh {
		t.Errorf("смена параметров: ok=%v rehash=%v", ok, rh)
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=4194304,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA", "$argon2id$v=18$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA"} {
		if _, _, err := VerifyPassword(goodPW, bad, DefaultParams); err == nil {
			t.Errorf("испорченный хэш принят: %q", bad)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, n := range []string{"admin", "_svc", "a-b_1", strings.Repeat("a", 32)} {
		if ValidateName(n) != nil {
			t.Errorf("имя %q отклонено", n)
		}
	}
	for _, n := range []string{"", "Admin", "1abc", "a b", "ад", strings.Repeat("a", 33), "a/b"} {
		if ValidateName(n) == nil {
			t.Errorf("имя %q принято", n)
		}
	}
	for _, p := range []string{"", "short", "admin12345"[:MinPasswordLen-1], strings.Repeat("я", 513), "abc\x00defghijk", "administrator"} {
		if ValidatePassword(p, "administrator") == nil {
			t.Errorf("пароль %q принят", p)
		}
	}
	if err := ValidatePassword("пароль-из-кириллицы", "admin"); err != nil {
		t.Error(err)
	}
}

func TestLoginLogout(t *testing.T) {
	e := newTEnv(t, nil)
	e.addAdmin(t, "admin", goodPW)
	c := e.client(t)
	if r := c.do("GET", "/api/v1/x", ""); r.status != 401 || r.code() != "unauthorized" {
		t.Fatalf("без входа: %d %s", r.status, r.raw)
	}
	bad := c.login("admin", "wrong password")
	unknown := c.login("nobody", "wrong password")
	if bad.status != 401 || bad.code() != "invalid_credentials" || bad.raw != unknown.raw || unknown.status != 401 {
		t.Fatalf("ответы неверного пароля и неизвестного имени различаются: %s | %s", bad.raw, unknown.raw)
	}
	r := c.login("Admin", goodPW) // регистр имени не важен
	if r.status != 200 {
		t.Fatalf("вход: %d %s", r.status, r.raw)
	}
	var sc *http.Cookie
	for _, ck := range r.sc {
		if ck.Name == SessionCookie {
			sc = ck
		}
	}
	if sc == nil || !sc.HttpOnly || !sc.Secure || sc.SameSite != http.SameSiteStrictMode || sc.Path != "/" || sc.Domain != "" {
		t.Fatalf("cookie сессии: %+v", sc)
	}
	if len(sc.Value) < 43 {
		t.Errorf("короткий идентификатор сессии: %q", sc.Value)
	}
	// В базе — только хэш.
	var n int
	e.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE id_hash = ?", tokenHash(sc.Value)).Scan(&n)
	var raw int
	e.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE id_hash = ?", sc.Value).Scan(&raw)
	if n != 1 || raw != 0 {
		t.Errorf("в базе: по хэшу %d, по открытому значению %d", n, raw)
	}
	if r := c.do("GET", "/api/v1/x", ""); r.status != 200 || r.body["user"] != "admin" {
		t.Fatalf("с сессией: %d %s", r.status, r.raw)
	}
	s := c.do("GET", "/api/v1/auth/session", "")
	if s.status != 200 || s.body["csrf_token"] != c.csrf || s.body["user"].(map[string]any)["name"] != "admin" {
		t.Fatalf("session: %d %s", s.status, s.raw)
	}
	csrf := c.csrf
	c.csrf = ""
	if r := c.do("POST", "/api/v1/auth/logout", ""); r.status != 403 || r.code() != "csrf_failed" {
		t.Fatalf("выход без CSRF: %d %s", r.status, r.raw)
	}
	c.csrf = csrf
	old := c.cookies[SessionCookie]
	if r := c.do("POST", "/api/v1/auth/logout", ""); r.status != 204 {
		t.Fatalf("выход: %d %s", r.status, r.raw)
	}
	c.cookies[SessionCookie] = old // предъявить старую cookie после выхода
	if r := c.do("GET", "/api/v1/x", ""); r.status != 401 {
		t.Fatalf("после выхода: %d", r.status)
	}
	// Журнал: неудачи и успех.
	evs, _ := e.log.Since(context.Background(), 0, 100)
	var types []string
	for _, ev := range evs {
		types = append(types, ev.Type)
		if strings.Contains(string(ev.Data), "wrong password") || strings.Contains(string(ev.Data), "nobody") {
			t.Errorf("в журнале пароль или несуществующее имя: %s", ev.Data)
		}
	}
	if strings.Join(types, ",") != "auth.login,auth.login,auth.login,auth.logout" {
		t.Errorf("события: %v", types)
	}
}

func TestSessionRotation(t *testing.T) {
	e := newTEnv(t, nil)
	e.addAdmin(t, "admin", goodPW)
	c := e.client(t)
	c.login("admin", goodPW)
	first := c.cookies[SessionCookie]
	c.login("admin", goodPW)
	if c.cookies[SessionCookie] == first {
		t.Fatal("повторный вход не сменил идентификатор")
	}
	c2 := e.client(t)
	c2.cookies[SessionCookie] = first
	if r := c2.do("GET", "/api/v1/x", ""); r.status != 401 {
		t.Errorf("старая сессия после повторного входа: %d", r.status)
	}
	c2.cookies[SessionCookie] = "подделка"
	if r := c2.do("GET", "/api/v1/x", ""); r.status != 401 {
		t.Errorf("поддельная cookie: %d", r.status)
	}
}

func TestTimeouts(t *testing.T) {
	e := newTEnv(t, func(o *Options) { o.IdleTimeout, o.AbsoluteTimeout = 30*time.Minute, 2*time.Hour })
	e.addAdmin(t, "admin", goodPW)

	// Простой.
	c := e.client(t)
	c.login("admin", goodPW)
	e.clk.Add(29 * time.Minute)
	if r := c.do("GET", "/api/v1/x", ""); r.status != 200 {
		t.Fatalf("до простоя: %d", r.status)
	}
	e.clk.Add(29 * time.Minute) // активность продлила сессию
	if r := c.do("GET", "/api/v1/x", ""); r.status != 200 {
		t.Fatalf("после активности: %d", r.status)
	}
	e.clk.Add(31 * time.Minute)
	if r := c.do("GET", "/api/v1/x", ""); r.status != 401 || r.code() != "session_expired" {
		t.Fatalf("простой: %d %s", r.status, r.raw)
	}

	// Абсолютный срок: активность каждые 20 мин не продлевает его.
	c = e.client(t)
	c.login("admin", goodPW)
	for i := 0; i < 5; i++ {
		e.clk.Add(20 * time.Minute)
		if r := c.do("GET", "/api/v1/x", ""); r.status != 200 {
			t.Fatalf("шаг %d: %d", i, r.status)
		}
	}
	e.clk.Add(20 * time.Minute) // 2 ч от входа
	if r := c.do("GET", "/api/v1/x", ""); r.status != 401 || r.code() != "session_expired" {
		t.Fatalf("абсолютный срок: %d %s", r.status, r.raw)
	}
	var n int
	e.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n)
	if n != 0 {
		t.Errorf("истёкшие сессии остались в базе: %d", n)
	}
}

func TestCSRF(t *testing.T) {
	e := newTEnv(t, nil)
	e.addAdmin(t, "admin", goodPW)
	c := e.client(t)
	c.login("admin", goodPW)
	good := c.csrf
	host := strings.TrimPrefix(e.srv.URL, "http://")

	for name, mod := range map[string]func(){
		"без токена":                func() { c.csrf = "" },
		"чужой токен":               func() { c.csrf = strings.Repeat("A", len(good)) },
		"чужой Origin":              func() { c.hdr["Origin"] = "http://evil.example" },
		"Origin null":               func() { c.hdr["Origin"] = "null" },
		"другая схема":              func() { c.hdr["Origin"] = "https://" + host },
		"Sec-Fetch-Site cross-site": func() { c.hdr["Sec-Fetch-Site"] = "cross-site" },
		"Sec-Fetch-Site same-site":  func() { c.hdr["Sec-Fetch-Site"] = "same-site" },
	} {
		c.csrf, c.hdr = good, map[string]string{}
		mod()
		if r := c.do("POST", "/api/v1/x", "{}"); r.status != 403 || r.code() != "csrf_failed" {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
	c.csrf = good
	c.hdr = map[string]string{"Origin": "http://" + host, "Sec-Fetch-Site": "same-origin"}
	if r := c.do("POST", "/api/v1/x", "{}"); r.status != 200 {
		t.Errorf("свой Origin с токеном: %d %s", r.status, r.raw)
	}
	c.hdr = map[string]string{}
	if r := c.do("POST", "/api/v1/x", "{}"); r.status != 200 {
		t.Errorf("без Origin (curl) с токеном: %d %s", r.status, r.raw)
	}
	// GET не требует токена.
	c.csrf = ""
	if r := c.do("GET", "/api/v1/x", ""); r.status != 200 {
		t.Errorf("GET: %d", r.status)
	}
	// Вход с чужого сайта (login CSRF).
	c2 := e.client(t)
	c2.hdr["Origin"] = "http://evil.example"
	if r := c2.login("admin", goodPW); r.status != 403 {
		t.Errorf("вход с чужим Origin: %d", r.status)
	}
	// Тело входа — только JSON.
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/auth/login", strings.NewReader("username=admin&password=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 400 {
		t.Errorf("вход формой: %v %v", res.StatusCode, err)
	}
}

func TestBruteForceAccount(t *testing.T) {
	e := newTEnv(t, nil)
	e.addAdmin(t, "admin", goodPW)

	// Браузер владельца, где уже был вход, — с cookie устройства.
	owner := e.client(t)
	if r := owner.login("admin", goodPW); r.status != 200 || owner.cookies[DeviceCookie] == "" {
		t.Fatalf("вход владельца: %d %v", r.status, owner.cookies)
	}
	owner.do("POST", "/api/v1/auth/logout", "")

	// Перебор с разных адресов — по имени учётной записи.
	att := e.client(t)
	for i := 1; i <= AccountPolicy.Free; i++ {
		att.hdr = map[string]string{}
		if r := att.login("admin", "guess-"+strconv.Itoa(i)); r.status != 401 {
			t.Fatalf("попытка %d: %d", i, r.status)
		}
	}
	if r := att.login("admin", "guess-x"); r.status != 401 {
		t.Fatalf("неудача, после которой блокировка: %d", r.status)
	}
	r := att.login("admin", goodPW)
	if r.status != 429 || r.code() != "too_many_attempts" || r.h.Get("Retry-After") == "" {
		t.Fatalf("верный пароль во время блокировки без cookie устройства: %d %s", r.status, r.raw)
	}
	// Владелец со своего браузера входит и во время атаки.
	if r := owner.login("admin", goodPW); r.status != 200 {
		t.Fatalf("владелец с cookie устройства: %d %s", r.status, r.raw)
	}
	// Блокировка временная: после неё попытка проходит.
	e.clk.Add(2 * time.Second)
	if r := att.login("admin", goodPW); r.status != 200 {
		t.Fatalf("после окончания блокировки: %d %s", r.status, r.raw)
	}
	// Рост блокировки ограничен сверху: даже после сотни неудач — не дольше Max.
	for i := 0; i < 100; i++ {
		e.clk.Add(AccountPolicy.Max)
		att.login("admin", "guess")
	}
	e.clk.Add(AccountPolicy.Max)
	if r := att.login("admin", goodPW); r.status != 200 {
		t.Fatalf("после максимальной блокировки: %d", r.status)
	}

	evs, _ := e.log.Since(context.Background(), 0, 1000)
	var lock bool
	for _, ev := range evs {
		if ev.Type == EventLockout && strings.Contains(string(ev.Data), `"scope":"account"`) {
			lock = true
		}
	}
	if !lock {
		t.Error("нет события auth.lockout")
	}
}

func TestBruteForceIP(t *testing.T) {
	e := newTEnv(t, func(o *Options) {
		o.AccountPolicy = LimitPolicy{Free: 1000, Base: time.Second, Max: time.Second, Window: time.Hour}
	})
	e.addAdmin(t, "admin", goodPW)
	c := e.client(t)
	for i := 0; i <= IPPolicy.Free; i++ {
		c.login("user"+strconv.Itoa(i), "x")
	}
	if r := c.login("admin", goodPW); r.status != 429 {
		t.Fatalf("адрес после %d неудач: %d", IPPolicy.Free+1, r.status)
	}
	e.clk.Add(IPPolicy.Base)
	if r := c.login("admin", goodPW); r.status != 200 {
		t.Fatalf("после блокировки адреса: %d", r.status)
	}
}

func TestLimiter(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	l := newLimiter(LimitPolicy{Free: 2, Base: time.Second, Max: 8 * time.Second, Window: time.Minute}, clk.Now)
	var got []time.Duration
	for i := 0; i < 7; i++ {
		got = append(got, l.Fail("k"))
	}
	want := []time.Duration{0, 0, 1, 2, 4, 8, 8}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("неудача %d: %v, ожидалось %v (%v)", i+1, got[i], want[i]*time.Second, got)
		}
	}
	if d := l.Blocked("k"); d != 8*time.Second {
		t.Errorf("Blocked: %v", d)
	}
	clk.Add(2 * time.Minute) // окно без неудач — счётчик сброшен
	if d := l.Fail("k"); d != 0 {
		t.Errorf("после окна: %v", d)
	}
	// Заполненная таблица не вытесняет живые записи.
	for i := len(l.m); i < maxLimiterEntries; i++ {
		l.m[strconv.Itoa(i)] = &limitEntry{last: clk.Now()}
	}
	l.Fail("k")
	l.Fail("k")
	if d := l.Fail("new"); d != 0 || l.m["new"] != nil {
		t.Error("новый ключ в заполненной таблице")
	}
	if l.Blocked("k") == 0 {
		t.Error("живая блокировка вытеснена")
	}
	if ipKey("[2001:db8:1:2:3:4:5:6]:443") != ipKey("[2001:db8:1:2:ffff::1]:80") || ipKey("10.0.0.1:1") != "10.0.0.1" {
		t.Error("ipKey")
	}
}

func TestSetupToken(t *testing.T) {
	e := newTEnv(t, nil)
	// Пароля по умолчанию нет: пользователей нет, вход невозможен.
	var n int
	e.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n)
	if n != 0 {
		t.Fatalf("пользователей на новой базе: %d", n)
	}
	c := e.client(t)
	if r := c.login("admin", "admin"); r.status != 401 {
		t.Fatalf("admin/admin: %d", r.status)
	}
	if r := c.do("GET", "/api/v1/x", ""); r.code() != "setup_required" {
		t.Fatalf("без администратора: %s", r.raw)
	}
	fi, err := os.Stat(e.token)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("файл токена: %v %v", fi, err)
	}
	b, _ := os.ReadFile(e.token)
	tok := strings.TrimSpace(string(b))
	if len(tok) < 43 {
		t.Fatalf("короткий токен %q", tok)
	}
	body := func(tok, name, pw string) string {
		b, _ := json.Marshal(SetupRequest{SetupToken: tok, Username: name, Password: pw})
		return string(b)
	}
	if r := c.do("POST", "/api/v1/auth/setup", body("неверный", "owner", goodPW)); r.status != 403 || r.code() != "invalid_setup_token" {
		t.Fatalf("неверный токен: %d %s", r.status, r.raw)
	}
	if r := c.do("POST", "/api/v1/auth/setup", body(tok, "owner", "short")); r.status != 400 || r.code() != "weak_password" {
		t.Fatalf("слабый пароль: %d %s", r.status, r.raw)
	}
	r := c.do("POST", "/api/v1/auth/setup", body(tok, "owner", goodPW))
	if r.status != 201 {
		t.Fatalf("setup: %d %s", r.status, r.raw)
	}
	c.csrf, _ = r.body["csrf_token"].(string)
	if r := c.do("GET", "/api/v1/x", ""); r.status != 200 {
		t.Fatalf("сессия после setup: %d", r.status)
	}
	if _, err := os.Stat(e.token); !os.IsNotExist(err) {
		t.Error("файл токена не удалён")
	}
	if r := c.do("POST", "/api/v1/auth/setup", body(tok, "second", goodPW)); r.status != 409 {
		t.Fatalf("повторный setup тем же токеном: %d %s", r.status, r.raw)
	}
	// После перезапуска с администратором токен не создаётся.
	svc := New(e.db, Options{Logger: slog.New(slog.DiscardHandler), Params: fastParams, SetupTokenFile: e.token})
	if err := svc.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.token); !os.IsNotExist(err) {
		t.Error("токен создан при наличии администратора")
	}
}

func TestSetupRace(t *testing.T) {
	e := newTEnv(t, nil)
	h, _ := HashPassword(goodPW, fastParams)
	st := Store{DB: e.db}
	if _, err := st.CreateUser(context.Background(), "a", h, true, true, e.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(context.Background(), "b", h, true, true, e.clk.Now()); err != ErrAlreadyConfigured {
		t.Fatalf("второй первичный администратор: %v", err)
	}
	if _, err := st.CreateUser(context.Background(), "a", h, true, false, e.clk.Now()); err != ErrUserExists {
		t.Fatalf("повтор имени: %v", err)
	}
}

func TestPasswordChange(t *testing.T) {
	e := newTEnv(t, nil)
	e.addAdmin(t, "admin", goodPW)
	c, other := e.client(t), e.client(t)
	c.login("admin", goodPW)
	other.login("admin", goodPW)
	pw := func(cur, nw string) resp {
		b, _ := json.Marshal(PasswordRequest{CurrentPassword: cur, NewPassword: nw})
		return c.do("POST", "/api/v1/auth/password", string(b))
	}
	if r := pw("wrong", "new password 123"); r.status != 403 || r.code() != "invalid_credentials" {
		t.Fatalf("неверный текущий: %d %s", r.status, r.raw)
	}
	if r := pw(goodPW, "short"); r.status != 400 || r.code() != "weak_password" {
		t.Fatalf("слабый: %d %s", r.status, r.raw)
	}
	if r := pw(goodPW, goodPW); r.status != 400 {
		t.Fatalf("тот же пароль: %d %s", r.status, r.raw)
	}
	old := c.cookies[SessionCookie]
	r := pw(goodPW, "new password 123")
	if r.status != 200 {
		t.Fatalf("смена: %d %s", r.status, r.raw)
	}
	if c.cookies[SessionCookie] == old {
		t.Error("сессия не сменилась")
	}
	c.csrf, _ = r.body["csrf_token"].(string)
	if r := c.do("POST", "/api/v1/x", "{}"); r.status != 200 {
		t.Errorf("новая сессия: %d %s", r.status, r.raw)
	}
	if r := other.do("GET", "/api/v1/x", ""); r.status != 401 {
		t.Errorf("другая сессия не отозвана: %d", r.status)
	}
	if r := e.client(t).login("admin", goodPW); r.status != 401 {
		t.Errorf("старый пароль: %d", r.status)
	}
	if r := e.client(t).login("admin", "new password 123"); r.status != 200 {
		t.Errorf("новый пароль: %d", r.status)
	}
}

func TestRehashOnLogin(t *testing.T) {
	e := newTEnv(t, func(o *Options) { o.Params = Params{Memory: 128, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32} })
	e.addAdmin(t, "admin", goodPW) // хэш с fastParams (m=64)
	if r := e.client(t).login("admin", goodPW); r.status != 200 {
		t.Fatal(r.raw)
	}
	var h string
	e.db.QueryRow("SELECT password FROM users WHERE name='admin'").Scan(&h)
	if !strings.Contains(h, "m=128,") {
		t.Errorf("хэш не пересчитан: %s", h)
	}
}

func TestStillValidAndDisabled(t *testing.T) {
	e := newTEnv(t, nil)
	e.addAdmin(t, "admin", goodPW)
	c := e.client(t)
	c.login("admin", goodPW)
	req := httptest.NewRequest("GET", "/api/v1/events", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: c.cookies[SessionCookie]})
	if !e.svc.StillValid(req) {
		t.Fatal("действующая сессия")
	}
	e.db.Exec("UPDATE users SET disabled = 1")
	if e.svc.StillValid(req) {
		t.Fatal("сессия отключённого пользователя")
	}
	e.db.Exec("UPDATE users SET disabled = 0")
	if r := c.login("admin", goodPW); r.status != 200 {
		t.Fatal(r.raw)
	}
	e.db.Exec("UPDATE users SET is_admin = 0")
	if r := c.do("GET", "/api/v1/x", ""); r.status != 403 || r.code() != "forbidden" {
		t.Errorf("не администратор: %d %s", r.status, r.raw)
	}
	if r := e.client(t).login("admin", goodPW); r.status != 401 {
		t.Errorf("вход не администратора: %d", r.status)
	}
}

func TestUnknownAuthRoute(t *testing.T) {
	e := newTEnv(t, nil)
	if r := e.client(t).do("GET", "/api/v1/auth/login", ""); r.status != 405 {
		t.Errorf("GET login: %d", r.status)
	}
}

// BenchmarkHashDefault — время argon2id с параметрами по умолчанию.
// go test -bench Hash -run x ./internal/auth (и с -tags purego — оценка
// для arm64, где нет ассемблерной реализации).
func BenchmarkHashDefault(b *testing.B) {
	for i := 0; i < b.N; i++ {
		HashPassword(goodPW, DefaultParams)
	}
}

// Смена пароля в вебе зовёт хук (пароль Samba); ошибка хука
// оставляет прежний пароль.
func TestPasswordChangeCallsHook(t *testing.T) {
	var gotName, gotPw string
	var hookErr error
	e := newTEnv(t, func(o *Options) {
		o.OnPassword = func(_ context.Context, n, p string) error { gotName, gotPw = n, p; return hookErr }
	})
	e.addAdmin(t, "admin", goodPW)
	c := e.client(t)
	c.login("admin", goodPW)
	b, _ := json.Marshal(PasswordRequest{CurrentPassword: goodPW, NewPassword: "new password 123"})
	hookErr = errors.New("helper недоступен")
	if r := c.do("POST", "/api/v1/auth/password", string(b)); r.status != 500 {
		t.Fatalf("ошибка хука: %d %s", r.status, r.raw)
	}
	if r := e.client(t).login("admin", goodPW); r.status != 200 {
		t.Fatal("прежний пароль должен действовать")
	}
	hookErr = nil
	if r := c.do("POST", "/api/v1/auth/password", string(b)); r.status != 200 || gotName != "admin" || gotPw != "new password 123" {
		t.Fatalf("%d %s %q %q", r.status, r.raw, gotName, gotPw)
	}
}
