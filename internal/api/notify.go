package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"gamayun-nas/internal/notify"
)

// Центр уведомлений и настройки каналов SMTP/webhook.
func (s *server) notifyRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/notifications", s.notifyH(s.listNotifications))
	m.HandleFunc("POST /api/v1/notifications/read", s.notifyH(s.readNotifications))
	m.HandleFunc("GET /api/v1/notifications/channels", s.notifyH(s.getChannels))
	m.HandleFunc("PUT /api/v1/notifications/channels/smtp", s.notifyH(s.putSMTP))
	m.HandleFunc("PUT /api/v1/notifications/channels/webhook", s.notifyH(s.putWebhook))
	m.HandleFunc("POST /api/v1/notifications/channels/smtp/test", s.notifyH(s.testSMTP))
	m.HandleFunc("POST /api/v1/notifications/channels/webhook/test", s.notifyH(s.testWebhook))
	// Тишина на время работ.
	m.HandleFunc("GET /api/v1/notifications/quiet", s.notifyH(s.getQuiet))
	m.HandleFunc("PUT /api/v1/notifications/quiet", s.notifyH(s.putQuiet))
	m.HandleFunc("DELETE /api/v1/notifications/quiet", s.notifyH(s.deleteQuiet))
}

func (s *server) getQuiet(w http.ResponseWriter, r *http.Request) {
	q, err := s.Notify.Quiet(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"quiet": q})
}

// putQuiet включает или меняет тишину: срок — until (RFC 3339) или
// minutes от текущего момента; причина обязательна; mute_critical —
// глушить и critical (явный выбор оператора).
func (s *server) putQuiet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Until        *time.Time `json:"until"`
		Minutes      int        `json:"minutes"`
		Reason       string     `json:"reason"`
		MuteCritical bool       `json:"mute_critical"`
	}
	if !s.body(w, r, &req) {
		return
	}
	var until time.Time
	switch {
	case req.Until != nil && req.Minutes == 0:
		until = *req.Until
	case req.Until == nil && req.Minutes > 0:
		until = time.Now().Add(time.Duration(req.Minutes) * time.Minute)
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "укажите срок: until или minutes")
		return
	}
	q, err := s.Notify.SetQuiet(r.Context(), until, req.Reason, req.MuteCritical, actor(r))
	switch {
	case errors.Is(err, notify.ErrQuietInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	case errors.Is(err, notify.ErrQuietEnding):
		writeError(w, http.StatusConflict, "quiet_ending", err.Error())
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.Logger.Warn("уведомления: тишина включена", "until", q.Until, "mute_critical", q.MuteCritical,
		"reason", q.Reason, "actor", actor(r))
	writeJSON(w, http.StatusOK, map[string]any{"quiet": q})
}

func (s *server) deleteQuiet(w http.ResponseWriter, r *http.Request) {
	ok, err := s.Notify.EndQuiet(r.Context(), "отменена", actor(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if ok {
		s.Logger.Info("уведомления: тишина отменена", "actor", actor(r))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ended": ok})
}

func (s *server) notifyH(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Notify == nil || s.Deliver == nil || s.DB == nil {
			writeError(w, http.StatusServiceUnavailable, "notify_unavailable", "уведомления не настроены")
			return
		}
		h(w, r)
	}
}

func (s *server) listNotifications(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	list, err := s.Notify.List(r.Context(), limit, before, q.Get("unread") == "1" || q.Get("unread") == "true")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	n, err := s.Notify.Unread(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "unread": n})
}

func (s *server) readNotifications(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UpTo int64 `json:"up_to"` // 0 — все
	}
	if !s.body(w, r, &req) {
		return
	}
	if _, err := s.Notify.MarkRead(r.Context(), req.UpTo); err != nil {
		s.internalError(w, r, err)
		return
	}
	n, err := s.Notify.Unread(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unread": n})
}

func (s *server) getChannels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sm, err := notify.LoadSMTP(ctx, s.DB)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	wh, err := notify.LoadWebhook(ctx, s.DB)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	st, err := s.Deliver.Status(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"smtp": sm.View(), "webhook": wh.View(), "status": st})
}

