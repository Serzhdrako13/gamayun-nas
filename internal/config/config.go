// Package config — конфигурация gamayund: JSON-файл + флаги командной строки.
//
// JSON выбран, потому что разбирается стандартной библиотекой (TOML
// потребовал бы зависимость). Неизвестные поля — ошибка: опечатка в
// конфигурации не должна молча включать значение по умолчанию.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// Config — настройки gamayund.
type Config struct {
	// Listen — адрес API (HTTPS, если TLS не отключён). По умолчанию только
	// loopback; сетевой адрес разрешён при включённых TLS и аутентификации.
	Listen string `json:"listen"`
	// HelperSocket — Unix-сокет gamayun-helper.
	HelperSocket string `json:"helper_socket"`
	// Database — файл SQLite состояния.
	Database string `json:"database"`
	// InsecureAllowRemote разрешает слушать не-loopback адрес при
	// отключённых TLS или аутентификации. Только для изолированного стенда.
	InsecureAllowRemote bool    `json:"insecure_allow_remote"`
	TLS                 TLS     `json:"tls"`
	Auth                Auth    `json:"auth"`
	Proxy               Proxy   `json:"proxy"`
	Catalog             Catalog `json:"catalog"`
}

// Catalog — каталог приложений.
type Catalog struct {
	// URL — подписанный архив каталога (https://…/catalog.tar.gz); пусто —
	// только импорт файлом.
	URL string `json:"url"`
	// Dir — распакованный каталог; пусто — catalog/ рядом с базой.
	Dir string `json:"dir"`
	// KeysDir — дополнительные доверенные открытые ключи (*.pub, base64).
	KeysDir string `json:"keys_dir"`
	// AllowHTTP — разрешить http:// в URL (только изолированный стенд).
	AllowHTTP bool `json:"allow_http"`
}

// Proxy — публикация приложений через Caddy. Пусто — пути по умолчанию.
type Proxy struct {
	// AdminSocket — admin API Caddy (служба gamayun-caddy).
	AdminSocket string `json:"admin_socket"`
	// AuthSocket — unix-сокет gamayund для forward_auth (группа gamayun, 0660).
	AuthSocket string `json:"auth_socket"`
}

// TLS — HTTPS.
type TLS struct {
	// Disable — обычный HTTP. Допустимо только на loopback (или с
	// insecure_allow_remote).
	Disable bool `json:"disable"`
	// CertFile, KeyFile — свой сертификат (PEM). Пусто — самоподписанный
	// в каталоге данных (tls/ рядом с базой).
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	// RedirectListen — адрес дополнительного HTTP-слушателя, который
	// перенаправляет на HTTPS (308). Пусто — не нужен.
	RedirectListen string `json:"redirect_listen"`
}

// Auth — аутентификация.
type Auth struct {
	// Disable — без входа (только для разработки; на не-loopback адресе —
	// только с insecure_allow_remote).
	Disable bool `json:"disable"`
	// SessionIdle — простой сессии; SessionAbsolute — предельный срок.
	SessionIdle     Duration `json:"session_idle"`
	SessionAbsolute Duration `json:"session_absolute"`
}

// Duration — длительность в JSON строкой ("30m", "12h").
type Duration time.Duration

// UnmarshalJSON разбирает строку time.ParseDuration.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("длительность — строка вида \"30m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON — строкой.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// Default — значения по умолчанию (пути пакета для Debian).
func Default() Config {
	return Config{
		Listen:       "127.0.0.1:8780",
		HelperSocket: "/run/gamayun-helper/helper.sock",
		Database:     "/var/lib/gamayun-nas/gamayun.db",
		Auth: Auth{
			SessionIdle:     Duration(30 * time.Minute),
			SessionAbsolute: Duration(12 * time.Hour),
		},
	}
}

// Load читает файл поверх значений по умолчанию. Поля, отсутствующие в
// файле, сохраняют значения по умолчанию.
func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("конфигурация: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("конфигурация %s: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return c, fmt.Errorf("конфигурация %s: лишние данные после объекта", path)
	}
	return c, nil
}

// Validate проверяет значения.
func (c Config) Validate() error {
	if c.HelperSocket == "" {
		return errors.New("конфигурация: пустой helper_socket")
	}
	if c.Database == "" {
		return errors.New("конфигурация: пустой database")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("конфигурация: listen %q: %w", c.Listen, err)
	}
	// Сетевой адрес — только с TLS и аутентификацией; иначе явный
	// insecure_allow_remote (изолированный стенд).
	if !IsLoopback(host) && (c.TLS.Disable || c.Auth.Disable) && !c.InsecureAllowRemote {
		return fmt.Errorf("конфигурация: listen %q не loopback, а TLS или аутентификация отключены — "+
			"включите их или (только для изолированного стенда) задайте insecure_allow_remote", c.Listen)
	}
	if u := c.Catalog.URL; u != "" && !strings.HasPrefix(u, "https://") && !(c.Catalog.AllowHTTP && strings.HasPrefix(u, "http://")) {
		return fmt.Errorf("конфигурация: catalog.url %q — нужен https:// (http — только с catalog.allow_http на стенде)", u)
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return errors.New("конфигурация: tls.cert_file и tls.key_file задаются вместе")
	}
	if c.TLS.RedirectListen != "" {
		if c.TLS.Disable {
			return errors.New("конфигурация: tls.redirect_listen без TLS не имеет смысла")
		}
		if _, _, err := net.SplitHostPort(c.TLS.RedirectListen); err != nil {
			return fmt.Errorf("конфигурация: tls.redirect_listen %q: %w", c.TLS.RedirectListen, err)
		}
	}
	idle, abs := time.Duration(c.Auth.SessionIdle), time.Duration(c.Auth.SessionAbsolute)
	if idle < time.Minute || idle > 24*time.Hour {
		return errors.New("конфигурация: auth.session_idle должен быть от 1m до 24h")
	}
	if abs < idle || abs > 30*24*time.Hour {
		return errors.New("конфигурация: auth.session_absolute должен быть не меньше session_idle и не больше 720h")
	}
	return nil
}

// IsLoopback — адрес доступен только с этой машины. Пустой хост (все
// интерфейсы) — не loopback; имя localhost считается loopback.
func IsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
