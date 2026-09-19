package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// The drawer is built from the catalog, and a key's presence is reported
// without the key: ADR-W017 A9 keeps keys in the environment.
func TestProviderKindsReportKeysWithoutRevealingThem(t *testing.T) {
	h := newHarness(t)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-very-secret")
	t.Setenv("OPENAI_API_KEY", "")
	var body struct {
		Providers []providerKindView `json:"providers"`
	}
	res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butler/providers", nil, &body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	set := map[string]bool{}
	for _, p := range body.Providers {
		set[p.Name] = p.KeySet
	}
	for _, want := range []string{"ollama", "openai", "openrouter", "anthropic", "openai-compatible"} {
		if _, ok := set[want]; !ok {
			t.Errorf("%s is not offered", want)
		}
	}
	if !set["openrouter"] || set["openai"] {
		t.Errorf("key presence = %v, want openrouter set and openai not", set)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "very-secret") {
		t.Fatal("a key was returned")
	}
}

// OpenRouter and OpenAI are both spoken to with the OpenAI client and
// each uses its OWN key — the reason one key per kind was not enough.
func TestAnOpenAICompatibleProviderListsWithItsOwnKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"data":[{"id":"openai/gpt-4o"},{"id":"anthropic/claude-sonnet-4"}]}`)
	}))
	defer srv.Close()
	t.Setenv("OPENROUTER_API_KEY", "sk-or")
	t.Setenv("OPENAI_API_KEY", "sk-oa")

	models, _, code, err := offeredModels(context.Background(), "openrouter", srv.URL+"/api/v1")
	if err != nil || code != http.StatusOK {
		t.Fatalf("listing = %d, %v", code, err)
	}
	if gotAuth != "Bearer sk-or" {
		t.Errorf("OpenRouter was asked with %q, want its own key", gotAuth)
	}
	if len(models) != 2 || models[0].Name != "anthropic/claude-sonnet-4" || models[0].Local {
		t.Errorf("models = %+v, want both, sorted, hosted", models)
	}
}

// Anthropic lists at /v1/models with its key header and a version.
func TestAnthropicListsItsModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("x-api-key") != "sk-ant" || r.Header.Get("anthropic-version") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"claude-sonnet-4-5"}]}`)
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	models, _, code, err := offeredModels(context.Background(), "anthropic", srv.URL)
	if err != nil || code != http.StatusOK || len(models) != 1 || models[0].Name != "claude-sonnet-4-5" {
		t.Fatalf("listing = %+v, %d, %v", models, code, err)
	}
}

// Without its key a hosted provider says which variable to set, rather
// than failing at the provider with a bare 401.
func TestAHostedProviderWithoutItsKeySaysWhichToSet(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	_, _, code, err := offeredModels(context.Background(), "openai", "")
	if code != http.StatusBadRequest || err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("listing without a key = %d, %v", code, err)
	}
}

// A self-hosted OpenAI-compatible server is local when it is on this
// network, and cloud otherwise — the safe direction.
func TestPrivateHostsAreLocal(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "192.168.1.99": true, "10.0.0.5": true, "100.64.0.59": true,
		"lmstudio": true, "box.local": true, "api.openai.com": false, "8.8.8.8": false,
	} {
		if got := privateHost(host); got != want {
			t.Errorf("privateHost(%q) = %v, want %v", host, got, want)
		}
	}
}
