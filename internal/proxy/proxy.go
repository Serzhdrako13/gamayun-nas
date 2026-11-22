// Package proxy — публикация веб-приложений через Caddy.
//
// Caddy (пакет gamayun-nas-apps, служба gamayun-caddy) слушает :443 и принимает
// конфигурацию только от gamayund через admin API на unix-сокете. gamayund строит
// конфиг целиком из опубликованных приложений: имя <app>-<хост>.local (mdns.go) →
// forward_auth к gamayund (unix-сокет, auth.AppVerify) → reverse_proxy на
// 127.0.0.1:<порт приложения>. Сертификаты выдаёт внутренний CA Caddy
// (tls internal); корневой сертификат отдаётся в веб для установки на клиентах.
//
// Сверка идемпотентна: POST /load с тем же конфигом Caddy не перезапускает.
// Caddy не установлен (нет сокета) — состояние «нет прокси», без ошибок в журнале.
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Пути по умолчанию (см. packaging/apps/systemd/gamayun-caddy.service, gamayund.service).
const (
	DefaultAdminSocket = "/run/gamayun-caddy/admin.sock"
	DefaultAuthSocket  = "/run/gamayund/proxy-auth.sock"
	VerifyPath         = "/verify"
	UserHeader         = "X-Gamayun-User"
	CAName             = "gamayun-nas Local CA"
)

// Route — опубликованное приложение.
type Route struct {
	App  string `json:"app"`  // id приложения
	Name string `json:"name"` // метка поддомена
	Port int    `json:"port"` // порт на 127.0.0.1
}

var labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// ValidName — метка поддомена: латиница в нижнем регистре, цифры, дефис, до 32.
func ValidName(s string) bool { return labelRe.MatchString(s) }

// Options — параметры службы.
type Options struct {
	Logger      *slog.Logger
	AdminSocket string
	AuthSocket  string
	// Hostname — имя NAS без домена (os.Hostname по умолчанию).
	Hostname func() string
	// PortalPort — порт веб-интерфейса gamayund (для возврата на вход).
	PortalPort string
	// Routes — текущие опубликованные приложения (из internal/apps).
	Routes func(ctx context.Context) []Route
	// Interval — период сверки (по умолчанию 30 с).
	Interval time.Duration
}

// Status — состояние для API.
type Status struct {
	Available bool      `json:"available"` // Caddy установлен и отвечает
	Domain    string    `json:"domain"`    // <хост>.local
	Routes    []Hosted  `json:"routes"`
	Applied   time.Time `json:"applied,omitzero"`
	Error     string    `json:"error,omitempty"`
}

// Hosted — маршрут с полным адресом.
type Hosted struct {
	Route
	Host string `json:"host"`
	URL  string `json:"url"`
	MDNS string `json:"mdns_error,omitempty"` // имя не опубликовано в mDNS
}

// Service — синхронизация конфигурации Caddy.
type Service struct {
	o       Options
	client  *http.Client
	trigger chan struct{}

	mu      sync.Mutex
	st      Status
	hosts   map[string]bool
	applied string // sha256 последнего принятого конфига
	errOnce string
	mdns    *mdns
}

// New создаёт службу.
func New(o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.AdminSocket == "" {
		o.AdminSocket = DefaultAdminSocket
	}
	if o.AuthSocket == "" {
		o.AuthSocket = DefaultAuthSocket
	}
	if o.Hostname == nil {
		o.Hostname = func() string { h, _ := os.Hostname(); return h }
	}
	if o.Interval <= 0 {
		o.Interval = 30 * time.Second
	}
	sock := o.AdminSocket
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	m := newMDNS(o.Logger, nil)
	return &Service{o: o, client: &http.Client{Transport: tr, Timeout: 30 * time.Second},
		trigger: make(chan struct{}, 1), hosts: map[string]bool{}, mdns: m}
}

