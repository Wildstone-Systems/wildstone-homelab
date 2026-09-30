package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func baseEnv(api string) map[string]string {
	return map[string]string{
		"KEYSTONE_API": api, "KEYSTONE_TOKEN": "REPLACED-token",
		"WILD_CONNECTION": "ada-mailbox", "WILD_NODE": "node-a",
		"WILD_PACKAGE": "mail-roundcube", "WILD_PACKAGE_VERSION": "1.1.0", "WILD_ADAPTER_CONTRACT": "1",
		"WILD_PARAM_IMAP_HOST": "imap.example.internal", "WILD_PARAM_IMAP_PORT": "993",
		"WILD_PARAM_IMAP_ENCRYPTION": "ssl", "WILD_PARAM_USERNAME_DOMAIN": "example.internal",
	}
}

func TestLoadConfigFailsClosed(t *testing.T) {
	for _, tc := range []struct{ key, value, want string }{
		{"KEYSTONE_TOKEN", "", "are required"},
		{"WILD_CONNECTION", "", "WILD_CONNECTION is required"},
		{"WILD_ADAPTER_CONTRACT", "", "must name the adapter contract"},
		// A rendering from a newer Keystone than this program: refused.
		{"WILD_ADAPTER_CONTRACT", "2", "understands 1"},
		{"WILD_PARAM_IMAP_ENCRYPTION", "none", "ssl or tls"},
	} {
		e := baseEnv("http://192.0.2.10:9441")
		e[tc.key] = tc.value
		if _, err := loadConfig(env(e)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s=%q: err = %v, want %q", tc.key, tc.value, err, tc.want)
		}
	}
	c, err := loadConfig(env(baseEnv("http://192.0.2.10:9441")))
	if err != nil {
		t.Fatal(err)
	}
	if c.Settings.IMAPHost != "imap.example.internal" || c.Settings.PackageVersion != "1.1.0" {
		t.Fatalf("settings = %+v", c.Settings)
	}
	if _, ok := readerFor(c).(mail.NotConfigured); !ok {
		t.Fatal("no credential, yet a reader was chosen")
	}
	e := baseEnv("x")
	e["WILD_MAILBOX_USERNAME"], e["WILD_SECRET_MAILBOX_APP_PASSWORD"] = "ada", "REPLACED-app-password"
	c, _ = loadConfig(env(e))
	if !c.Credentials.Present() {
		t.Fatal("credential not read")
	}
}

// fakeKeystone hands out a fixed queue of work and records results.
type fakeKeystone struct {
	mu      sync.Mutex
	queue   []adapter.Work
	results []map[string]any
	auth    []string
}

func (f *fakeKeystone) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	switch r.URL.Path {
	case "/api/v1/integration-work/claim":
		var work *adapter.Work
		if len(f.queue) > 0 {
			work, f.queue = &f.queue[0], f.queue[1:]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"work": work})
	case "/api/v1/integration-work/result":
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		f.results = append(f.results, m)
		_, _ = w.Write([]byte(`{}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// The program, end to end against a fake control plane: every read is
// answered not_configured, a mutating request is refused, and nothing is
// ever reported as an empty mailbox.
func TestRunAnswersNotConfiguredAndRefusesWrites(t *testing.T) {
	fk := &fakeKeystone{queue: []adapter.Work{
		{Seq: 1, Connection: "ada-mailbox", Kind: "query", Capability: "mail.changes", RequestJSON: json.RawMessage(`{}`), Epoch: 1},
		{Seq: 2, Connection: "ada-mailbox", Kind: "query", Capability: "mail.capabilities", Epoch: 1},
		{Seq: 3, Connection: "ada-mailbox", Kind: "action", Capability: "mail.send", RequestJSON: json.RawMessage(`{"to":"x"}`), Epoch: 1},
		{Seq: 4, Connection: "ada-mailbox", Kind: "query", Capability: "mail.move", Epoch: 1},
	}}
	srv := httptest.NewServer(fk)
	defer srv.Close()
	e := baseEnv(srv.URL)
	e["WILD_HEALTH_ADDR"] = "127.0.0.1:0"
	c, err := loadConfig(env(e))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, c, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		fk.mu.Lock()
		n := len(fk.results)
		fk.mu.Unlock()
		if n == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d results", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run = %v; a signal is a clean stop", err)
	}
	for _, res := range fk.results {
		if res["resultType"] != "unavailable" {
			t.Errorf("result %v: want unavailable", res)
		}
	}
	for _, i := range []int{0, 1} {
		if !strings.Contains(fk.results[i]["error"].(string), mail.OutcomeNotConfigured) {
			t.Errorf("read %v does not say not_configured", fk.results[i])
		}
	}
	for _, i := range []int{2, 3} {
		if !strings.Contains(fk.results[i]["error"].(string), "does not implement") {
			t.Errorf("write %v was not refused as unimplemented", fk.results[i])
		}
	}
	for _, a := range fk.auth {
		if a != "Bearer REPLACED-token" {
			t.Fatalf("authorization = %q", a)
		}
	}
}

func TestHealthIsReadyOnlyAfterKeystoneAnswers(t *testing.T) {
	c, _ := loadConfig(env(baseEnv("http://192.0.2.10:9441")))
	r := &adapter.Runner{}
	rec := httptest.NewRecorder()
	healthHandler(r, c, time.Minute).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"configured":false`) {
		t.Fatalf("health before contact = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "REPLACED") {
		t.Fatal("health leaked the token")
	}
}
