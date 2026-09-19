package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// A key entered in the Console is used, and is never returned by any
// answer or recorded in any event (ADR-W017 A10).
func TestAProviderKeyEnteredInTheConsoleIsUsedAndNeverReturned(t *testing.T) {
	h := newHarness(t)
	if err := provider.UseKeyFile(filepath.Join(t.TempDir(), "provider-keys.json")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTER_API_KEY", "")
	const secret = "sk-or-v1-entered-in-the-console-0123456789"

	var set map[string]any
	res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butler/providers/openrouter/key", map[string]string{"key": secret}, &set)
	if res.StatusCode != http.StatusOK || set["keySource"] != "console" {
		t.Fatalf("set: %d %+v", res.StatusCode, set)
	}

	var kinds struct {
		Providers []providerKindView `json:"providers"`
	}
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butler/providers", nil, &kinds)
	for _, k := range kinds.Providers {
		if k.Name == "openrouter" && (!k.KeySet || k.KeySource != "console") {
			t.Errorf("openrouter = %+v, want set from the console", k)
		}
	}

	// The listing uses it.
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"data":[{"id":"openai/gpt-4o"}]}`)
	}))
	defer srv.Close()
	if _, _, _, err := offeredModels(context.Background(), "openrouter", srv.URL+"/api/v1"); err != nil || gotAuth != "Bearer "+secret {
		t.Errorf("listing used %q, %v", gotAuth, err)
	}

	// Nowhere it could leak: the answers above, and the event log.
	raw, _ := json.Marshal([]any{set, kinds})
	events, _ := h.s.DB.EventsAfter(0, 1000)
	evRaw, _ := json.Marshal(events)
	if strings.Contains(string(raw), secret) || strings.Contains(string(evRaw), secret) {
		t.Fatal("the key was returned or recorded")
	}
	if !strings.Contains(string(evRaw), "provider.key_set") {
		t.Error("setting a key left no event")
	}

	var gone map[string]any
	h.do(runtimedb.RoleAdmin, http.MethodDelete, "/api/v1/butler/providers/openrouter/key", nil, &gone)
	if gone["keySet"] != false {
		t.Errorf("after removal: %+v", gone)
	}
}
