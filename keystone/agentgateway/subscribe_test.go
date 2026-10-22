package agentgateway

// The Events family's proposal half (ADR-W039 §3.6, Phase 4, #341):
// Propose's ProposableAgentSubscription branch. The match->Task->
// delivery half is keystone/automation's own evidence
// (subscriptions_test.go); the capability-narrowing-at-use half is
// invoke_test.go's.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func subscriptionDoc(t *testing.T, matchKind string, caps []string) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(agentapi.SubscriptionProposal{
		MatchKind: matchKind,
		Match:     json.RawMessage(`{"eventType":"person.arrived_home","subject":"mika","sameSubject":true}`),
		Audience:  "aud_owner", Capabilities: caps,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// TestProposeSubscriptionFilesAPlanNotAnEnabledSubscription: proposing
// never enables anything. The Plan exists and is `proposed`; no row in
// agent_subscriptions exists until an Owner approves it, which this
// package cannot do on its own (approval is api's own approvePlan
// Subscription branch — see api/handlers_agent_subscriptions_test.go).
func TestProposeSubscriptionFilesAPlanNotAnEnabledSubscription(t *testing.T) {
	f := newFixture(t)
	f.wireProposals()
	f.bindRuntime(t, "runtime-alfred-1", "alfred")
	ctx := WithPrincipal(context.Background(), "runtime-alfred-1")
	_, h := f.openTask(t, "alfred", "mika")

	ref, err := f.g.Propose(ctx, agentapi.ProposeRequest{
		Handle: h, Kind: agentapi.ProposableAgentSubscription,
		Document: subscriptionDoc(t, "event_type", []string{"get_health"}),
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if ref.ID == "" {
		t.Fatal("no plan id returned")
	}
	rec, err := f.db.GetPlan(ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != runtimedb.PlanProposed {
		t.Fatalf("plan state = %s, want proposed", rec.State)
	}

	b, err := f.db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	subs, err := f.db.ListAgentSubscriptions(b.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 0 {
		t.Fatalf("subscriptions after a mere PROPOSE = %+v, want none: nothing is enabled without approval (ADR-W039 §3.6)", subs)
	}
}

// TestProposeSubscriptionCannotWidenCapabilities: a capability this
// Agent's session is not currently offered is refused at propose time,
// before any Plan is filed for an Owner to even see.
func TestProposeSubscriptionCannotWidenCapabilities(t *testing.T) {
	f := newFixture(t)
	f.wireProposals()
	f.bindRuntime(t, "runtime-alfred-1", "alfred")
	ctx := WithPrincipal(context.Background(), "runtime-alfred-1")
	_, h := f.openTask(t, "alfred", "mika")

	before, err := f.db.ListPlans(100)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.g.Propose(ctx, agentapi.ProposeRequest{
		Handle: h, Kind: agentapi.ProposableAgentSubscription,
		Document: subscriptionDoc(t, "event_type", []string{"a_tool_this_agent_does_not_hold"}),
	})
	if err == nil {
		t.Fatal("a widening capability was accepted")
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("widening capability error = %v, want ErrRefused", err)
	}
	after, err := f.db.ListPlans(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("plan count changed from %d to %d; a refused proposal must file nothing", len(before), len(after))
	}
}