func (s *Service) label() string {
	h := strings.ToLower(strings.TrimSpace(s.o.Hostname()))
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

// Domain — <хост>.local (адрес веб-интерфейса NAS).
func (s *Service) Domain() string { return s.label() + ".local" }

// Host — имя приложения <app>-<хост>.local.
func (s *Service) Host(name string) string { return name + "-" + s.label() + ".local" }

// CheckName — метка имени приложения укладывается в 63 символа DNS.
func (s *Service) CheckName(name string) error {
	if n := len(name) + 1 + len(s.label()); n > 63 {
		return fmt.Errorf("имя %q с именем NAS длиннее 63 символов (%d)", s.Host(name), n)
	}
	return nil
}

// Allowed — поддомен опубликован (для auth.AppGateOptions).
func (s *Service) Allowed(host string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hosts[host]
}

// Portal — адрес веб-интерфейса NAS для возврата на вход.
func (s *Service) Portal(string) string {
	u := "https://" + s.Domain()
	if s.o.PortalPort != "" && s.o.PortalPort != "443" {
		u += ":" + s.o.PortalPort
	}
	return u
}

// Status — последнее состояние.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.Routes = append([]Hosted{}, s.st.Routes...)
	return st
}

// Trigger — сверить немедленно (маршруты изменились).
func (s *Service) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Run — цикл сверки до отмены ctx.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.o.Interval)
	defer t.Stop()
	defer s.mdns.stop()
	for {
		s.Sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.trigger:
		}
	}
}

// Sync строит конфиг и отправляет его Caddy, если он изменился или Caddy
// мог потерять его (перезапуск — проверяется по хэшу в /config/).
func (s *Service) Sync(ctx context.Context) {
	var routes []Route
	if s.o.Routes != nil {
		routes = s.o.Routes(ctx)
	}
	dom := s.Domain()
	hosted := make([]Hosted, 0, len(routes))
	hosts := map[string]bool{}
	var names []string
	for _, r := range routes {
		if s.CheckName(r.Name) != nil {
			continue
		}
		h := s.Host(r.Name)
		hosts[h] = true
		names = append(names, h)
		hosted = append(hosted, Hosted{Route: r, Host: h, URL: "https://" + h + "/"})
	}
	sort.Slice(hosted, func(i, j int) bool { return hosted[i].Host < hosted[j].Host })
	merr := s.mdns.sync(ctx, names)
	for i := range hosted {
		hosted[i].MDNS = merr
	}
	cfg := Build(s.o.AdminSocket, s.o.AuthSocket, hosted)
	body, _ := json.Marshal(cfg)
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])

	s.mu.Lock()
	s.hosts = hosts
	s.st.Domain, s.st.Routes = dom, hosted
	s.mu.Unlock()

	err := s.apply(ctx, body, want)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err == nil:
		s.st.Available, s.st.Error = true, ""
		if s.applied != want {
			s.applied = want
			s.st.Applied = time.Now()
			s.o.Logger.Info("прокси: конфигурация Caddy применена", "routes", len(hosted))
		}
		s.errOnce = ""
	case errors.Is(err, errNoCaddy):
		s.st.Available, s.st.Error, s.applied = false, "", ""
	default:
		s.st.Available, s.st.Error, s.applied = true, err.Error(), ""
		if s.errOnce != err.Error() {
			s.errOnce = err.Error()
			s.o.Logger.Warn("прокси: конфигурация Caddy не применена", "err", err)
		}
	}
}

var errNoCaddy = errors.New("Caddy не установлен или не запущен")

// apply — POST /load, если текущий конфиг Caddy отличается от нужного
// (сравнение по содержимому: Caddy после перезапуска с --resume хранит свой).
func (s *Service) apply(ctx context.Context, body []byte, _ string) error {
	if _, err := os.Stat(s.o.AdminSocket); err != nil {
		return errNoCaddy
	}
	cur, err := s.do(ctx, "GET", "/config/", nil)
	if errors.Is(err, errNoCaddy) {
		return err
	}
	if err == nil && sameJSON(cur, body) {
		return nil
	}
	_, err = s.do(ctx, "POST", "/load", body)
	return err
}

