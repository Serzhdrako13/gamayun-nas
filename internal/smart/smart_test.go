package smart

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/disks"
)

func load(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func i64(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func TestParseQEMUUnsupported(t *testing.T) {
	in, err := Parse("w1", load(t, "qemu.json"))
	if err != nil {
		t.Fatal(err)
	}
	if in.Supported || in.Passed != nil || in.TemperatureC != nil {
		t.Fatalf("QEMU: SMART не должен считаться доступным: %+v", in)
	}
	if in.Model != "QEMU QEMU HARDDISK" {
		t.Errorf("модель %q", in.Model)
	}
}

func TestParseATAOK(t *testing.T) {
	in, err := Parse("w1", load(t, "ata_ok.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !in.Supported || in.Passed == nil || !*in.Passed {
		t.Fatalf("итог: %+v", in)
	}
	if i64(in.TemperatureC) != 34 || i64(in.PowerOnHours) != 31234 {
		t.Errorf("температура/часы: %d/%d", i64(in.TemperatureC), i64(in.PowerOnHours))
	}
	if i64(in.Reallocated) != 0 || i64(in.Pending) != 0 || i64(in.Uncorrectable) != 0 {
		t.Errorf("секторы: %d %d %d", i64(in.Reallocated), i64(in.Pending), i64(in.Uncorrectable))
	}
	if in.TestRunning || in.LastTest == nil || in.LastTest.Type != TestShort || !in.LastTest.Passed {
		t.Errorf("самотест: %+v %+v", in.TestRunning, in.LastTest)
	}
}

func TestParseATAFailing(t *testing.T) {
	in, err := Parse("w2", load(t, "ata_bad.json"))
	if err != nil {
		t.Fatal(err)
	}
	if in.Passed == nil || *in.Passed {
		t.Fatal("итог FAILED не распознан")
	}
	if i64(in.Reallocated) != 120 || i64(in.Pending) != 8 || i64(in.Uncorrectable) != 8 {
		t.Errorf("секторы: %d %d %d", i64(in.Reallocated), i64(in.Pending), i64(in.Uncorrectable))
	}
	if !in.TestRunning || i64(in.TestRemaining) != 90 {
		t.Errorf("идущий тест: %v %d", in.TestRunning, i64(in.TestRemaining))
	}
	if in.LastTest == nil || in.LastTest.Passed || in.LastTest.Type != TestLong {
		t.Errorf("последний тест: %+v", in.LastTest)
	}
	if len(in.Messages) != 1 {
		t.Errorf("сообщения: %v", in.Messages)
	}
}

func TestParseNVMe(t *testing.T) {
	in, err := Parse("e1", load(t, "nvme.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !in.Supported || !*in.Passed || i64(in.Uncorrectable) != 0 || i64(in.TemperatureC) != 38 {
		t.Fatalf("NVMe: %+v", in)
	}
	if !in.TestRunning || i64(in.TestRemaining) != 60 || in.LastTest == nil || !in.LastTest.Passed {
		t.Errorf("NVMe самотест: %v %d %+v", in.TestRunning, i64(in.TestRemaining), in.LastTest)
	}
	if in.Reallocated != nil || in.Pending != nil {
		t.Error("у NVMe нет ATA-атрибутов секторов")
	}
}

func TestParseBadJSON(t *testing.T) {
	if _, err := Parse("x", []byte("not json")); err == nil {
		t.Fatal("ожидалась ошибка")
	}
}

type fakeRunner struct {
	res   CmdResult
	args  []string
	calls []string
	// next — ответы по очереди (пусто — res).
	next []CmdResult
}

func (f *fakeRunner) Run(_ context.Context, _ time.Duration, path string, args ...string) (CmdResult, error) {
	f.args = append([]string{path}, args...)
	f.calls = append(f.calls, strings.Join(f.args, " "))
	if len(f.next) > 0 {
		r := f.next[0]
		f.next = f.next[1:]
		return r, nil
	}
	return f.res, nil
}

func exec(r *fakeRunner, ds ...disks.Disk) *Executor {
	return &Executor{Runner: r, Smartctl: "/usr/sbin/smartctl", Observe: func() (disks.Snapshot, error) {
		return disks.Snapshot{Disks: ds}, nil
	}}
}

func TestReadByID(t *testing.T) {
	r := &fakeRunner{res: CmdResult{Stdout: load(t, "ata_ok.json")}}
	e := exec(r, disks.Disk{KernelName: "sda", ID: "a"}, disks.Disk{KernelName: "sdb", ID: "b"})
	in, err := e.Read(context.Background(), DiskParams{DiskID: "b"})
	if err != nil || !in.Supported || in.DiskID != "b" {
		t.Fatalf("%+v %v", in, err)
	}
	if got := strings.Join(r.args, " "); !strings.HasSuffix(got, "-n standby -i -H -A -l selftest /dev/sdb") {
		t.Errorf("команда: %s", got)
	}
	// Статус «диск неисправен» (бит 3) — не ошибка команды.
	r.res = CmdResult{ExitCode: 8, Stdout: load(t, "ata_bad.json")}
	if in, err := e.Read(context.Background(), DiskParams{DiskID: "b"}); err != nil || *in.Passed {
		t.Fatalf("FAILED: %+v %v", in, err)
	}
	// Бит 0 — ошибка разбора командной строки.
	r.res = CmdResult{ExitCode: 1}
	if _, err := e.Read(context.Background(), DiskParams{DiskID: "b"}); err == nil {
		t.Fatal("код 1 должен быть ошибкой")
	}
	// Диск спит: не будим, не ошибка.
	r.res = CmdResult{ExitCode: 2, Stdout: []byte(`{"smartctl":{"messages":[{"string":"Device is in STANDBY mode, exit(2)"}]}}`)}
	if in, err := e.Read(context.Background(), DiskParams{DiskID: "b"}); err != nil || !in.Standby {
		t.Fatalf("standby: %+v %v", in, err)
	}
	r.res = CmdResult{ExitCode: 2}
	if _, err := e.Read(context.Background(), DiskParams{DiskID: "b"}); err == nil {
		t.Fatal("код 2 без standby — ошибка открытия")
	}
}

func TestUnknownOrDuplicateID(t *testing.T) {
	r := &fakeRunner{}
	e := exec(r, disks.Disk{KernelName: "sda", ID: "a"}, disks.Disk{KernelName: "sdb", ID: "a"})
	if _, err := e.Read(context.Background(), DiskParams{DiskID: "a"}); err == nil {
		t.Error("дубликат ID должен отклоняться")
	}
	if _, err := e.Read(context.Background(), DiskParams{DiskID: "z"}); err == nil {
		t.Error("неизвестный ID должен отклоняться")
	}
	if r.args != nil {
		t.Error("smartctl не должен запускаться")
	}
	e.Observe = func() (disks.Snapshot, error) { return disks.Snapshot{}, errors.New("x") }
	if _, err := e.Test(context.Background(), TestParams{DiskID: "a", Type: TestShort}); err == nil {
		t.Error("ошибка снимка должна возвращаться")
	}
}

func TestStartTest(t *testing.T) {
	r := &fakeRunner{}
	e := exec(r, disks.Disk{KernelName: "sdc", ID: "c"})
	res, err := e.Test(context.Background(), TestParams{DiskID: "c", Type: TestLong})
	if err != nil || !res.Started {
		t.Fatalf("%+v %v", res, err)
	}
	if got := strings.Join(r.args, " "); got != "/usr/sbin/smartctl -t long /dev/sdc" {
		t.Errorf("команда: %s", got)
	}
	r.res = CmdResult{ExitCode: 4, Stdout: []byte("...\nShort offline self test failed [unsupported scsi opcode]\n")}
	res, err = e.Test(context.Background(), TestParams{DiskID: "c", Type: TestShort})
	if err != nil || res.Started || !strings.Contains(res.Message, "unsupported") {
		t.Fatalf("неподдерживаемый: %+v %v", res, err)
	}
}

func TestParamsValidate(t *testing.T) {
	for _, bad := range []string{"", "a b", "../x", strings.Repeat("a", 200)} {
		if (DiskParams{bad}).Validate() == nil {
			t.Errorf("%q должен отклоняться", bad)
		}
	}
	if (TestParams{DiskID: "a", Type: "conveyance"}).Validate() == nil {
		t.Error("вид теста conveyance не поддерживается")
	}
}

// SMART есть, но выключен в диске — не «не поддерживается».
func TestParseDisabled(t *testing.T) {
	in, err := Parse("w1", load(t, "ata_disabled.json"))
	if err != nil {
		t.Fatal(err)
	}
	if in.Supported || !in.Disabled || in.Passed != nil || in.Model != "TOSHIBA HDWD110" {
		t.Fatalf("выключенный SMART: %+v", in)
	}
	q, _ := Parse("w2", load(t, "qemu.json"))
	if q.Disabled {
		t.Error("QEMU: не «выключен», а «нет SMART»")
	}
	ok, _ := Parse("w3", load(t, "ata_ok.json"))
	if ok.Disabled || !ok.Supported {
		t.Errorf("включённый: %+v", ok)
	}
}

func TestSet(t *testing.T) {
	r := &fakeRunner{next: []CmdResult{{}, {Stdout: load(t, "ata_ok.json")}}}
	e := exec(r, disks.Disk{KernelName: "sdd", ID: "d"})
	in, err := e.Set(context.Background(), SetParams{DiskID: "d", On: true})
	if err != nil || !in.Supported || in.Disabled {
		t.Fatalf("%+v %v", in, err)
	}
	if len(r.calls) != 2 || r.calls[0] != "/usr/sbin/smartctl -s on /dev/sdd" || !strings.Contains(r.calls[1], " -i -H -A ") {
		t.Fatalf("вызовы: %q", r.calls)
	}
	r.calls = nil
	r.next = []CmdResult{{}, {ExitCode: 4, Stdout: load(t, "ata_disabled.json")}}
	if in, err := e.Set(context.Background(), SetParams{DiskID: "d"}); err != nil || !in.Disabled || r.calls[0] != "/usr/sbin/smartctl -s off /dev/sdd" {
		t.Fatalf("off: %+v %v %q", in, err, r.calls)
	}
	// Отказ диска включить — ошибка с текстом smartctl.
	r.next = []CmdResult{{ExitCode: 4, Stdout: []byte("...\nSMART Enable failed: Input/output error\n")}}
	if _, err := e.Set(context.Background(), SetParams{DiskID: "d", On: true}); err == nil || !strings.Contains(err.Error(), "Enable failed") {
		t.Fatalf("отказ: %v", err)
	}
	if (SetParams{DiskID: "../x"}).Validate() == nil {
		t.Error("недопустимый ID")
	}
}
