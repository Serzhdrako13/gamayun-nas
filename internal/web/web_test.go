package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandler(t *testing.T) {
	h := handler(fstest.MapFS{
		"index.html":    {Data: []byte("<html>app</html>")},
		"assets/a-1.js": {Data: []byte("js")},
	})
	get := func(method, p string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, p, nil))
		return w
	}
	if w := get("GET", "/"); w.Code != 200 || !strings.Contains(w.Body.String(), "app") {
		t.Fatalf("/: %d %q", w.Code, w.Body.String())
	}
	if w := get("GET", "/storage/pools"); w.Code != 200 || !strings.Contains(w.Body.String(), "app") {
		t.Fatalf("SPA-маршрут: %d", w.Code)
	}
	w := get("GET", "/assets/a-1.js")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %v", w.Code, w.Header())
	}
	if w := get("GET", "/assets/none.js"); w.Code != 404 {
		t.Fatalf("нет файла: %d", w.Code)
	}
	if w := get("POST", "/"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", w.Code)
	}
	if w := get("GET", "/../../etc/passwd"); w.Code != 200 && w.Code != 404 {
		t.Fatalf("traversal: %d", w.Code)
	}
}