// smtpReq — настройки SMTP в запросе; password: нет поля — прежний, "" — удалить.
type smtpReq struct {
	notify.SMTPConfig
	Password *string `json:"password"`
}

type webhookReq struct {
	notify.WebhookConfig
	Secret *string `json:"secret"`
}

func (s *server) smtpFrom(w http.ResponseWriter, r *http.Request) (notify.SMTPConfig, bool) {
	var req smtpReq
	if !s.body(w, r, &req) {
		return notify.SMTPConfig{}, false
	}
	old, err := notify.LoadSMTP(r.Context(), s.DB)
	if err != nil {
		s.internalError(w, r, err)
		return old, false
	}
	c := notify.MergeSMTP(old, req.SMTPConfig, req.Password)
	if err := c.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return c, false
	}
	return c, true
}

func (s *server) webhookFrom(w http.ResponseWriter, r *http.Request) (notify.WebhookConfig, bool) {
	var req webhookReq
	if !s.body(w, r, &req) {
		return notify.WebhookConfig{}, false
	}
	old, err := notify.LoadWebhook(r.Context(), s.DB)
	if err != nil {
		s.internalError(w, r, err)
		return old, false
	}
	c := notify.MergeWebhook(old, req.WebhookConfig, req.Secret)
	if err := c.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return c, false
	}
	return c, true
}

func (s *server) putSMTP(w http.ResponseWriter, r *http.Request) {
	c, ok := s.smtpFrom(w, r)
	if !ok {
		return
	}
	if err := notify.SaveSMTP(r.Context(), s.DB, c); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Deliver.ApplySMTP(c)
	// Секреты в журнал не пишутся.
	s.Logger.Info("уведомления: настройки SMTP изменены", "enabled", c.Enabled, "host", c.Host, "port", c.Port,
		"security", c.Security, "recipients", len(c.To), "insecure_skip_verify", c.InsecureSkipVerify, "actor", actor(r))
	writeJSON(w, http.StatusOK, c.View())
}

func (s *server) putWebhook(w http.ResponseWriter, r *http.Request) {
	c, ok := s.webhookFrom(w, r)
	if !ok {
		return
	}
	if err := notify.SaveWebhook(r.Context(), s.DB, c); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Deliver.ApplyWebhook(c)
	s.Logger.Info("уведомления: настройки webhook изменены", "enabled", c.Enabled, "url", redactURL(c.URL),
		"signed", c.Secret != "", "insecure_skip_verify", c.InsecureSkipVerify, "actor", actor(r))
	writeJSON(w, http.StatusOK, c.View())
}

// testSMTP — тестовое письмо по присланным (ещё не сохранённым) настройкам;
// пустой пароль в запросе — сохранённый.
func (s *server) testSMTP(w http.ResponseWriter, r *http.Request) {
	c, ok := s.smtpFrom(w, r)
	if !ok {
		return
	}
	c.Enabled = true
	if err := c.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	s.channelTest(w, r, "smtp", func(ctx context.Context) error { return (&notify.SMTPSender{Cfg: c}).Test(ctx) })
}

func (s *server) testWebhook(w http.ResponseWriter, r *http.Request) {
	c, ok := s.webhookFrom(w, r)
	if !ok {
		return
	}
	c.Enabled = true
	if err := c.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	s.channelTest(w, r, "webhook", func(ctx context.Context) error { return (&notify.WebhookSender{Cfg: c}).Test(ctx) })
}

// channelTest — результат проверки канала: 200 {ok, error}. Ошибка канала —
// не ошибка запроса (оператору нужен текст причины).
func (s *server) channelTest(w http.ResponseWriter, r *http.Request, ch string, f func(context.Context) error) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	err := f(ctx)
	if err != nil {
		s.Logger.Warn("уведомления: проверка канала не прошла", "channel", ch, "err", err, "actor", actor(r))
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.Logger.Info("уведомления: проверка канала прошла", "channel", ch, "actor", actor(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// redactURL — только схема и узел: путь и параметры адреса webhook часто
// содержат токен получателя.
func redactURL(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "?"
	}
	return p.Scheme + "://" + p.Host
}
