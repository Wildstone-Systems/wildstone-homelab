package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/mail"
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

// fakeKeystone hands out a queue of work and records results. Every
// field is guarded by mu: the runner's HTTP calls land on server
// goroutines, including a claim still in flight when the test cancels
// the run, so the test reads only through snapshot, after the server
// has been closed.
type fakeKeystone struct {
	mu      sync.Mutex
	queue   []adapter.Work
	results []map[string]any
	auth    []string
}

func (f *fakeKeystone) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	switch r.URL.Path {
	case "/api/v1/integration-work/claim":
		var work *adapter.Work
		if len(f.queue) > 0 {
			work, f.queue = &f.queue[0], f.queue[1:]
		}
		f.mu.Unlock()
		if work == nil {
			// A short long-poll, so an idle runner is not a busy loop.
			time.Sleep(10 * time.Millisecond)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"work": work})
		return
	case "/api/v1/integration-work/result":
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		f.results = append(f.results, m)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
		return
	}
	f.mu.Unlock()
	w.WriteHeader(http.StatusNotFound)
}

func (f *fakeKeystone) push(work ...adapter.Work) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, work...)
}

// snapshot copies what was recorded.
func (f *fakeKeystone) snapshot() (results []map[string]any, auth []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.results...), append([]string(nil), f.auth...)
}

// waitResults waits until n results have been reported and returns them.
func (f *fakeKeystone) waitResults(t *testing.T, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		results, _ := f.snapshot()
		if len(results) >= n {
			return results
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d results: %v", len(results), n, results)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startRun runs the program against fk until the returned stop is
// called; stop waits for a clean exit and closes the control plane, so
// no handler is still running when the test reads what it recorded.
// setEnv, when set, adjusts the rendered environment; roots, when set,
// is the trust pool the IMAP reader validates against.
func startRun(t *testing.T, fk *fakeKeystone, setEnv func(map[string]string), roots *x509.CertPool) (stop func()) {
	t.Helper()
	srv := httptest.NewServer(fk)
	e := baseEnv(srv.URL)
	e["WILD_HEALTH_ADDR"] = "127.0.0.1:0"
	if setEnv != nil {
		setEnv(e)
	}
	c, err := loadConfig(env(e))
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	c.RootCAs = roots
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, c, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("run = %v; a signal is a clean stop", err)
			}
			srv.Close()
		})
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
	stop := startRun(t, fk, nil, nil)
	fk.waitResults(t, 4)
	stop()
	results, auth := fk.snapshot()
	if len(results) != 4 {
		t.Fatalf("results = %v", results)
	}
	for _, res := range results {
		if res["resultType"] != "unavailable" {
			t.Errorf("result %v: want unavailable", res)
		}
	}
	for _, i := range []int{0, 1} {
		if !strings.Contains(results[i]["error"].(string), mail.OutcomeNotConfigured) {
			t.Errorf("read %v does not say not_configured", results[i])
		}
	}
	for _, i := range []int{2, 3} {
		if !strings.Contains(results[i]["error"].(string), "does not implement") {
			t.Errorf("write %v was not refused as unimplemented", results[i])
		}
	}
	for _, a := range auth {
		if a != "Bearer REPLACED-token" {
			t.Fatalf("authorization = %q", a)
		}
	}
}

func TestHealthIsReadyOnlyAfterKeystoneAnswers(t *testing.T) {
	c, _ := loadConfig(env(baseEnv("http://192.0.2.10:9441")))
	r := &adapter.Runner{}
	rec := httptest.NewRecorder()
	healthHandler(r, c, false, time.Minute).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"configured":false`) {
		t.Fatalf("health before contact = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "REPLACED") {
		t.Fatal("health leaked the token")
	}
}

// readerFor mounts the IMAP reader only with a credential AND a
// connection key, and a scope it fully understands.
func TestReaderForNeedsCredentialKeyAndScope(t *testing.T) {
	e := baseEnv("http://192.0.2.10:9441")
	e["WILD_MAILBOX_USERNAME"], e["WILD_SECRET_MAILBOX_APP_PASSWORD"] = "ada", "REPLACED-app-password"
	c, err := loadConfig(env(e))
	if err != nil {
		t.Fatal(err)
	}
	if nc, ok := readerFor(c).(mail.NotConfigured); !ok || !strings.Contains(nc.Reason, "fingerprint key") {
		t.Fatalf("no key, reader = %#v", readerFor(c))
	}
	e["WILD_SECRET_MAILBOX_FINGERPRINT_KEY"] = "too-short"
	c, _ = loadConfig(env(e))
	if _, ok := readerFor(c).(mail.NotConfigured); !ok {
		t.Fatal("a short key was accepted")
	}
	e["WILD_SECRET_MAILBOX_FINGERPRINT_KEY"] = strings.Repeat("k", 32)
	c, _ = loadConfig(env(e))
	if _, ok := readerFor(c).(mail.NotConfigured); ok {
		t.Fatalf("configured connection not mounted: %#v", readerFor(c))
	}
	for _, bad := range []string{`{"folders":["INBOX"],"store":true}`, `{"folders":["*"]}`, `{"folders":[]} {}`, `nope`} {
		e["WILD_MAILBOX_FILTERS"] = bad
		if _, err := loadConfig(env(e)); err == nil {
			t.Errorf("filters %s accepted", bad)
		}
	}
	secret := []byte(strings.Repeat("k", 32))
	if string(connectionKey(secret, "ada-mailbox")) == string(connectionKey(secret, "grace-mailbox")) {
		t.Fatal("one secret gave two connections the same key")
	}
}
