package main

import (
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
)

// OpenRouter is its own provider with its own key, spoken to with the
// OpenAI client at OpenRouter's address.
func TestOpenRouterIsBuiltWithItsOwnKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "sk-oa")
	if _, _, err := newButlerProvider(butlerConfig{Provider: "openrouter", Model: "openai/gpt-4o"}); err == nil ||
		!strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Fatalf("without its key: %v, want a refusal naming OPENROUTER_API_KEY (not borrowing OpenAI's)", err)
	}
	t.Setenv("OPENROUTER_API_KEY", "sk-or")
	p, backend, err := newButlerProvider(butlerConfig{Provider: "openrouter", Model: "openai/gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*provider.OpenAIProvider); !ok || backend.Endpoint != "https://openrouter.ai/api/v1" || backend.Model != "openai/gpt-4o" {
		t.Errorf("built %T at %+v", p, backend)
	}
	if _, _, err := newButlerProvider(butlerConfig{Provider: "made-up"}); err == nil {
		t.Error("an unknown provider was built")
	}
}
