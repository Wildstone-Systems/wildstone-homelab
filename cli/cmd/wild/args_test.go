package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Go's flag package "stops parsing just before the first non-flag
// argument", so a subcommand that takes a positional MUST split it off
// before parsing. Getting this wrong is not a usage nit: `wild butler
// ask "..." -surface family` silently swallowed the flag into the
// question and asked the OPERATOR butler, which is the one mistake
// ADR-W026 A1 exists to make impossible.
//
// These run the real subcommands against a recording server, so they
// assert on the request that would actually be sent.
type recorder struct {
	mu     sync.Mutex
	paths  []string
	bodies []string
}

func (r *recorder) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.paths = append(r.paths, req.URL.Path)
		r.bodies = append(r.bodies, string(body))
		r.mu.Unlock()
		w.Write([]byte(`{}`)) //nolint:errcheck // test fake
	}))
	t.Cleanup(srv.Close)
	t.Setenv("WILD_TOKEN", "wst-test")
	return srv
}

// The question goes to the surface the FLAG names, not to whichever
// endpoint a parsing accident leaves as the default.
func TestButlerAskSendsToTheNamedSurface(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantURL string
	}{
		{"family surface", []string{"ask", "did anyone have breakfast?", "-surface", "family"},
			"/api/v1/household/butler/ask"},
		{"admin by default", []string{"ask", "why is jellyfin unhealthy?"},
			"/api/v1/butler/ask"},
		{"admin named", []string{"ask", "why is jellyfin unhealthy?", "-surface", "admin"},
			"/api/v1/butler/ask"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			srv := rec.start(t)
			if code := runButler(append(tc.args, "-api", srv.URL)); code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if len(rec.paths) != 1 || rec.paths[0] != tc.wantURL {
				t.Fatalf("posted to %v, want %s", rec.paths, tc.wantURL)
			}
			// And the flags did not end up inside the question.
			if strings.Contains(rec.bodies[0], "-surface") || strings.Contains(rec.bodies[0], "-api") {
				t.Fatalf("a flag was swallowed into the question: %s", rec.bodies[0])
			}
		})
	}
}

// An unquoted question with a flag after it is refused rather than sent
// to the wrong butler: the flag was never parsed, and guessing which
// surface was meant is exactly the wrong move.
func TestButlerAskRefusesAFlagAfterUnquotedWords(t *testing.T) {
	rec := &recorder{}
	srv := rec.start(t)
	code := runButler([]string{"ask", "why", "is", "jellyfin", "unhealthy", "-surface", "family", "-api", srv.URL})
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (refused)", code)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.paths) != 0 {
		t.Fatalf("a request was sent anyway: %v", rec.paths)
	}
}

// Unquoted words with no trailing flag still make a question.
func TestButlerAskJoinsUnquotedWords(t *testing.T) {
	rec := &recorder{}
	srv := rec.start(t)
	if code := runButler([]string{"ask", "why is jellyfin unhealthy", "-api", srv.URL}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !strings.Contains(rec.bodies[0], "why is jellyfin unhealthy") {
		t.Fatalf("question = %s", rec.bodies[0])
	}
}

// The documented install command — `wild chat adapter <capability>
// -node <node> -surface family` — has to actually mint the credential
// rather than print usage. It is in install.md and in all four chat
// packages.
func TestChatAdapterParsesTheDocumentedCommand(t *testing.T) {
	rec := &recorder{}
	srv := rec.start(t)
	code := runChat([]string{"adapter", "chat-telegram-family", "-node", "n1", "-surface", "family", "-api", srv.URL})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.paths) != 1 || rec.paths[0] != "/api/v1/chat/adapters/chat-telegram-family" {
		t.Fatalf("posted to %v", rec.paths)
	}
	if !strings.Contains(rec.bodies[0], `"surface":"family"`) || !strings.Contains(rec.bodies[0], `"node":"n1"`) {
		t.Fatalf("body = %s, want the parsed flags", rec.bodies[0])
	}
}

// A missing or misspelled surface is refused before anything is sent.
func TestChatAdapterRefusesAnUnknownSurface(t *testing.T) {
	rec := &recorder{}
	srv := rec.start(t)
	for _, args := range [][]string{
		{"adapter", "chat-telegram-family", "-node", "n1", "-surface", "household", "-api", srv.URL},
		{"adapter", "chat-telegram-family", "-surface", "family", "-api", srv.URL}, // no node
		{"adapter", "-node", "n1", "-surface", "family", "-api", srv.URL},          // no capability
	} {
		if code := runChat(args); code != 2 {
			t.Fatalf("%v: exit = %d, want 2", args, code)
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.paths) != 0 {
		t.Fatalf("a bad command still sent something: %v", rec.paths)
	}
}

// `wild chat revoke <id> -api URL` reaches the id it was given, on the
// host it was given.
func TestChatRevokeParsesPositionalThenFlags(t *testing.T) {
	rec := &recorder{}
	srv := rec.start(t)
	if code := runChat([]string{"revoke", "7", "-api", srv.URL}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.paths) != 1 || rec.paths[0] != "/api/v1/chat/bindings/7/revoke" {
		t.Fatalf("posted to %v", rec.paths)
	}
}
