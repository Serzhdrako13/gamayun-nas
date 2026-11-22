//go:build linux

package nas

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Перенос каталога между каталогами одной шары.
func TestShareMove(t *testing.T) {
	e, share, outside, _ := dirOpsFixture(t)
	ctx := context.Background()
	mv := func(p ShareMoveParams) (ShareDirOpResult, error) {
		p.Pool, p.Share = "p1", "docs"
		return e.ShareMove(ctx, p)
	}
	before := ino(t, filepath.Join(share, "a/b"))
	if res, err := mv(ShareMoveParams{Path: "a/b", To: "z"}); err != nil || res.Path != "z/b" || !res.Changed {
		t.Fatalf("в z: %+v %v", res, err)
	}
	if ino(t, filepath.Join(share, "z/b")) != before {
		t.Fatal("перенесён не тот каталог")
	}
	if _, err := os.Lstat(filepath.Join(share, "a/b")); !os.IsNotExist(err) {
		t.Fatal("источник остался")
	}
	if res, err := mv(ShareMoveParams{Path: "z/b", To: ""}); err != nil || res.Path != "b" || !res.Changed {
		t.Fatalf("в корень: %+v %v", res, err)
	}
	if res, err := mv(ShareMoveParams{Path: "b", To: ""}); err != nil || res.Changed || res.Path != "b" {
		t.Fatalf("тот же родитель: %+v %v", res, err)
	}
	// Существующее несовместимое с Windows имя переносится как есть.
	if err := os.Mkdir(filepath.Join(share, "CON"), 0o755); err != nil {
		t.Fatal(err)
	}
	if res, err := mv(ShareMoveParams{Path: "CON", To: "b"}); err != nil || res.Path != "b/CON" {
		t.Fatalf("CON: %+v %v", res, err)
	}

	if err := os.MkdirAll(filepath.Join(share, "a/x/full"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		p    ShareMoveParams
		want error
	}{
		{"имя занято в цели", ShareMoveParams{Path: "a/full", To: "a/x"}, ErrConflict},
		{"в себя", ShareMoveParams{Path: "a", To: "a"}, ErrInvalid},
		{"в подкаталог", ShareMoveParams{Path: "a", To: "a/x"}, ErrInvalid},
		{"в подкаталог глубже", ShareMoveParams{Path: "a", To: "a/x/full"}, ErrInvalid},
		{"корень шары", ShareMoveParams{Path: "", To: "z"}, ErrInvalid},
		{".. в цели", ShareMoveParams{Path: "z", To: "a/../.."}, ErrInvalid},
		{"абсолютная цель", ShareMoveParams{Path: "z", To: outside}, ErrInvalid},
		{"абсолютный источник", ShareMoveParams{Path: outside + "/secret", To: "z"}, ErrInvalid},
		{"источника нет", ShareMoveParams{Path: "a/nope", To: "z"}, ErrNotFound},
		{"цели нет", ShareMoveParams{Path: "z", To: "nope"}, ErrNotFound},
		{"цель — ссылка наружу", ShareMoveParams{Path: "z", To: "escape"}, ErrRefused},
		{"цель через ссылку внутрь", ShareMoveParams{Path: "z", To: "inner"}, ErrRefused},
		{"цель через «..»-ссылку", ShareMoveParams{Path: "z", To: "up"}, ErrRefused},
		{"цель — файл", ShareMoveParams{Path: "z", To: "file.txt"}, ErrRefused},
		{"источник — ссылка", ShareMoveParams{Path: "a/tolink", To: "z"}, ErrRefused},
		{"источник — ссылка наружу", ShareMoveParams{Path: "escape", To: "z"}, ErrRefused},
		{"источник за ссылкой наружу", ShareMoveParams{Path: "escape/secret", To: "z"}, ErrRefused},
		{"источник — файл", ShareMoveParams{Path: "file.txt", To: "z"}, ErrRefused},
	}
	for _, c := range cases {
		if _, err := mv(c.p); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, ожидалось %v", c.name, err, c.want)
		}
	}
	for _, p := range []string{"a/full/f", "a/x/full", "z", "file.txt", "a/tolink", "b/CON"} {
		if _, err := os.Lstat(filepath.Join(share, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if fi, _ := os.Lstat(filepath.Join(share, "escape")); fi == nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("ссылка escape затронута")
	}
	ents, _ := os.ReadDir(outside)
	if len(ents) != 1 || ents[0].Name() != "secret" {
		t.Errorf("вне шары изменилось: %v", ents)
	}
}

func TestShareMoveParamsValidate(t *testing.T) {
	ok := []ShareMoveParams{{Path: "a", To: ""}, {Path: "a/b", To: "ab"}, {Path: "a", To: "ab/a"}, {Path: "x y/Фото", To: "c:d"}}
	for _, p := range ok {
		p.Pool, p.Share = "p1", "docs"
		if err := p.Validate(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	bad := []ShareMoveParams{{Path: "", To: "a"}, {Path: "a", To: "a"}, {Path: "a", To: "a/b"}, {Path: "a/b", To: "a/b/c/d"},
		{Path: "../a", To: ""}, {Path: "a", To: "/etc"}, {Path: "a", To: "b/"}, {Path: "a", To: "b/../c"}}
	for _, p := range bad {
		p.Pool, p.Share = "p1", "docs"
		if err := p.Validate(); err == nil {
			t.Errorf("%+v принято", p)
		}
	}
	if err := (ShareMoveParams{Pool: "../p", Share: "docs", Path: "a"}).Validate(); err == nil {
		t.Error("пул принят")
	}
}
