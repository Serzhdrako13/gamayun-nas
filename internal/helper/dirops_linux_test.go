package helper

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gamayun-nas/internal/nas"
)

// TestShareDirMutateOps — nas.share.mkdir/rename/rmdir через реестр helper
// на временном каталоге. chown подменён: без root fchown(0, gid)
// невозможен; путь по умолчанию проверяется в internal/nas.
func TestShareDirMutateOps(t *testing.T) {
	d := fakeDeps(t)
	mount := t.TempDir()
	chowns := 0
	d.NAS = &nas.Executor{
		MountRoot: mount,
		Mounted:   func(string) bool { return true },
		DirChown:  func(int) error { chowns++; return nil },
	}
	share := nas.ShareDir(mount, "p1", "docs")
	for _, dir := range []string{"a/full", "z"} {
		if err := os.MkdirAll(filepath.Join(share, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(share, "a/full/f"), []byte("x"), 0o644)
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(share, "escape"))
	os.Symlink("a", filepath.Join(share, "inner"))
	os.Symlink("z", filepath.Join(share, "zlink"))
	r := DefaultRegistry(d)
	for _, op := range []string{nas.OpShareMkdir, nas.OpShareRename, nas.OpShareRmdir, nas.OpShareMove} {
		if !r.IsMutating(op) {
			t.Errorf("%s не помечена изменяющей", op)
		}
	}
	exec := func(op, params string) Response {
		t.Helper()
		return r.Execute(context.Background(), op, json.RawMessage(params))
	}

	// Успех: mkdir → rename → rmdir.
	if resp := exec(nas.OpShareMkdir, `{"pool":"p1","share":"docs","path":"a","name":"Новая"}`); !resp.OK || string(resp.Result) != `{"path":"a/Новая","changed":true}` {
		t.Fatalf("mkdir: %+v %s", resp, resp.Result)
	}
	if st, err := os.Lstat(filepath.Join(share, "a/Новая")); err != nil || st.Mode() != os.ModeDir|nas.ShareDirMode || chowns != 1 {
		t.Fatalf("mkdir: %v %v chowns=%d", st, err, chowns)
	}
	if resp := exec(nas.OpShareRename, `{"pool":"p1","share":"docs","path":"a/Новая","name":"Готово"}`); !resp.OK || string(resp.Result) != `{"path":"a/Готово","changed":true}` {
		t.Fatalf("rename: %+v %s", resp, resp.Result)
	}
	if resp := exec(nas.OpShareRmdir, `{"pool":"p1","share":"docs","path":"a/Готово"}`); !resp.OK || string(resp.Result) != `{"path":"a/Готово","changed":true}` {
		t.Fatalf("rmdir: %+v %s", resp, resp.Result)
	}
	if _, err := os.Lstat(filepath.Join(share, "a/Готово")); !os.IsNotExist(err) {
		t.Fatal("каталог не удалён")
	}

	// Перенос z → a/z и обратно в корень.
	if resp := exec(nas.OpShareMove, `{"pool":"p1","share":"docs","path":"z","to":"a"}`); !resp.OK || string(resp.Result) != `{"path":"a/z","changed":true}` {
		t.Fatalf("move: %+v %s", resp, resp.Result)
	}
	if resp := exec(nas.OpShareMove, `{"pool":"p1","share":"docs","path":"a/z","to":""}`); !resp.OK || string(resp.Result) != `{"path":"z","changed":true}` {
		t.Fatalf("move обратно: %+v %s", resp, resp.Result)
	}

	for _, c := range []struct{ op, params, code string }{
		// Параметры.
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","path":"..","name":"x"}`, CodeInvalidParams},
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","path":"a/../..","name":"x"}`, CodeInvalidParams},
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","path":"/etc","name":"x"}`, CodeInvalidParams},
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","name":".."}`, CodeInvalidParams},
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","name":"a/b"}`, CodeInvalidParams},
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","name":""}`, CodeInvalidParams},
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","name":"x","extra":1}`, CodeInvalidParams},
		{nas.OpShareMkdir, `{"pool":"../p1","share":"docs","name":"x"}`, CodeInvalidParams},
		{nas.OpShareRename, `{"pool":"p1","share":"docs","path":"","name":"x"}`, CodeInvalidParams},
		{nas.OpShareRename, `{"pool":"p1","share":"docs","path":"z","name":"../x"}`, CodeInvalidParams},
		// Windows-несовместимые имена — отказ и в helper (последний рубеж).
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","path":"a","name":"a:b"}`, CodeInvalidParams},
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","name":"CON"}`, CodeInvalidParams},
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","name":"nul.txt"}`, CodeInvalidParams},
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","name":"x."}`, CodeInvalidParams},
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","name":"x "}`, CodeInvalidParams},
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","name":"a*"}`, CodeInvalidParams},
		{nas.OpShareRename, `{"pool":"p1","share":"docs","path":"z","name":"Com7"}`, CodeInvalidParams},
		{nas.OpShareRename, `{"pool":"p1","share":"docs","path":"z","name":"a|b"}`, CodeInvalidParams},
		{nas.OpShareRmdir, `{"pool":"p1","share":"docs","path":""}`, CodeInvalidParams},
		{nas.OpShareRmdir, `{"pool":"p1","share":"docs"}`, CodeInvalidParams},
		{nas.OpShareRmdir, `{"pool":"p1","share":"../docs","path":"z"}`, CodeInvalidParams},
		// Перенос — в себя/подкаталог, корень, «..» в цели.
		{nas.OpShareMove, `{"pool":"p1","share":"docs","path":"a","to":"a/full"}`, CodeInvalidParams},
		{nas.OpShareMove, `{"pool":"p1","share":"docs","path":"","to":"z"}`, CodeInvalidParams},
		{nas.OpShareMove, `{"pool":"p1","share":"docs","path":"z","to":".."}`, CodeInvalidParams},
		{nas.OpShareMove, `{"pool":"p1","share":"docs","path":"z","to":"escape"}`, CodeRefused},
		{nas.OpShareMove, `{"pool":"p1","share":"docs","path":"z","to":"inner"}`, CodeRefused},
		{nas.OpShareMove, `{"pool":"p1","share":"docs","path":"escape","to":"z"}`, CodeRefused},
		{nas.OpShareMove, `{"pool":"p1","share":"docs","path":"z","to":"nope"}`, CodeNotFound},
		// Ссылки в пути и цель-ссылка.
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","path":"escape","name":"x"}`, CodeRefused},
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","path":"inner/full","name":"x"}`, CodeRefused},
		{nas.OpShareRename, `{"pool":"p1","share":"docs","path":"zlink","name":"x"}`, CodeRefused},
		{nas.OpShareRename, `{"pool":"p1","share":"docs","path":"escape","name":"x"}`, CodeRefused},
		{nas.OpShareRmdir, `{"pool":"p1","share":"docs","path":"zlink"}`, CodeRefused},
		{nas.OpShareRmdir, `{"pool":"p1","share":"docs","path":"escape"}`, CodeRefused},
		// Занято, не пусто, нет.
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","path":"a","name":"full"}`, CodeConflict},
		{nas.OpShareRename, `{"pool":"p1","share":"docs","path":"z","name":"a"}`, CodeConflict},
		{nas.OpShareRmdir, `{"pool":"p1","share":"docs","path":"a/full"}`, CodeConflict},
		{nas.OpShareRmdir, `{"pool":"p1","share":"docs","path":"nope"}`, CodeNotFound},
		{nas.OpShareRename, `{"pool":"p1","share":"docs","path":"nope","name":"x"}`, CodeNotFound},
		{nas.OpShareMkdir, `{"pool":"p1","share":"docs","path":"nope","name":"x"}`, CodeNotFound},
	} {
		resp := exec(c.op, c.params)
		if resp.OK || resp.Error.Code != c.code {
			t.Errorf("%s %s: %+v, ожидался %s", c.op, c.params, resp.Error, c.code)
		}
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("вне шары появились записи: %v", ents)
	}
	for _, p := range []string{"a/full/f", "z", "zlink", "escape"} {
		if _, err := os.Lstat(filepath.Join(share, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if chowns != 1 {
		t.Errorf("chown вызван %d раз", chowns)
	}
}

// Коды ошибок nas → протокол helper: API различает 404/409.
func TestRegistryErrorCodesNAS(t *testing.T) {
	r := NewRegistry(
		Define("t.notfound", time.Second, func(context.Context, NoParams) (any, error) {
			return nil, fmt.Errorf("%w: каталог a", nas.ErrNotFound)
		}),
		Define("t.conflict", time.Second, func(context.Context, NoParams) (any, error) {
			return nil, fmt.Errorf("%w: a уже существует", nas.ErrConflict)
		}),
	)
	for op, code := range map[string]string{"t.notfound": CodeNotFound, "t.conflict": CodeConflict} {
		if resp := r.Execute(context.Background(), op, nil); resp.Error == nil || resp.Error.Code != code {
			t.Errorf("%s: %+v", op, resp.Error)
		}
	}
}
