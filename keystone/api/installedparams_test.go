package api

import (
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
)

// The near-miss this exists to prevent.
//
// home-assistant was installed with timezone=CST. The package default is
// UTC. The form offered UTC, because a default and the deployment's facts
// were all it knew — so upgrading through the console would have moved a
// household's clock, with nothing anywhere saying so. What the capability
// is actually running with is the only answer that is true of it.
func TestInstalledParamWinsOverThePackageDefault(t *testing.T) {
	got := resolveParams(
		[]pkgspec.ParamDecl{{Name: "timezone", Default: "UTC"}},
		deploymentFacts{},
		map[string]string{"timezone": "CST"},
	)
	if len(got) != 1 {
		t.Fatalf("want one param, got %d", len(got))
	}
	if got[0].Installed != "CST" {
		t.Errorf("want installed=CST, got %q", got[0].Installed)
	}
	// The default is still carried: a form showing a first-time install
	// needs it, and the two answer different questions.
	if got[0].Default != "UTC" {
		t.Errorf("the package default must survive alongside it, got %q", got[0].Default)
	}
}

// A param the install never set has no installed value, and the form
// falls back to the deployment's fact or the package's default.
func TestUnsetParamsHaveNoInstalledValue(t *testing.T) {
	got := resolveParams(
		[]pkgspec.ParamDecl{{Name: "timezone", Default: "UTC"}, {Name: "node", Source: "node"}},
		deploymentFacts{nodes: []string{"n1"}},
		map[string]string{"timezone": "CST"},
	)
	by := map[string]paramView{}
	for _, v := range got {
		by[v.Name] = v
	}
	if by["node"].Installed != "" {
		t.Errorf("node was never installed with a value, got %q", by["node"].Installed)
	}
	if by["node"].Suggested != "n1" {
		t.Errorf("the deployment fact should still be offered, got %q", by["node"].Suggested)
	}
}

// A first install has nothing to remember, and must not invent anything.
func TestNoInstallLeavesEveryParamUninstalled(t *testing.T) {
	got := resolveParams(
		[]pkgspec.ParamDecl{{Name: "timezone", Default: "UTC"}},
		deploymentFacts{},
		nil,
	)
	if got[0].Installed != "" {
		t.Errorf("want empty, got %q", got[0].Installed)
	}
}
