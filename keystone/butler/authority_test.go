package butler

// The two authority paths, and what each one does when something is
// missing (design §10, §10.1, §18.6).
//
// Every negative case here was run against a deliberately broken
// implementation before it was trusted — see the mutation notes on each
// one. A passing suite proves nothing until it has been shown to fail.

import (
	"context"
	"strings"
	"testing"
)

// recordingAuthorizer answers yes to everything and remembers what it
// was asked, so a test can assert what the gateway put on the query as
// well as what came back.
type recordingAuthorizer struct {
	seen []ToolAuthorityQuery
}

func (r *recordingAuthorizer) AuthorizeTool(q ToolAuthorityQuery) (ToolDecision, error) {
	r.seen = append(r.seen, q)
	return ToolDecision{Allowed: true}, nil
}

// A person's tool call with no authorizer is REFUSED. This is the
// permissive default dying where it matters, and it is the one test
// this slice exists for.
//
// Mutation: restoring `return ToolDecision{Allowed: structural}` on the
// person-facing branch makes this fail.
func TestPersonFacingExecutionWithNoAuthorizerIsDenied(t *testing.T) {
	gw := NewToolGateway(nil, nil)
	gw.SetCaller(CallerPersonFacing, "mika", testConnection)

	d := gw.checkTool("get_system_overview", gw.tools["get_system_overview"])
	if d.Allowed {
		t.Fatal("a person's tool call was allowed with nothing able to say whether they may")
	}
	if !strings.Contains(d.Reason, "mika") {
		t.Fatalf("the refusal does not say who was refused: %q", d.Reason)
	}

	// And at the dispatch boundary, not merely in the seam.
	out, err := gw.Dispatch(context.Background(), "get_system_overview", nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !out.Rejected {
		t.Fatalf("dispatch ran the tool anyway: %q", out.Content)
	}
}

// The deliberate behaviour that must not regress. "No administrative
// controls are configured" is a real state, and a deployment that has
// never configured one still answers questions about what it registers.
//
// Mutation: deleting the CallerAdministrative branch — so every path
// falls through to the person-facing rules — makes this fail. That is
// the OVER-CORRECTION this test exists to catch, and it is why the
// slice is scoped rather than a blanket removal.
func TestAdministrativePathStillAllowsStructuralWithNoAuthorizer(t *testing.T) {
	gw := NewToolGateway(nil, nil).ForAdministration()

	d := gw.checkTool("get_system_overview", gw.tools["get_system_overview"])
	if !d.Allowed {
		t.Fatalf("an administrative check was refused for want of controls: %q", d.Reason)
	}

	// A household inventory is the same: no member, no Connection, and
	// still a usable answer about what the surface registers.
	hh := NewHouseholdGateway("", HouseholdSeams{}).ForAdministration()
	if !hh.checkTool("get_household_events", hh.tools["get_household_events"]).Allowed {
		t.Fatal("the family inventory was refused for want of controls")
	}
}

// The two paths are told apart by what the caller SAID, never by what
// a query happens to carry. A query that states neither is refused.
//
// Mutation: defaulting the switch's zero value to CallerAdministrative
// — the shape "an empty Person must mean an administrator is asking" —
// makes this fail. So does inferring the path from q.Person == "",
// because the query below carries a Person and an authorizer that would
// say yes, and is still refused for not having said which path it is on.
func TestTheTwoPathsAreDistinguishedExplicitly(t *testing.T) {
	auth := &recordingAuthorizer{}

	unstated := ToolAuthorityQuery{
		Person: "mika", Connection: testConnection,
		AgentClass: string(AgentClassOwner), Tool: "get_system_overview", Structural: true,
	}
	if d := runAuthority(auth, unstated); d.Allowed {
		t.Fatal("a query that never said which path it was on was allowed")
	}
	if len(auth.seen) != 0 {
		t.Fatal("an unstated query reached the resolver")
	}

	// Both entry points STATE it, and the resolver sees which.
	if d := authorizeForPerson(auth, unstated); !d.Allowed {
		t.Fatalf("a stated person-facing query was refused: %q", d.Reason)
	}
	if d := authorizeAdministrative(auth, unstated); !d.Allowed {
		t.Fatalf("a stated administrative query was refused: %q", d.Reason)
	}
	if len(auth.seen) != 2 {
		t.Fatalf("the resolver saw %d queries, want 2", len(auth.seen))
	}
	if auth.seen[0].Caller != CallerPersonFacing || auth.seen[1].Caller != CallerAdministrative {
		t.Fatalf("the resolver could not tell the paths apart: %v, %v",
			auth.seen[0].Caller, auth.seen[1].Caller)
	}
}

// Missing identity fails closed — §10.1 says so in as many words — and
// it fails closed BEFORE the resolver is consulted, because a resolver
// asked about nobody is a resolver that may answer about anybody.
//
// Mutation: moving the Person check below the a.AuthorizeTool call
// makes the second assertion fail while the first still passes, which
// is exactly the kind of escape slice 9 saw four of.
func TestMissingPersonFailsClosed(t *testing.T) {
	auth := &recordingAuthorizer{}
	q := ToolAuthorityQuery{
		Connection: testConnection,
		AgentClass: string(AgentClassOwner), Tool: "get_system_overview", Structural: true,
	}
	d := authorizeForPerson(auth, q)
	if d.Allowed {
		t.Fatal("a session that does not say who is asking was allowed")
	}
	if !strings.Contains(d.Reason, "who is asking") {
		t.Fatalf("the refusal does not name the missing identity: %q", d.Reason)
	}
	if len(auth.seen) != 0 {
		t.Fatal("a query with no person reached the resolver")
	}
}

// The Connection leg, carried and failing closed. What a Connection
// ADMITS is slice 11's (#212) to define; that it must be NAMED is this
// slice's, and it is enforced here rather than left as a field nobody
// fills in.
//
// Mutation: treating an empty Connection as "no ceiling, carry on" —
// the shape the connection ceiling itself uses for a nil Permits —
// makes this fail.
func TestMissingConnectionFailsClosed(t *testing.T) {
	auth := &recordingAuthorizer{}
	q := ToolAuthorityQuery{
		Person:     "mika",
		AgentClass: string(AgentClassOwner), Tool: "get_system_overview", Structural: true,
	}
	d := authorizeForPerson(auth, q)
	if d.Allowed {
		t.Fatal("a session that does not say where it arrived from was allowed")
	}
	if !strings.Contains(d.Reason, "connection") {
		t.Fatalf("the refusal does not name the missing connection: %q", d.Reason)
	}
	if len(auth.seen) != 0 {
		t.Fatal("a query with no connection reached the resolver")
	}
}

// resourceAwareAuthorizer is a resolver that says yes to every tool and
// carries a set of resource rules.
type resourceAwareAuthorizer struct {
	rules ResourceRules
}

func (r resourceAwareAuthorizer) AuthorizeTool(ToolAuthorityQuery) (ToolDecision, error) {
	return ToolDecision{Allowed: true}, nil
}

func (r resourceAwareAuthorizer) AdmitResource(q ToolAuthorityQuery) (ToolDecision, error) {
	return r.rules.AdmitResource(q)
}

// THE CLASS-CLOSURE TEST, and the most important one in the slice.
//
// A resource kind this package has never heard of is refused by the
// general rule, and stays refused until a rule FOR THAT KIND admits it.
// The kind below is invented here, in the test, and appears nowhere in
// the package: that is the point. If the general rule could answer for
// it, the defect class §10 was written to close would be open again.
//
// Mutations, both directions:
//   - admitting an unknown kind (`if !ok { return allowed }` in
//     ResourceRules.AdmitResource) makes the first case fail;
//   - refusing every resource outright, rule or no rule, makes the
//     second case fail. Over-correction is a defect here too: a general
//     rule that can never be satisfied is not a closed class, it is a
//     broken one, and a resource whose own rule says yes must be
//     reachable.
func TestGeneralRuleRequiresResourceGrantsWithoutEnumeratingThem(t *testing.T) {
	// A resource kind invented for this test. Nothing in the package
	// knows the word.
	const kind = "greenhouse_sensor"

	base := ToolAuthorityQuery{
		Person: "mika", Connection: testConnection,
		AgentClass: string(AgentClassOwner), Tool: "get_system_overview", Structural: true,
		Resource: ResourceRef{Kind: kind, ID: "bed-3"},
	}

	// No rule for it: the general rule refuses, and says why in terms
	// of the kind rather than the tool.
	none := resourceAwareAuthorizer{rules: ResourceRules{}}
	d := authorizeForPerson(none, base)
	if d.Allowed {
		t.Fatal("the general rule admitted a resource kind nothing has a rule for")
	}
	if !strings.Contains(d.Reason, kind) {
		t.Fatalf("the refusal does not name the resource kind: %q", d.Reason)
	}

	// A resolver that has no resource rules at all — not even an empty
	// set — is the same answer. Layer 2 missing is layer 2 refusing.
	if authorizeForPerson(&recordingAuthorizer{}, base).Allowed {
		t.Fatal("a resolver with no resource rules admitted a resource")
	}

	// Add the kind's own rule, and it is reachable — but only through
	// that rule, and only for what the rule itself permits.
	admitted := resourceAwareAuthorizer{rules: ResourceRules{
		kind: func(q ToolAuthorityQuery) (ToolDecision, error) {
			if q.Resource.ID != "bed-3" {
				return ToolDecision{Allowed: false, Reason: "not that one"}, nil
			}
			return ToolDecision{Allowed: true}, nil
		},
	}}
	if d := authorizeForPerson(admitted, base); !d.Allowed {
		t.Fatalf("a resource its own rule admits was refused: %q", d.Reason)
	}

	other := base
	other.Resource.ID = "bed-9"
	if authorizeForPerson(admitted, other).Allowed {
		t.Fatal("the general rule reached past the resource's own rule")
	}

	// And a DIFFERENT new kind is still refused by the same resolver:
	// admitting one kind admits one kind.
	second := base
	second.Resource.Kind = "beehive_scale"
	if authorizeForPerson(admitted, second).Allowed {
		t.Fatal("a rule for one resource kind admitted another")
	}
}

// The manifest must not offer a model a tool that dispatch would
// refuse. The two now have a Person leg to disagree about, and they
// share one call so they cannot.
//
// Mutation: giving the manifest its own authorize call with a
// hard-coded administrative caller — the shape the code had before this
// slice, where describing and dispatching each built their own query —
// makes this fail on the family surface's first tool.
func TestDescribingAndDispatchingAgree(t *testing.T) {
	seams := HouseholdSeams{
		Events: func(context.Context, HouseholdLookup) ([]HouseholdFact, error) { return nil, nil },
	}
	// A session that was never told where it arrived from: every tool
	// is refused at dispatch, so none may be offered.
	gw := NewHouseholdGateway("sam", seams)
	gw.SetAuthorizer(permitAll{})
	gw.SetCaller(CallerPersonFacing, "sam", "")

	m := gw.Capabilities(false)
	if len(m.Tools) == 0 {
		t.Fatal("the manifest listed no tools at all, so it cannot be compared")
	}
	for _, tool := range m.Tools {
		out, err := gw.Dispatch(context.Background(), tool.Name, nil)
		if err != nil {
			t.Fatalf("Dispatch(%s): %v", tool.Name, err)
		}
		if tool.Available && out.Rejected {
			t.Fatalf("%s was offered to the model and refused at dispatch: %q",
				tool.Name, out.Content)
		}
	}
}
