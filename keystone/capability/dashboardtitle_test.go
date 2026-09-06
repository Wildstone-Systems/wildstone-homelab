package capability

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// expandDashboard expands the SHIPPED dashboard package. The shipped
// package is the subject: the bug this file pins was a quoting choice in
// packages/dashboard/package.yaml, so a fixture restating the template
// would keep passing while the thing operators install stayed broken.
func expandDashboard(t *testing.T, householdName string) ([]resource.Resource, error) {
	t.Helper()
	pkg, err := pkgspec.Load("../../packages/dashboard/package.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return NewEngine().Expand(pkg, map[string]string{
		"public_domain":  "example.com",
		"node":           "node-a",
		"household_name": householdName,
	}, &Catalog{})
}

func dashboardTitleEnv(t *testing.T, res []resource.Resource) string {
	t.Helper()
	for _, r := range res {
		if r.Kind == resource.KindContainer && r.Container != nil {
			if v, ok := r.Container.Env["WILDSTONE_DASHBOARD_TITLE"]; ok {
				return v
			}
		}
	}
	t.Fatal("no WILDSTONE_DASHBOARD_TITLE in the expanded container")
	return ""
}

// TestHouseholdNameSurvivesExpansion.
//
// household_name is the only param in the catalog that is deliberately
// free text a person writes, and it was carried in a DOUBLE-QUOTED YAML
// scalar. A name containing a quote — `The "Reed" Family`, which is a
// perfectly ordinary thing to call a household — closed the scalar early,
// so the rendered Container was invalid YAML and the capability could not
// be enabled at all. The Go-side sanitising never got a chance to run:
// the failure was two layers earlier, at expansion.
//
// A literal block scalar processes no escapes, so every one of these is
// just characters. Raised by automated PR review.
func TestHouseholdNameSurvivesExpansion(t *testing.T) {
	for _, name := range []string{
		`The "Reed" Family`,
		"The O'Neills",
		`Home: the good one`,
		`C:\Users\family`,
		"Casa Muñoz #1",
		"The Halls & the Bhattacharyas",
		"Household",
	} {
		res, err := expandDashboard(t, name)
		if err != nil {
			t.Errorf("household_name %q: expansion failed: %v", name, err)
			continue
		}
		// Exact, not merely present: a quoting scheme that mangles the
		// name into something close is its own bug.
		if got := dashboardTitleEnv(t, res); got != name {
			t.Errorf("household_name %q round-tripped as %q", name, got)
		}
	}
}

// TestHouseholdNameCannotInjectYAML. The block scalar removes the
// escaping problem; it must not remove the structural guard. A newline
// is what would let a param add a sibling key — another env var, a mount,
// an owner override — and mergeParams refuses it for every param.
func TestHouseholdNameCannotInjectYAML(t *testing.T) {
	_, err := expandDashboard(t, "x\n        EVIL_ENV: \"pwned")
	if err == nil {
		t.Fatal("a newline in a param value was accepted — YAML structure injection is reachable")
	}
	if !strings.Contains(err.Error(), "control character") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}