func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func (s *Service) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://caddy"+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := s.client.Do(req)
	if err != nil {
		var ne *net.OpError
		if errors.As(err, &ne) && ne.Op == "dial" {
			return nil, errNoCaddy
		}
		return nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode/100 != 2 {
		return b, fmt.Errorf("Caddy %s %s: %d %s", method, path, res.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// RootCA — корневой сертификат внутреннего CA (PEM) для установки на клиентах.
func (s *Service) RootCA(ctx context.Context) ([]byte, error) {
	if _, err := os.Stat(s.o.AdminSocket); err != nil {
		return nil, errNoCaddy
	}
	b, err := s.do(ctx, "GET", "/pki/ca/local", nil)
	if err != nil {
		return nil, err
	}
	var v struct {
		Root string `json:"root_certificate"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.Root == "" {
		return nil, fmt.Errorf("Caddy: корневой сертификат не получен")
	}
	return []byte(v.Root), nil
}

// ErrNoCaddy — прокси недоступен.
func ErrNoCaddy(err error) bool { return errors.Is(err, errNoCaddy) }

type obj = map[string]any

// Build — полный JSON-конфиг Caddy. Admin API — только unix-сокет (иначе
// /load вернул бы его на localhost:2019). Редирект с :80 выключен (порт 80
// может занимать редирект gamayund). Доверие CA в систему не ставится.
func Build(adminSock, authSock string, routes []Hosted) obj {
	var rs []any
	var subjects []string
	for _, r := range routes {
		subjects = append(subjects, r.Host)
		rs = append(rs, obj{
			"match":    []any{obj{"host": []string{r.Host}}},
			"terminal": true,
			"handle": []any{obj{"handler": "subroute", "routes": []any{obj{"handle": []any{
				// Подделанный клиентом заголовок пользователя — удалить до проверки.
				obj{"handler": "headers", "request": obj{"delete": []string{UserHeader}}},
				forwardAuth(authSock),
				obj{"handler": "reverse_proxy", "upstreams": []any{obj{"dial": "127.0.0.1:" + strconv.Itoa(r.Port)}}},
			}}}}},
		})
	}
	if rs == nil {
		rs = []any{}
	}
	apps := obj{
		"pki": obj{"certificate_authorities": obj{"local": obj{"name": CAName, "install_trust": false}}},
	}
	if len(routes) > 0 {
		apps["http"] = obj{"servers": obj{"gamayun": obj{
			"listen":          []string{":443"},
			"routes":          rs,
			"automatic_https": obj{"disable_redirects": true},
		}}}
		apps["tls"] = obj{"automation": obj{"policies": []any{obj{
			"subjects": subjects, "issuers": []any{obj{"module": "internal"}},
		}}}}
	}
	return obj{
		"@id":   "gamayun-config",
		"admin": obj{"listen": "unix/" + adminSock + "|0660"},
		"apps":  apps,
	}
}

// forwardAuth — эквивалент директивы forward_auth Caddyfile (caddy adapt 2.11.9).
func forwardAuth(sock string) obj {
	val := "{http.reverse_proxy.header." + UserHeader + "}"
	return obj{
		"handler":   "reverse_proxy",
		"upstreams": []any{obj{"dial": "unix/" + sock}},
		"rewrite":   obj{"method": "GET", "uri": VerifyPath},
		"headers": obj{"request": obj{"set": obj{
			"X-Forwarded-Method": []string{"{http.request.method}"},
			"X-Forwarded-Uri":    []string{"{http.request.uri}"},
		}}},
		"handle_response": []any{obj{
			"match": obj{"status_code": []int{2}},
			"routes": []any{
				obj{"handle": []any{obj{"handler": "vars"}}},
				obj{"handle": []any{obj{"handler": "headers", "request": obj{"delete": []string{UserHeader}}}}},
				obj{
					"match":  []any{obj{"not": []any{obj{"vars": obj{val: []string{""}}}}}},
					"handle": []any{obj{"handler": "headers", "request": obj{"set": obj{UserHeader: []string{val}}}}},
				},
			},
		}},
	}
}
