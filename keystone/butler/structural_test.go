package butler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageNeverImportsReconcile is the structural half of ADR-W006 A1/
// B3's no-apply guarantee: this package's non-test source must never import
// keystone/reconcile — the package that owns Applier/Reconciler/Reconcile,
// the only apply-capable surface in Keystone. If a future change adds that
// import, this test fails loudly rather than relying on review to catch it.
// (_test.go files are exempt: the harness recreates its own minimal
// read-only fake agent rather than reaching for reconcile's, precisely so
// even the tests don't need that import — see harness_test.go's doc
// comment — but the guarantee this test enforces is about production code.)
func TestPackageNeverImportsReconcile(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	// runtimedb joined the list with the durable runner (ADR-W030 P3).
	// The Butler checkpoints through the TaskLedger interface it
	// defines, implemented in keystone/butlerstore — so it holds a
	// checkpoint seam, not a database handle, and cannot reach the
	// store's full API by following a field.
	forbidden := []string{`keystone/reconcile"`, `keystone/runtimedb"`}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "provider" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		for _, imp := range forbidden {
			if strings.Contains(string(data), imp) {
				t.Fatalf("%s imports %s — the Butler must hold neither an apply-capable dependency (ADR-W006 A1) nor the runtime database itself (ADR-W030 P3)",
					name, strings.TrimSuffix(imp, `"`))
			}
		}
	}
}
