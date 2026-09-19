package pkgtool

// The filedrop capability's own conformance check.
//
// Same claim the calendar's makes, about the same seam: two halves have
// to agree and neither can see the other. The package's `agentTools`
// block is what the domestic gateway will ALLOW;
// keystone/domestic.FiledropTools is what it will actually DISPATCH. A
// tool in one and not the other fails in a way nothing reports at
// install time, which is why it is pinned here rather than reviewed.

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
)

func TestFiledropToolsAreDeclaredByThePackage(t *testing.T) {
	sp, ok := walkShipped(t)["filedrop-wildstone"]
	if !ok {
		t.Fatal("filedrop-wildstone is not in the catalog")
	}
	if sp.pkg.Metadata.Capability != domestic.FiledropCapability {
		t.Fatalf("the package's capability is %q but the executors register under %q: every call would be "+
			"refused as a tool the capability does not declare",
			sp.pkg.Metadata.Capability, domestic.FiledropCapability)
	}

	declared := map[string]string{}
	for _, tool := range sp.pkg.AgentTools {
		declared[tool.Name] = tool.Risk
	}
	for name, tier := range domestic.FiledropTools {
		got, present := declared[name]
		if !present {
			t.Errorf("keystone registers an executor for filedrop.%s, which the package does not declare: "+
				"the gateway refuses a tool its capability has not offered, so the executor is wired and dead", name)
			continue
		}
		if got != tier {
			t.Errorf("filedrop.%s is declared at tier %q and registered at %q: the gateway refuses the mismatch, "+
				"which reads to a household as the drop box being broken", name, got, tier)
		}
	}
	for name := range declared {
		if _, present := domestic.FiledropTools[name]; !present {
			t.Errorf("the package declares filedrop.%s, which nothing registers: a Butler lists it to the "+
				"household and the gateway answers \"no executor is registered for this tool\"", name)
		}
	}

	// Every tool has to be one a family session can actually carry
	// (ADR-W024 A5/A5.1) — asked of the one definition rather than a
	// copy of the set.
	for name, tier := range declared {
		if !domestic.TierAllowed(tier, true) {
			t.Errorf("filedrop.%s is declared at tier %q, which a family session cannot carry, "+
				"so the tool would never be callable", name, tier)
		}
	}

	// And the split that tier exists to make: a confirm-tier tool must
	// be refused to an unattended rule. A drop box holds the only copy
	// of what people put in it, so a delete running at 3am with nobody
	// asked is exactly the failure A5.1 was added to prevent.
	for name, tier := range declared {
		if tier == domestic.TierConfirm && domestic.TierAllowed(tier, false) {
			t.Errorf("filedrop.%s is at the confirm tier but an unattended rule may carry it", name)
		}
	}
	if declared[domestic.FileToolDelete] != domestic.TierConfirm {
		t.Errorf("filedrop.%s is declared at %q: deleting a household file is irreversible and must ask first",
			domestic.FileToolDelete, declared[domestic.FileToolDelete])
	}

	// NOTHING HANDS OVER BYTES. The absence of a read-the-file tool is
	// the design rather than a phase, so it is pinned: a drop box holds
	// the household's documents, and piping them through a model's
	// context is a disclosure nobody asked for.
	for _, forbidden := range []string{"read_file", "get_file", "open_file", "download", "upload_file", "upload"} {
		if _, present := declared[forbidden]; present {
			t.Errorf("the package declares filedrop.%s: no agent tool may move a household file's CONTENTS "+
				"through a model", forbidden)
		}
	}
}
