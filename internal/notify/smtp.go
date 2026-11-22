package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	"gamayun-nas/internal/brand"
)

// Режимы защиты SMTP.
const (
	SMTPStartTLS = "starttls" // обычный порт (587/25) + обязательный STARTTLS
	SMTPTLS      = "tls"      // TLS сразу (465)
	SMTPNone     = "none"     // без шифрования — только без пароля (локальный relay)
)

// SMTPConfig — настройки канала SMTP. Password хранится в базе (файл 0600)
// и наружу не отдаётся: API показывает только признак PasswordSet.
type SMTPConfig struct {
	Enabled  bool     `json:"enabled"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Security string   `json:"security"`
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	// CACert — PEM доверенного сертификата/ЦС (свой сервер с самоподписанным
	// сертификатом). Пусто — системные корни.
	CACert string `json:"ca_cert,omitempty"`
	// InsecureSkipVerify — не проверять сертификат (явный выбор оператора).
	InsecureSkipVerify bool `json:"insecure_skip_verify,omitempty"`
}

// Validate — проверка настроек перед сохранением (включённый канал).
func (c SMTPConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Host == "" || strings.ContainsAny(c.Host, " \r\n/") {
		return errors.New("smtp: не указан или неверен сервер")
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("smtp: неверный порт")
	}
	switch c.Security {
	case SMTPStartTLS, SMTPTLS:
	case SMTPNone:
		if c.Username != "" || c.Password != "" {
			return errors.New("smtp: вход по паролю без шифрования запрещён")
		}
	default:
		return errors.New("smtp: неизвестный режим защиты")
	}
	if _, err := mail.ParseAddress(c.From); err != nil {
		return errors.New("smtp: неверный адрес отправителя")
	}
	if len(c.To) == 0 {
		return errors.New("smtp: нет получателей")
	}
	for _, t := range c.To {
		if _, err := mail.ParseAddress(t); err != nil {
			return fmt.Errorf("smtp: неверный адрес получателя %q", t)
		}
	}
	if c.CACert != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(c.CACert)) {
		return errors.New("smtp: доверенный сертификат не в формате PEM")
	}
	return nil
}

// SMTPSender — отправка писем по SMTPConfig.
type SMTPSender struct {
	Cfg      SMTPConfig
	Hostname string // имя узла в письме и EHLO (пусто — os.Hostname)
	Now      func() time.Time
}

func (s *SMTPSender) hostname() string {
	if s.Hostname != "" {
		return s.Hostname
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "localhost"
}

func (s *SMTPSender) tlsConfig() (*tls.Config, error) {
	tc := &tls.Config{ServerName: s.Cfg.Host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: s.Cfg.InsecureSkipVerify}
	if s.Cfg.CACert != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(s.Cfg.CACert)) {
			return nil, errors.New("smtp: доверенный сертификат не в формате PEM")
		}
		tc.RootCAs = pool
	}
	return tc, nil
}

// Send отправляет уведомление.
func (s *SMTPSender) Send(ctx context.Context, n Notification) error {
	subj, body := Format(n, s.hostname())
	return s.send(ctx, subj, body)
}

// Test — тестовое письмо (кнопка «Проверить»).
func (s *SMTPSender) Test(ctx context.Context) error {
	h := s.hostname()
	return s.send(ctx, fmt.Sprintf("[%s %s] Тестовое письмо", brand.Name, h),
		fmt.Sprintf("Это тестовое письмо от %s (%s). Настройка SMTP работает.\n", brand.Name, h))
}

func (s *SMTPSender) send(ctx context.Context, subject, body string) error {
	if err := s.Cfg.Validate(); err != nil {
		return err
	}
	tc, err := s.tlsConfig()
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(s.Cfg.Host, strconv.Itoa(s.Cfg.Port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp: соединение: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if s.Cfg.Security == SMTPTLS {
		tconn := tls.Client(conn, tc)
		if err := tconn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return fmt.Errorf("smtp: TLS: %w", err)
		}
		conn = tconn
	}
	c, err := smtp.NewClient(conn, s.Cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp: приветствие: %w", err)
	}
	defer c.Close()
	if err := c.Hello(s.hostname()); err != nil {
		return fmt.Errorf("smtp: EHLO: %w", err)
	}
	if s.Cfg.Security == SMTPStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("smtp: сервер не поддерживает STARTTLS")
		}
		if err := c.StartTLS(tc); err != nil {
			return fmt.Errorf("smtp: STARTTLS: %w", err)
		}
	}
	if s.Cfg.Username != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("smtp: сервер не поддерживает вход (AUTH)")
		}
		// Пароль в текст ошибки не попадает: net/smtp возвращает ответ сервера.
		if err := c.Auth(smtp.PlainAuth("", s.Cfg.Username, s.Cfg.Password, s.Cfg.Host)); err != nil {
			return fmt.Errorf("smtp: вход: %w", err)
		}
	}
	from, _ := mail.ParseAddress(s.Cfg.From)
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("smtp: MAIL FROM: %w", err)
	}
	var to []string
	for _, t := range s.Cfg.To {
		a, _ := mail.ParseAddress(t)
		if err := c.Rcpt(a.Address); err != nil {
			return fmt.Errorf("smtp: RCPT TO %s: %w", a.Address, err)
		}
		to = append(to, a.String())
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: DATA: %w", err)
	}
	if _, err := w.Write(s.message(from.String(), to, subject, body)); err != nil {
		w.Close()
		return fmt.Errorf("smtp: передача: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: письмо не принято: %w", err)
	}
	return c.Quit()
}

func (s *SMTPSender) message(from string, to []string, subject, body string) []byte {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	var b bytes.Buffer
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", from)
	h("To", strings.Join(to, ", "))
	h("Subject", mime.QEncoding.Encode("utf-8", subject))
	h("Date", now().Format(time.RFC1123Z))
	h("Message-ID", fmt.Sprintf("<%d.%s@%s>", now().UnixNano(), strings.ToLower(brand.Name), s.hostname()))
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "8bit")
	h("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	// Точки в начале строк экранирует writer net/smtp (textproto.DotWriter).
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		b.WriteString(line + "\r\n")
	}
	return b.Bytes()
}

var sevTitle = map[string]string{"info": "Информация", "warning": "Предупреждение", "critical": "Критично"}

var typeTitle = map[string]string{
	"disk.added":                       "Добавлен диск",
	"disk.removed":                     "Диск отключился",
	"disk.returned":                    "Диск вернулся",
	"disk.failed":                      "Диск отказал",
	"array.degraded":                   "Массив RAID неполный: выбыл диск",
	"array.recovering":                 "Идёт восстановление массива",
	"array.recovered":                  "Массив восстановлен",
	"integrity.errors":                 "Найдены ошибки целостности данных",
	"array.readd_failed":               "Диск не возвращён в массив",
	"pool.emergency_ro":                "Пул аварийно перешёл в режим только для чтения",
	"pool.emergency_ro_cleared":        "Пул снова доступен для записи",
	"pool.protection_bypassed":         "Запас избыточности исчерпан, защита пула выключена — пул остаётся на запись",
	"pool.protection_bypassed_cleared": "Пул больше не работает без защиты при исчерпанном запасе",
	"pool.scrub_finished":              "Проверка данных пула завершена",
	"pool.scrub_failed":                "Проверка данных пула не завершена",
	"disk.smart_failed":                "SMART: диск неисправен",
	"disk.smart_sectors":               "SMART: растёт число плохих секторов",
	"disk.smart_test_failed":           "SMART: самотест с ошибкой",
	"disk.smart_test_passed":           "SMART: самотест пройден",
	"disk.smart_enabled":               "SMART включён на диске",
	"disk.smart_disabled":              "SMART выключен на диске",
	TypeQuietStarted:                   "Включена тишина уведомлений",
	TypeQuietEnded:                     "Тишина уведомлений закончилась",
	"snapshot.created":                 "Создан снимок папки",
	"snapshot.deleted":                 "Удалён снимок папки",
	"snapshot.restored":                "Восстановлено из снимка",
	"snapshot.restore_failed":          "Восстановление из снимка не выполнено",
	"snapshot.skipped":                 "Снимок папки пропущен",
	"snapshot.failed":                  "Снимок папки не создан",
	"disk.overheat":                    "Перегрев диска", "disk.temp_normal": "Температура диска в норме",
}

// quietSuppressed — число подавленных в данных уведомления об окончании тишины.
func quietSuppressed(n Notification) int {
	if n.Type != TypeQuietEnded {
		return 0
	}
	var d struct {
		Suppressed int `json:"suppressed"`
	}
	_ = json.Unmarshal(n.Data, &d)
	return d.Suppressed
}

// Format — тема и текст уведомления. Data — как есть (секретов в событиях нет).
func Format(n Notification, host string) (subject, body string) {
	title := typeTitle[n.Type]
	if title == "" {
		title = n.Type
	}
	sev := sevTitle[n.Severity]
	if sev == "" {
		sev = n.Severity
	}
	if k := quietSuppressed(n); k > 0 {
		title += fmt.Sprintf(": за время тишины не отправлено событий — %d", k)
	}
	subject = fmt.Sprintf("[%s %s] %s: %s", brand.Name, host, sev, title)
	if n.Subject != "" {
		subject += " (" + n.Subject + ")"
	}
	if n.Repeats > 0 {
		subject += fmt.Sprintf(" — повторов: %d", n.Repeats)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nУзел: %s\nВремя: %s\nУровень: %s\nТип: %s\n", title, host,
		n.Time.Local().Format("2006-01-02 15:04:05 MST"), sev, n.Type)
	if n.Subject != "" {
		fmt.Fprintf(&b, "Объект: %s\n", n.Subject)
	}
	if n.Repeats > 0 {
		fmt.Fprintf(&b, "\nСерия: после первого уведомления событие повторилось ещё %d раз", n.Repeats)
		if n.RepeatsSince != nil {
			fmt.Fprintf(&b, " (с %s)", n.RepeatsSince.Local().Format("15:04:05"))
		}
		b.WriteString("; ниже — последнее. Все повторы — в центре уведомлений.\n")
	}
	if len(n.Data) > 0 && string(n.Data) != "null" {
		var pretty bytes.Buffer
		if json.Indent(&pretty, n.Data, "", "  ") == nil {
			fmt.Fprintf(&b, "\nПодробности:\n%s\n", pretty.String())
		}
	}
	fmt.Fprintf(&b, "\nСобытие №%d. Письмо отправлено автоматически.\n", n.EventID)
	return subject, b.String()
}
