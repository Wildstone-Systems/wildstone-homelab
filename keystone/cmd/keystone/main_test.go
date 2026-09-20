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

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
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
		if err := serve(gwLis, apiLis, "", "localhost,127.0.0.1", dataDir, 100*time.Millisecond, 200*time.Millisecond, "/etc/wildstone", time.Hour, defaultResticImage, "", "", "", butlerConfig{}, householdConfig{}, log, &bootstrap); err != nil {
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

// Seeding restores what the previous process managed — but an unhealthy
// row is that process saying it did NOT get the resource into its
// desired state. A failed Volume create leaves "absent: create planned,
// not applied"; seeding it would restore the volume as present, and
// since a Volume cannot be observed on a node, the missing volume would
// compare equal to desired forever: no create ever planned again, and
// the next status write calling it healthy. A resource that does not
// exist, green for good. Raised by review on the F76 fix.
func TestSeedRefsKeepsOnlyRowsThatSayTheResourceWasThere(t *testing.T) {
	byNode := seedRefs([]runtimedb.Observed{
		{Kind: "Volume", Name: "there", Node: "node-a", Healthy: true},
		{Kind: "Volume", Name: "never-created", Node: "node-a", Detail: "absent: create planned, not applied"},
		{Kind: "Secret", Name: "awaiting", Node: "node-a", AwaitingApproval: true,
			Detail: "Create is waiting for your approval; nothing is wrong with this resource"},
	})
	var got []string
	for _, ref := range byNode["node-a"] {
		got = append(got, string(ref.Kind)+"/"+ref.Name)
	}
	if len(got) != 1 || got[0] != "Volume/there" {
		t.Errorf("seeded %v; want only the row that reported the resource present", got)
	}
}

// A Butler's client is built from the model, not from the assignment's
// name.
//
// They are the same string for anything configured from the provider's
// own list, and differ for a row named before that — "ollama-cloud"
// pointing at gemma4:31b-cloud. Passing the name asks the provider for
// a model called "ollama-cloud", which it does not have, and the person
// gets a 404 naming something they never typed.
//
// This is here rather than in butlerlive because butlerlive's tests
// supply their own providers, so the one line where an assignment
// becomes a provider call had nothing looking at it.
func TestAButlersClientAsksForTheModelNotTheAssignmentName(t *testing.T) {
	p, err := butlerProviderFor(runtimedb.Butler{
		Name: "alfred", Provider: "ollama",
		Model:         "ollama-cloud",     // what the assignment is called here
		ProviderModel: "gemma4:31b-cloud", // what Ollama calls it
		Endpoint:      "http://192.0.2.10:11434",
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	ol, ok := p.(*provider.OllamaProvider)
	if !ok {
		t.Fatalf("provider = %T, want *provider.OllamaProvider", p)
	}
	if ol.Model != "gemma4:31b-cloud" {
		t.Fatalf("asked the provider for %q, want the model gemma4:31b-cloud — the assignment's name is a key, not a model", ol.Model)
	}
}
