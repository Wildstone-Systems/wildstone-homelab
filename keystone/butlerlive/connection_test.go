package butlerlive

import (
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/portal"
)

// yes is a resolver that permits everything, standing in for a Butler
// whose grants, profile and administrative controls all say go ahead.
// It is the case that matters: the ceiling has to hold when everything
// underneath it is a yes.
type yes struct{ asked []string }

func (y *yes) AuthorizeTool(q butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	y.asked = append(y.asked, q.Tool)
	return butler.ToolDecision{Allowed: true, Reason: "granted"}, nil
}

// no is a resolver that refuses everything, for the other direction: a
// ceiling must not be able to turn a no into a yes.
type no struct{}

func (no) AuthorizeTool(butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	return butler.ToolDecision{Allowed: false, Reason: "an administrator turned that off"}, nil
}

// An operator tool is refused on the household connection even when
// everything underneath it permits it — which is the implementation
// package's acceptance criterion, and the case that a filter applied
// only when building a tool list would miss.
func TestTheHouseholdConnectionRefusesOperatorToolsWhateverIsUnderneath(t *testing.T) {
	inner := &yes{}
	a := WithConnection(inner, portal.Permits)

	for _, tool := range []string{
		"infra.restart_container",
		"containers.restart",
		"nodes.drain",
		"packages.install",
		"capabilities.enable",
		"identity.manage",
		"secrets.reveal",
		"policy.exempt",
		"plans.approve",
		"audit.read",
		"backups.run",
	} {
		got, err := a.AuthorizeTool(butler.ToolAuthorityQuery{Tool: tool, Structural: true})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if got.Allowed {
			t.Errorf("%s was allowed from the household dashboard", tool)
		}
		// Not requestable: no approval can grant this HERE, and offering
		// to ask would send a member to the owner for nothing.
		if got.Requestable {
			t.Errorf("%s was offered as requestable", tool)
		}
		// The refusal names the surface, not an error code.
		if !strings.Contains(got.Reason, "dashboard") {
			t.Errorf("%s reason = %q, want it to name the surface", tool, got.Reason)
		}
	}
	// And the resolver underneath was never even asked about them: the
	// ceiling answers first, so a resolver bug cannot leak through it.
	if len(inner.asked) != 0 {
		t.Errorf("the authority underneath was consulted for denied tools: %v", inner.asked)
	}
}

// The household's own tools still work, and still go through the
// resolver that actually decides them. A ceiling that refused everything
// it had not been told about would take working household function away
// and call it security.
func TestTheHouseholdConnectionLetsHouseholdToolsThrough(t *testing.T) {
	inner := &yes{}
	a := WithConnection(inner, portal.Permits)
	for _, tool := range []string{
		"pantry.read_list", "pantry.write", "locks.unlock", "home.lights",
		"notifications.notify", "calendar.read", "wiki.internal", "media.search",
	} {
		got, err := a.AuthorizeTool(butler.ToolAuthorityQuery{Tool: tool, Structural: true})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if !got.Allowed {
			t.Errorf("%s was refused: %q", tool, got.Reason)
		}
	}
	if len(inner.asked) == 0 {
		t.Error("nothing reached the resolver underneath; the ceiling is answering everything")
	}
}

// A ceiling only ever lowers. It must not be able to turn a refusal
// underneath it into permission.
func TestTheCeilingCannotRaiseWhatIsUnderneathIt(t *testing.T) {
	a := WithConnection(no{}, portal.Permits)
	got, err := a.AuthorizeTool(butler.ToolAuthorityQuery{Tool: "pantry.read_list", Structural: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Fatal("a tool the authority refused was allowed by the connection")
	}
	if !strings.Contains(got.Reason, "administrator") {
		t.Errorf("reason = %q, want the resolver's own answer to survive", got.Reason)
	}
}

// A denied family must not claim a tool that merely starts with the same
// letters. "infra" denies infra.* and not infra_helper.read.
func TestDeniedFamiliesStopAtADottedBoundary(t *testing.T) {
	a := WithConnection(&yes{}, portal.Permits)
	for _, tool := range []string{"infra_helper.read", "nodescan.list", "packagesafe.read", "secretsanta.read"} {
		got, _ := a.AuthorizeTool(butler.ToolAuthorityQuery{Tool: tool, Structural: true})
		if !got.Allowed {
			t.Errorf("%s was denied by a prefix it only shares letters with", tool)
		}
	}
}

// A turn that names no connection is unaffected: the chat adapters and
// the Console each have their own ceiling, and this one is not theirs.
func TestNoConnectionMeansNoCeiling(t *testing.T) {
	inner := &yes{}
	if got := WithConnection(inner, nil); got != butler.ToolAuthorizer(inner) {
		t.Fatal("a nil connection wrapped the authorizer anyway")
	}
	// And with no resolver underneath either, an unconfigured
	// deployment still behaves as one: structurally present tools work,
	// which is not the same answer as "everything is disabled".
	a := WithConnection(nil, portal.Permits)
	got, err := a.AuthorizeTool(butler.ToolAuthorityQuery{Tool: "pantry.read_list", Structural: true})
	if err != nil || !got.Allowed {
		t.Errorf("unconfigured deployment: allowed=%v err=%v", got.Allowed, err)
	}
	// The ceiling still applies, even with nothing underneath.
	got, _ = a.AuthorizeTool(butler.ToolAuthorityQuery{Tool: "infra.restart_container", Structural: true})
	if got.Allowed {
		t.Error("the ceiling lapsed when there was no resolver under it")
	}
}
