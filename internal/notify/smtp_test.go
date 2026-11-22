package notify

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/tlscert"
)

// fakeSMTP — минимальный SMTP-сервер: STARTTLS/неявный TLS, AUTH PLAIN.
type fakeSMTP struct {
	ln       net.Listener
	tlsConf  *tls.Config
	implicit bool
	noTLS    bool // не объявлять STARTTLS
	user     string
	pass     string
	certPEM  string

	mu   sync.Mutex
	msgs []fakeMsg
	auth []string // строки AUTH как пришли (проверка, что пароль только в TLS)
}

type fakeMsg struct {
	from string
	to   []string
	data string
	tls  bool
}

func newFakeSMTP(t *testing.T, implicit bool) *fakeSMTP {
	t.Helper()
	certPEM, keyPEM, err := tlscert.Generate(tlscert.Names{Hosts: []string{"localhost"}, IPs: []net.IP{net.IPv4(127, 0, 0, 1)}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, tlsConf: &tls.Config{Certificates: []tls.Certificate{cert}}, implicit: implicit,
		user: "nas", pass: "s3cret", certPEM: string(certPEM)}
	if implicit {
		f.ln = tls.NewListener(ln, f.tlsConf)
	}
	t.Cleanup(func() { f.ln.Close() })
	go func() {
		for {
			c, err := f.ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) cfg(sec string) SMTPConfig {
	return SMTPConfig{Enabled: true, Host: "localhost", Port: f.port(), Security: sec,
		Username: f.user, Password: f.pass, From: "NAS <nas@example.org>",
		To: []string{"admin@example.org", "Дежурный <duty@example.org>"}, CACert: f.certPEM}
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer func() { c.Close() }()
	isTLS := f.implicit
	r := bufio.NewReader(c)
	w := func(s string) { c.Write([]byte(s + "\r\n")) }
	w("220 fake ESMTP")
	var m fakeMsg
	authed := false
	for {
		c.SetDeadline(time.Now().Add(5 * time.Second))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch cmd {
		case "EHLO", "HELO":
			w("250-fake")
			if !isTLS && !f.noTLS {
				w("250-STARTTLS")
			}
			if isTLS {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case "STARTTLS":
			w("220 go")
			tc := tls.Server(c, f.tlsConf)
			if err := tc.Handshake(); err != nil {
				return
			}
			c, r, isTLS = tc, bufio.NewReader(tc), true
		case "AUTH":
			f.mu.Lock()
			f.auth = append(f.auth, line)
			f.mu.Unlock()
			parts := strings.Fields(line)
			raw, _ := base64.StdEncoding.DecodeString(parts[len(parts)-1])
			if string(raw) == "\x00"+f.user+"\x00"+f.pass {
				authed = true
				w("235 ok")
			} else {
				w("535 bad credentials")
			}
		case "MAIL":
			if f.user != "" && !authed {
				w("530 auth required")
				continue
			}
			m = fakeMsg{from: line, tls: isTLS}
			w("250 ok")
		case "RCPT":
			m.to = append(m.to, line)
			w("250 ok")
		case "DATA":
			w("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			m.data = b.String()
			f.mu.Lock()
			f.msgs = append(f.msgs, m)
			f.mu.Unlock()
			w("250 queued")
		case "QUIT":
			w("221 bye")
			return
		default:
			w("502 ?")
		}
	}
}

func note() Notification {
	return Notification{ID: 1, EventID: 42, Time: time.Now(), Type: events.ArrayDegraded, Severity: events.SevCritical,
		Subject: "pool:test1", Data: []byte(`{"pool":"test1","missing":1}`)}
}

func TestSMTPStartTLSAndImplicit(t *testing.T) {
	for _, sec := range []string{SMTPStartTLS, SMTPTLS} {
		t.Run(sec, func(t *testing.T) {
			f := newFakeSMTP(t, sec == SMTPTLS)
			s := &SMTPSender{Cfg: f.cfg(sec), Hostname: "nas1"}
			if err := s.Send(context.Background(), note()); err != nil {
				t.Fatal(err)
			}
			if len(f.msgs) != 1 {
				t.Fatalf("писем %d", len(f.msgs))
			}
			m := f.msgs[0]
			if !m.tls || len(m.to) != 2 || !strings.Contains(m.to[1], "duty@example.org") {
				t.Fatalf("письмо: %+v", m)
			}
			for _, want := range []string{"Subject: =?utf-8?q?", "pool:test1", "\"missing\": 1", "Событие №42", "Auto-Submitted: auto-generated"} {
				if !strings.Contains(m.data, want) {
					t.Fatalf("нет %q в письме:\n%s", want, m.data)
				}
			}
		})
	}
}

func TestSMTPTLSVerification(t *testing.T) {
	f := newFakeSMTP(t, false)
	c := f.cfg(SMTPStartTLS)
	c.CACert = "" // системные корни — самоподписанный не доверен
	err := (&SMTPSender{Cfg: c}).Test(context.Background())
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("ждали отказ проверки сертификата: %v", err)
	}
	if len(f.auth) != 0 {
		t.Fatal("пароль отправлен до проверки TLS")
	}
	c.InsecureSkipVerify = true
	if err := (&SMTPSender{Cfg: c}).Test(context.Background()); err != nil {
		t.Fatalf("insecure_skip_verify: %v", err)
	}
}

func TestSMTPNoStartTLSRefused(t *testing.T) {
	f := newFakeSMTP(t, false)
	f.noTLS = true
	err := (&SMTPSender{Cfg: f.cfg(SMTPStartTLS)}).Test(context.Background())
	if err == nil || !strings.Contains(err.Error(), "не поддерживает STARTTLS") {
		t.Fatalf("ждали отказ: %v", err)
	}
	if len(f.auth) != 0 {
		t.Fatal("пароль ушёл открытым текстом")
	}
}

func TestSMTPBadPasswordNotInError(t *testing.T) {
	f := newFakeSMTP(t, false)
	c := f.cfg(SMTPStartTLS)
	c.Password = "wrong-pass-123"
	err := (&SMTPSender{Cfg: c}).Test(context.Background())
	if err == nil || strings.Contains(err.Error(), c.Password) || !strings.Contains(err.Error(), "535") {
		t.Fatalf("ошибка входа: %v", err)
	}
}

func TestSMTPUnreachableTimeout(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0") // принимает, но молчит
	defer ln.Close()
	c := SMTPConfig{Enabled: true, Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Security: SMTPStartTLS,
		From: "a@b.c", To: []string{"d@e.f"}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := (&SMTPSender{Cfg: c}).Test(ctx); err == nil {
		t.Fatal("ждали ошибку")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("таймаут не сработал")
	}
}

func TestSMTPValidate(t *testing.T) {
	ok := SMTPConfig{Enabled: true, Host: "mail.example.org", Port: 587, Security: SMTPStartTLS, From: "a@b.c", To: []string{"d@e.f"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(*SMTPConfig){
		func(c *SMTPConfig) { c.Host = "" },
		func(c *SMTPConfig) { c.Port = 0 },
		func(c *SMTPConfig) { c.Security = "ssl3" },
		func(c *SMTPConfig) { c.Security, c.Username = SMTPNone, "u" },
		func(c *SMTPConfig) { c.From = "нет" },
		func(c *SMTPConfig) { c.To = nil },
		func(c *SMTPConfig) { c.To = []string{"x\r\nBcc: y@z"} },
		func(c *SMTPConfig) { c.CACert = "мусор" },
	}
	for i, f := range bad {
		c := ok
		c.To = append([]string(nil), ok.To...)
		f(&c)
		if c.Validate() == nil {
			t.Fatalf("случай %d прошёл проверку", i)
		}
	}
	if (SMTPConfig{}).Validate() != nil {
		t.Fatal("выключенный канал не проверяется")
	}
}

func TestSMTPEndToEndViaOutbox(t *testing.T) {
	ctx := context.Background()
	f := newFakeSMTP(t, false)
	db, l := open(t, filepath.Join(t.TempDir(), "d.db"))
	if err := SaveSMTP(ctx, db, f.cfg(SMTPStartTLS)); err != nil {
		t.Fatal(err)
	}
	c, err := LoadSMTP(ctx, db)
	if err != nil || c.Password != f.pass || c.Port != f.port() {
		t.Fatalf("загрузка: %+v %v", c, err)
	}
	d := NewDeliverer(db, DeliverOptions{})
	d.ApplySMTP(c)
	s := New(db, l, Options{Channels: d.Channels, OnQueued: d.Kick})
	s.Init(ctx)
	emit(t, l, events.DiskRemoved, events.SevCritical)
	s.CatchUp(ctx)
	if _, err := d.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.msgs) != 1 {
		t.Fatalf("писем %d", len(f.msgs))
	}
	st, _ := d.Status(ctx)
	if st[0].LastSuccess == nil {
		t.Fatalf("статус: %+v", st)
	}
}
