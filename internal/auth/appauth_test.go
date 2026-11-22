package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const appHost = "jelly.nas.local"

func gateEnv(t *testing.T) (*tenv, *client) {
	t.Helper()
	e := newTEnv(t, nil)
	e.svc.SetAppGate(AppGateOptions{
		Allowed: func(h string) bool { return h == appHost },
		Portal:  func(string) string { return "https://nas.local:8780" },
	})
	e.addAdmin(t, "admin", goodPW)
	c := e.client(t)
	if r := c.login("admin", goodPW); r.status != 200 {
		t.Fatalf("вход: %d %s", r.status, r.raw)
	}
	return e, c
}

// appLoginReq — GET app-login с cookie клиента, без перехода по редиректу.
func appLoginReq(e *tenv, c *client, q string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/v1/auth/app-login?"+q, nil)
	for k, v := range c.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	w := httptest.NewRecorder()
	e.svc.appLogin(w, req)
	return w
}

// verifyReq — запрос Caddy forward_auth.
func verifyReq(e *tenv, host, uri, appCookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/verify", nil)
	req.Header.Set("X-Forwarded-Host", host)
	req.Header.Set("X-Forwarded-Uri", uri)
	if appCookie != "" {
		req.AddCookie(&http.Cookie{Name: AppCookie, Value: appCookie})
	}
	w := httptest.NewRecorder()
	e.svc.AppVerify().ServeHTTP(w, req)
	return w
}

