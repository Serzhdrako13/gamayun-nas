package helper

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/storage/home"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "h.json")
		os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	c, err := LoadConfig(write(`{"destructive_allow_ids":["wwn-0x5000000000000101"],"mount_root":"/mnt/x"}`))
	if err != nil || len(c.DestructiveAllowIDs) != 1 || c.MountRoot != "/mnt/x" {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = LoadConfig(write(`{}`))
	if err != nil || len(c.DestructiveAllowIDs) != 0 || c.MountRoot != "/srv/gamayun-nas" {
		t.Fatalf("значения по умолчанию: %+v %v", c, err)
	}
	for name, s := range map[string]string{
		"неизвестное поле":    `{"allow":[]}`,
		"плохая идентичность": `{"destructive_allow_ids":["/dev/sdb"]}`,
		"повтор":              `{"destructive_allow_ids":["wwn-0x1","wwn-0x1"]}`,
		"относительный путь":  `{"mount_root":"mnt"}`,
		"корень":              `{"mount_root":"/"}`,
		"лишние данные":       `{} {}`,
	} {
		if _, err := LoadConfig(write(s)); err == nil {
			t.Errorf("%s: ожидалась ошибка", name)
		}
	}
	if DefaultConfig().Validate() != nil || len(DefaultConfig().DestructiveAllowIDs) != 0 {
		t.Error("по умолчанию разрушающие операции должны быть запрещены")
	}
}

func homeDeps(t *testing.T, allow []string) Deps {
	d := fakeDeps(t)
	d.Home = &home.Executor{Runner: RunnerAdapter{d.Runner}, Tools: home.DefaultTools(),
		Paths:    home.Paths{Disks: d.Disks, Dev: t.TempDir(), Mdstat: filepath.Join(t.TempDir(), "mdstat")},
		AllowIDs: allow, MountRoot: "/mnt/x"}
	return d
}

func TestRegistryWithHome(t *testing.T) {
	r := DefaultRegistry(homeDeps(t, nil))
	got := strings.Join(r.Names(), ",")
	want := "disks.observe,home.disk.release,home.pool.add_disk,home.pool.assemble,home.pool.create,home.pool.create_rollback,home.pool.destroy,home.pool.grow,home.pool.import,home.pool.replace,home.pool.replace_finish,home.pool.scan,home.pool.scrub_cancel,home.pool.scrub_start,home.pool.scrub_status,home.pool.set_readonly,home.pool.status,system.info"
	if got != want {
		t.Fatalf("операции %s", got)
	}
	for _, op := range []string{home.OpCreate, home.OpAddDisk, home.OpReplace, home.OpReplaceFinish, home.OpGrow, home.OpScrubStart, home.OpScrubCancel} {
		if !r.IsMutating(op) {
			t.Errorf("%s должна быть изменяющей", op)
		}
	}
	for _, op := range []string{home.OpStatus, "disks.observe", "system.info"} {
		if r.IsMutating(op) {
			t.Errorf("%s не изменяет систему", op)
		}
	}
}

// Пустой allowlist: create отклоняется кодом refused до каких-либо команд;
// неверные параметры — invalid_params.
func TestHomeOpsRefusedAndInvalid(t *testing.T) {
	r := DefaultRegistry(homeDeps(t, nil))
	ctx := context.Background()
	p := home.CreateParams{Pool: "p1", Disks: []home.DiskRef{{ID: "wwn-0x1", SizeBytes: 1 << 30}, {ID: "wwn-0x2", SizeBytes: 1 << 30}},
		Arrays: []home.ArraySpec{{Level: "raid1", SliceBytes: 1<<30 - home.PartitionReserve, Members: []string{"wwn-0x1", "wwn-0x2"}}}}
	raw, _ := json.Marshal(p)
	resp := r.Execute(ctx, home.OpCreate, raw)
	if resp.OK || resp.Error.Code != CodeRefused { // несуществующие диски — отказ проверок
		t.Fatalf("%+v", resp.Error)
	}
	for _, bad := range []string{
		`{"pool":"P1","disks":[],"arrays":[]}`,
		`{"pool":"p1","integrity":false,"disks":[{"id":"wwn-0x1","size_bytes":1073741824}],"arrays":[{"level":"raid1","slice_bytes":1071644672,"members":["wwn-0x1"]}]}`,
		`{"pool":"p1","cmd":"rm -rf /"}`,
	} {
		if resp := r.Execute(ctx, home.OpCreate, json.RawMessage(bad)); resp.OK || resp.Error.Code != CodeInvalidParams {
			t.Errorf("%s: %+v", bad, resp)
		}
	}
	if resp := r.Execute(ctx, home.OpGrow, json.RawMessage(`{"pool":"p1"}`)); resp.OK || resp.Error.Code != CodeRefused {
		t.Errorf("grow несуществующего пула: %+v", resp)
	}
}

func TestRunnerAdapter(t *testing.T) {
	a := RunnerAdapter{&Runner{}}
	r, err := a.Run(context.Background(), 5*time.Second, "/bin/sh", "-c", "echo out; echo err >&2; exit 3")
	if err != nil || r.ExitCode != 3 || string(r.Stdout) != "out\n" || string(r.Stderr) != "err\n" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := a.Run(context.Background(), time.Second, "sh"); err == nil {
		t.Fatal("относительный путь должен отклоняться")
	}
}

// nas.share.dirs читает только внутри каталога шары; параметры строгие.
func TestShareDirsOp(t *testing.T) {
	d := fakeDeps(t)
	mount := t.TempDir()
	d.NAS = &nas.Executor{MountRoot: mount}
	if err := os.MkdirAll(nas.ShareDir(mount, "p1", "docs")+"/a/b", 0o755); err != nil {
		t.Fatal(err)
	}
	r := DefaultRegistry(d)
	resp := r.Execute(context.Background(), nas.OpShareDirs, json.RawMessage(`{"pool":"p1","share":"docs","path":"a","depth":1}`))
	if !resp.OK || !strings.Contains(string(resp.Result), `"path":"a/b"`) {
		t.Fatalf("%+v %s", resp, resp.Result)
	}
	for _, bad := range []string{
		`{"pool":"p1","share":"docs","path":"../..","depth":1}`,
		`{"pool":"p1","share":"docs","path":"/etc","depth":1}`,
		`{"pool":"../p1","share":"docs","depth":1}`,
		`{"pool":"p1","share":"../docs","depth":1}`,
		`{"pool":"p1","share":"docs","depth":50}`,
		`{"pool":"p1","share":"docs","depth":1,"extra":1}`,
	} {
		if resp := r.Execute(context.Background(), nas.OpShareDirs, json.RawMessage(bad)); resp.OK || resp.Error.Code != CodeInvalidParams {
			t.Errorf("%s: %+v", bad, resp)
		}
	}
}
