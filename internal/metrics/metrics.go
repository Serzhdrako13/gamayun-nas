// Package metrics — текущая нагрузка для виджетов интерфейса:
// ЦП по разнице двух замеров /proc/stat, память из /proc/meminfo, скорость
// сети по /proc/net/dev (без lo и виртуальных интерфейсов). Читает gamayund
// без root; история и графики — коллектор.
package metrics

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Snapshot — ответ GET /api/v1/system/metrics.
type Snapshot struct {
	CPUPercent  *float64 `json:"cpu_percent"` // nil — первый замер, разницы ещё нет
	CPUCount    int      `json:"cpu_count"`
	MemTotal    int64    `json:"mem_total_bytes"`
	MemUsed     int64    `json:"mem_used_bytes"` // total − available
	NetRxPerSec *float64 `json:"net_rx_bytes_per_sec"`
	NetTxPerSec *float64 `json:"net_tx_bytes_per_sec"`
	NetIfaces   []string `json:"net_ifaces"`
	// NetByIface — скорость по каждому адаптеру, порядок как
	// в NetIfaces; скорость nil — первый замер адаптера или сброс счётчика.
	NetByIface []IfaceRate `json:"net_by_iface"`
	Load1      float64     `json:"load1"`
	UptimeSec  float64     `json:"uptime_sec"`
}

// IfaceRate — скорость одного сетевого адаптера, байт/с.
type IfaceRate struct {
	Name     string   `json:"name"`
	RxPerSec *float64 `json:"rx_bytes_per_sec"`
	TxPerSec *float64 `json:"tx_bytes_per_sec"`
}

type counters struct{ rx, tx uint64 }

// Sampler хранит предыдущий замер; безопасен для параллельных запросов.
type Sampler struct {
	Proc string // корень procfs, по умолчанию /proc
	Now  func() time.Time

	mu                  sync.Mutex
	at                  time.Time
	busy, total, rx, tx uint64
	have                bool
	ifs                 map[string]counters
}

func (s *Sampler) path(p string) string {
	root := s.Proc
	if root == "" {
		root = "/proc"
	}
	return root + "/" + p
}

// Sample снимает показатели; ЦП и сеть — средние с прошлого вызова.
func (s *Sampler) Sample() Snapshot {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	var out Snapshot
	busy, total, ncpu := readCPU(s.path("stat"))
	out.CPUCount = ncpu
	out.MemTotal, out.MemUsed = readMem(s.path("meminfo"))
	rx, tx, ifs, per := readNet(s.path("net/dev"))
	out.NetIfaces = ifs
	out.NetByIface = make([]IfaceRate, 0, len(ifs))
	out.Load1, out.UptimeSec = readFloat1(s.path("loadavg")), readFloat1(s.path("uptime"))

	s.mu.Lock()
	defer s.mu.Unlock()
	t := now()
	if s.have {
		if dt := total - s.total; total > s.total && dt > 0 {
			v := float64(busy-s.busy) / float64(dt) * 100
			out.CPUPercent = &v
		}
		if sec := t.Sub(s.at).Seconds(); sec > 0 && rx >= s.rx && tx >= s.tx {
			r, w := float64(rx-s.rx)/sec, float64(tx-s.tx)/sec
			out.NetRxPerSec, out.NetTxPerSec = &r, &w
		}
	}
	sec := t.Sub(s.at).Seconds()
	for _, name := range ifs {
		ir := IfaceRate{Name: name}
		c := per[name]
		if p, ok := s.ifs[name]; ok && s.have && sec > 0 && c.rx >= p.rx && c.tx >= p.tx {
			r, w := float64(c.rx-p.rx)/sec, float64(c.tx-p.tx)/sec
			ir.RxPerSec, ir.TxPerSec = &r, &w
		}
		out.NetByIface = append(out.NetByIface, ir)
	}
	s.at, s.busy, s.total, s.rx, s.tx, s.have, s.ifs = t, busy, total, rx, tx, true, per
	return out
}

func readCPU(path string) (busy, total uint64, n int) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if f[0] == "cpu" {
			for i, v := range f[1:] {
				x, _ := strconv.ParseUint(v, 10, 64)
				total += x
				if i != 3 && i != 4 { // idle, iowait
					busy += x
				}
			}
		} else if strings.HasPrefix(f[0], "cpu") {
			n++
		}
	}
	return
}

func readMem(path string) (total, used int64) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var avail int64
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	return total, total - avail
}

// Виртуальные интерфейсы не считаются: иначе трафик мостов и контейнеров
// учитывается дважды.
var virtualPrefix = []string{"lo", "veth", "docker", "br-", "virbr", "tap", "tun", "vnet"}

func readNet(path string) (rx, tx uint64, ifs []string, per map[string]counters) {
	per = map[string]counters{}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, l := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		skip := false
		for _, p := range virtualPrefix {
			skip = skip || strings.HasPrefix(name, p)
		}
		f := strings.Fields(rest)
		if skip || len(f) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(f[0], 10, 64)
		t, _ := strconv.ParseUint(f[8], 10, 64)
		rx, tx = rx+r, tx+t
		ifs = append(ifs, name)
		per[name] = counters{r, t}
	}
	return
}

func readFloat1(path string) float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}
