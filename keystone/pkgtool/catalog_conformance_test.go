package pkgtool

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/trait"
)

// The catalog conformance gate (ADR-W025 A3). Lint is the package
// contract test (ADR-W010 A5), and until now nothing ran it over the
// packages this repository actually ships — so a rule could be added,
// pass its own unit test, and be violated by half the catalog without
// anything saying so. F31 is the recorded instance: ingress-tailnet
// shipped `classifications: [admin]`, a value that does not exist, and
// lint called the package clean because the check was not yet written.
//
// This is also the check a NEW package meets first. Drop a directory into
// packages/, run `go test ./pkgtool/`, and the answer names what is
// wrong — before an install, before a plan, before an operator.

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the repository root")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// packageDirs are the immediate subdirectories of packages/ holding a
// package.yaml, sorted so failures list in a stable order.
func packageDirs(t *testing.T) []string {
	t.Helper()
	root := filepath.Join(repoRoot(t), "packages")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading packages/: %v", err)
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "package.yaml")); err != nil {
			continue // a catalog directory is allowed to hold other things
		}
		dirs = append(dirs, dir)
	}
	if len(dirs) == 0 {
		t.Fatal("found no packages under packages/; this test would pass vacuously")
	}
	sort.Strings(dirs)
	return dirs
}

// TestEveryShippedPackageLintsClean runs the full contract test over each
// package with the whole shipped catalog available to satisfy its
// requires — which is what a real deployment offers, and what the
// `-catalog` flags on a command line only approximate.
func TestEveryShippedPackageLintsClean(t *testing.T) {
	dirs := packageDirs(t)
	for _, dir := range dirs {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			issues, err := Lint(dir, LintOptions{CatalogDirs: dirs})
			if err != nil {
				t.Fatalf("lint could not run: %v", err)
			}
			for _, issue := range issues {
				t.Errorf("%s", issue)
			}
		})
	}
}

// TestEveryShippedRequirementIsSatisfiable asks the question one package
// at a time cannot: does this CATALOG hold a provider for everything it
// asks of itself?
//
// A requirement nothing in the catalog provides is not a package defect —
// it is a legitimate way to depend on something an operator supplies
// separately — but in a catalog that ships as one unit it is almost
// always a typo or a package that was renamed. The failure it otherwise
// produces arrives at install time, phrased as "unsatisfied requirement",
// in front of someone who did not write the package.
func TestEveryShippedRequirementIsSatisfiable(t *testing.T) {
	dirs := packageDirs(t)

	provided := map[string]bool{}
	pkgs := map[string]*pkgspec.Package{}
	for _, dir := range dirs {
		pkg, err := pkgspec.Load(filepath.Join(dir, "package.yaml"))
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(dir), err)
		}
		pkgs[filepath.Base(dir)] = pkg
		for _, prov := range pkg.Provides {
			provided[prov.Trait+":"+prov.Mode] = true
		}
	}

	names := make([]string, 0, len(pkgs))
	for name := range pkgs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		for _, req := range pkgs[name].Requires {
			if trait.IsStructural(req.Trait) {
				continue // the system satisfies it; no package need advertise it
			}
			if !provided[req.Trait+":"+req.Mode] {
				t.Errorf("%s requires %s:%s and no shipped package provides it",
					name, req.Trait, req.Mode)
			}
		}
	}
}

// TestEveryShippedExportIsCarriedByItsTrait is the mirror of the
// requirement check, and catches the failure that is otherwise invisible:
// a provider exporting a key under a name no consumer will ever look up.
// Nothing fails — the value is simply never seen — so the package author
// believes they published it and every consumer's template fails on a
// missing key, with nothing connecting the two facts.
func TestEveryShippedExportIsCarriedByItsTrait(t *testing.T) {
	for _, dir := range packageDirs(t) {
		name := filepath.Base(dir)
		pkg, err := pkgspec.Load(filepath.Join(dir, "package.yaml"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, prov := range pkg.Provides {
			if len(prov.Exports) == 0 {
				continue
			}
			tr, ok := trait.Lookup(prov.Trait)
			if !ok {
				t.Errorf("%s: unknown trait %q", name, prov.Trait)
				continue
			}
			mode, ok := tr.LookupMode(prov.Mode)
			if !ok {
				t.Errorf("%s: trait %q has no mode %q", name, prov.Trait, prov.Mode)
				continue
			}
			keys := make([]string, 0, len(prov.Exports))
			for k := range prov.Exports {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if !mode.HasExport(key) {
					t.Errorf("%s exports %q, which trait %s:%s does not carry",
						name, key, prov.Trait, prov.Mode)
				}
			}
		}
	}
}
