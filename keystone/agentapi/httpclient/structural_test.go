package httpclient

// TestPackageNeverImportsWildstoneInternals is this package's own
// evidence, one layer out from keystone/agentapi's own structural test
// (ADR-W039 §10, extended to the transport by ADR-W040): a
// runtime built on this Client reaches Keystone the way any process
// outside it does, over net/http and encoding/json, never by following
// a Go import into the runtime database, the store, the domestic
// executors or the API itself. If this package ever needed one of
// them, it would no longer be proof the Agent API is reachable from
// outside the process — it would be Keystone code standing in for that
// proof.
//
// Parsed imports of non-test files only, so a comment naming a package
// — as this one does — is not a match.

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestPackageNeverImportsWildstoneInternals(t *testing.T) {
	forbidden := []string{
		"keystone/runtimedb",
		"keystone/statefile",
		"keystone/domestic",
		"keystone/butler",
		"keystone/butler/provider",
		"keystone/butlerstore",
		"keystone/butlerlive",
		"keystone/agentgateway",
		"keystone/agentruntime",
		"keystone/api",
		"database/sql",
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
			if path == "github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi" {
				continue
			}
			for _, f := range forbidden {
				if path == f || strings.HasSuffix(path, "/"+f) {
					t.Errorf("%s imports %s — an external runtime's HTTP client must not reach a Wildstone internal (ADR-W040)", name, path)
				}
			}
			if strings.HasPrefix(path, "github.com/Wildstone-Systems/wildstone-homelab/keystone/") &&
				path != "github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi" {
				t.Errorf("%s imports %s — this Client's only Wildstone dependency is keystone/agentapi itself", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test source files were checked; the test is looking in the wrong place")
	}
}
