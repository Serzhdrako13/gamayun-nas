package helper

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type fdParams struct {
	Fail bool `json:"fail"`
}

func (fdParams) Validate() error { return nil }

// Файл передаётся вместе с ответом (SCM_RIGHTS); ошибка — без файла.
func TestCallFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "f")
	os.WriteFile(src, []byte("содержимое"), 0o600)
	reg := NewRegistry(
		Define("test.open", 5e9, func(_ context.Context, p fdParams) (any, error) {
			if p.Fail {
				return nil, errors.New("не вышло")
			}
			f, err := os.Open(src)
			if err != nil {
				return nil, err
			}
			return WithFile{Result: map[string]int{"size": 20}, File: f}, nil
		}),
		Define("test.plain", 5e9, func(context.Context, fdParams) (any, error) { return "ok", nil }),
	)
	ts := startServer(t, &Server{Registry: reg, AllowedUID: 7, PeerCred: fakePeer(7)})
	c := &Client{Socket: ts.sock}
	ctx := context.Background()
	var res struct{ Size int }
	for i := 0; i < 3; i++ { // повтор: дескрипторы не текут и не путаются
		f, err := c.CallFile(ctx, "test.open", fdParams{}, &res)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(f)
		f.Close()
		if string(b) != "содержимое" || res.Size != 20 {
			t.Fatalf("получено %q %+v", b, res)
		}
	}
	if f, err := c.CallFile(ctx, "test.open", fdParams{Fail: true}, nil); err == nil || f != nil {
		t.Fatalf("ошибка операции: %v %v", f, err)
	}
	var re *RemoteError
	if _, err := c.CallFile(ctx, "test.plain", nil, nil); err == nil || errors.As(err, &re) {
		t.Fatalf("ответ без файла: %v", err)
	}
	// Обычный Call к операции с файлом: результат приходит, файл закрывается.
	if err := c.Call(ctx, "test.open", fdParams{}, &res); err != nil || res.Size != 20 {
		t.Fatalf("Call: %+v %v", res, err)
	}
}
