package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Ключи настроек каналов в таблице settings (JSON).
const (
	smtpKey    = "notify.smtp"
	webhookKey = "notify.webhook"
)

// LoadSMTP — настройки SMTP (нет записи — пустые, канал выключен).
func LoadSMTP(ctx context.Context, db *sql.DB) (SMTPConfig, error) {
	var c SMTPConfig
	var v string
	err := db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", smtpKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal([]byte(v), &c)
}

// SaveSMTP проверяет и сохраняет настройки SMTP.
func SaveSMTP(ctx context.Context, db *sql.DB, c SMTPConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	v, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		"INSERT INTO settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", smtpKey, string(v))
	return err
}

// ApplySMTP включает или отключает канал по настройкам.
func (d *Deliverer) ApplySMTP(c SMTPConfig) {
	if !c.Enabled {
		d.SetSender(ChannelSMTP, nil)
		return
	}
	d.SetSender(ChannelSMTP, &SMTPSender{Cfg: c})
}

// LoadWebhook — настройки webhook (нет записи — канал выключен).
func LoadWebhook(ctx context.Context, db *sql.DB) (WebhookConfig, error) {
	var c WebhookConfig
	var v string
	err := db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", webhookKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal([]byte(v), &c)
}

// SaveWebhook проверяет и сохраняет настройки webhook.
func SaveWebhook(ctx context.Context, db *sql.DB, c WebhookConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	v, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		"INSERT INTO settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", webhookKey, string(v))
	return err
}

// ApplyWebhook включает или отключает канал по настройкам.
func (d *Deliverer) ApplyWebhook(c WebhookConfig) {
	if !c.Enabled {
		d.SetSender(ChannelWebhook, nil)
		return
	}
	d.SetSender(ChannelWebhook, &WebhookSender{Cfg: c})
}
