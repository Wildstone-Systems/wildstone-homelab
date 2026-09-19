package pkgtool

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
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

// TestNoTwoCapabilitiesDeclareOneResource catches the collision the
// engine cannot see at install time. A resource is identified by kind
// and name alone, so when a package from ANOTHER capability declares
// the same pair, installing it silently re-owns the existing resource:
// calendar-radicale's Route/calendar replaced mail-roundcube's, moving
// Davis's address to Radicale with nothing in the plan saying so.
//
// Sharing within one capability is allowed and deliberate — sibling
// profiles such as the two book packages share Secret/restic-books, one
// household repository whichever app is reading it.
func TestNoTwoCapabilitiesDeclareOneResource(t *testing.T) {
	type claim struct{ pkg, capability string }
	claims := map[pkgspec.ResourceKey][]claim{}
	for _, dir := range packageDirs(t) {
		pkg, err := pkgspec.Load(filepath.Join(dir, "package.yaml"))
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(dir), err)
		}
		for _, k := range pkgspec.ResourceKeys(pkg) {
			claims[k] = append(claims[k], claim{pkg.Metadata.Name, pkg.Metadata.Capability})
		}
	}
	for k, cs := range claims {
		for _, c := range cs[1:] {
			if c.capability != cs[0].capability {
				t.Errorf("%s/%s is declared by %s (capability %s) and %s (capability %s); installing one re-owns the other's",
					k.Kind, k.Name, cs[0].pkg, cs[0].capability, c.pkg, c.capability)
			}
		}
	}
}

// Every profile of a capability declares the same audience (ADR-W033
// design §4, slice 8).
//
// An entitlement is about the CAPABILITY, not the package: a household
// that swaps one photos profile for another has not changed who may see
// the photos. So the reconcile loop keeps one audience per capability
// and never has to choose between profiles — and this is what makes
// that a fact rather than an assumption.
//
// Two profiles disagreeing would not fail loudly. The loop would take
// whichever the catalog walk reached last, so the household's access
// would depend on a map iteration order, and switching profiles could
// silently widen or narrow who may use the service. That is the kind of
// difference nobody notices until somebody reaches something.
//
// A profile that declares nothing is not a disagreement: it has said
// nothing, and the seeding pass treats an empty declaration as "this
// service names no audience of its own".
func TestProfilesOfACapabilityDeclareTheSameAudience(t *testing.T) {
	byCapability := map[string]map[string]string{} // capability -> audience -> package
	for _, dir := range packageDirs(t) {
		pkg, err := pkgspec.Load(filepath.Join(dir, "package.yaml"))
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if len(pkg.AccessGroups) == 0 {
			continue
		}
		groups := append([]string(nil), pkg.AccessGroups...)
		sort.Strings(groups)
		audience := strings.Join(groups, ",")
		if byCapability[pkg.Metadata.Capability] == nil {
			byCapability[pkg.Metadata.Capability] = map[string]string{}
		}
		byCapability[pkg.Metadata.Capability][audience] = pkg.Metadata.Name
	}
	for capability, audiences := range byCapability {
		if len(audiences) <= 1 {
			continue
		}
		var said []string
		for audience, pkg := range audiences {
			said = append(said, fmt.Sprintf("%s says [%s]", pkg, audience))
		}
		sort.Strings(said)
		t.Errorf("capability %q has profiles that disagree about who it is for: %s.\n"+
			"An entitlement is keyed on the capability, so the loop keeps one "+
			"audience for it and would take whichever the catalog walk reached "+
			"last — making a household's access depend on map iteration order.",
			capability, strings.Join(said, "; "))
	}
}
