package butler

// P2's evidence (ADR-W030): Household cannot discover or dispatch
// Operator-only tools, QA-only and unsupported resource kinds are
// excluded, and manifest and executor coverage cannot drift.

import (
	"context"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// operatorOnlyTools are the names a household session must never see or
// be able to run. Derived from the operator gateway rather than typed
// out, so a tool added there is covered here the day it lands.
func operatorOnlyTools(t *testing.T) []string {
	t.Helper()
	family := NewHouseholdGateway("mika", HouseholdSeams{})
	familyNames := map[string]bool{}
	for name := range family.tools {
		familyNames[name] = true
	}
	var out []string
	for name := range NewToolGateway(nil, nil).tools {
		if !familyNames[name] {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		t.Fatal("no operator-only tools found; the disjointness this test protects has gone")
	}
	return out
}

// The household surface cannot DISCOVER an operator tool: not through
// its offered specs, not through its manifest, and not through the
// self-capability tool's rendered answer.
func TestHouseholdCannotDiscoverOperatorTools(t *testing.T) {
	seams, _ := householdSeams(t)
	gw := NewHouseholdGateway("mika", seams)
	only := operatorOnlyTools(t)

	m := gw.Capabilities(false)
	if m.Profile != ProfileHousehold {
		t.Fatalf("profile = %q, want household", m.Profile)
	}
	rendered := m.Describe()
	for _, name := range only {
		for _, tool := range m.Tools {
			if tool.Name == name {
				t.Fatalf("household manifest names the operator-only tool %q", name)
			}
		}
		if strings.Contains(rendered, name) {
			t.Fatalf("household capability description names the operator-only tool %q:\n%s", name, rendered)
		}
	}

	// And the tool itself, dispatched for real, says the same thing.
	out, err := gw.Dispatch(context.Background(), "describe_capabilities", nil)
	if err != nil {
		t.Fatalf("Dispatch describe_capabilities: %v", err)
	}
	for _, name := range only {
		if strings.Contains(out.Content, name) {
			t.Fatalf("describe_capabilities on the family surface leaked %q:\n%s", name, out.Content)
		}
	}
	if !strings.Contains(out.Content, "get_household_events") {
		t.Fatalf("describe_capabilities did not report this surface's own tools:\n%s", out.Content)
	}
}

// And cannot DISPATCH one. The family gateway's map does not contain
// them, so naming one is an unknown tool rather than a refused one —
// the capability is absent, not guarded.
func TestHouseholdCannotDispatchOperatorTools(t *testing.T) {
	seams, _ := householdSeams(t)
	gw := NewHouseholdGateway("mika", seams)
	for _, name := range operatorOnlyTools(t) {
		if _, err := gw.Dispatch(context.Background(), name, map[string]any{}); err == nil {
			t.Fatalf("family gateway dispatched the operator-only tool %q", name)
		}
	}
}

// A household manifest reports no resource kinds on any verdict: this
// surface has no Statefile and no propose path, so a kind list would
// describe a capability that structurally does not exist.
func TestHouseholdManifestHasNoResourceKinds(t *testing.T) {
	seams, _ := householdSeams(t)
	gw := NewHouseholdGateway("mika", seams)
	for _, readOnly := range []bool{false, true} {
		if kinds := gw.Capabilities(readOnly).ProposableKinds; len(kinds) != 0 {
			t.Fatalf("household manifest (readOnly=%t) lists kinds %v", readOnly, kinds)
		}
	}
}

// A QA-only session has no propose path, so it is offered no kinds and
// no mutating tools — the manifest agrees with the enforcement point
// rather than describing the full surface it is not running.
func TestQAOnlyManifestExcludesKindsAndMutatingTools(t *testing.T) {
	gw := NewToolGateway(newTestStore(t), nil)

	full := gw.Capabilities(false)
	if len(full.ProposableKinds) != len(resource.KnownKinds()) {
		t.Fatalf("full manifest kinds = %d, want the whole registry (%d)",
			len(full.ProposableKinds), len(resource.KnownKinds()))
	}
	if full.ModelVerdict != VerdictFull {
		t.Fatalf("verdict = %q, want %q", full.ModelVerdict, VerdictFull)
	}

	qa := gw.Capabilities(true)
	if qa.ModelVerdict != VerdictQAOnly {
		t.Fatalf("verdict = %q, want %q", qa.ModelVerdict, VerdictQAOnly)
	}
	if len(qa.ProposableKinds) != 0 {
		t.Fatalf("QA-only manifest lists proposable kinds %v", qa.ProposableKinds)
	}
	for _, tool := range qa.Tools {
		if tool.Mutating && tool.Available {
			t.Fatalf("QA-only manifest offers the mutating tool %q", tool.Name)
		}
	}
	if !strings.Contains(qa.Describe(), "propose_change") {
		t.Fatal("QA-only description hides propose_change entirely; it should say it is withheld and why")
	}
	// The two sessions are different capabilities and must not share a
	// fingerprint — that value is what a work trace records to tell one
	// run's offered surface from another's (ADR-W030 A11).
	if full.Fingerprint() == qa.Fingerprint() {
		t.Fatal("full and QA-only sessions produce the same manifest fingerprint")
	}
}

// Coverage cannot drift: every registered tool appears in its surface's
// manifest, and every manifest entry is a registered tool. A tool added
// without a manifest entry would be a capability nothing can report;
// an entry without a tool would be a promise nothing can keep.
func TestManifestCoversEveryRegisteredTool(t *testing.T) {
	seams, _ := householdSeams(t)
	admin := NewToolGateway(newTestStore(t), nil)
	family := NewHouseholdGateway("mika", seams)

	for _, tc := range []struct {
		name       string
		registered map[string]bool
		manifest   CapabilityManifest
	}{
		{"operator", namesOf(admin.tools), admin.Capabilities(false)},
		{"household", namesOfHousehold(family.tools), family.Capabilities(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[string]bool{}
			for _, tool := range tc.manifest.Tools {
				if !tc.registered[tool.Name] {
					t.Fatalf("manifest names %q, which is not a registered tool", tool.Name)
				}
				if tool.Summary == "" {
					t.Fatalf("manifest entry %q has no summary", tool.Name)
				}
				seen[tool.Name] = true
			}
			for name := range tc.registered {
				if !seen[name] {
					t.Fatalf("registered tool %q is missing from the manifest", name)
				}
			}
		})
	}
}

func namesOf(m map[string]toolEntry) map[string]bool {
	out := map[string]bool{}
	for name := range m {
		out[name] = true
	}
	return out
}

func namesOfHousehold(m map[string]householdEntry) map[string]bool {
	out := map[string]bool{}
	for name := range m {
		out[name] = true
	}
	return out
}

// Executor coverage cannot drift either: the manifest's actions come
// from the same Vocabulary seam list_household_actions reads, and an
// action above the domestic tier — which this surface cannot dispatch —
// is left out rather than advertised and refused.
func TestManifestActionsMatchWiredExecutors(t *testing.T) {
	seams, _ := householdSeams(t)
	seams.Vocabulary = func() (HouseholdVocabulary, error) {
		return HouseholdVocabulary{Actions: []HouseholdAction{
			{Tool: "notifications.notify", Tier: "domestic"},
			{Tool: "pantry.read_list", Tier: "read"},
			{Tool: "containers.restart", Tier: "infrastructure"},
		}}, nil
	}
	gw := NewHouseholdGateway("mika", seams)
	m := gw.Capabilities(false)
	var got []string
	for _, a := range m.Actions {
		got = append(got, a.Tool)
	}
	if len(got) != 2 || got[0] != "notifications.notify" || got[1] != "pantry.read_list" {
		t.Fatalf("manifest actions = %v, want the domestic and read tiers only", got)
	}
	if strings.Contains(m.Describe(), "containers.restart") {
		t.Fatal("manifest advertises an infrastructure-tier action this surface cannot dispatch")
	}
}

// A manifest that cannot read the deployment's actions says so rather
// than reporting an empty list, which would read as "nothing is wired".
func TestManifestReportsAnUnreadableVocabulary(t *testing.T) {
	seams, _ := householdSeams(t)
	seams.Vocabulary = func() (HouseholdVocabulary, error) {
		return HouseholdVocabulary{}, context.DeadlineExceeded
	}
	gw := NewHouseholdGateway("mika", seams)
	if !strings.Contains(gw.Capabilities(false).Describe(), "could not be read") {
		t.Fatal("an unreadable action list was reported as no actions")
	}
}

// The fingerprint is stable across calls and moves when the offered
// surface does — it is the manifest_version a work trace records.
func TestManifestFingerprintIsStableAndSensitive(t *testing.T) {
	gw := NewToolGateway(newTestStore(t), nil)
	first := gw.Capabilities(false).Fingerprint()
	if first != gw.Capabilities(false).Fingerprint() {
		t.Fatal("fingerprint is not stable across calls")
	}
	// Removing the Statefile changes what is available, and the
	// fingerprint has to notice.
	bare := NewToolGateway(nil, nil)
	if bare.Capabilities(false).Fingerprint() == first {
		t.Fatal("fingerprint did not change when tools became unavailable")
	}
}

// The operator log reader is offered, bounded, and reports honestly
// when this deployment has no node read surface.
func TestContainerLogsToolIsOperatorOnlyAndBounded(t *testing.T) {
	family := NewHouseholdGateway("mika", HouseholdSeams{})
	if _, ok := family.tools["get_container_logs"]; ok {
		t.Fatal("the family surface registered the container log reader")
	}
	gw := NewToolGateway(newTestStore(t), nil)
	out, err := gw.Dispatch(context.Background(), "get_container_logs", map[string]any{
		"container": "immich-server", "node": "n1",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(out.Content, "no node read surface") {
		t.Fatalf("unconfigured reader answered: %q", out.Content)
	}
	if !strings.Contains(gw.tools["get_container_logs"].Spec.Description, "Read-only") {
		t.Fatal("the log tool does not tell the model it is read-only")
	}
	if !gw.tools["get_container_logs"].Untrusted {
		t.Fatal("container logs are not marked untrusted; application output is attacker-influenceable")
	}
}
