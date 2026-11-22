// Package smart — чтение SMART и запуск самотестов дисков через smartctl.
// Работает внутри gamayun-helper: диск задаётся стабильным ID,
// имя ядра берётся из свежего снимка дисков.
package smart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gamayun-nas/internal/disks"
)

// Операции helper.
const (
	OpRead = "disk.smart.read"
	OpTest = "disk.smart.test"
	OpSet  = "disk.smart.set" // включить/выключить SMART в диске
)

// Виды самотеста.
const (
	TestShort = "short"
	TestLong  = "long"
)

// CmdResult — итог внешней команды.
type CmdResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// Runner — запуск программы без shell (реализация — helper.Runner).
type Runner interface {
	Run(ctx context.Context, timeout time.Duration, path string, args ...string) (CmdResult, error)
}

// Executor — операции SMART.
type Executor struct {
	Runner   Runner
	Smartctl string // /usr/sbin/smartctl
	Disks    disks.Paths
	// Observe — снимок дисков; nil — disks.Observe(Disks).
	Observe func() (disks.Snapshot, error)
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// DiskParams — диск по стабильному ID.
type DiskParams struct {
	DiskID string `json:"disk_id"`
}

// Validate — формат ID.
func (p DiskParams) Validate() error {
	if !idRe.MatchString(p.DiskID) {
		return errors.New("disk_id: недопустимое значение")
	}
	return nil
}

// TestParams — запуск самотеста.
type TestParams struct {
	DiskID string `json:"disk_id"`
	Type   string `json:"type"`
}

// Validate — ID и вид теста.
func (p TestParams) Validate() error {
	if err := (DiskParams{p.DiskID}).Validate(); err != nil {
		return err
	}
	if p.Type != TestShort && p.Type != TestLong {
		return errors.New("type: short или long")
	}
	return nil
}

// SetParams — включение (On=true) или выключение SMART в диске.
type SetParams struct {
	DiskID string `json:"disk_id"`
	On     bool   `json:"on"`
}

// Validate — формат ID.
func (p SetParams) Validate() error { return DiskParams{p.DiskID}.Validate() }

// SelfTest — последняя запись журнала самотестов.
type SelfTest struct {
	Type          string `json:"type"`   // short, long, …
	Passed        bool   `json:"passed"` // завершён без ошибок
	Status        string `json:"status"` // текст smartctl
	LifetimeHours *int64 `json:"lifetime_hours,omitempty"`
}

// Info — сведения SMART диска. Supported=false — SMART недоступен
// (виртуальный диск, USB-мост без SAT): остальные поля пусты.
type Info struct {
	DiskID    string `json:"disk_id"`
	Supported bool   `json:"supported"`
	// Disabled — SMART есть в диске, но выключен (флаг в диске):
	// Supported=false, данных нет, включается smartctl -s on.
	Disabled bool `json:"disabled,omitempty"`
	// ManualOff — выключен оператором вручную: автовключение не трогает
	// (заполняет сервис обслуживания, не helper).
	ManualOff bool `json:"manual_off,omitempty"`
	// Passed — общий итог самооценки диска (nil — не сообщает).
	Passed       *bool  `json:"passed,omitempty"`
	Protocol     string `json:"protocol,omitempty"` // ATA, NVMe, SCSI
	Model        string `json:"model,omitempty"`
	Serial       string `json:"serial,omitempty"`
	TemperatureC *int64 `json:"temperature_c,omitempty"`
	PowerOnHours *int64 `json:"power_on_hours,omitempty"`
	// Reallocated/Pending/Uncorrectable — секторы (ATA 5/197/198; NVMe —
	// media_errors в Uncorrectable); nil — диск не сообщает.
	Reallocated   *int64 `json:"reallocated,omitempty"`
	Pending       *int64 `json:"pending,omitempty"`
	Uncorrectable *int64 `json:"uncorrectable,omitempty"`
	// TestRunning — идёт самотест; TestRemaining — осталось, %.
	TestRunning   bool      `json:"test_running,omitempty"`
	TestRemaining *int64    `json:"test_remaining,omitempty"`
	LastTest      *SelfTest `json:"last_test,omitempty"`
	// Standby — диск спал, опрос пропущен (не будим).
	Standby bool `json:"standby,omitempty"`
	// Messages — предупреждения smartctl (например, «FAILING_NOW»).
	Messages []string `json:"messages,omitempty"`
}

// raw — нужная часть JSON smartctl 7.x.
type raw struct {
	Smartctl struct {
		ExitStatus int `json:"exit_status"`
		Messages   []struct {
			String   string `json:"string"`
			Severity string `json:"severity"`
		} `json:"messages"`
	} `json:"smartctl"`
	Device struct {
		Protocol string `json:"protocol"`
	} `json:"device"`
	ModelName    string `json:"model_name"`
	SCSIModel    string `json:"scsi_model_name"`
	SerialNumber string `json:"serial_number"`
	SmartSupport *struct {
		Available bool `json:"available"`
		Enabled   bool `json:"enabled"`
	} `json:"smart_support"`
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current int64 `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours int64 `json:"hours"`
	} `json:"power_on_time"`
	ATASmartAttributes *struct {
		Table []struct {
			ID  int `json:"id"`
			Raw struct {
				Value int64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	ATASmartData *struct {
		SelfTest struct {
			Status struct {
				Value            int    `json:"value"`
				String           string `json:"string"`
				RemainingPercent *int64 `json:"remaining_percent"`
			} `json:"status"`
		} `json:"self_test"`
	} `json:"ata_smart_data"`
	ATASelfTestLog *struct {
		Standard struct {
			Table []struct {
				Type struct {
					String string `json:"string"`
				} `json:"type"`
				Status struct {
					String string `json:"string"`
					Passed bool   `json:"passed"`
				} `json:"status"`
				LifetimeHours *int64 `json:"lifetime_hours"`
			} `json:"table"`
		} `json:"standard"`
	} `json:"ata_smart_self_test_log"`
	NVMeHealth *struct {
		MediaErrors *int64 `json:"media_errors"`
	} `json:"nvme_smart_health_information_log"`
	NVMeSelfTestLog *struct {
		CurrentOperation *struct {
			Value int `json:"value"`
		} `json:"current_self_test_operation"`
		CurrentCompletion *int64 `json:"current_self_test_completion_percent"`
		Table             []struct {
			Type struct {
				String string `json:"string"`
			} `json:"self_test_code"`
			Result struct {
				Value  int    `json:"value"`
				String string `json:"string"`
			} `json:"self_test_result"`
			PowerOnHours *int64 `json:"power_on_hours"`
		} `json:"table"`
	} `json:"nvme_self_test_log"`
	SCSIGrownDefects *int64 `json:"scsi_grown_defect_list"`
}

// Parse разбирает JSON smartctl (-j -i -H -A -l selftest).
func Parse(id string, b []byte) (Info, error) {
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return Info{}, fmt.Errorf("smartctl: разбор JSON: %w", err)
	}
	in := Info{DiskID: id, Protocol: r.Device.Protocol, Serial: r.SerialNumber, Model: r.ModelName}
	if in.Model == "" {
		in.Model = r.SCSIModel
	}
	for _, m := range r.Smartctl.Messages {
		in.Messages = append(in.Messages, m.String)
	}
	// SMART есть, если smartctl получил итог самооценки или атрибуты;
	// smart_support.available=false (QEMU, мосты без SAT) — нет.
	in.Supported = r.SmartStatus != nil || r.ATASmartAttributes != nil || r.NVMeHealth != nil
	if r.SmartSupport != nil && !r.SmartSupport.Available {
		in.Supported = false
	}
	// SMART есть, но выключен в диске — smartctl не отдаёт данных.
	disabled := r.SmartSupport != nil && r.SmartSupport.Available && !r.SmartSupport.Enabled
	if disabled {
		in.Supported = false
	}
	if !in.Supported {
		return Info{DiskID: id, Protocol: in.Protocol, Model: in.Model, Serial: in.Serial, Messages: in.Messages, Disabled: disabled}, nil
	}
	if r.SmartStatus != nil {
		p := r.SmartStatus.Passed
		in.Passed = &p
	}
	if r.Temperature != nil && r.Temperature.Current > 0 {
		t := r.Temperature.Current
		in.TemperatureC = &t
	}
	if r.PowerOnTime != nil {
		h := r.PowerOnTime.Hours
		in.PowerOnHours = &h
	}
	if r.ATASmartAttributes != nil {
		for _, a := range r.ATASmartAttributes.Table {
			v := a.Raw.Value & 0xFFFFFFFF // старшие байты raw — поставщицкие
			switch a.ID {
			case 5:
				in.Reallocated = &v
			case 197:
				in.Pending = &v
			case 198:
				in.Uncorrectable = &v
			}
		}
	}
	if r.ATASmartData != nil {
		// Код 15 (0xF_) — самотест выполняется.
		if st := r.ATASmartData.SelfTest.Status; st.Value>>4 == 0xF {
			in.TestRunning = true
			in.TestRemaining = st.RemainingPercent
		}
	}
	if r.ATASelfTestLog != nil && len(r.ATASelfTestLog.Standard.Table) > 0 {
		e := r.ATASelfTestLog.Standard.Table[0]
		in.LastTest = &SelfTest{Type: testType(e.Type.String), Passed: e.Status.Passed, Status: e.Status.String, LifetimeHours: e.LifetimeHours}
	}
	if r.NVMeHealth != nil {
		in.Uncorrectable = r.NVMeHealth.MediaErrors
	}
	if l := r.NVMeSelfTestLog; l != nil {
		if l.CurrentOperation != nil && l.CurrentOperation.Value != 0 {
			in.TestRunning = true
			if l.CurrentCompletion != nil {
				rem := 100 - *l.CurrentCompletion
				in.TestRemaining = &rem
			}
		}
		if len(l.Table) > 0 {
			e := l.Table[0]
			in.LastTest = &SelfTest{Type: testType(e.Type.String), Passed: e.Result.Value == 0, Status: e.Result.String, LifetimeHours: e.PowerOnHours}
		}
	}
	if r.SCSIGrownDefects != nil {
		in.Reallocated = r.SCSIGrownDefects
	}
	return in, nil
}

func testType(s string) string {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "short"):
		return TestShort
	case strings.Contains(s, "extended"), strings.Contains(s, "long"):
		return TestLong
	}
	return s
}

// kname — имя ядра диска id по свежему снимку; только целые диски.
func (e *Executor) kname(id string) (string, error) {
	obs := e.Observe
	if obs == nil {
		obs = func() (disks.Snapshot, error) { return disks.Observe(e.Disks) }
	}
	snap, err := obs()
	if err != nil {
		return "", err
	}
	var found []string
	for _, d := range snap.Disks {
		if d.ID == id && d.KernelName != "" {
			found = append(found, d.KernelName)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("диск %s найден %d раз", id, len(found))
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9]{1,31}$`).MatchString(found[0]) {
		return "", fmt.Errorf("диск %s: недопустимое имя ядра %q", id, found[0])
	}
	return found[0], nil
}

