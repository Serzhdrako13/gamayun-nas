package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"gamayun-nas/internal/brand"
)

// WebhookConfig — настройки канала webhook. Secret (ключ подписи HMAC)
// наружу не отдаётся: API показывает только признак SecretSet.
type WebhookConfig struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
	Secret  string `json:"secret,omitempty"`
	// CACert — PEM доверенного сертификата/ЦС получателя. Пусто — системные корни.
	CACert             string `json:"ca_cert,omitempty"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
	// AllowHTTP — разрешить http:// (только явно; по умолчанию только https).
	AllowHTTP bool `json:"allow_http,omitempty"`
}

// Validate — проверка настроек перед сохранением (включённый канал).
func (c WebhookConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" {
		return errors.New("webhook: неверный адрес")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !c.AllowHTTP {
			return errors.New("webhook: адрес http:// без шифрования не разрешён")
		}
		if c.Secret != "" {
			return errors.New("webhook: ключ подписи по http:// не передаётся — нужен https://")
		}
	default:
		return errors.New("webhook: адрес должен начинаться с https://")
	}
	if u.User != nil {
		return errors.New("webhook: логин и пароль в адресе не допускаются — используйте ключ подписи")
	}
	if c.CACert != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(c.CACert)) {
		return errors.New("webhook: доверенный сертификат не в формате PEM")
	}
	return nil
}

// WebhookPayload — тело запроса (JSON, версия схемы 1).
type WebhookPayload struct {
	Version int          `json:"version"`
	Source  string       `json:"source"`
	Host    string       `json:"host"`
	Test    bool         `json:"test,omitempty"`
	Title   string       `json:"title"`
	Event   Notification `json:"event"`
}

// WebhookSender — POST JSON на адрес получателя.
type WebhookSender struct {
	Cfg      WebhookConfig
	Hostname string
	Now      func() time.Time
}

// Заголовки подписи: X-Gamayun-Timestamp (Unix, с) и
// X-Gamayun-Signature: sha256=hex(HMAC-SHA256(secret, timestamp + "." + body)).
const (
	HeaderTimestamp = "X-Gamayun-Timestamp"
	HeaderSignature = "X-Gamayun-Signature"
)

func (s *WebhookSender) hostname() string {
	if s.Hostname != "" {
		return s.Hostname
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "localhost"
}

// Send отправляет уведомление.
func (s *WebhookSender) Send(ctx context.Context, n Notification) error {
	title, _ := Format(n, s.hostname())
	return s.post(ctx, WebhookPayload{Version: 1, Source: brand.Name, Host: s.hostname(), Title: title, Event: n})
}

// Test — тестовый запрос (кнопка «Проверить»).
func (s *WebhookSender) Test(ctx context.Context) error {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return s.post(ctx, WebhookPayload{Version: 1, Source: brand.Name, Host: s.hostname(), Test: true,
		Title: fmt.Sprintf("[%s %s] Тестовое уведомление", brand.Name, s.hostname()),
		Event: Notification{Time: now().UTC(), Type: "notify.test", Severity: "info"}})
}

// Sign — подпись тела (для проверки на стороне получателя и в тестах).
func Sign(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func (s *WebhookSender) post(ctx context.Context, p WebhookPayload) error {
	if err := s.Cfg.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: s.Cfg.InsecureSkipVerify}
	if s.Cfg.CACert != "" {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(s.Cfg.CACert))
		tc.RootCAs = pool
	}
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: tc, Proxy: nil, DisableKeepAlives: true},
		// Перенаправления не выполняем: подпись и тело не должны уйти по чужому адресу.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Cfg.URL, bytes.NewReader(body))
	if err != nil {
		return errors.New("webhook: неверный адрес")
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", brand.Name+"-notify/1")
	if s.Cfg.Secret != "" {
		now := time.Now
		if s.Now != nil {
			now = s.Now
		}
		ts := strconv.FormatInt(now().Unix(), 10)
		req.Header.Set(HeaderTimestamp, ts)
		req.Header.Set(HeaderSignature, Sign(s.Cfg.Secret, ts, body))
	}
	resp, err := client.Do(req)
	if err != nil {
		// url.Error содержит полный адрес (в пути бывает токен получателя) —
		// в текст ошибки (журнал, outbox, API) берём только причину.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("webhook: %s: %w", redactHost(s.Cfg.URL), err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook: ответ %s", resp.Status)
	}
	return nil
}

func redactHost(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "?"
	}
	return p.Host
}
