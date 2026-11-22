package proxy

// Публикация адресов через D-Bus avahi-daemon (org.freedesktop.Avahi).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/godbus/dbus/v5"
)

const (
	avahiBus    = "org.freedesktop.Avahi"
	avahiServer = "org.freedesktop.Avahi.Server"
	avahiGroup  = "org.freedesktop.Avahi.EntryGroup"
	// AvahiProtocol: INET=0, INET6=1; AvahiPublishFlags: NO_REVERSE=16.
	avahiInet, avahiInet6 = int32(0), int32(1)
	avahiNoReverse        = uint32(16)
	// AvahiEntryGroupState: 2 — established, 3 — collision, 4 — failure.
	groupEstablished, groupCollision, groupFailure = int32(2), int32(3), int32(4)
)

type avahiDBus struct {
	conn  *dbus.Conn
	group dbus.ObjectPath
}

func (a *avahiDBus) connect() error {
	if a.conn != nil && a.conn.Connected() {
		return nil
	}
	a.Close()
	c, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("D-Bus: %w", err)
	}
	a.conn = c
	return nil
}

func (a *avahiDBus) Publish(ctx context.Context, es []entry) error {
	if err := a.connect(); err != nil {
		return err
	}
	if a.group != "" {
		// Reset снимает прежние записи; группы нет (avahi перезапущен) — новая.
		if err := a.conn.Object(avahiBus, a.group).CallWithContext(ctx, avahiGroup+".Reset", 0).Err; err != nil {
			a.group = ""
		}
	}
	if len(es) == 0 {
		return nil
	}
	if a.group == "" {
		if err := a.conn.Object(avahiBus, "/").CallWithContext(ctx, avahiServer+".EntryGroupNew", 0).Store(&a.group); err != nil {
			return fmt.Errorf("avahi: %w", err)
		}
	}
	g := a.conn.Object(avahiBus, a.group)
	for _, e := range es {
		proto := avahiInet
		if strings.Contains(e.Addr, ":") {
			proto = avahiInet6
		}
		if err := g.CallWithContext(ctx, avahiGroup+".AddAddress", 0, int32(e.Iface), proto, avahiNoReverse, e.Host, e.Addr).Err; err != nil {
			g.CallWithContext(ctx, avahiGroup+".Reset", 0)
			return fmt.Errorf("avahi: %s %s: %w", e.Host, e.Addr, err)
		}
	}
	if err := g.CallWithContext(ctx, avahiGroup+".Commit", 0).Err; err != nil {
		return fmt.Errorf("avahi: commit: %w", err)
	}
	return nil
}

func (a *avahiDBus) Healthy() error {
	if a.group == "" {
		return nil // публиковать нечего
	}
	if a.conn == nil || !a.conn.Connected() {
		return errors.New("соединение D-Bus потеряно")
	}
	var st int32
	if err := a.conn.Object(avahiBus, a.group).Call(avahiGroup+".GetState", 0).Store(&st); err != nil {
		return fmt.Errorf("avahi: %w", err)
	}
	switch st {
	case groupCollision:
		return errors.New("конфликт имени mDNS (имя занято другим устройством)")
	case groupFailure:
		return errors.New("avahi: сбой публикации")
	}
	return nil
}

func (a *avahiDBus) Close() {
	if a.conn != nil {
		a.conn.Close()
	}
	a.conn, a.group = nil, ""
}
