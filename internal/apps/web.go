package apps

// Публикация веб-приложения через прокси (Caddy, поддомен).

import (
	"context"
	"sort"

	"gamayun-nas/internal/apps/compose"
	"gamayun-nas/internal/events"
	"gamayun-nas/internal/proxy"
)

// WebRoute — публикация приложения по имени <Name>-<хост>.local
// с проксированием на 127.0.0.1:<Port> (порт приложения из compose).
type WebRoute struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// loopbackPort — порт приложения TCP, опубликованный на 127.0.0.1.
func loopbackPort(ports []compose.Port, port int) bool {
	for _, p := range ports {
		if p.Host == port && p.Proto == "tcp" && !p.Network {
			return true
		}
	}
	return false
}

// keepWeb — сохранить публикацию после обновления compose, если порт остался.
func keepWeb(w *WebRoute, ports []compose.Port) *WebRoute {
	if w == nil || !loopbackPort(ports, w.Port) {
		return nil
	}
	c := *w
	return &c
}

func (s *Service) routesChanged() {
	if s.OnRoutes != nil {
		s.OnRoutes()
	}
}

// SetWeb публикует приложение через прокси (w != nil) или снимает публикацию.
// Имя поддомена — уникально среди приложений; порт — TCP приложения на 127.0.0.1
// (порт «доступно в сети» прокси не нужен).
func (s *Service) SetWeb(ctx context.Context, id string, w *WebRoute, actor string) (Project, error) {
	if err := s.lockApp(id); err != nil {
		return Project{}, err
	}
	defer s.unlockApp(id)
	m, err := s.loadProjects(ctx)
	if err != nil {
		return Project{}, err
	}
	p, ok := m[id]
	if !ok || p.Removed {
		return Project{}, wrapf(ErrNotFound, "приложение %s не найдено", id)
	}
	if w != nil {
		if !proxy.ValidName(w.Name) {
			return Project{}, wrapf(ErrInvalidSvc, "имя поддомена %q: латиница в нижнем регистре, цифры и дефис, до 32 символов", w.Name)
		}
		if s.WebName != nil {
			if err := s.WebName(w.Name); err != nil {
				return Project{}, wrapf(ErrInvalidSvc, "%v", err)
			}
		}
		if !loopbackPort(p.Ports, w.Port) {
			return Project{}, wrapf(ErrInvalidSvc, "порт %d не является TCP-портом приложения на 127.0.0.1", w.Port)
		}
		for oid, o := range m {
			if oid != id && o.Web != nil && o.Web.Name == w.Name {
				return Project{}, wrapf(ErrConflictSvc, "поддомен %q уже занят приложением %s", w.Name, oid)
			}
		}
		c := *w
		w = &c
	}
	prev := p.Web
	p.Web = w
	p.Updated = s.now().UTC()
	m[id] = p
	if err := s.saveProjects(ctx, m); err != nil {
		return Project{}, err
	}
	data := map[string]any{"app": id, "actor": actor}
	if w != nil {
		data["name"], data["port"] = w.Name, w.Port
		s.emit(ctx, EvAppWeb, events.SevInfo, "app:"+id, data)
	} else if prev != nil {
		data["name"] = prev.Name
		s.emit(ctx, EvAppWeb, events.SevInfo, "app:"+id, data)
	}
	s.routesChanged()
	return p, nil
}

// Routes — опубликованные приложения для прокси.
func (s *Service) Routes(ctx context.Context) []proxy.Route {
	m, err := s.loadProjects(ctx)
	if err != nil {
		s.Logger.Warn("прокси: приложения не прочитаны", "err", err)
		return nil
	}
	var r []proxy.Route
	for id, p := range m {
		if p.Web != nil && !p.Removed && loopbackPort(p.Ports, p.Web.Port) {
			r = append(r, proxy.Route{App: id, Name: p.Web.Name, Port: p.Web.Port})
		}
	}
	sort.Slice(r, func(i, j int) bool { return r[i].Name < r[j].Name })
	return r
}
