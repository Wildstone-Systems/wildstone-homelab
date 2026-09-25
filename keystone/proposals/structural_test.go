package proposals

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestPackageNeverImportsTheAPI is ADR-W039 Phase 3's own structural
// evidence (#339): keystone/proposals exists so keystone/agentgateway
// can file a Plan without importing keystone/api (a cycle, since api
// already imports agentgateway for the Agent API's HTTP handlers).
// That only holds if the dependency runs one way — proposals must
// never import api, however indirectly a future change might be
// tempted to reach back for a Server method or an HTTP type.
//
// Parsed imports of non-test files only, so a comment naming the
// package — as this file and proposals.go's own doc comment do — is
// not a match.
func TestPackageNeverImportsTheAPI(t *testing.T) {
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
			if path == "keystone/api" || strings.HasPrefix(path, "keystone/api/") ||
				strings.HasSuffix(path, "/keystone/api") || strings.Contains(path, "/keystone/api/") {
				t.Errorf("%s imports %s — keystone/proposals must never import keystone/api (ADR-W039 §8 Phase 3, #339)", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test source files were checked; the test is looking in the wrong place")
	}
}
