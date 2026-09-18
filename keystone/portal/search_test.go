package portal

import (
	"net/url"
	"strings"
	"testing"
)

// Every engine is a real, complete query prefix.
//
// A row with a typo in it is a search box that goes somewhere wrong, and
// nothing else in the system would notice.
func TestEveryEngineIsAUsableQueryPrefix(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Engines {
		if e.Key == "" || e.Name == "" {
			t.Errorf("engine %+v is missing a key or a name", e)
		}
		if seen[e.Key] {
			t.Errorf("two engines share the key %q", e.Key)
		}
		seen[e.Key] = true

		// https only: this is a household's search terms.
		if !strings.HasPrefix(e.Query, "https://") {
			t.Errorf("%s does not use https: %q", e.Key, e.Query)
		}
		// It must END in the parameter the terms are appended to,
		// otherwise the first search silently loses them.
		if !strings.HasSuffix(e.Query, "=") {
			t.Errorf("%s does not end where the terms go: %q", e.Key, e.Query)
		}
		u, err := url.Parse(e.Query + "test")
		if err != nil {
			t.Errorf("%s does not parse: %v", e.Key, err)
			continue
		}
		if u.Host == "" {
			t.Errorf("%s has no host", e.Key)
		}
	}
}

// A household that has chosen nothing gets the engine that asks least.
func TestTheDefaultEngineIsThePrivateOne(t *testing.T) {
	got, explicit := EngineFor("")
	if explicit {
		t.Error("an empty setting reported itself as a choice")
	}
	if got.Key != DefaultEngine {
		t.Errorf("default = %q, want %q", got.Key, DefaultEngine)
	}
	if got.Key != "duckduckgo" {
		t.Errorf("the default is %q; a household that never opens the setting should not "+
			"get the engine that keeps the most", got.Key)
	}
}

// A key nobody recognises falls back rather than breaking the box.
//
// The alternative is a household whose search stops working because of
// a typo in a settings file, which is a worse outcome than quietly
// using the default.
func TestAnUnknownEngineFallsBackRatherThanFailing(t *testing.T) {
	for _, key := range []string{"gogle", "askjeeves", "  ", "javascript:alert(1)", "https://evil.example"} {
		got, explicit := EngineFor(key)
		if got.Key != DefaultEngine {
			t.Errorf("EngineFor(%q) = %q, want the default", key, got.Key)
		}
		if explicit {
			t.Errorf("EngineFor(%q) reported itself as a recognised choice", key)
		}
	}
	// And a real one is honoured, case-insensitively.
	for _, key := range []string{"google", "Google", " GOOGLE "} {
		got, explicit := EngineFor(key)
		if got.Key != "google" || !explicit {
			t.Errorf("EngineFor(%q) = %q explicit=%v", key, got.Key, explicit)
		}
	}
}

// The address a search goes to comes from this table and nowhere else.
//
// If a settings file could supply the URL, a typo would be a redirect to
// anywhere and a hostile edit would be a phishing page wearing the
// household's own dashboard as its referrer.
func TestNoConfiguredValueCanBecomeTheAddress(t *testing.T) {
	hostile, _ := EngineFor("https://evil.example/?q=")
	for _, e := range Engines {
		if hostile.Query == e.Query {
			return // it resolved to a row from the table, which is the point
		}
	}
	t.Fatalf("a configured address became the query URL: %q", hostile.Query)
}
