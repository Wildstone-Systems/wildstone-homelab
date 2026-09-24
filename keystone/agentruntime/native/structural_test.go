package native

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestPackageNeverReachesWildstoneDirectly is the strict half of
// ADR-W039 Phase 2's split (§7.1, §8 Phase 2): the native reasoning
// loop reaches Wildstone state only through keystone/agentapi's Client
// interface, never by importing the database, the tool gateways, or
// anything that could hand it one. keystone/butler keeps ToolGateway
// and HouseholdGateway (the two tool gateways) and the legacy
// closure-driven Session — this package holds only Runner and Phraser,
// and it must never import any of the following, in production code:
//
//   - keystone/runtimedb, keystone/statefile, keystone/domestic,
//     keystone/butlerstore: the runtime database and its two
//     supporting stores. A Runner that could reach any of these could
//     reconstruct the closure path this split removed.
//   - keystone/butler: the package holding the tool gateways. This
//     package reaches a Toolset only as agentapi.Manifest/InvokeResult,
//     never as a Go value.
//   - keystone/api, keystone/gateway: the HTTP surfaces. A client
//     package that imported its own transport could not be driven by a
//     different one, which is the property a later out-of-process
//     runtime needs.
//   - database/sql: there is no database seam narrow enough for this
//     package to hold; it holds agentapi.Client and nothing else.
//
// Exact-path matching only (no prefix match): keystone/butler/provider
// is a real, needed dependency (the provider-neutral chat types) and is
// a different import path from keystone/butler, so it is not caught
// here.
//
// Parsed imports of non-test files only, so a comment naming a
// forbidden package — as this one does — is not a match, and a test
// file may still use keystone/butler's fixtures to build a real
// ToolGateway/HouseholdGateway to drive through a Session, exactly as
// keystone/butler's own tests do.
func TestPackageNeverReachesWildstoneDirectly(t *testing.T) {
	const module = "github.com/chickenrat17/wildstone-homelab/keystone"
	forbidden := map[string]bool{
		module + "/runtimedb":   true,
		module + "/statefile":   true,
		module + "/domestic":    true,
		module + "/butlerstore": true,
		module + "/butler":      true,
		module + "/api":         true,
		module + "/gateway":     true,
		"database/sql":          true,
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
			if forbidden[path] {
				t.Errorf("%s imports %q — the native reasoning loop must reach Wildstone only through keystone/agentapi (ADR-W039 §7.1, §8 Phase 2)", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test source files were checked; the test is looking in the wrong place")
	}
}
