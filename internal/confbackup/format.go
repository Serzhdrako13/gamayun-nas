// Package confbackup — резервная копия настроек gamayun-nas.
//
// Копия — JSON в gzip без секретов: хэши паролей, сессии, пароль SMTP и
// ключ подписи webhook в неё не попадают (VISION 2026-11-11). Поэтому
// шифрование не нужно, а при восстановлении пароли и секреты задаются
// заново (мастер восстановления).
//
// Совместимость: в копии — версия формата, версия схемы базы и версия
// приложения. Формат новее известного коду — отказ с понятной ошибкой;
// старый совместимый формат приводится к текущему функцией upgrade.
package confbackup

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"gamayun-nas/internal/maintenance"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/notify"
	"gamayun-nas/internal/store"
	"gamayun-nas/internal/version"
)

// FormatName — признак файла копии.
const FormatName = "gamayun-nas-config"

// FormatVersion — текущая версия формата. Менять при несовместимом
// изменении Data и добавлять шаг в upgrade.
const FormatVersion = 1

// MinFormatVersion — самая старая версия формата, которую код умеет
// привести к текущей.
const MinFormatVersion = 1

// MaxSize — предел размера файла копии (сжатого) и распакованного JSON.
const (
	MaxSize         = 32 << 10
	MaxUnpackedSize = 4 << 20
)

// Ошибки разбора: ErrFormat — не файл копии или повреждён; ErrIncompatible
// — копия от более новой версии (формат или схема базы).
var (
	ErrFormat       = errors.New("файл не является резервной копией настроек gamayun-nas или повреждён")
	ErrIncompatible = errors.New("резервная копия несовместима с этой версией")
)

// Backup — файл копии.
type Backup struct {
	Format        string    `json:"format"`
	FormatVersion int       `json:"format_version"`
	AppVersion    string    `json:"app_version"`
	SchemaVersion int       `json:"schema_version"`
	CreatedAt     time.Time `json:"created_at"`
	Hostname      string    `json:"hostname,omitempty"`
	// SHA256 — контрольная сумма Data (JSON, как записан): обнаруживает
	// порчу и ручную правку. Не защищает от подделки (подписи нет).
	SHA256 string          `json:"sha256"`
	Data   json.RawMessage `json:"data"`
}

// Data — содержимое копии (формат 1).
type Data struct {
	Users  []User  `json:"users"`
	Groups []Group `json:"groups"`
	Shares []Share `json:"shares"`
	// Pools — пулы на момент копии, только для сверки: пулы не создаются
	// восстановлением (данные живут на дисках, сборка — отдельно).
	Pools       []Pool                `json:"pools"`
	Settings    Settings              `json:"settings"`
	Maintenance *maintenance.Schedule `json:"maintenance,omitempty"`
	SMTP        *notify.SMTPConfig    `json:"smtp,omitempty"`
	Webhook     *notify.WebhookConfig `json:"webhook,omitempty"`
	Secrets     SecretFlags           `json:"secrets"`
	// SFTP — назначение выгрузки (без ключа NAS: он свой у каждой установки).
	SFTP *SFTPConfig `json:"sftp,omitempty"`
}

// User — учётка (пароль не хранится).
type User struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Admin   bool   `json:"admin"`
	SSH     bool   `json:"ssh"`
}

// Group — группа и её члены (стандартные группы — только члены).
type Group struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

// Share — общая папка и права.
type Share struct {
	Name    string       `json:"name"`
	Pool    string       `json:"pool"`
	Comment string       `json:"comment"`
	Access  []nas.Access `json:"access"`
}

// Pool — пул (для сверки).
type Pool struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Integrity bool   `json:"integrity"`
}

// Settings — настройки сервера.
type Settings struct {
	ServerName string             `json:"server_name,omitempty"`
	Timezone   string             `json:"timezone,omitempty"`
	NTP        *bool              `json:"ntp,omitempty"`
	Homes      nas.HomesInfo      `json:"homes"`
	SSHGuard   *nas.GuardSettings `json:"ssh_guard,omitempty"`
}

// SecretFlags — какие секреты были заданы (сами значения в копию не входят).
type SecretFlags struct {
	SMTPPassword  bool `json:"smtp_password"`
	WebhookSecret bool `json:"webhook_secret"`
	// WebhookURL — в адресе были учётные данные или параметры запроса
	// (часто это токен): в копии адрес без них.
	WebhookURL bool `json:"webhook_url,omitempty"`
}

// Identity — часовой пояс и NTP с работающей системы (через helper);
// nil — не включать в копию.
type Identity struct {
	Timezone string
	NTP      bool
}