// Read — сведения SMART диска. Биты 0–1 кода smartctl — ошибка команды
// или открытия устройства; прочие биты — состояние диска (в JSON).
func (e *Executor) Read(ctx context.Context, p DiskParams) (Info, error) {
	k, err := e.kname(p.DiskID)
	if err != nil {
		return Info{}, err
	}
	r, err := e.Runner.Run(ctx, 30*time.Second, e.Smartctl, "-j", "-n", "standby", "-i", "-H", "-A", "-l", "selftest", "/dev/"+k)
	if err != nil {
		return Info{}, fmt.Errorf("smartctl: %w", err)
	}
	if r.ExitCode&1 != 0 {
		return Info{}, fmt.Errorf("smartctl: код %d: %s", r.ExitCode, strings.TrimSpace(string(r.Stderr)))
	}
	if r.ExitCode&2 != 0 {
		if strings.Contains(strings.ToUpper(string(r.Stdout)), "STANDBY") {
			// -n standby: диск спит — не будим ради опроса.
			return Info{DiskID: p.DiskID, Supported: true, Standby: true}, nil
		}
		return Info{}, fmt.Errorf("smartctl: устройство не открыто (код %d)", r.ExitCode)
	}
	return Parse(p.DiskID, r.Stdout)
}

// TestResult — итог запуска самотеста.
type TestResult struct {
	DiskID  string `json:"disk_id"`
	Type    string `json:"type"`
	Started bool   `json:"started"`
	Message string `json:"message,omitempty"`
}

