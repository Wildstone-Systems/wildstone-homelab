package agentapi

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestPackageNeverImportsWildstoneInternals is ADR-W039 §8 Phase 0's
// evidence: the contract compiles with no dependency on the packages it
// is the boundary for. It is the counterpart of keystone/butler's
// no-reconcile test — the same guarantee, one layer further out.
//
// The list is every package a runtime must never be able to reach by
// following a type: the runtime database and the Statefile (the state
// itself), the domestic executors (the effects), the Butler and its
// provider (the native runtime, which must consume this package rather
// than be consumed by it), the store (the ledger and memory seams), the
// public API, and database/sql as a stand-in for any driver at all. If
// this package ever needed one of them, it would no longer be a
// contract; it would be an implementation with a contract's name.
//
// Non-test files only, and by the parsed import table rather than a
// substring, so a comment naming a package — as this one does — is not
// a match.
func TestPackageNeverImportsWildstoneInternals(t *testing.T) {
	forbidden := []string{
		"keystone/runtimedb",
		"keystone/statefile",
		"keystone/domestic",
		"keystone/butler",
		"keystone/butler/provider",
		"keystone/butlerstore",
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
			for _, f := range forbidden {
				if path == f || strings.HasSuffix(path, "/"+f) {
					t.Errorf("%s imports %s — the Agent API contract must not reach a Wildstone internal (ADR-W039 §8 Phase 0)", name, path)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test source files were checked; the test is looking in the wrong place")
	}
}
