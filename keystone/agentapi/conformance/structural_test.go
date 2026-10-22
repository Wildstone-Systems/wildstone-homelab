package conformance

// TestPackageImportsOnlyAgentapi is this package's own version of
// keystone/agentapi's and keystone/agentapi/httpclient's structural
// tests, one layer further out (#343): a
// conformance suite runnable against ANY agentapi.Client must not itself
// depend on Wildstone's own implementation of one. If this package ever
// imported keystone/agentgateway, keystone/api or any other Wildstone
// internal, an external runtime author could no longer import it into
// their own module without dragging Keystone's own internals along —
// which would make it proof of nothing outside this repository.
//
// The only Wildstone import this package may hold is keystone/agentapi
// itself, the contract every Target's Client already speaks. Everything
// else it uses is the standard library — most visibly "testing", since
// every check takes a *testing.T, which is the whole point: this is a
// library OF tests, not a program that happens to use some of Go's
// testing types.
//
// Parsed imports of non-test files only, so a comment naming a package
// — as this one does, several times — is not a match, and so the
// example_external_test.go/run_test.go files (which legitimately import
// httpclient and other test-only helpers to demonstrate and exercise
// this package) are not held to this package's own, stricter rule.

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestPackageImportsOnlyAgentapi(t *testing.T) {
	const allowed = "github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
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
			if path == allowed {
				continue
			}
			if strings.HasPrefix(path, "github.com/Wildstone-Systems/wildstone-homelab/") {
				t.Errorf("%s imports %s — a conformance suite runnable against any agentapi.Client must import nothing under keystone except agentapi itself (#343)", name, path)
				continue
			}
			if strings.Contains(path, ".") && !strings.HasPrefix(path, "golang.org/x/") {
				// A dot before the first slash marks a non-standard-library
				// module path (the same heuristic Go's own tooling uses);
				// golang.org/x/... is the standard library's own overflow
				// and is fine here as it is for keystone/agentapi itself.
				t.Errorf("%s imports %s — this package's only non-stdlib dependency is keystone/agentapi", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test source files were checked; the test is looking in the wrong place")
	}
}
