package main

import (
	"encoding/json"

	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBootServesBothPlanes is the E2/K6 boot test: one DataDir, both
// planes up, first-start bootstrap token printed once and usable against
// the public API, and the out-of-band ingestion loop noticing a real git
// edit — the whole ADR-W007 wiring through the actual serve path.
func TestBootServesBothPlanes(t *testing.T) {
	dataDir := t.TempDir()
	gwLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	apiLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var bootstrap strings.Builder
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	go func() {
		if err := serve(gwLis, apiLis, "", "localhost,127.0.0.1", dataDir, 100*time.Millisecond, 200*time.Millisecond, "/etc/wildstone", time.Hour, defaultResticImage, "", "", butlerConfig{}, householdConfig{}, log, &bootstrap); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()

	apiURL := "http://" + apiLis.Addr().String()

	// healthz answers unauthenticated once the API is up.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(apiURL + "/api/v1/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("API never became healthy: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The agent-plane port accepts TCP (full join/session coverage lives
	// in the gateway/agent suites).
	conn, err := net.DialTimeout("tcp", gwLis.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("agent plane not listening: %v", err)
	}
	conn.Close()

	// First start printed exactly one wst_ token, and it authenticates as
	// the bootstrap admin.
	printed := bootstrap.String()
	if strings.Count(printed, "wst_") != 1 {
		t.Fatalf("bootstrap output = %q, want exactly one token", printed)
	}
	token := printed[strings.Index(printed, "wst_"):]
	token = strings.TrimSpace(token)
	whoami := func() (map[string]any, int) {
		req, _ := http.NewRequest("GET", apiURL+"/api/v1/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		return body, resp.StatusCode
	}
	id, status := whoami()
	if status != 200 || id["name"] != "admin" || id["role"] != "admin" {
		t.Fatalf("whoami = %+v (%d)", id, status)
	}

	// The ingestion loop notices a human git edit and serves the new
	// resource through the API.
	stateRoot := filepath.Join(dataDir, "statefile")
	doc := "apiVersion: wildstone.dev/v1\nkind: Volume\nmetadata:\n  name: photos\n  owner: capability/test\n  node: n1\nspec:\n  name: photos\n"
	if err := os.MkdirAll(filepath.Join(stateRoot, "volume"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "volume", "photos.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", "hand edit"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=mika", "-c", "user.email=m@example.invalid", "-c", "gc.auto=0"}, args...)...)
		cmd.Dir = stateRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequest("GET", apiURL+"/api/v1/resources", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var views []map[string]any
		json.NewDecoder(resp.Body).Decode(&views)
		resp.Body.Close()
		if len(views) == 1 && views[0]["name"] == "photos" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ingested resource never served; got %+v", views)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
