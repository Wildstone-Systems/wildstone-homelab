package agentgateway

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestPackageNeverImportsTheAPIOrAnApplier is the Wildstone side's
// structural evidence (ADR-W039, #337). This package MAY hold the
// runtime database, the store, the Butler surfaces and the operation
// identity — it is the side that decides — and it must never reach:
//
//   - keystone/api or keystone/cmd: the gateway sits below the API and
//     is called by it; a gateway that imported its caller could not
//     be driven by a second transport, which is the property Phase 2c's
//     byte-identical test needs.
//   - keystone/reconcile: the only apply-capable surface in Keystone.
//     butler/structural_test.go pins that propose_change produces a
//     Plan nothing here can apply; the gateway now runs propose_change
//     on the runner's behalf, so the same ceiling is pinned again one
//     layer up.
//
// Parsed imports of non-test files only, so a comment naming a package
// — as this one does — is not a match.
func TestPackageNeverImportsTheAPIOrAnApplier(t *testing.T) {
	forbidden := []string{
		"keystone/api",
		"keystone/cmd",
		"keystone/reconcile",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var checked int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		checked++
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, f := range forbidden {
				if path == f || strings.HasPrefix(path, f+"/") || strings.HasSuffix(path, "/"+f) {
					t.Errorf("%s imports %s — the Agent Gateway must reach neither its caller nor an applier (ADR-W039, #337)", name, path)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test source files were checked; the test is looking in the wrong place")
	}
}
