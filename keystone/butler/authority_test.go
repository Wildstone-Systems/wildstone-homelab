package butler

// The two authority paths, and what each one does when something is
// missing (design §10, §10.1, §18.6).
//
// Every negative case here was run against a deliberately broken
// implementation before it was trusted — see the mutation notes on each
// one. A passing suite proves nothing until it has been shown to fail.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
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

func (r *recordingAuthorizer) AdmitPerson(ToolAuthorityQuery) (ToolDecision, error) {
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
	// refusePerson makes layer 1 say no for one person, so a test can
	// show which layer answered.
	refusePerson string
}

func (r resourceAwareAuthorizer) AuthorizeTool(ToolAuthorityQuery) (ToolDecision, error) {
	return ToolDecision{Allowed: true}, nil
}

// admits reports whether layer 1 lets the caller through at all. nil
// means yes, which is what most of these tests want.
func (r resourceAwareAuthorizer) AdmitPerson(q ToolAuthorityQuery) (ToolDecision, error) {
	if r.refusePerson != "" && q.Person == r.refusePerson {
		return ToolDecision{Allowed: false, Reason: r.refusePerson + " is no longer admitted"}, nil
	}
	return ToolDecision{Allowed: true}, nil
}

func (r resourceAwareAuthorizer) AdmitResource(q ToolAuthorityQuery) (ToolDecision, error) {
	return r.rules.AdmitResource(q)
}

// Layer 1 answers before layer 2 is asked anything.
//
// §10 stacks the layers in order and a later one never widens an
// earlier one — but ORDER is not only about widening. A person nobody
// has admitted must be told they may not do work at all, not handed a
// refusal that describes a resource: that answer says something about
// the resource to somebody with no standing to hear it, and it means a
// resource rule ran a lookup for a caller layer 1 had not admitted.
//
// The first version of this slice checked only that the Person FIELD
// was filled in, and then ran the resource rule, and only reached the
// real admission check inside the resolver afterwards. That is §10's
// ordering inverted, in the one function whose whole job is the
// ordering. Found by a review bot on #286.
//
// Mutation: moving the AdmitPerson call below the resource block makes
// both halves of this fail.
func TestLayerOneAnswersBeforeLayerTwoIsAsked(t *testing.T) {
	const kind = "greenhouse_sensor"
	var resourceRuleRan bool

	a := resourceAwareAuthorizer{
		refusePerson: "ghost",
		rules: ResourceRules{
			kind: func(ToolAuthorityQuery) (ToolDecision, error) {
				resourceRuleRan = true
				return ToolDecision{Allowed: true}, nil
			},
		},
	}
	q := ToolAuthorityQuery{
		Person: "ghost", Connection: testConnection,
		AgentClass: string(AgentClassOwner), Tool: "get_system_overview", Structural: true,
		Resource: ResourceRef{Kind: kind, ID: "bed-3"},
	}

	d := authorizeForPerson(a, q)
	if d.Allowed {
		t.Fatal("a person layer 1 refused was allowed")
	}
	// The refusal is about the PERSON, not about the resource.
	if strings.Contains(d.Reason, kind) {
		t.Fatalf("an unadmitted caller was told about the resource: %q", d.Reason)
	}
	if !strings.Contains(d.Reason, "ghost") {
		t.Fatalf("the refusal does not name the person: %q", d.Reason)
	}
	// And the resource's own rule never ran for them.
	if resourceRuleRan {
		t.Fatal("a resource rule ran for a caller layer 1 had not admitted")
	}

	// An admitted caller still reaches layer 2, so this is an ordering
	// test rather than a test that layer 2 stopped working.
	q.Person = "mika"
	if d := authorizeForPerson(a, q); !d.Allowed {
		t.Fatalf("an admitted caller was refused: %q", d.Reason)
	}
	if !resourceRuleRan {
		t.Fatal("layer 2 was never reached for an admitted caller")
	}
}

// A resolver that cannot answer layer 1 refuses every person-facing
// call, the same way one with no resource rules admits no resource.
//
// Mutation: treating a resolver with no AdmitPerson as "layer 1 passes"
// makes this fail — and that is the shape the code had before, where
// the real check lived inside AuthorizeTool and anything reached
// beforehand ran unguarded.
func TestAResolverThatCannotAnswerLayerOneRefuses(t *testing.T) {
	// Implements AuthorizeTool and nothing else.
	var bare cannotAnswerLayerOne
	d := authorizeForPerson(bare, ToolAuthorityQuery{
		Person: "mika", Connection: testConnection,
		AgentClass: string(AgentClassOwner), Tool: "get_system_overview", Structural: true,
	})
	if d.Allowed {
		t.Fatal("a resolver that cannot answer layer 1 allowed a person's tool call")
	}
}

