package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, root, name, s string) {
	t.Helper()
	p := filepath.Join(root, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSample(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(1000, 0)
	s := &Sampler{Proc: root, Now: func() time.Time { return now }}
	write(t, root, "stat", "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 50 0 50 350 50 0 0 0\ncpu1 50 0 50 350 50 0 0 0\nintr 1\n")
	write(t, root, "meminfo", "MemTotal: 1000 kB\nMemFree: 100 kB\nMemAvailable: 400 kB\n")
	write(t, root, "net/dev", "Inter-|...\n face |bytes ...\n    lo: 999 1 0 0 0 0 0 0 999 1 0 0 0 0 0 0\n  eth0: 1000 1 0 0 0 0 0 0 500 1 0 0 0 0 0 0\nveth1: 7 0 0 0 0 0 0 0 7 0 0 0 0 0 0 0\n")
	write(t, root, "loadavg", "0.50 0.40 0.30 1/100 42\n")
	write(t, root, "uptime", "3600.5 7000.0\n")
	a := s.Sample()
	if a.CPUPercent != nil || a.NetRxPerSec != nil || a.CPUCount != 2 || a.MemTotal != 1024000 || a.MemUsed != 614400 ||
		len(a.NetIfaces) != 1 || a.NetIfaces[0] != "eth0" || a.Load1 != 0.5 || a.UptimeSec != 3600.5 {
		t.Fatalf("первый замер: %+v", a)
	}
	// +100 занятых и +100 простоя тиков, +2000/+1000 байт за 2 с
	write(t, root, "stat", "cpu  150 0 150 750 150 0 0 0 0 0\n")
	write(t, root, "net/dev", "  eth0: 3000 1 0 0 0 0 0 0 1500 1 0 0 0 0 0 0\n")
	now = now.Add(2 * time.Second)
	b := s.Sample()
	if b.CPUPercent == nil || *b.CPUPercent != 50 || *b.NetRxPerSec != 1000 || *b.NetTxPerSec != 500 {
		t.Fatalf("второй замер: cpu=%v rx=%v tx=%v", b.CPUPercent, b.NetRxPerSec, b.NetTxPerSec)
	}
	if len(b.NetByIface) != 1 || b.NetByIface[0].Name != "eth0" || *b.NetByIface[0].RxPerSec != 1000 || *b.NetByIface[0].TxPerSec != 500 {
		t.Fatalf("по адаптерам: %+v", b.NetByIface)
	}
}

// Скорость по каждому адаптеру; новый адаптер — без скорости.
func TestSamplePerIface(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(1000, 0)
	s := &Sampler{Proc: root, Now: func() time.Time { return now }}
	write(t, root, "net/dev", "  eth0: 1000 1 0 0 0 0 0 0 500 1 0 0 0 0 0 0\n  eth1: 0 1 0 0 0 0 0 0 0 1 0 0 0 0 0 0\n")
	if a := s.Sample(); len(a.NetByIface) != 2 || a.NetByIface[0].RxPerSec != nil {
		t.Fatalf("%+v", a.NetByIface)
	}
	write(t, root, "net/dev", "  eth0: 1000 1 0 0 0 0 0 0 500 1 0 0 0 0 0 0\n  eth1: 4000 1 0 0 0 0 0 0 8000 1 0 0 0 0 0 0\n  eth2: 5 1 0 0 0 0 0 0 5 1 0 0 0 0 0 0\n")
	now = now.Add(4 * time.Second)
	b := s.Sample()
	if len(b.NetByIface) != 3 || *b.NetByIface[0].RxPerSec != 0 || *b.NetByIface[1].RxPerSec != 1000 || *b.NetByIface[1].TxPerSec != 2000 || b.NetByIface[2].RxPerSec != nil {
		t.Fatalf("%+v", b.NetByIface)
	}
}

func TestSampleMissingProc(t *testing.T) {
	s := &Sampler{Proc: t.TempDir()}
	if v := s.Sample(); v.CPUCount != 0 || v.MemTotal != 0 {
		t.Fatalf("%+v", v)
	}
}