// Collect собирает копию из базы. Секреты вычищаются здесь и только здесь.
func Collect(ctx context.Context, db *sql.DB, id *Identity, now time.Time) (Backup, Data, error) {
	var d Data
	var err error
	if d.Users, err = users(ctx, db); err != nil {
		return Backup{}, d, err
	}
	if d.Groups, err = groups(ctx, db); err != nil {
		return Backup{}, d, err
	}
	if d.Shares, err = shares(ctx, db); err != nil {
		return Backup{}, d, err
	}
	if d.Pools, err = pools(ctx, db); err != nil {
		return Backup{}, d, err
	}
	kv, err := settingsKV(ctx, db)
	if err != nil {
		return Backup{}, d, err
	}
	d.Settings.ServerName = kv["server.name"]
	d.Settings.Homes = nas.HomesInfo{Enabled: kv["homes.enabled"] == "1", Pool: kv["homes.pool"]}
	if v, ok := kv["sshguard"]; ok {
		var g nas.GuardSettings
		if json.Unmarshal([]byte(v), &g) == nil {
			d.Settings.SSHGuard = &g
		}
	}
	if v, ok := kv["maintenance.schedule"]; ok {
		var sc maintenance.Schedule
		if json.Unmarshal([]byte(v), &sc) == nil {
			d.Maintenance = &sc
		}
	}
	if id != nil {
		d.Settings.Timezone = id.Timezone
		ntp := id.NTP
		d.Settings.NTP = &ntp
	}
	if _, ok := kv["notify.smtp"]; ok {
		c, err := notify.LoadSMTP(ctx, db)
		if err != nil {
			return Backup{}, d, err
		}
		d.Secrets.SMTPPassword = c.Password != ""
		c.Password = ""
		d.SMTP = &c
	}
	if _, ok := kv["notify.webhook"]; ok {
		c, err := notify.LoadWebhook(ctx, db)
		if err != nil {
			return Backup{}, d, err
		}
		d.Secrets.WebhookSecret = c.Secret != ""
		c.Secret = ""
		if u, err := url.Parse(c.URL); err != nil {
			d.Secrets.WebhookURL, c.URL = true, ""
		} else if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			u.User, u.RawQuery, u.Fragment, u.ForceQuery = nil, "", "", false
			d.Secrets.WebhookURL, c.URL = true, u.String()
		}
		d.Webhook = &c
	}
	if _, ok := kv[keySFTP]; ok {
		c, err := LoadSFTP(ctx, db)
		if err != nil {
			return Backup{}, d, err
		}
		d.SFTP = &c
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return Backup{}, d, err
	}
	sv, err := store.SchemaVersion(ctx, db)
	if err != nil {
		return Backup{}, d, err
	}
	sum := sha256.Sum256(raw)
	b := Backup{
		Format: FormatName, FormatVersion: FormatVersion, AppVersion: version.Version,
		SchemaVersion: sv, CreatedAt: now.UTC().Truncate(time.Second), Hostname: d.Settings.ServerName,
		SHA256: hex.EncodeToString(sum[:]), Data: raw,
	}
	return b, d, nil
}

// Encode пишет копию: gzip(JSON).
func Encode(b Backup) ([]byte, error) {
	j, err := json.MarshalIndent(b, "", " ")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Name = "gamayun-config.json"
	zw.ModTime = b.CreatedAt
	if _, err := zw.Write(j); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > MaxSize {
		return nil, fmt.Errorf("копия настроек больше %d КиБ", MaxSize>>10)
	}
	return buf.Bytes(), nil
}

