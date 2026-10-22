package butler

// The manifest-level half of H7's authority claim (#353, ADR-W039 §5):
// a Member-class Agent's manifest never lists host.* at all, and an
// Owner-class Agent's does, marked OptIn, only once the family is
// actually wired.

import (
	"context"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/domestic"
)

// hostToolNames is the closed menu, restated here so this test does not
// depend on iterating gw.tools in a way that would silently pass if the
// registration were removed.
var hostToolNames = []string{
	"host.container_logs", "host.container_stats", "host.disk_usage", "host.unit_status",
	"host.container_restart", "host.prune_images", "host.reconcile_poke",
}

// TestHostOpsNeverAppearInAMemberManifest is deliverable (b): a
// Member-class Agent (HouseholdGateway) never registers the host.*
// family at all, so it is absent from the manifest structurally — not a
// tool the authority resolver refused, a tool this gateway does not
// have. checkTool is never even reached for a name that is not a key in
// gw.tools.
func TestHostOpsNeverAppearInAMemberManifest(t *testing.T) {
	hh := newTestHouseholdGateway("sam", HouseholdSeams{
		Events: func(context.Context, HouseholdLookup) ([]HouseholdFact, error) {
			return nil, nil
		},
	})
	manifest := hh.Capabilities(false)
	if manifest.AgentClass != AgentClassMember {
		t.Fatalf("manifest.AgentClass = %q, want %q", manifest.AgentClass, AgentClassMember)
	}
	for _, tool := range manifest.Tools {
		if strings.HasPrefix(tool.Name, domestic.HostOpsCapability+".") {
			t.Fatalf("Member manifest lists %s — host.* must never appear on this surface", tool.Name)
		}
	}
	// Specs() (what actually reaches a provider) must agree.
	for _, spec := range hh.OfferedSpecs(false) {
		if strings.HasPrefix(spec.Name, domestic.HostOpsCapability+".") {
			t.Fatalf("Member OfferedSpecs offered %s", spec.Name)
		}
	}
}

// TestHostOpsUnavailableUntilWired: on the Owner surface, the closed
// menu is always STRUCTURALLY present (so an Owner can be told it
// exists and how to turn it on), but every entry reports Available:false
// until HostActions is wired — the deployment-wide flag H7 deliverable 4
// asks for.
func TestHostOpsUnavailableUntilWired(t *testing.T) {
	gw := newTestToolGateway(newTestStore(t), nil)
	manifest := gw.Capabilities(false)
	found := map[string]bool{}
	for _, tool := range manifest.Tools {
		if !strings.HasPrefix(tool.Name, domestic.HostOpsCapability+".") {
			continue
		}
		found[tool.Name] = true
		if tool.Available {
			t.Fatalf("%s reports Available with no HostActions wired", tool.Name)
		}
		if !tool.OptIn {
			t.Fatalf("%s does not report OptIn — the whole family must (ADR-W033 §6.2)", tool.Name)
		}
		if tool.AgentClass != AgentClassOwner {
			t.Fatalf("%s reports AgentClass %q, want %q", tool.Name, tool.AgentClass, AgentClassOwner)
		}
	}
	for _, name := range hostToolNames {
		if !found[name] {
			t.Fatalf("Owner manifest is missing %s", name)
		}
	}
}

// TestHostOpsAvailableOnceWired: once HostActions is wired (Tier 3 on)
// and authority admits the caller, the family reports Available on the
// Owner surface.
func TestHostOpsAvailableOnceWired(t *testing.T) {
	gw := newTestToolGateway(newTestStore(t), nil)
	gw.HostActions = &domestic.Gateway{}
	manifest := gw.Capabilities(false)
	seen := map[string]bool{}
	for _, tool := range manifest.Tools {
		if strings.HasPrefix(tool.Name, domestic.HostOpsCapability+".") {
			seen[tool.Name] = true
			if !tool.Available {
				t.Fatalf("%s reports unavailable even though HostActions is wired and authority admits it: %s",
					tool.Name, tool.Reason)
			}
		}
	}
	for _, name := range hostToolNames {
		if !seen[name] {
			t.Fatalf("Owner manifest is missing %s once wired", name)
		}
	}
}
