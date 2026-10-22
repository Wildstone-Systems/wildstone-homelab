package agentgateway

// The capability-narrowing-at-use half of ADR-W039 §3.6 (Phase 4,
// #341): a Task a subscription spawned may only use ITS capability set,
// re-read live at every call — never a snapshot — so narrowing or
// disabling a subscription after it already matched still binds the
// Task it spawned.

import (
	"context"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/automation"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerlive"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// openSubscriptionTask opens a Task exactly as
// automation.SubscriptionEngine.spawn does: GrantsJSON names the
// subscription, and nothing else about it differs from an ordinary
// conversation task.
func (f *fixture) openSubscriptionTask(t *testing.T, butlerName, owner string, subID int64) (string, agentapi.Handle) {
	t.Helper()
	b, err := f.db.GetButler(butlerName)
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.db.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: owner, Owner: owner,
		Surface: b.Surface, AgentClass: b.AgentClass, Audience: runtimedb.ButlerAudiencePrivate,
		// A real subscription-spawned task carries no Connection
		// (§10.2's "a routine runs unattended and borrows nobody's
		// Connection" applies here too); this fixture's own toolset
		// happens to need one to dispatch a real tool at all, which is
		// the fixture's business, not this test's, so ConsoleConnection
		// stands in — same as openAgentTask does everywhere else in
		// this package's tests.
		OriginConnection: butlerlive.ConsoleConnection,
		Objective:        "subscription match", ObjectiveKind: runtimedb.ObjectiveEffectful,
		Retention:  runtimedb.ButlerRetentionManual,
		GrantsJSON: automation.SubscriptionGrantsJSON(subID),
	})
	if err != nil {
		t.Fatalf("InsertButlerTask: %v", err)
	}
	if err := f.db.SetTaskButler(id, butlerName); err != nil {
		t.Fatalf("SetTaskButler: %v", err)
	}
	h, err := f.ledger.IssueHandle(id)
	if err != nil {
		t.Fatalf("IssueHandle: %v", err)
	}
	return id, h
}

func TestSubscriptionTaskMayOnlyUseItsCapabilitySet(t *testing.T) {
	f := newFixture(t)
	b, err := f.db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	subID, err := f.db.InsertAgentSubscription(b.Seq, "event_type", `{}`, "", []string{"get_health"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.ApproveAgentSubscription(subID, "mika"); err != nil {
		t.Fatal(err)
	}

	_, h := f.openSubscriptionTask(t, "alfred", "mika", subID)
	res, err := f.g.Invoke(context.Background(), agentapi.InvokeRequest{Handle: h, Tool: "get_health", Args: []byte(`{"node":"n1"}`)})
	if err != nil {
		t.Fatalf("Invoke(get_health): %v", err)
	}
	if res.Outcome != agentapi.OutcomeOK {
		t.Fatalf("Invoke(get_health) = %+v, want ok — it IS in the subscription's capability set", res)
	}
}

func TestSubscriptionTaskCannotWidenPastItsCapabilitySet(t *testing.T) {
	f := newFixture(t)
	b, err := f.db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	// A capability set that names a DIFFERENT tool than the one this
	// Agent's session actually offers — the subscription can only
	// narrow, so naming a tool it never had changes nothing about what
	// the Task may do.
	subID, err := f.db.InsertAgentSubscription(b.Seq, "event_type", `{}`, "", []string{"some_other_tool"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.ApproveAgentSubscription(subID, "mika"); err != nil {
		t.Fatal(err)
	}

	_, h := f.openSubscriptionTask(t, "alfred", "mika", subID)
	res, err := f.g.Invoke(context.Background(), agentapi.InvokeRequest{Handle: h, Tool: "get_health", Args: []byte(`{"node":"n1"}`)})
	if err != nil {
		t.Fatalf("Invoke(get_health): %v", err)
	}
	if res.Outcome != agentapi.OutcomeDenied {
		t.Fatalf("Invoke(get_health) = %+v, want denied — it is not in the subscription's capability set", res)
	}
}

// TestDisablingASubscriptionBindsItsAlreadySpawnedTask is the LIVE half
// of "may only narrow": the check re-reads the subscription row at
// every call, so disabling it after the Task already exists still
// refuses the next call — not merely new subscriptions from now on.
func TestDisablingASubscriptionBindsItsAlreadySpawnedTask(t *testing.T) {
	f := newFixture(t)
	b, err := f.db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	subID, err := f.db.InsertAgentSubscription(b.Seq, "event_type", `{}`, "", []string{"get_health"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.ApproveAgentSubscription(subID, "mika"); err != nil {
		t.Fatal(err)
	}
	_, h := f.openSubscriptionTask(t, "alfred", "mika", subID)

	if err := f.db.SetAgentSubscriptionEnabled(subID, false, "mika"); err != nil {
		t.Fatal(err)
	}
	res, err := f.g.Invoke(context.Background(), agentapi.InvokeRequest{Handle: h, Tool: "get_health", Args: []byte(`{"node":"n1"}`)})
	if err != nil {
		t.Fatalf("Invoke(get_health): %v", err)
	}
	if res.Outcome != agentapi.OutcomeDenied {
		t.Fatalf("Invoke on a task from a just-disabled subscription = %+v, want denied", res)
	}
}
