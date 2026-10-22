package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A key entered in the Console is kept root-only, used when the
// environment has none, and never beats one that the environment has.
func TestAConsoleKeyIsKeptPrivatelyAndTheEnvironmentWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider-keys.json")
	if err := UseKeyFile(path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTER_API_KEY", "")
	or, _ := LookupKind("openrouter")
	gen := KeyGeneration()

	if err := SetKey(or, "sk-or-from-the-console"); err != nil {
		t.Fatal(err)
	}
	if or.Key() != "sk-or-from-the-console" || or.KeySource() != "console" {
		t.Errorf("key %q from %q, want the console's", or.Key(), or.KeySource())
	}
	if KeyGeneration() == gen {
		t.Error("the key generation did not move, so a cached client would keep the old key")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}

	// A restart reads it back.
	if err := UseKeyFile(path); err != nil || or.Key() != "sk-or-from-the-console" {
		t.Errorf("after reload the key is %q, %v", or.Key(), err)
	}

	t.Setenv("OPENROUTER_API_KEY", "sk-or-from-the-environment")
	if or.Key() != "sk-or-from-the-environment" || or.KeySource() != "environment" {
		t.Errorf("the environment did not win: %q from %q", or.Key(), or.KeySource())
	}

	t.Setenv("OPENROUTER_API_KEY", "")
	if err := RemoveKey(or); err != nil || or.KeySet() {
		t.Errorf("after removal the key is set=%v, %v", or.KeySet(), err)
	}
}

// A malformed key is refused without being repeated back.
func TestABadKeyIsRefusedWithoutEchoingIt(t *testing.T) {
	if err := UseKeyFile(filepath.Join(t.TempDir(), "k.json")); err != nil {
		t.Fatal(err)
	}
	oa, _ := LookupKind("openai")
	err := SetKey(oa, "sk-has a-space-secret")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("err = %v; want a refusal that does not quote the value", err)
	}
	ol, _ := LookupKind("ollama")
	if SetKey(ol, "sk-anything-long") == nil {
		t.Error("a provider with no key accepted one")
	}
}