// Decode читает и проверяет копию: признак, контрольную сумму,
// совместимость версий; старый формат приводит к текущему.
// schemaVersion — версия схемы базы, известная коду (store.LatestVersion).
func Decode(r io.Reader, schemaVersion int) (Backup, Data, error) {
	var b Backup
	var d Data
	lr := io.LimitReader(r, MaxSize+1)
	zr, err := gzip.NewReader(lr)
	if err != nil {
		return b, d, ErrFormat
	}
	j, err := io.ReadAll(io.LimitReader(zr, MaxUnpackedSize+1))
	if err != nil {
		return b, d, ErrFormat
	}
	if len(j) > MaxUnpackedSize {
		return b, d, fmt.Errorf("%w: слишком большой", ErrFormat)
	}
	if err := json.Unmarshal(j, &b); err != nil || b.Format != FormatName {
		return b, d, ErrFormat
	}
	if b.FormatVersion > FormatVersion {
		return b, d, fmt.Errorf("%w: формат копии %d новее поддерживаемого %d (копия сделана версией %s) — обновите систему до версии не ниже %s",
			ErrIncompatible, b.FormatVersion, FormatVersion, b.AppVersion, b.AppVersion)
	}
	if b.FormatVersion < MinFormatVersion {
		return b, d, fmt.Errorf("%w: формат копии %d слишком старый (поддерживается с %d)", ErrIncompatible, b.FormatVersion, MinFormatVersion)
	}
	if b.SchemaVersion > schemaVersion {
		return b, d, fmt.Errorf("%w: схема базы в копии %d новее известной этой версии %d (копия сделана версией %s) — обновите систему",
			ErrIncompatible, b.SchemaVersion, schemaVersion, b.AppVersion)
	}
	// Data хэшируется в компактном виде: отступы файла на сумму не влияют.
	var compact bytes.Buffer
	if err := json.Compact(&compact, b.Data); err != nil {
		return b, d, ErrFormat
	}
	sum := sha256.Sum256(compact.Bytes())
	if !strings.EqualFold(hex.EncodeToString(sum[:]), b.SHA256) {
		return b, d, fmt.Errorf("%w: контрольная сумма не совпадает", ErrFormat)
	}
	raw, err := upgrade(b.FormatVersion, b.Data)
	if err != nil {
		return b, d, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&d); err != nil {
		return b, d, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	if err := d.validate(); err != nil {
		return b, d, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	return b, d, nil
}

// upgrade приводит Data старого формата к текущему. Шаги добавляются при
// повышении FormatVersion: case 1: raw = from1to2(raw); fallthrough ...
func upgrade(from int, raw json.RawMessage) (json.RawMessage, error) {
	switch from {
	case FormatVersion:
		return raw, nil
	}
	return nil, fmt.Errorf("%w: нет перехода с формата %d", ErrIncompatible, from)
}

// validate — имена и ссылки в копии допустимы (до любых изменений).
func (d Data) validate() error {
	seen := map[string]bool{}
	for _, u := range d.Users {
		if !nas.ValidName(u.Name) {
			return fmt.Errorf("недопустимое имя пользователя %q", u.Name)
		}
		if seen[u.Name] {
			return fmt.Errorf("пользователь %s повторяется", u.Name)
		}
		seen[u.Name] = true
	}
	for _, g := range d.Groups {
		if !nas.ValidName(g.Name) {
			return fmt.Errorf("недопустимое имя группы %q", g.Name)
		}
		for _, m := range g.Members {
			if !seen[m] {
				return fmt.Errorf("группа %s: нет пользователя %s", g.Name, m)
			}
		}
	}
	for _, s := range d.Shares {
		if !nas.ValidShare(s.Name) {
			return fmt.Errorf("недопустимое имя папки %q", s.Name)
		}
	}
	return nil
}

func users(ctx context.Context, db *sql.DB) ([]User, error) {
	rows, err := db.QueryContext(ctx, `SELECT u.name, u.enabled, u.ssh,
		EXISTS (SELECT 1 FROM nas_group_members m WHERE m.user = u.name AND m.grp = ?)
		FROM nas_users u ORDER BY u.name`, nas.GroupAdmins)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.Name, &u.Enabled, &u.SSH, &u.Admin); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func groups(ctx context.Context, db *sql.DB) ([]Group, error) {
	rows, err := db.QueryContext(ctx, `SELECT g.name, m.user FROM nas_groups g
		LEFT JOIN nas_group_members m ON m.grp = g.name ORDER BY g.name, m.user`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		var name string
		var member sql.NullString
		if err := rows.Scan(&name, &member); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, Group{Name: name, Members: []string{}})
		}
		if member.Valid {
			out[len(out)-1].Members = append(out[len(out)-1].Members, member.String)
		}
	}
	return out, rows.Err()
}

func shares(ctx context.Context, db *sql.DB) ([]Share, error) {
	rows, err := db.QueryContext(ctx, `SELECT s.name, s.pool, s.comment, a.kind, a.subject, a.level
		FROM shares s LEFT JOIN share_access a ON a.share = s.name ORDER BY s.name, a.kind, a.subject`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Share{}
	for rows.Next() {
		var s Share
		var kind, subj, level sql.NullString
		if err := rows.Scan(&s.Name, &s.Pool, &s.Comment, &kind, &subj, &level); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Name != s.Name {
			s.Access = []nas.Access{}
			out = append(out, s)
		}
		if kind.Valid {
			last := &out[len(out)-1]
			last.Access = append(last.Access, nas.Access{Kind: kind.String, Subject: subj.String, Level: level.String})
		}
	}
	return out, rows.Err()
}

func pools(ctx context.Context, db *sql.DB) ([]Pool, error) {
	rows, err := db.QueryContext(ctx, "SELECT name, type, integrity FROM pools ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Pool{}
	for rows.Next() {
		var p Pool
		if err := rows.Scan(&p.Name, &p.Type, &p.Integrity); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func settingsKV(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT key, value FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kv := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		kv[k] = v
	}
	return kv, rows.Err()
}
