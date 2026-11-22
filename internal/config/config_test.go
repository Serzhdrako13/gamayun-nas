package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConf(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gamayund.json")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMergesDefaults(t *testing.T) {
	c, err := Load(writeConf(t, `{"database": "/tmp/x.db"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Database != "/tmp/x.db" || c.Listen != Default().Listen || c.HelperSocket != Default().HelperSocket {
		t.Errorf("конфигурация: %+v", c)
	}
}

func TestLoadRejects(t *testing.T) {
	for name, body := range map[string]string{
		"неизвестное поле": `{"listen": "127.0.0.1:1", "lisen": "0.0.0.0:80"}`,
		"не тот тип":       `{"listen": 8080}`,
		"мусор после":      `{} {}`,
		"не JSON":          `listen = "x"`,
	} {
		if _, err := Load(writeConf(t, body)); err == nil {
			t.Errorf("%s: ожидалась ошибка", name)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "нет.json")); err == nil {
		t.Error("отсутствующий файл должен давать ошибку")
	}
}

func TestValidateListen(t *testing.T) {
	ok := []string{"127.0.0.1:8780", "[::1]:8780", "localhost:1"}
	remote := []string{"0.0.0.0:8780", ":8780", "192.168.1.5:80", "[::]:80"}
	for _, l := range append(ok, remote...) {
		c := Default()
		c.Listen = l
		// TLS и вход включены по умолчанию: сетевой адрес разрешён без флага.
		if err := c.Validate(); err != nil {
			t.Errorf("%s с TLS и входом: %v", l, err)
		}
	}
	for _, l := range ok {
		c := Default()
		c.Listen = l
		c.TLS.Disable, c.Auth.Disable = true, true
		if err := c.Validate(); err != nil {
			t.Errorf("%s без TLS/входа на loopback: %v", l, err)
		}
	}
	for _, l := range remote {
		for _, mod := range []func(*Config){
			func(c *Config) { c.TLS.Disable = true },
			func(c *Config) { c.Auth.Disable = true },
		} {
			c := Default()
			c.Listen = l
			mod(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("%s без TLS или входа: должен отклоняться без insecure_allow_remote", l)
			}
			c.InsecureAllowRemote = true
			if err := c.Validate(); err != nil {
				t.Errorf("%s с insecure_allow_remote: %v", l, err)
			}
		}
	}
	c := Default()
	c.Listen = "127.0.0.1"
	if c.Validate() == nil {
		t.Error("адрес без порта должен отклоняться")
	}
	c = Default()
	c.Database = ""
	if c.Validate() == nil {
		t.Error("пустой database должен отклоняться")
	}
}

func TestTLSAuthConfig(t *testing.T) {
	c, err := Load(writeConf(t, `{"tls": {"cert_file": "/c.pem", "key_file": "/k.pem"},
		"auth": {"session_idle": "15m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.TLS.CertFile != "/c.pem" || time.Duration(c.Auth.SessionIdle) != 15*time.Minute ||
		time.Duration(c.Auth.SessionAbsolute) != 12*time.Hour {
		t.Errorf("конфигурация: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
	for name, body := range map[string]string{
		"длительность числом":   `{"auth": {"session_idle": 900}}`,
		"неверная длительность": `{"auth": {"session_idle": "15 минут"}}`,
		"неизвестное поле tls":  `{"tls": {"cert": "/c.pem"}}`,
	} {
		if _, err := Load(writeConf(t, body)); err == nil {
			t.Errorf("%s: ожидалась ошибка", name)
		}
	}
	for name, mod := range map[string]func(*Config){
		"cert без key":         func(c *Config) { c.TLS.CertFile = "/c.pem" },
		"redirect без TLS":     func(c *Config) { c.TLS.Disable, c.TLS.RedirectListen = true, ":80" },
		"redirect без порта":   func(c *Config) { c.TLS.RedirectListen = "0.0.0.0" },
		"простой < 1m":         func(c *Config) { c.Auth.SessionIdle = Duration(time.Second) },
		"срок меньше простоя":  func(c *Config) { c.Auth.SessionAbsolute = Duration(time.Minute) },
		"срок больше 30 суток": func(c *Config) { c.Auth.SessionAbsolute = Duration(31 * 24 * time.Hour) },
	} {
		c := Default()
		mod(&c)
		if c.Validate() == nil {
			t.Errorf("%s: должно отклоняться", name)
		}
	}
}
