package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestWithConsole pins the ADR-W016 A5 mount: /api/* stays the API's,
// real files serve as themselves, and unknown paths fall back to
// index.html (the SPA convention) — including "../" shapes, which the
// stdlib's path cleaning keeps inside dir.
func TestWithConsole(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>console</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("js-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "api-response")
	})
	srv := httptest.NewServer(withConsole(apiHandler, dir))
	defer srv.Close()

	get := func(p string) string {
		t.Helper()
		res, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		buf := make([]byte, 256)
		n, _ := res.Body.Read(buf)
		return string(buf[:n])
	}

	if got := get("/api/v1/healthz"); got != "api-response" {
		t.Fatalf("/api/* = %q, want the API handler", got)
	}
	if got := get("/assets/app.js"); got != "js-content" {
		t.Fatalf("asset = %q", got)
	}
	for _, p := range []string{"/", "/plans", "/deep/client/route"} {
		if got := get(p); got != "<html>console</html>" {
			t.Fatalf("%s = %q, want the SPA index fallback", p, got)
		}
	}
}
