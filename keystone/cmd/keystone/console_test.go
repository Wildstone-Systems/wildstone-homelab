package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	// nil: no trust boundary to enforce, which is the posture of a
	// household with no proxy in front and what this test is about.
	srv := httptest.NewServer(withConsole(apiHandler, dir, nil))
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

// The Console's pages are refused to a peer that is not the identity
// proxy, once there is one.
//
// packages/wildstone-console states the requirement beside the Route it
// renders: "the listener this points at must not also be reachable
// directly. A trusted header on an open port is not authentication."
// Nothing enforced it, so a household that put the Console behind
// Authelia still served the entire Console — sign-in page, bundle and
// all — to anything that could reach Keystone's port, where the way in
// is a bearer token and the proxy is never consulted.
//
// The API is deliberately NOT part of this claim, and the test says so
// rather than leaving it to be inferred from silence: `wild` is an API
// client, the installer drives the same endpoints, and recovery from a
// broken proxy needs them. A gate that closed /api too would turn a
// misconfigured ingress into a house nobody can administer.
func TestTheConsolePagesAreRefusedToAnyoneButTheProxy(t *testing.T) {
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

	// The proxy is whoever sets this header; the real predicate asks
	// whether the peer address is the configured ingress, which a
	// single-process test cannot vary.
	viaProxy := func(r *http.Request) bool { return r.Header.Get("X-Test-Via-Proxy") == "yes" }
	srv := httptest.NewServer(withConsole(apiHandler, dir, viaProxy))
	defer srv.Close()

	get := func(p string, proxied bool) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+p, nil)
		if err != nil {
			t.Fatal(err)
		}
		if proxied {
			req.Header.Set("X-Test-Via-Proxy", "yes")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		buf := make([]byte, 256)
		n, _ := res.Body.Read(buf)
		return res.StatusCode, string(buf[:n])
	}

	// Directly: every page shape is refused, the bundle included. An
	// asset is checked by name because gating only index.html would
	// still hand out the Console itself.
	for _, p := range []string{"/", "/plans", "/deep/client/route", "/assets/app.js"} {
		code, body := get(p, false)
		if code != http.StatusForbidden {
			t.Errorf("direct GET %s = %d, want 403; body %q", p, code, body)
		}
		if strings.Contains(body, "console") || strings.Contains(body, "js-content") {
			t.Errorf("direct GET %s served the Console anyway: %q", p, body)
		}
	}

	// Through the proxy: unchanged.
	if code, body := get("/", true); code != http.StatusOK || body != "<html>console</html>" {
		t.Errorf("proxied GET / = %d %q, want the Console", code, body)
	}
	if code, body := get("/assets/app.js", true); code != http.StatusOK || body != "js-content" {
		t.Errorf("proxied GET asset = %d %q", code, body)
	}

	// And the API answers either way, because recovery depends on it.
	if code, body := get("/api/v1/healthz", false); code != http.StatusOK || body != "api-response" {
		t.Errorf("the gate closed /api to a direct caller (%d %q); "+
			"`wild`, the installer and post-lockout recovery all need it", code, body)
	}
}
