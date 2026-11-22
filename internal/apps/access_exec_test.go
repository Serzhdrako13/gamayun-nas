package apps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/nas"
)

func accessRig(t *testing.T) *rig {
	r := newRig(t)
	r.e.Paths.Passwd = r.root + "/etc/passwd"
	r.e.Paths.Group = r.root + "/etc/group"
	os.WriteFile(r.e.Paths.Passwd, []byte("root:x:0:0::/root:/bin/bash\nann:x:1001:1000::/x:/usr/sbin/nologin\n"), 0o644)
	os.WriteFile(r.e.Paths.Group, []byte("root:x:0:\ngamayun-users:x:1000:\n"), 0o644)
	return r
}

func TestUserEnsureDelete(t *testing.T) {
	r := accessRig(t)
	ctx := context.Background()
	res, err := r.e.UserEnsure(ctx, UserParams{ID: "web", UID: 59001})
	if err != nil || !res.Changed || res.Name != "app-web" {
		t.Fatalf("%+v %v", res, err)
	}
	cmds := strings.Join(r.f.commands(), "\n")
	if !strings.Contains(cmds, "groupadd --gid 59001 -- app-web") ||
		!strings.Contains(cmds, "useradd --uid 59001 --gid 59001 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin") {
		t.Fatal(cmds)
	}
	// уже есть — без команд
	os.WriteFile(r.e.Paths.Passwd, []byte("app-web:x:59001:59001::/nonexistent:/usr/sbin/nologin\nbob:x:59002:1000::/:/bin/sh\n"), 0o644)
	os.WriteFile(r.e.Paths.Group, []byte("app-web:x:59001:\ngamayun-users:x:1000:\n"), 0o644)
	n := len(r.f.commands())
	if res, err := r.e.UserEnsure(ctx, UserParams{ID: "web", UID: 59001}); err != nil || res.Changed || len(r.f.commands()) != n {
		t.Fatalf("повтор: %+v %v", res, err)
	}
	// UID занят чужой учёткой; имя с другим UID — отказ
	if _, err := r.e.UserEnsure(ctx, UserParams{ID: "db", UID: 59002}); !errors.Is(err, ErrRefused) {
		t.Fatalf("занятый UID: %v", err)
	}
	if _, err := r.e.UserEnsure(ctx, UserParams{ID: "web", UID: 59003}); !errors.Is(err, ErrRefused) {
		t.Fatalf("имя с другим UID: %v", err)
	}
	if _, err := r.e.UserDelete(ctx, UserParams{ID: "web", UID: 59003}); !errors.Is(err, ErrRefused) {
		t.Fatalf("удаление чужого UID: %v", err)
	}
	if res, err := r.e.UserDelete(ctx, UserParams{ID: "web", UID: 59001}); err != nil || !res.Changed {
		t.Fatalf("удаление: %+v %v", res, err)
	}
	if !strings.Contains(strings.Join(r.f.commands(), "\n"), "userdel -- app-web") {
		t.Fatal("нет userdel")
	}
	// параметры: диапазон
	for _, p := range []UserParams{{ID: "web", UID: 1000}, {ID: "web", UID: 0}, {ID: "web", UID: 60000}, {ID: "../x", UID: 59001}} {
		if p.Validate() == nil {
			t.Fatalf("принято: %+v", p)
		}
	}
	// сбой useradd — созданная группа удаляется
	os.WriteFile(r.e.Paths.Passwd, []byte(""), 0o644)
	os.WriteFile(r.e.Paths.Group, []byte("gamayun-users:x:1000:\n"), 0o644)
	r.f.fail["useradd"] = true
	if _, err := r.e.UserEnsure(ctx, UserParams{ID: "x", UID: 59010}); err == nil {
		t.Fatal("сбой useradd не замечен")
	}
	if !strings.Contains(strings.Join(r.f.commands(), "\n"), "groupdel -- app-x") {
		t.Fatal("группа не удалена после сбоя")
	}
}

func TestACLParams(t *testing.T) {
	ok := ACLParams{ID: "web", UID: 59001, Pool: "p1", Share: "Photos", Mode: "ro"}
	if ok.Validate() != nil {
		t.Fatal("верные параметры")
	}
	for _, p := range []ACLParams{
		{ID: "web", UID: 1001, Pool: "p1", Share: "Photos", Mode: "ro"},
		{ID: "web", UID: 59001, Pool: "p1", Share: ".apps", Mode: "ro"},
		{ID: "web", UID: 59001, Pool: "p1", Share: "../etc", Mode: "ro"},
		{ID: "web", UID: 59001, Pool: "p1", Share: "Photos", Mode: "rwx"},
		{ID: "web", UID: 59001, Pool: "../", Share: "Photos", Mode: "ro"},
	} {
		if p.Validate() == nil {
			t.Fatalf("принято: %+v", p)
		}
	}
}

func TestACLJob(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip()
	}
	r := accessRig(t)
	ctx := context.Background()
	dir := nas.ShareDir(r.e.MountRoot, "p1", "Photos")
	os.MkdirAll(filepath.Join(dir, "2024"), 0o770)
	for i := 0; i < 50; i++ {
		os.WriteFile(filepath.Join(dir, "2024", "f"+string(rune('a'+i%26))+string(rune('a'+i/26))), nil, 0o660)
	}
	wait := func(j ACLJob) ACLJob {
		t.Helper()
		for i := 0; i < 200; i++ {
			s, err := r.e.ACLStatus(ctx, JobParams{Job: j.Job})
			if err != nil {
				t.Fatal(err)
			}
			if s.State != JobRunning {
				return s
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("задание не завершилось")
		return j
	}
	j, err := r.e.ACLStart(ctx, ACLParams{ID: "web", UID: 59001, Pool: "p1", Share: "Photos", Mode: GrantModeRW})
	if err != nil {
		t.Fatal(err)
	}
	s := wait(j)
	if s.State == JobFailed && strings.Contains(s.Error, "not supported") {
		t.Skip("ФС без ACL")
	}
	if s.State != JobDone || s.Total != 52 || s.Done != 52 {
		t.Fatalf("%+v", s)
	}
	c, err := r.e.ACLCheck(ctx, ShareParams{Pool: "p1", Share: "Photos"})
	if err != nil || c.Users["59001"] != GrantModeRW || c.Default["59001"] != GrantModeRW || !c.GroupDefault {
		t.Fatalf("%+v %v", c, err)
	}
	j, _ = r.e.ACLStart(ctx, ACLParams{ID: "web", UID: 59001, Pool: "p1", Share: "Photos", Mode: GrantModeRevoke, DropGroupDefault: true})
	if s = wait(j); s.State != JobDone {
		t.Fatalf("%+v", s)
	}
	if c, _ = r.e.ACLCheck(ctx, ShareParams{Pool: "p1", Share: "Photos"}); len(c.Users)+len(c.Default) != 0 || c.GroupDefault {
		t.Fatalf("после отзыва: %+v", c)
	}
	// неизвестное задание, несуществующая папка, пул не смонтирован
	if _, err := r.e.ACLStatus(ctx, JobParams{Job: strings.Repeat("0", 32)}); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	if _, err := r.e.ACLStart(ctx, ACLParams{ID: "web", UID: 59001, Pool: "p1", Share: "Nope", Mode: "ro"}); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	r.f.mounted = false
	r.f.writeMountinfo()
	if _, err := r.e.ACLStart(ctx, ACLParams{ID: "web", UID: 59001, Pool: "p1", Share: "Photos", Mode: "ro"}); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
}
