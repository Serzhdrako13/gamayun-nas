package apps

// Реестр портов — порты NAS, приложений и занятые на хосте.

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gamayun-nas/internal/apps/compose"
)

// Владельцы портов.
const (
	OwnerNAS    = "nas"    // службы NAS (зарезервировано)
	OwnerApp    = "app"    // приложение
	OwnerSystem = "system" // занят процессом на NAS (не приложением)
)

// ReservedPorts — порты служб NAS: не выдаются приложениям никогда.
var ReservedPorts = map[string]string{
	"22/tcp": "SSH", "53/tcp": "DNS", "53/udp": "DNS", "80/tcp": "веб-интерфейс (HTTP)", "443/tcp": "веб-интерфейс (HTTPS)",
	"111/tcp": "rpcbind", "111/udp": "rpcbind", "137/udp": "SMB (NetBIOS)", "138/udp": "SMB (NetBIOS)", "139/tcp": "SMB",
	"445/tcp": "SMB", "2049/tcp": "NFS", "5353/udp": "mDNS", "3702/udp": "WS-Discovery", "5357/tcp": "WS-Discovery",
	"8780/tcp": "веб-интерфейс GamayunNas",
}

// PortEntry — строка реестра.
type PortEntry struct {
	Port    int    `json:"port"`
	Proto   string `json:"proto"`
	Owner   string `json:"owner"`
	Title   string `json:"title,omitempty"` // служба NAS
	App     string `json:"app,omitempty"`
	Service string `json:"service,omitempty"`
	// Network — доступен в сети (0.0.0.0); иначе только на NAS (127.0.0.1).
	Network bool `json:"network"`
}

// Listening — занятые порты хоста из /proc/net (без root): "8080/tcp" → true.
// TCP — в состоянии LISTEN, UDP — привязанные.
func Listening(procNet string) map[string]bool {
	out := map[string]bool{}
	for _, f := range []struct{ file, proto, state string }{
		{"tcp", "tcp", "0A"}, {"tcp6", "tcp", "0A"}, {"udp", "udp", "07"}, {"udp6", "udp", "07"},
	} {
		fh, err := os.Open(filepath.Join(procNet, f.file))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Scan() // заголовок
		for sc.Scan() {
			fs := strings.Fields(sc.Text())
			if len(fs) < 4 || fs[3] != f.state {
				continue
			}
			i := strings.LastIndexByte(fs[1], ':')
			if i < 0 {
				continue
			}
			p, err := strconv.ParseUint(fs[1][i+1:], 16, 16)
			if err == nil && p > 0 {
				out[compose.PortKey(int(p), f.proto)] = true
			}
		}
		fh.Close()
	}
	return out
}

// registry — реестр по записям приложений и занятым портам хоста.
func registry(projects map[string]Project, listening map[string]bool) []PortEntry {
	var out []PortEntry
	seen := map[string]bool{}
	for k, title := range ReservedPorts {
		p, proto := splitKey(k)
		out = append(out, PortEntry{Port: p, Proto: proto, Owner: OwnerNAS, Title: title, Network: true})
		seen[k] = true
	}
	for _, pr := range projects {
		if pr.Removed {
			continue
		}
		for _, p := range pr.Ports {
			k := compose.PortKey(p.Host, p.Proto)
			seen[k] = true
			out = append(out, PortEntry{Port: p.Host, Proto: p.Proto, Owner: OwnerApp, App: pr.ID, Service: p.Service, Network: p.Network})
		}
	}
	for k := range listening {
		if seen[k] {
			continue
		}
		p, proto := splitKey(k)
		out = append(out, PortEntry{Port: p, Proto: proto, Owner: OwnerSystem})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Proto < out[j].Proto
	})
	return out
}

func splitKey(k string) (int, string) {
	ps, proto, _ := strings.Cut(k, "/")
	p, _ := strconv.Atoi(ps)
	return p, proto
}

// Коды конфликтов портов.
const (
	PortReserved = "port_reserved"
	PortApp      = "port_app"
	PortBusy     = "port_busy"
)

// portConflicts — порты приложения id против реестра. Свои текущие порты
// приложения (изменение) — не конфликт, в т. ч. занятые его docker-proxy.
func portConflicts(id string, ports []compose.Port, projects map[string]Project, listening map[string]bool) []compose.Issue {
	own := map[string]bool{}
	if pr, ok := projects[id]; ok {
		for _, p := range pr.Ports {
			own[compose.PortKey(p.Host, p.Proto)] = true
		}
	}
	others := map[string]string{}
	for _, pr := range projects {
		if pr.ID == id || pr.Removed {
			continue
		}
		for _, p := range pr.Ports {
			others[compose.PortKey(p.Host, p.Proto)] = pr.ID
		}
	}
	var iss []compose.Issue
	for _, p := range ports {
		k := compose.PortKey(p.Host, p.Proto)
		switch {
		case ReservedPorts[k] != "":
			iss = append(iss, compose.Issue{Code: PortReserved, Service: p.Service, Detail: "порт " + k + " занят службой NAS: " + ReservedPorts[k]})
		case others[k] != "":
			iss = append(iss, compose.Issue{Code: PortApp, Service: p.Service, Detail: "порт " + k + " занят приложением " + others[k]})
		case listening[k] && !own[k]:
			iss = append(iss, compose.Issue{Code: PortBusy, Service: p.Service, Detail: "порт " + k + " уже занят на NAS"})
		}
	}
	return iss
}