type cannotAnswerLayerOne struct{}

func (cannotAnswerLayerOne) AuthorizeTool(ToolAuthorityQuery) (ToolDecision, error) {
	return ToolDecision{Allowed: true}, nil
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

// THE OFFERED LIST is what actually reaches a provider, and it must not
// name a tool dispatch would refuse.
//
// This is a third place that has to agree, and the one the first
// version of this slice missed: TestDescribingAndDispatchingAgree below
// compared the MANIFEST against dispatch and passed, while
// OfferedSpecs — Session.specs and Runner.Run both send it — still
// returned the raw registry. A model was handed tools its person could
// not use. Found by a review bot on #286, which is why the test that
// should have caught it is written from the call the provider makes
// rather than from the one that is easiest to reach.
//
// A name is itself a disclosure: a member restricted from a capability
// should not learn it exists here from the tool list.
//
// Mutation: returning gw.ToolSpecs() from OfferedSpecs — the code as it
// stood — makes this fail on the first tool.
func TestTheOfferedListNeverNamesAToolDispatchWouldRefuse(t *testing.T) {
	// A session that says who is asking and where from, with a resolver
	// that refuses one specific tool. Nothing structural is withheld,
	// so any tool missing from the offered list is missing because
	// authority said so.
	// A real Statefile: a gateway with none panics inside the read
	// tools, and this test dispatches every tool it was offered.
	store, err := statefile.New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatalf("statefile.New: %v", err)
	}
	gw := NewToolGateway(store, nil)
	gw.SetAuthorizer(refuseOne{tool: "get_system_overview"})
	gw.SetCaller(CallerPersonFacing, "mika", testConnection)

	offered := gw.OfferedSpecs(false)
	if len(offered) == 0 {
		t.Fatal("nothing was offered at all, so the comparison proves nothing")
	}
	for _, spec := range offered {
		if spec.Name == "get_system_overview" {
			t.Fatal("a refused tool was offered to the model by name")
		}
		out, err := gw.Dispatch(context.Background(), spec.Name, nil)
		if err != nil {
			t.Fatalf("Dispatch(%s): %v", spec.Name, err)
		}
		if out.Rejected {
			t.Fatalf("%s was offered to the model and refused at dispatch: %q",
				spec.Name, out.Content)
		}
	}

	// The family surface answers the same way.
	hh := NewHouseholdGateway("sam", HouseholdSeams{
		Events: func(context.Context, HouseholdLookup) ([]HouseholdFact, error) { return nil, nil },
	})
	hh.SetAuthorizer(refuseOne{tool: "get_household_events"})
	hh.SetCaller(CallerPersonFacing, "sam", testConnection)
	for _, spec := range hh.OfferedSpecs(false) {
		if spec.Name == "get_household_events" {
			t.Fatal("a refused family tool was offered to the model by name")
		}
	}

	// And a session nobody told who is asking offers nothing at all,
	// rather than offering everything.
	bare := NewToolGateway(nil, nil)
	if got := bare.OfferedSpecs(false); len(got) != 0 {
		t.Fatalf("an unstated session offered %d tool(s)", len(got))
	}
}

// refuseOne permits everything except one named tool.
type refuseOne struct{ tool string }

func (r refuseOne) AdmitPerson(ToolAuthorityQuery) (ToolDecision, error) {
	return ToolDecision{Allowed: true}, nil
}

func (r refuseOne) AuthorizeTool(q ToolAuthorityQuery) (ToolDecision, error) {
	if q.Tool == r.tool {
		return ToolDecision{Allowed: false, Reason: "an administrator turned that off"}, nil
	}
	return ToolDecision{Allowed: q.Structural}, nil
}

