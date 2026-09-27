package pkgtool

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
)

// The pantry package's agentTools and keystone/domestic.PantryTools are
// two halves of one list (ADR-W042 A5): a tool the package declares that
// nothing registers is a tool the Butler offers and cannot perform, and a
// registered tool the package does not declare is refused on every call.
func TestPantryToolsAreDeclaredByThePackage(t *testing.T) {
	sp, ok := walkShipped(t)["pantry-pantryos"]
	if !ok {
		t.Fatal("pantry-pantryos is not in the catalog")
	}
	if sp.pkg.Metadata.Capability != domestic.PantryCapability {
		t.Fatalf("the package's capability is %q but the executors register under %q",
			sp.pkg.Metadata.Capability, domestic.PantryCapability)
	}
	declared := map[string]string{}
	for _, tool := range sp.pkg.AgentTools {
		declared[tool.Name] = tool.Risk
	}
	for name, tier := range domestic.PantryTools {
		got, present := declared[name]
		if !present {
			t.Errorf("keystone registers an executor for pantry.%s, which the package does not declare", name)
			continue
		}
		if got != tier {
			t.Errorf("pantry.%s is declared at tier %q and registered at %q", name, got, tier)
		}
	}
	for name, tier := range declared {
		if _, present := domestic.PantryTools[name]; !present {
			t.Errorf("the package declares pantry.%s, which nothing registers", name)
		}
		if !domestic.TierAllowed(tier, true) {
			t.Errorf("pantry.%s is declared at tier %q, which a family session cannot carry", name, tier)
		}
	}
	// Only finishing a cook touches stock all at once; it alone asks.
	if declared["finish_cooking"] != domestic.TierConfirm {
		t.Error("finish_cooking must be confirm: it takes every ingredient out of the pantry at once")
	}
}
