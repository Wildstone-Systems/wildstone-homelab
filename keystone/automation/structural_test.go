package automation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageNeverImportsApplyOrModel pins the engine's two boundaries
// (ADR-W024 A5/A6): it never holds keystone/reconcile, the only
// apply-capable surface, and never holds keystone/butler, the model. A
// decision is code over facts; what acts on it, and what talks to a
// person about it, live behind their own seams.
func TestPackageNeverImportsApplyOrModel(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{`keystone/reconcile"`, `keystone/butler"`, `keystone/butler/provider"`} {
			if strings.Contains(string(data), forbidden) {
				t.Fatalf("%s imports %s — the automation engine decides; it neither applies nor prompts", name, forbidden)
			}
		}
	}
}
