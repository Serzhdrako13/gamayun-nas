package nas

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type runFunc func(path string, args []string) CmdResult

func (f runFunc) Run(_ context.Context, _ time.Duration, _ []byte, path string, args ...string) (CmdResult, error) {
	return f(path, args), nil
}

// btrfsExec — исполнитель, у которого ФС пула «Btrfs»: подтомы — каталоги из
// набора subvols; «btrfs subvolume create» в подмене создаёт каталог.
func btrfsExec(t *testing.T, subvols map[string]bool) (*Executor, *[]string) {
	t.Helper()
	e := newExec(t, &fakeRunner{})
	var cmds []string
	e.FS = func(dir string) (FSInfo, error) { return FSInfo{Btrfs: true, Subvol: subvols[dir]}, nil }
	e.Runner = runFunc(func(path string, args []string) CmdResult {
		cmds = append(cmds, path+" "+strings.Join(args, " "))
		switch {
		case path == "/usr/bin/btrfs" && len(args) == 3 && args[0] == "subvolume" && args[1] == "create":
			if err := os.Mkdir(args[2], 0o755); err != nil {
				return CmdResult{ExitCode: 1, Stderr: []byte(err.Error())}
			}
			subvols[args[2]] = true
		case path == "/usr/bin/install":
			os.MkdirAll(args[len(args)-1], 0o755)
		case path == "/usr/bin/id":
			return CmdResult{ExitCode: 1}
		}
		return CmdResult{}
	})
	return e, &cmds
}

func countPrefix(cmds []string, prefix string) int {
	n := 0
	for _, c := range cmds {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestSharesApplyCreatesSubvolume(t *testing.T) {
	sv := map[string]bool{}
	e, cmds := btrfsExec(t, sv)
	dir := ShareDir(e.MountRoot, "p1", "docs")
	p := SharesParams{Shares: []Share{{Name: "docs", Pool: "p1"}}}
	if _, err := e.SharesApply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if !sv[dir] || countPrefix(*cmds, "/usr/bin/btrfs subvolume create "+dir) != 1 {
		t.Fatalf("подтом не создан: %v", *cmds)
	}
	// Порядок: родитель shares, подтом, затем режим/владелец корня подтома.
	var order []string
	for _, c := range *cmds {
		if strings.HasPrefix(c, "/usr/bin/install") || strings.HasPrefix(c, "/usr/bin/btrfs") {
			order = append(order, c)
		}
	}
	if len(order) != 3 || !strings.HasSuffix(order[0], "-- "+filepath.Dir(dir)) || !strings.HasPrefix(order[1], "/usr/bin/btrfs") || !strings.HasSuffix(order[2], "-- "+dir) {
		t.Fatalf("порядок команд: %v", order)
	}
	// Повтор: уже подтом — ничего не создаётся.
	*cmds = nil
	if _, err := e.SharesApply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if countPrefix(*cmds, "/usr/bin/btrfs") != 0 {
		t.Fatalf("повтор не должен создавать подтом: %v", *cmds)
	}
}

func TestSharesApplyConvertsOnlyEmptyDir(t *testing.T) {
	sv := map[string]bool{}
	e, cmds := btrfsExec(t, sv)
	empty := ShareDir(e.MountRoot, "p1", "empty")
	full := ShareDir(e.MountRoot, "p1", "full")
	for _, d := range []string{empty, full} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(full, "data.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := e.SharesApply(context.Background(), SharesParams{Shares: []Share{{Name: "empty", Pool: "p1"}, {Name: "full", Pool: "p1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !sv[empty] || !res.Changed {
		t.Fatalf("пустой каталог должен стать подтомом: %v", *cmds)
	}
	if sv[full] || countPrefix(*cmds, "/usr/bin/btrfs subvolume create "+full) != 0 {
		t.Fatalf("непустой каталог трогать нельзя: %v", *cmds)
	}
	if b, err := os.ReadFile(filepath.Join(full, "data.txt")); err != nil || string(b) != "x" {
		t.Fatalf("данные папки full: %q %v", b, err)
	}
}

func TestSharesApplyNoSubvolumeOutsideBtrfs(t *testing.T) {
	r := &fakeRunner{}
	e := newExec(t, r) // ФС — не Btrfs
	if _, err := e.SharesApply(context.Background(), SharesParams{Shares: []Share{{Name: "docs", Pool: "p1"}}}); err != nil {
		t.Fatal(err)
	}
	if r.has("/usr/bin/btrfs") {
		t.Fatalf("на не-Btrfs подтом не создаётся: %+v", r.calls)
	}
}

func TestHomesApplyCreatesSubvolume(t *testing.T) {
	sv := map[string]bool{}
	e, cmds := btrfsExec(t, sv)
	if _, err := e.HomesApply(context.Background(), HomesParams{Enabled: true, Pool: "p1"}); err != nil {
		t.Fatal(err)
	}
	if base := HomesDir(e.MountRoot, "p1"); !sv[base] {
		t.Fatalf("каталог homes — подтом: %v", *cmds)
	}
}

func TestSharesApplyShadowCopy(t *testing.T) {
	r := &fakeRunner{}
	e := newExec(t, r)
	p := SharesParams{Shares: []Share{{Name: "docs", Pool: "p1", Write: []string{"ann"}}}, Homes: "p1"}
	res, err := e.SharesApply(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(e.IncludeFile)
	if strings.Contains(string(b), "shadow_copy2") {
		t.Fatalf("без модуля строк shadow_copy2 быть не должно:\n%s", b)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "vfs_shadow_copy2") {
		t.Fatalf("нет предупреждения об отсутствии модуля: %v", res.Warnings)
	}
	e.ShadowCopy = func() bool { return true }
	if res, err = e.SharesApply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(e.IncludeFile)
	conf := string(b)
	for _, want := range []string{
		"vfs objects = shadow_copy2",
		"shadow:snapdir = " + e.MountRoot + "/p1/.snapshots/docs\n",
		"shadow:snapdir = " + e.MountRoot + "/p1/.snapshots/homes\n",
		"shadow:localtime = no",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("нет %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "%Y") {
		t.Errorf("формат имён — умолчание модуля, «%%» в конфиге не нужен:\n%s", conf)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "vfs_shadow_copy2") {
			t.Fatalf("модуль есть — предупреждения быть не должно: %v", res.Warnings)
		}
	}
}

func TestSharesApplySubvolumeFailureFallsBack(t *testing.T) {
	sv := map[string]bool{}
	e, cmds := btrfsExec(t, sv)
	inner := e.Runner
	e.Runner = runFunc(func(path string, args []string) CmdResult {
		if path == "/usr/bin/btrfs" {
			*cmds = append(*cmds, path+" "+strings.Join(args, " "))
			return CmdResult{ExitCode: 1, Stderr: []byte("ERROR: нет поддержки")}
		}
		r, _ := inner.Run(context.Background(), 0, nil, path, args...)
		return r
	})
	res, err := e.SharesApply(context.Background(), SharesParams{Shares: []Share{{Name: "docs", Pool: "p1"}}})
	if err != nil {
		t.Fatalf("сбой подтома не должен ломать применение: %v", err)
	}
	dir := ShareDir(e.MountRoot, "p1", "docs")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() || sv[dir] {
		t.Fatalf("папка должна быть обычным каталогом: %v", err)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "подтом не создан") {
		t.Fatalf("нет предупреждения: %v", res.Warnings)
	}
}