// Test запускает самотест (выполняет сам диск, в фоне; итог — в журнале
// самотестов, читается Read). Неразрушающий: данные не меняются.
func (e *Executor) Test(ctx context.Context, p TestParams) (TestResult, error) {
	k, err := e.kname(p.DiskID)
	if err != nil {
		return TestResult{}, err
	}
	res := TestResult{DiskID: p.DiskID, Type: p.Type}
	r, err := e.Runner.Run(ctx, 30*time.Second, e.Smartctl, "-t", p.Type, "/dev/"+k)
	if err != nil {
		return res, fmt.Errorf("smartctl: %w", err)
	}
	if r.ExitCode != 0 {
		out := strings.TrimSpace(string(r.Stdout))
		if i := strings.LastIndex(out, "\n"); i >= 0 {
			out = strings.TrimSpace(out[i+1:])
		}
		res.Message = out
		return res, nil
	}
	res.Started = true
	return res, nil
}

// Set включает или выключает SMART в диске (smartctl -s on|off) и
// возвращает свежие сведения. Данные диска не меняются; флаг хранится
// в самом диске и переживает перезагрузку.
func (e *Executor) Set(ctx context.Context, p SetParams) (Info, error) {
	k, err := e.kname(p.DiskID)
	if err != nil {
		return Info{}, err
	}
	arg := "off"
	if p.On {
		arg = "on"
	}
	r, err := e.Runner.Run(ctx, 30*time.Second, e.Smartctl, "-s", arg, "/dev/"+k)
	if err != nil {
		return Info{}, fmt.Errorf("smartctl: %w", err)
	}
	if r.ExitCode&3 != 0 || (r.ExitCode != 0 && p.On) {
		return Info{}, fmt.Errorf("smartctl -s %s: код %d: %s", arg, r.ExitCode, lastLine(r.Stdout, r.Stderr))
	}
	return e.Read(ctx, DiskParams{p.DiskID})
}

func lastLine(bs ...[]byte) string {
	for _, b := range bs {
		out := strings.TrimSpace(string(b))
		if out == "" {
			continue
		}
		if i := strings.LastIndex(out, "\n"); i >= 0 {
			out = strings.TrimSpace(out[i+1:])
		}
		return out
	}
	return ""
}
