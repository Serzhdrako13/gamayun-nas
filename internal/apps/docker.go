package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrDockerDown — сокет Docker недоступен (служба остановлена).
var ErrDockerDown = errors.New("Docker не запущен")

// DockerClient — минимальный клиент Engine API через unix-сокет (helper,
// root). Без Go SDK: нужны только /info и /events.
type DockerClient struct {
	http *http.Client
}

// NewDockerClient — клиент к сокету sock.
func NewDockerClient(sock string) *DockerClient {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}
	return &DockerClient{http: &http.Client{Transport: tr}}
}

func (c *DockerClient) get(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	u := "http://docker" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%w: %v", ErrDockerDown, err)
		}
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("Docker %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

// Info — нужные поля GET /info.
type Info struct {
	ServerVersion     string
	DockerRootDir     string
	Containers        int
	ContainersRunning int
}

// Info — GET /info.
func (c *DockerClient) Info(ctx context.Context) (Info, error) {
	var in Info
	resp, err := c.get(ctx, "/info", nil)
	if err != nil {
		return in, err
	}
	defer resp.Body.Close()
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&in)
	return in, err
}

// DockerEvent — событие контейнера для журнала gamayund. Только безопасные
// поля: метки и переменные окружения контейнера (могут содержать секреты)
// не передаются.
type DockerEvent struct {
	TimeNano  int64  `json:"time_nano"`
	Action    string `json:"action"` // die, oom, restart, health_status
	Health    string `json:"health,omitempty"`
	Container string `json:"container"` // имя
	ID        string `json:"id"`        // 12 символов
	Image     string `json:"image,omitempty"`
	ExitCode  string `json:"exit_code,omitempty"`
	// OOM — die по нехватке памяти (State.OOMKilled): Docker 29 на cgroup v2
	// события oom не присылает, 2026-11-20).
	OOM     bool   `json:"oom,omitempty"`
	Project string `json:"project,omitempty"` // com.docker.compose.project
}

type rawEvent struct {
	Type     string
	Action   string
	TimeNano int64 `json:"timeNano"`
	Actor    struct {
		ID         string
		Attributes map[string]string
	}
}

// Значимые действия.
func wanted(action string) (string, string, bool) {
	switch {
	case action == "die", action == "oom", action == "restart":
		return action, "", true
	case strings.HasPrefix(action, "health_status:"):
		return "health_status", strings.TrimSpace(strings.TrimPrefix(action, "health_status:")), true
	}
	return "", "", false
}

func nanoArg(n int64) string { return fmt.Sprintf("%d.%09d", n/1e9, n%1e9) }

// Events — GET /events с since/until (поток закрывается Docker в until);
// не больше max событий. end — до какого момента события прочитаны: until,
// момент обрыва потока (Docker остановлен) или время последнего события при
// достижении max.
func (c *DockerClient) Events(ctx context.Context, since, until int64, max int) (out []DockerEvent, end int64, err error) {
	q := url.Values{}
	q.Set("since", nanoArg(since))
	q.Set("until", nanoArg(until))
	q.Set("filters", `{"type":["container"]}`)
	wait := time.Until(time.Unix(0, until)) + 15*time.Second
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	resp, err := c.get(ctx, "/events", q)
	if err != nil {
		return nil, since, err
	}
	defer resp.Body.Close()
	out = []DockerEvent{}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<20))
	for {
		if len(out) >= max {
			return out, out[len(out)-1].TimeNano, nil
		}
		var r rawEvent
		if err := dec.Decode(&r); err != nil {
			// Конец потока в until или обрыв (остановка Docker): прочитано до «сейчас».
			end = min(until, time.Now().UnixNano())
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || len(out) > 0 {
				return out, end, nil
			}
			return out, since, err
		}
		if r.TimeNano <= since {
			continue
		}
		act, health, ok := wanted(r.Action)
		if r.Type != "container" || !ok {
			continue
		}
		id := r.Actor.ID
		if len(id) > 12 {
			id = id[:12]
		}
		a := r.Actor.Attributes
		ev := DockerEvent{TimeNano: r.TimeNano, Action: act, Health: health, Container: a["name"], ID: id,
			Image: a["image"], Project: a["com.docker.compose.project"]}
		if act == "die" {
			ev.ExitCode = a["exitCode"]
			if ev.ExitCode == "137" {
				ev.OOM = c.oomKilled(ctx, r.Actor.ID)
			}
		}
		out = append(out, ev)
	}
}

// oomKilled — State.OOMKilled контейнера (удалённый — false).
func (c *DockerClient) oomKilled(ctx context.Context, id string) bool {
	if id == "" || strings.ContainsAny(id, "/?#") {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := c.get(ctx, "/containers/"+id+"/json", nil)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var v struct{ State struct{ OOMKilled bool } }
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&v) != nil {
		return false
	}
	return v.State.OOMKilled
}