// A refusal from Permits goes through the same pipeline a dispatched
// refusal does: redacted, and bounded.
//
// A refusal is CONTENT. It can carry administrator-supplied text —
// runtimedb appends butler_tool_scope.reason to it — so a credential
// pasted into a control's reason, or an essay, would otherwise reach
// the model unredacted and unbounded through the one path that never
// calls Execute. ADR-W006 B3 centralizes redaction and the size bound
// precisely "so a new tool cannot forget it"; Permits was new and
// forgot. Found by a review bot on #287.
//
// THE TWO HAZARDS NEED SEPARATE REASONS. The first version of this test
// put a secret and nine thousand filler bytes in one string, and
// redaction collapsed the filler along with the secret — so the size
// assertion never bit, and a mutation that dropped the bound escaped.
// The long reason below is deliberately free of anything secret-shaped.
//
// Mutations: returning d.Reason fails the redaction case; redacting
// without bounding fails the length case.
func TestAPermitsRefusalIsRedactedAndBounded(t *testing.T) {
	const secret = "password: hunter2-the-real-one"
	// Prose, so redaction has nothing to take and only the bound can
	// shorten it.
	long := strings.Repeat("the administrator wrote a very long explanation. ", 300)

	t.Run("redacted", func(t *testing.T) {
		gw := newPermitsGateway(secret)
		_, why := gw.Permits("get_system_overview", false)
		if strings.Contains(why, "hunter2-the-real-one") {
			t.Fatalf("a secret in an administrator's reason reached the caller: %q", why)
		}
	})

	t.Run("bounded", func(t *testing.T) {
		gw := newPermitsGateway(long)
		_, why := gw.Permits("get_system_overview", false)
		if len(why) > 400 {
			t.Fatalf("the refusal is %d bytes, past the session's 200-byte bound", len(why))
		}
	})

	t.Run("identical to dispatch", func(t *testing.T) {
		// The two paths must not tell a model different stories about
		// one refusal.
		for _, reason := range []string{secret, long} {
			gw := newPermitsGateway(reason)
			_, why := gw.Permits("get_system_overview", false)
			out, err := gw.Dispatch(context.Background(), "get_system_overview", nil)
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if out.Content != why {
				t.Fatalf("retrieval and dispatch refusals differ:\n  permits:  %q\n  dispatch: %q",
					why, out.Content)
			}
		}
	})

	t.Run("family surface too", func(t *testing.T) {
		hh := NewHouseholdGateway("sam", HouseholdSeams{MaxContentBytes: 200})
		hh.SetAuthorizer(reasonAuthorizer{reason: secret + " and " + long})
		hh.SetCaller(CallerPersonFacing, "sam", testConnection)
		_, why := hh.Permits("get_household_events", false)
		if strings.Contains(why, "hunter2-the-real-one") {
			t.Fatalf("a secret reached the caller on the family surface: %q", why)
		}
		if len(why) > 400 {
			t.Fatalf("the family refusal is %d bytes, past its bound", len(why))
		}
	})
}

func newPermitsGateway(reason string) *ToolGateway {
	gw := NewToolGateway(nil, nil)
	gw.MaxContentBytes = 200
	gw.SetAuthorizer(reasonAuthorizer{reason: reason})
	gw.SetCaller(CallerPersonFacing, "mika", testConnection)
	return gw
}

// reasonAuthorizer refuses everything with one fixed reason, standing
// in for an administrator's control text.
type reasonAuthorizer struct{ reason string }

func (reasonAuthorizer) AdmitPerson(ToolAuthorityQuery) (ToolDecision, error) {
	return ToolDecision{Allowed: true}, nil
}

func (r reasonAuthorizer) AuthorizeTool(ToolAuthorityQuery) (ToolDecision, error) {
	return ToolDecision{Allowed: false, Reason: r.reason}, nil
}

// Permits refuses ANY mutating tool in a read-only session, which is
// what dispatch refuses — not the one name that happens to be mutating
// today.
//
// Permits is the sole gate on the cached-retrieval path, so a second
// mutating tool would otherwise have its recorded result served into a
// read-only session that Execute would refuse. Found by review on #287,
// where Permits tested `name == "propose_change"` while
// DispatchReadOnly tested entry.Mutating.
//
// Mutation: comparing the name instead of the flag makes this fail.
func TestPermitsRefusesAnyMutatingToolWhenReadOnly(t *testing.T) {
	gw := NewToolGateway(nil, nil)
	gw.SetAuthorizer(permitAll{})
	gw.SetCaller(CallerPersonFacing, "mika", testConnection)
	// A second mutating tool, registered for this test. There is only
	// one today, which is exactly why a name comparison looked correct.
	gw.tools["change_the_house"] = toolEntry{
		Spec:     provider.Tool{Name: "change_the_house", Description: "changes something"},
		Mutating: true,
		Handler: func(context.Context, *ToolGateway, map[string]any) (ToolOutcome, error) {
			return ToolOutcome{Content: "done"}, nil
		},
	}

	for _, name := range []string{"propose_change", "change_the_house"} {
		if allowed, _ := gw.Permits(name, true); allowed {
			t.Errorf("%s was permitted in a read-only session", name)
		}
		// And dispatch agrees, which is the property that matters.
		out, err := gw.DispatchReadOnly(context.Background(), name, nil)
		if err != nil {
			t.Fatalf("DispatchReadOnly(%s): %v", name, err)
		}
		if !out.Rejected {
			t.Errorf("%s ran in a read-only session", name)
		}
	}

	// A read tool is unaffected, so this is not a test that refuses
	// everything when readOnly is set.
	if allowed, why := gw.Permits("get_system_overview", true); !allowed {
		t.Fatalf("a read tool was refused in a read-only session: %q", why)
	}
}

