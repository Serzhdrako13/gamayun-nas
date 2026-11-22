package proxy

// mDNS-имена приложений: <app>-<хост>.local —
// одна метка, как у самого NAS. nss-mdns (mdns4_minimal) многоуровневые имена
// <app>.<хост>.local не разрешает.
//
// Публикация — через D-Bus avahi-daemon (EntryGroup.AddAddress) с индексом интерфейса:
// на каждом интерфейсе объявляется только его адрес, как avahi делает для имени хоста.
// avahi-publish -a так не умеет (объявляет адрес на всех интерфейсах: на клиент
// получал адрес соседней сети). Группа записей одна на все имена; при изменении набора
// (имена, адреса) — Reset и публикация заново; конфликт имени, перезапуск avahi — на
// следующей сверке. Группа живёт, пока открыто соединение D-Bus gamayund.

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
)

// entry — имя на адресе конкретного интерфейса.
type entry struct {
	Iface int
	Host  string
	Addr  string
}

// publisher — публикация набора записей целиком (avahi D-Bus; тесты — подделка).
type publisher interface {
	// Publish заменяет опубликованный набор; пустой — снять всё.
	Publish(ctx context.Context, es []entry) error
	// Healthy — опубликованное ещё действует (нет конфликта, avahi не перезапускался).
	Healthy() error
	Close()
}

type ifAddr struct {
	Iface int
	Addr  string
}

type mdns struct {
	log  *slog.Logger
	pub  publisher
	addr func() []ifAddr // адреса LAN; nil — lanAddrs
	mu   sync.Mutex
	last string // ключ опубликованного набора
	warn string
}

func newMDNS(log *slog.Logger, pub publisher) *mdns {
	if pub == nil {
		pub = &avahiDBus{}
	}
	return &mdns{log: log, pub: pub}
}

// skipIface — интерфейсы, на которых имена приложений не объявляются.
func skipIface(name string) bool {
	for _, p := range []string{"lo", "docker", "veth", "br-", "virbr", "tun", "tap", "wg"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// lanAddrs — IPv4 и глобальные IPv6 работающих LAN-интерфейсов.
func lanAddrs() []ifAddr {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []ifAddr
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 || skipIface(i.Name) {
			continue
		}
		as, _ := i.Addrs()
		for _, a := range as {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.IsGlobalUnicast() { // без loopback, link-local, multicast
				out = append(out, ifAddr{i.Index, ipn.IP.String()})
			}
		}
	}
	return out
}

// sync приводит публикацию к hosts × адреса LAN. Возвращает ошибку (общую для всех имён) или "".
func (m *mdns) sync(ctx context.Context, hosts []string) string {
	addrs := m.addr
	if addrs == nil {
		addrs = lanAddrs
	}
	var es []entry
	if len(hosts) > 0 {
		as := addrs()
		for _, h := range hosts {
			for _, a := range as {
				es = append(es, entry{a.Iface, h, a.Addr})
			}
		}
	}
	sort.Slice(es, func(i, j int) bool { return fmt.Sprint(es[i]) < fmt.Sprint(es[j]) })
	key := fmt.Sprint(es)
	m.mu.Lock()
	defer m.mu.Unlock()
	if key == m.last {
		err := m.pub.Healthy()
		if err == nil {
			return ""
		}
		m.log.Warn("прокси: публикация mDNS утрачена, повтор", "err", err)
	}
	m.last = ""
	if err := m.pub.Publish(ctx, es); err != nil {
		msg := err.Error()
		if m.warn != msg {
			m.warn = msg
			m.log.Warn("прокси: имена mDNS не опубликованы", "err", msg)
		}
		return msg
	}
	m.last, m.warn = key, ""
	return ""
}

func (m *mdns) stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pub.Close()
	m.last = ""
}
