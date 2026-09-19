package provider

import (
	"os"
	"strings"
)

// Kind is one kind of model provider a Butler can think with.
//
// One catalog, read by everything that has to agree about providers: the
// factory that builds a Butler's client, the listing that asks a provider
// what it serves, and the Console's provider drawer. Before it, "which
// providers exist" was a switch in cmd/keystone, an allow-list in the API
// and a hardcoded "ollama" in the Console, and they had drifted — the
// Console could add only Ollama although the factory built three kinds.
//
// ONE KEY PER PROVIDER. Each hosted kind reads its own environment
// variable, so OpenAI and OpenRouter — both spoken to with the OpenAI
// client — can be used side by side. A key comes from the environment
// or is entered in the Console (ADR-W017 A10, keystore.go), and the
// environment wins. Either way it is never returned: KeySet and
// KeySource report whether one is there and from where, nothing more.
type Kind struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// DefaultEndpoint is where the provider answers when nobody says
	// otherwise. Empty for "other OpenAI-compatible", which has no
	// address worth guessing.
	DefaultEndpoint string `json:"defaultEndpoint,omitempty"`
	// KeyEnv names the environment variable holding this provider's key.
	KeyEnv string `json:"keyEnv,omitempty"`
	// KeyRequired says a request without the key cannot succeed. False
	// for local servers, which usually take none.
	KeyRequired bool `json:"keyRequired"`
	// Hosted says the provider runs outside the house, so every model it
	// serves is a cloud model. A local server decides per model.
	Hosted bool `json:"hosted"`
	// Client is which wire protocol it speaks: ollama, openai or
	// anthropic. OpenRouter and every OpenAI-compatible server speak
	// openai.
	Client string `json:"client"`
}

// Kinds is every provider kind, in the order the Console offers them.
var Kinds = []Kind{
	{Name: "ollama", Label: "Ollama", DefaultEndpoint: "http://localhost:11434", Client: "ollama"},
	{Name: "openai", Label: "OpenAI", DefaultEndpoint: "https://api.openai.com/v1",
		KeyEnv: "OPENAI_API_KEY", KeyRequired: true, Hosted: true, Client: "openai"},
	{Name: "openrouter", Label: "OpenRouter", DefaultEndpoint: "https://openrouter.ai/api/v1",
		KeyEnv: "OPENROUTER_API_KEY", KeyRequired: true, Hosted: true, Client: "openai"},
	{Name: "anthropic", Label: "Anthropic", DefaultEndpoint: "https://api.anthropic.com",
		KeyEnv: "ANTHROPIC_API_KEY", KeyRequired: true, Hosted: true, Client: "anthropic"},
	{Name: "openai-compatible", Label: "Other OpenAI-compatible (LM Studio, vLLM, LiteLLM…)",
		KeyEnv: "OPENAI_COMPATIBLE_API_KEY", Client: "openai"},
}

// LookupKind finds a kind by name.
func LookupKind(name string) (Kind, bool) {
	for _, k := range Kinds {
		if k.Name == strings.TrimSpace(name) {
			return k, true
		}
	}
	return Kind{}, false
}

// Key is this provider's key: its environment variable, or else the one
// entered in the Console (keystore.go), or "". The environment wins.
func (k Kind) Key() string {
	if k.KeyEnv == "" {
		return ""
	}
	if v := strings.TrimSpace(os.Getenv(k.KeyEnv)); v != "" {
		return v
	}
	return storedKey(k.Name)
}

// KeySet reports whether the key is present, never what it is.
func (k Kind) KeySet() bool { return k.Key() != "" }

// Endpoint is the address to use: the one given, or the default.
func (k Kind) Endpoint(given string) string {
	if e := strings.TrimRight(strings.TrimSpace(given), "/"); e != "" {
		return e
	}
	return k.DefaultEndpoint
}
