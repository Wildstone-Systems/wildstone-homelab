package pkgtool

// The lists capability's own conformance check: the package's
// `agentTools` block is what the domestic gateway will ALLOW, and
// keystone/domestic.ListsTools is what it will actually DISPATCH. Pinned
// here for the reason the drop box's is (filedrop_wiring_test.go): a
// tool in one and not the other fails in a way nothing reports at
// install time.

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
)

func TestListsToolsAreDeclaredByThePackage(t *testing.T) {
	sp, ok := walkShipped(t)["lists-wildstone"]
	if !ok {
		t.Fatal("lists-wildstone is not in the catalog")
	}
	if sp.pkg.Metadata.Capability != domestic.ListsCapability {
		t.Fatalf("the package's capability is %q but the executors register under %q: every call would be "+
			"refused as a tool the capability does not declare",
			sp.pkg.Metadata.Capability, domestic.ListsCapability)
	}
	declared := map[string]string{}
	for _, tool := range sp.pkg.AgentTools {
		declared[tool.Name] = tool.Risk
	}
	for name, tier := range domestic.ListsTools {
		got, present := declared[name]
		if !present {
			t.Errorf("keystone registers an executor for lists.%s, which the package does not declare", name)
			continue
		}
		if got != tier {
			t.Errorf("lists.%s is declared at tier %q and registered at %q", name, got, tier)
		}
	}
	for name, tier := range declared {
		if _, present := domestic.ListsTools[name]; !present {
			t.Errorf("the package declares lists.%s, which nothing registers", name)
		}
		if !domestic.TierAllowed(tier, true) {
			t.Errorf("lists.%s is declared at tier %q, which a family session cannot carry", name, tier)
		}
	}
	// Deleting a whole list asks first, and a rule firing on its own
	// cannot do it at all.
	if declared[domestic.ListToolDelete] != domestic.TierConfirm || domestic.TierAllowed(domestic.TierConfirm, false) {
		t.Errorf("lists.%s must be confirm-tier and refused to an unattended rule", domestic.ListToolDelete)
	}
}
