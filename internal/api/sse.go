package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gamayun-nas/internal/events"
)

const replayPage = 500

// events — поток Server-Sent Events.
//
// Без Last-Event-ID передаются только новые события: клиент сначала
// подписывается, затем читает /api/v1/tasks. С Last-Event-ID (заголовок
// или ?last_event_id=) сначала досылаются журнальные события после него.
// Эфемерные события (прогресс) идут без id и не досылаются.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	var last int64
	lastStr := r.Header.Get("Last-Event-ID")
	if lastStr == "" {
		lastStr = r.URL.Query().Get("last_event_id")
	}
	if lastStr != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(lastStr), 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "Last-Event-ID должен быть неотрицательным целым")
			return
		}
		last = n
	}

	rc := http.NewResponseController(w)
	// Поток длинный: снимаем общий WriteTimeout сервера (ошибку
	// «не поддерживается» в тестовых обёртках игнорируем).
	_ = rc.SetWriteDeadline(time.Time{})

	// Подписка до чтения журнала, чтобы не потерять события между ними.
	sub := s.Events.Subscribe(256)
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	if rc.Flush() != nil {
		return
	}

	sent := last
	if last > 0 {
		for {
			evs, err := s.Events.Since(r.Context(), sent, replayPage)
			if err != nil {
				s.Logger.Error("SSE: чтение журнала", "err", err)
				return
			}
			for _, ev := range evs {
				if writeEvent(w, ev) != nil {
					return
				}
				sent = ev.ID
			}
			if rc.Flush() != nil {
				return
			}
			if len(evs) < replayPage {
				break
			}
		}
	}

	ping := time.NewTicker(s.Heartbeat)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if s.SessionCheck != nil && !s.SessionCheck(r) {
				return // сессия завершена или истекла
			}
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case ev, ok := <-sub.C:
			if !ok {
				return // отстали: клиент переподключится с Last-Event-ID
			}
			if ev.ID != 0 && ev.ID <= sent {
				continue // уже отправлено при досылке
			}
			if writeEvent(w, ev) != nil || rc.Flush() != nil {
				return
			}
			if ev.ID != 0 {
				sent = ev.ID
			}
		}
	}
}

func writeEvent(w http.ResponseWriter, ev events.Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	var b strings.Builder
	if ev.ID != 0 {
		fmt.Fprintf(&b, "id: %d\n", ev.ID)
	}
	fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", ev.Type, data)
	_, err = fmt.Fprint(w, b.String())
	return err
}
