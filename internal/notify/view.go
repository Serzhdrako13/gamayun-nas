package notify

// Представление настроек для API: секреты (пароль SMTP, ключ webhook)
// никогда не отдаются, только признак «задан».

// SMTPView — настройки SMTP для API.
type SMTPView struct {
	SMTPConfig
	Password    string `json:"password,omitempty"` // всегда пусто в ответе
	PasswordSet bool   `json:"password_set"`
}

// WebhookView — настройки webhook для API.
type WebhookView struct {
	WebhookConfig
	Secret    string `json:"secret,omitempty"` // всегда пусто в ответе
	SecretSet bool   `json:"secret_set"`
}

// View — настройки SMTP без пароля.
func (c SMTPConfig) View() SMTPView {
	return SMTPView{SMTPConfig: c, PasswordSet: c.Password != ""}
}

// View — настройки webhook без ключа.
func (c WebhookConfig) View() WebhookView {
	return WebhookView{WebhookConfig: c, SecretSet: c.Secret != ""}
}

// SecretUpdate — изменение секрета в запросе: nil — оставить прежний,
// "" — удалить, иначе — новое значение.
type SecretUpdate = *string

// MergeSMTP — новые настройки с секретом из старых, если он не передан.
func MergeSMTP(old, upd SMTPConfig, pw SecretUpdate) SMTPConfig {
	upd.Password = old.Password
	if pw != nil {
		upd.Password = *pw
	}
	return upd
}

// MergeWebhook — то же для webhook.
func MergeWebhook(old, upd WebhookConfig, secret SecretUpdate) WebhookConfig {
	upd.Secret = old.Secret
	if secret != nil {
		upd.Secret = *secret
	}
	return upd
}