func loc(t *testing.T, w *httptest.ResponseRecorder) *url.URL {
	t.Helper()
	if w.Code != http.StatusFound {
		t.Fatalf("ожидался 302, получен %d: %s", w.Code, w.Body.String())
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func appCookieOf(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == AppCookie {
			return c
		}
	}
	return nil
}

// fullLogin — весь путь: verify без cookie → app-login → callback → cookie.
func fullLogin(t *testing.T, e *tenv, c *client, rd string) string {
	t.Helper()
	u := loc(t, verifyReq(e, appHost, rd, ""))
	if u.Host != "nas.local:8780" || u.Path != "/api/v1/auth/app-login" || u.Query().Get("host") != appHost ||
		u.Query().Get("rd") != rd {
		t.Fatalf("редирект на портал: %s", u)
	}
	cb := loc(t, appLoginReq(e, c, u.RawQuery))
	if cb.Scheme != "https" || cb.Host != appHost || cb.Path != AppCallbackPath {
		t.Fatalf("редирект на callback: %s", cb)
	}
	w := verifyReq(e, appHost, cb.RequestURI(), "")
	back := loc(t, w)
	if back.String() != rd {
		t.Fatalf("возврат: %s, ожидался %s", back, rd)
	}
	ck := appCookieOf(w)
	if ck == nil || !ck.Secure || !ck.HttpOnly || ck.Path != "/" || ck.Domain != "" {
		t.Fatalf("cookie приложения: %+v", ck)
	}
	return ck.Value
}

func TestAppGateFlow(t *testing.T) {
	e, c := gateEnv(t)
	tok := fullLogin(t, e, c, "/web/index.html?x=1")
	w := verifyReq(e, appHost, "/api/items", tok)
	if w.Code != 200 || w.Header().Get(AppUserHeader) != "admin" {
		t.Fatalf("доступ с cookie: %d %q", w.Code, w.Header().Get(AppUserHeader))
	}
	// Cookie другого поддомена не подходит.
	e.svc.SetAppGate(AppGateOptions{
		Allowed: func(h string) bool { return h == appHost || h == "other.nas.local" },
		Portal:  func(string) string { return "https://nas.local:8780" },
	})
	if w := verifyReq(e, "other.nas.local", "/", tok); w.Code != http.StatusFound {
		t.Fatalf("чужой поддомен пропущен: %d", w.Code)
	}
	// Не опубликованный поддомен — 403.
	if w := verifyReq(e, "evil.nas.local", "/", tok); w.Code != http.StatusForbidden {
		t.Fatalf("неопубликованный: %d", w.Code)
	}
}

func TestAppGateCodeSingleUse(t *testing.T) {
	e, c := gateEnv(t)
	cb := loc(t, appLoginReq(e, c, url.Values{"host": {appHost}, "rd": {"/"}}.Encode()))
	if w := verifyReq(e, appHost, cb.RequestURI(), ""); appCookieOf(w) == nil {
		t.Fatal("первый callback не выдал cookie")
	}
	w := verifyReq(e, appHost, cb.RequestURI(), "")
	if appCookieOf(w) != nil || !strings.Contains(w.Header().Get("Location"), "app-login") {
		t.Fatalf("повторный код принят: %v", w.Header())
	}
	// Код другого поддомена не принимается.
	e.svc.SetAppGate(AppGateOptions{
		Allowed: func(h string) bool { return h == appHost || h == "b.nas.local" },
		Portal:  func(string) string { return "https://nas.local:8780" },
	})
	cb = loc(t, appLoginReq(e, c, url.Values{"host": {appHost}, "rd": {"/"}}.Encode()))
	if w := verifyReq(e, "b.nas.local", cb.RequestURI(), ""); appCookieOf(w) != nil {
		t.Fatal("код принят на другом поддомене")
	}
	// Просроченный код.
	cb = loc(t, appLoginReq(e, c, url.Values{"host": {appHost}, "rd": {"/"}}.Encode()))
	e.clk.Add(appCodeTTL + time.Second)
	if w := verifyReq(e, appHost, cb.RequestURI(), ""); appCookieOf(w) != nil {
		t.Fatal("просроченный код принят")
	}
}

func TestAppLoginNoSession(t *testing.T) {
	e, _ := gateEnv(t)
	anon := e.client(t)
	u := loc(t, appLoginReq(e, anon, url.Values{"host": {appHost}, "rd": {"/a"}}.Encode()))
	if u.Path != "/" || !strings.HasPrefix(u.Query().Get("app_login"), "/api/v1/auth/app-login?") {
		t.Fatalf("без сессии: %s", u)
	}
	// Неопубликованный поддомен — без редиректа (нет открытого перенаправления).
	if w := appLoginReq(e, anon, "host=evil.example.com&rd=/"); w.Code != http.StatusBadRequest {
		t.Fatalf("чужой host: %d", w.Code)
	}
}

func TestAppGateParentRevoked(t *testing.T) {
	e, c := gateEnv(t)
	tok := fullLogin(t, e, c, "/")
	if r := c.do("POST", "/api/v1/auth/logout", ""); r.status != 204 {
		t.Fatalf("выход: %d", r.status)
	}
	if w := verifyReq(e, appHost, "/", tok); w.Code != http.StatusFound {
		t.Fatalf("после выхода из NAS приложение доступно: %d", w.Code)
	}
}

func TestAppGateIdleAndTouch(t *testing.T) {
	e, c := gateEnv(t)
	tok := fullLogin(t, e, c, "/")
	// Пользование приложением продлевает и сессию NAS.
	for i := 0; i < 4; i++ {
		e.clk.Add(DefaultIdleTimeout / 2)
		if w := verifyReq(e, appHost, "/", tok); w.Code != 200 {
			t.Fatalf("шаг %d: %d", i, w.Code)
		}
	}
	if r := c.do("GET", "/api/v1/x", ""); r.status != 200 {
		t.Fatalf("сессия NAS не продлена: %d", r.status)
	}
	e.clk.Add(DefaultIdleTimeout)
	if w := verifyReq(e, appHost, "/", tok); w.Code != http.StatusFound {
		t.Fatalf("после простоя доступ остался: %d", w.Code)
	}
}

func TestSafeReturn(t *testing.T) {
	for in, want := range map[string]string{
		"/a?b=1": "/a?b=1", "//evil.com/": "/", "/\\evil": "/", "https://evil": "/", "": "/", "/a\r\nX: y": "/",
	} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q)=%q, ожидалось %q", in, got, want)
		}
	}
}

func TestAppGateDisabledWithoutOptions(t *testing.T) {
	e := newTEnv(t, nil)
	req := httptest.NewRequest("GET", "/verify", nil)
	req.Header.Set("X-Forwarded-Host", appHost)
	w := httptest.NewRecorder()
	e.svc.AppVerify().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("без настройки: %d", w.Code)
	}
	_ = context.Background()
}