// No shipped tool reaches a §10 layer-2 resource yet, and the day one
// does, it has to say so.
//
// The path from a tool's declaration through the query to the
// resource's own rule is complete and tested; what does not exist is a
// resource to put in it, because layer 2 is defined BY the resource
// (memory §11, secrets §12) and those slices bring both the tools and
// the rules. This test is what makes that a STATED fact rather than an
// assumption: a tool that starts reaching a resource fails here until
// somebody writes its rule.
//
// Mutation: declaring Reaches on any registered tool makes this fail,
// which is the point — the failure is the prompt to add the rule.
func TestNoShippedToolReachesAResourceWithoutARule(t *testing.T) {
	for name, entry := range NewToolGateway(nil, nil).tools {
		if entry.Reaches != "" {
			t.Errorf("operator tool %s reaches a %s: give that kind a ResourceRule, "+
				"then update this test to expect it", name, entry.Reaches)
		}
	}
	for name, entry := range NewHouseholdGateway("sam", HouseholdSeams{}).tools {
		if entry.Reaches != "" {
			t.Errorf("family tool %s reaches a %s: give that kind a ResourceRule, "+
				"then update this test to expect it", name, entry.Reaches)
		}
	}
}

// A tool that DECLARES a resource is checked against that resource's
// own rule, and refused when there is none.
//
// This is the wire from a tool's declaration, through the query, to
// layer 2. TestNoShippedToolReachesAResourceWithoutARule above says no
// tool uses it yet; this one says it works, so "no tool sets it" is a
// statement about the tools and not an excuse for untested plumbing.
// The first version of this slice had the wire and no test for it, and
// a mutation that cut it escaped.
//
// Mutation: dropping `Resource: ResourceRef{Kind: entry.Reaches}` from
// the query builder makes the first case fail.
func TestADeclaredResourceIsCheckedAgainstItsOwnRule(t *testing.T) {
	const kind = "greenhouse_sensor"

	withTool := func(a ToolAuthorizer) *ToolGateway {
		gw := NewToolGateway(nil, nil)
		// Registered here rather than shipped: the point is a tool that
		// reaches a resource, and no shipped tool does yet.
		gw.tools["read_greenhouse"] = toolEntry{
			Spec:    provider.Tool{Name: "read_greenhouse", Description: "read a bed"},
			Reaches: kind,
			Handler: func(context.Context, *ToolGateway, map[string]any) (ToolOutcome, error) {
				return ToolOutcome{Content: "23C"}, nil
			},
		}
		gw.SetAuthorizer(a)
		gw.SetCaller(CallerPersonFacing, "mika", testConnection)
		return gw
	}

	// No rule for the kind: refused, and the refusal names the kind.
	gw := withTool(resourceAwareAuthorizer{rules: ResourceRules{}})
	d := gw.checkTool("read_greenhouse", gw.tools["read_greenhouse"])
	if d.Allowed {
		t.Fatal("a tool declaring a resource ran with no rule for that resource")
	}
	if !strings.Contains(d.Reason, kind) {
		t.Fatalf("the refusal does not name the resource kind: %q", d.Reason)
	}

	// It is refused at dispatch and withheld from the offered list too,
	// so the three places agree about it like any other refusal.
	out, err := gw.Dispatch(context.Background(), "read_greenhouse", nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !out.Rejected {
		t.Fatalf("dispatch ran it anyway: %q", out.Content)
	}
	for _, spec := range gw.OfferedSpecs(false) {
		if spec.Name == "read_greenhouse" {
			t.Fatal("a tool refused for want of a resource rule was still offered")
		}
	}

	// With the kind's own rule, it works — otherwise this would pass
	// against an implementation that refuses every declared resource.
	ok := withTool(resourceAwareAuthorizer{rules: ResourceRules{
		kind: func(ToolAuthorityQuery) (ToolDecision, error) {
			return ToolDecision{Allowed: true}, nil
		},
	}})
	if d := ok.checkTool("read_greenhouse", ok.tools["read_greenhouse"]); !d.Allowed {
		t.Fatalf("a resource its own rule admits was refused: %q", d.Reason)
	}

	// A tool that declares NO resource is untouched by any of it.
	if !ok.checkTool("get_system_overview", ok.tools["get_system_overview"]).Allowed {
		t.Fatal("a tool that reaches no resource was refused by the resource leg")
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
