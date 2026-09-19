package pkgtool

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
)

// The vocabulary check on a package's declared audience (ADR-W033
// design §4, slice 8).
//
// Why it is worth a lint rule rather than trusting authors: an
// entitlement defaults to deny, so a package that meant `adults` and
// wrote `Adults` declares an audience nobody is in. The install
// succeeds, the app appears, and it refuses everyone — which reads as a
// broken app rather than as a one-character typo in a manifest. That is
// the same shape as `classifications: [admin]` (F31), in a field that is
// load-bearing from its first day.
func TestAccessGroupVocabulary(t *testing.T) {
	for _, g := range []string{
		pkgspec.AccessGroupFamily, pkgspec.AccessGroupAdults, pkgspec.AccessGroupKids,
	} {
		pkg := &pkgspec.Package{AccessGroups: []string{g}}
		if issues := accessGroupIssues(pkg); len(issues) != 0 {
			t.Errorf("%q is a household group and was rejected: %v", g, issues)
		}
	}

	for _, g := range []string{"Adults", "family_users", "everyone", "media", "owners", " kids"} {
		pkg := &pkgspec.Package{AccessGroups: []string{g}}
		issues := accessGroupIssues(pkg)
		if len(issues) != 1 {
			t.Errorf("%q produced %d issues, want 1 — a group name outside the "+
				"closed set means an audience nobody is in", g, len(issues))
			continue
		}
		if issues[0].Rule != "access-group" {
			t.Errorf("%q reported under rule %q", g, issues[0].Rule)
		}
	}
}

// The likeliest mistake gets told what it was.
//
// `Adults` is the household's display name and `adults` is the seed key,
// and slice 6 made them deliberately independent so a rename does not
// re-point every rule. That independence is exactly what makes the
// capitalised form a plausible thing to write, so the message says so
// rather than only listing the three valid values.
func TestACapitalisedGroupNameIsNamedAsSuch(t *testing.T) {
	issues := accessGroupIssues(&pkgspec.Package{AccessGroups: []string{"Adults"}})
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}
	if !strings.Contains(issues[0].Msg, `did you mean "adults"`) {
		t.Errorf("the message does not name the likely fix: %q", issues[0].Msg)
	}
	// And a name that is not merely miscapitalised does NOT get the
	// suggestion, or it would claim every typo is a case error.
	other := accessGroupIssues(&pkgspec.Package{AccessGroups: []string{"grownups"}})
	if len(other) != 1 || strings.Contains(other[0].Msg, "did you mean") {
		t.Errorf("an unrelated name was offered a correction: %v", other)
	}
}

// A package declaring no audience is not a package declaring "everyone".
//
// Empty means the service names no audience of its own, and what it
// grants is still decided by entitlement rows that default to deny.
// Linting an empty declaration as an error would force every one of the
// 60 shipped manifests to state an audience in the same change that
// introduced the field.
func TestAPackageMayDeclareNoAccessGroups(t *testing.T) {
	if issues := accessGroupIssues(&pkgspec.Package{}); len(issues) != 0 {
		t.Errorf("a package with no accessGroups was linted: %v", issues)
	}
}

// pkgspec checks SHAPE and lint checks VOCABULARY (ADR-W025).
//
// The split matters for upgrades: a manifest written against a newer
// group vocabulary must still LOAD on an older Keystone, or a catalog
// update makes the whole catalog unreadable rather than making one
// package unavailable. So Validate must accept a name it has never
// heard of, and lint is where it stops.
func TestAnUnknownGroupStillLoads(t *testing.T) {
	pkg, err := pkgspec.Parse([]byte(`
apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: x, version: 1.0.0, capability: x }
accessGroups: [a-group-from-the-future]
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Volume
    metadata: { name: x, node: n }
    spec: { name: x }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := pkg.Validate(); err != nil {
		t.Errorf("a manifest naming an unknown group failed to load: %v — "+
			"vocabulary belongs to lint, or a newer catalog becomes unreadable", err)
	}
	if issues := accessGroupIssues(pkg); len(issues) != 1 {
		t.Errorf("lint did not catch it: %v", issues)
	}
}

// Shape IS pkgspec's, though: blank and duplicate entries are structural
// and rejected at load.
func TestBlankAndDuplicateAccessGroupsAreRefusedAtLoad(t *testing.T) {
	for _, groups := range [][]string{
		{""},
		{pkgspec.AccessGroupAdults, pkgspec.AccessGroupAdults},
	} {
		pkg := &pkgspec.Package{
			Metadata:     pkgspec.Metadata{Name: "x", Version: "1.0.0", Capability: "x"},
			AccessGroups: groups,
			Templates:    []string{"apiVersion: wildstone.dev/v1"},
		}
		if err := pkg.Validate(); err == nil {
			t.Errorf("accessGroups %v was accepted; blank and repeated entries are "+
				"shape errors, not vocabulary", groups)
		}
	}
}
