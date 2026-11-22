package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/store"
)

type fixture struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newFixture() fixture {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	return fixture{pub, priv}
}

// files — подписанный каталог с одним приложением.
func (f fixture) files(t *testing.T, serial int64, mut func(map[string][]byte)) map[string][]byte {
	t.Helper()
	compose := []byte(sample(""))
	files := map[string][]byte{"apps/demo-web/compose.yaml": compose, "apps/demo-web/icon.svg": []byte("<svg/>")}
	ix := Index{Schema: 1, Serial: serial, Created: time.Unix(0, 0).UTC(), Apps: []IndexApp{{
		ID: "demo-web", Version: "1.0.0", Files: map[string]string{"compose.yaml": SHA256(compose), "icon.svg": SHA256([]byte("<svg/>"))},
	}}}
	idx, err := ix.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	files[IndexFile], files[SigFile] = idx, Sign(f.priv, idx)
	if mut != nil {
		mut(files)
	}
	return files
}

func (f fixture) archive(t *testing.T, serial int64, mut func(map[string][]byte)) []byte {
	b, err := Pack(f.files(t, serial, mut))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newStore(t *testing.T, f fixture) *Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Store{DB: db, Dir: filepath.Join(t.TempDir(), "catalog"), Trusted: []ed25519.PublicKey{f.pub}}
}

func TestStoreLoad(t *testing.T) {
	ctx := context.Background()
	f := newFixture()
	s := newStore(t, f)
	st, err := s.Load(ctx, f.archive(t, 3, nil), "file", false)
	if err != nil || st.Serial != 3 || st.Apps != 1 || st.KeyID != KeyID(f.pub) {
		t.Fatalf("load: %+v %v", st, err)
	}
	apps, err := s.Apps()
	if err != nil || len(apps) != 1 || apps[0].Manifest.ID != "demo-web" {
		t.Fatalf("apps: %v %v", apps, err)
	}
	// Тот же номер — можно (переустановка), меньший — нет, кроме явного разрешения.
	if _, err := s.Load(ctx, f.archive(t, 3, nil), "file", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, f.archive(t, 2, nil), "file", false); !errors.Is(err, ErrSerialRollback) {
		t.Fatalf("откат: %v", err)
	}
	if st, err := s.Load(ctx, f.archive(t, 2, nil), "file", true); err != nil || st.Serial != 2 {
		t.Fatalf("откат с подтверждением: %v", err)
	}
	if got, _ := s.State(ctx); got.Serial != 2 {
		t.Fatalf("state: %+v", got)
	}
}

func TestStoreRejects(t *testing.T) {
	ctx := context.Background()
	f := newFixture()
	other := newFixture()
	cases := map[string][]byte{
		"чужой ключ": other.archive(t, 1, nil),
		"подменён файл": f.archive(t, 1, func(m map[string][]byte) {
			m["apps/demo-web/icon.svg"] = []byte("<svg onload=x/>")
		}),
		"лишний файл": f.archive(t, 1, func(m map[string][]byte) { m["apps/demo-web/extra.sh"] = []byte("x") }),
		"нет подписи": f.archive(t, 1, func(m map[string][]byte) { delete(m, SigFile) }),
		"не архив":    []byte("hello"),
	}
	for name, a := range cases {
		s := newStore(t, f)
		if _, err := s.Load(ctx, a, "file", false); err == nil {
			t.Errorf("%s: принят", name)
		}
		if st, _ := s.State(ctx); st.Serial != 0 {
			t.Errorf("%s: состояние изменилось", name)
		}
	}
}

func rawTar(t *testing.T, h tar.Header, body string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	h.Size = int64(len(body))
	if err := tw.WriteHeader(&h); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(body))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestExtractUnsafe(t *testing.T) {
	cases := map[string][]byte{
		"..":      rawTar(t, tar.Header{Name: "../etc/x", Typeflag: tar.TypeReg, Mode: 0o644}, "x"),
		"абсолют": rawTar(t, tar.Header{Name: "/etc/x", Typeflag: tar.TypeReg, Mode: 0o644}, "x"),
		"symlink": rawTar(t, tar.Header{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, ""),
		"скрытый": rawTar(t, tar.Header{Name: "apps/.x", Typeflag: tar.TypeReg, Mode: 0o644}, "x"),
	}
	for name, a := range cases {
		if _, err := Extract(bytes.NewReader(a)); err == nil {
			t.Errorf("%s: принят", name)
		}
	}
}

func TestStoreFetch(t *testing.T) {
	ctx := context.Background()
	f := newFixture()
	arch := f.archive(t, 7, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/catalog.tar.gz" {
			http.NotFound(w, r)
			return
		}
		w.Write(arch)
	}))
	defer srv.Close()
	s := newStore(t, f)
	if _, err := s.Fetch(ctx, srv.URL+"/catalog.tar.gz"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("http без разрешения: %v", err)
	}
	s.AllowHTTP = true
	if st, err := s.Fetch(ctx, srv.URL+"/catalog.tar.gz"); err != nil || st.Serial != 7 {
		t.Fatalf("fetch: %+v %v", st, err)
	}
	if _, err := s.Fetch(ctx, srv.URL+"/missing"); err == nil {
		t.Fatal("404 принят")
	}
}
