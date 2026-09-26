package agentgateway

// FilterDelivery/FilterDeliveries (disclosure.go, ADR-W039 §3.5 rule 3,
// #345): the same processing-class disclosure filter
// disclosure_test.go's Recall/Invoke/Manifest tables cover, now proven
// at the delivery boundary — the ONE function both work.go's
// Gateway.Deliveries and api/handlers_agent.go's agentDeliveries route
// call, so a runtime pulling either way sees the identical row.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// restrictedTurn opens a task on joan for mika, links it to a fresh
// session, and records ONE local-only-flagged memory sourced from that
// task's own conversation — the same provenance chain migration 0070's
// privacy_edges view already walks for a resumed conversation
// (butlerlive.privacyProvider, C10) — so the session itself comes back
// local_only from runtimedb.CheckSessionPrivacy without any direct
// content_privacy row on the session.
func restrictedTurn(t *testing.T, f *fixture) (taskID, sessionID string, handle agentapi.Handle) {
	t.Helper()
	taskID, handle = f.openTask(t, "joan", "mika")
	sessionID, err := f.db.InsertButlerSession(runtimedb.ButlerSession{
		Principal: "test", ActingFor: "mika", Butler: "joan", Surface: runtimedb.ButlerSurfaceFamily,
	})
	if err != nil {
		t.Fatalf("InsertButlerSession: %v", err)
	}
	if err := f.db.SetButlerTaskSession(taskID, sessionID); err != nil {
		t.Fatalf("SetButlerTaskSession: %v", err)
	}
	seq, err := f.db.RecordMemory(runtimedb.MemoryRecord{
		Scope: runtimedb.MemoryScopePersonal, Kind: runtimedb.MemoryFact,
		Content: "the safe combination is nine four one seven",
		Owner:   "mika", Author: "mika", Butler: "joan",
		Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "household",
		SourceKind: "conversation", SourceID: taskID,
	})
	if err != nil {
		t.Fatalf("RecordMemory: %v", err)
	}
	if err := f.db.SetMemoryPrivacy("mika", seq, runtimedb.PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatalf("SetMemoryPrivacy: %v", err)
	}
	if err := f.db.CheckSessionPrivacy(sessionID, runtimedb.ModelCloud, runtimedb.MemoryDestination{}); !errors.Is(err, runtimedb.ErrPrivacy) {
		t.Fatalf("fixture is broken: session %s is not local_only-restricted (err=%v)", sessionID, err)
	}
	return taskID, sessionID, handle
}

// restrictedPlan is proposal_outcome's own fixture: a Plan whose ONLY
// session source is restrictedTurn's own local_only session, exactly
// LinkPlanSessionPrivacy's own shape for a plan generated from a private
// conversation (migration 0070's own comment on plan_session_sources).
func restrictedPlan(t *testing.T, f *fixture, sessionID string) string {
	t.Helper()
	planID, err := f.db.InsertPlan("runtime-agent", "mika", "a restricted plan", "low", `{}`)
	if err != nil {
		t.Fatalf("InsertPlan: %v", err)
	}
	if err := f.db.LinkPlanSessionPrivacy(sessionID, []string{planID}); err != nil {
		t.Fatalf("LinkPlanSessionPrivacy: %v", err)
	}
	if err := f.db.CheckPlanPrivacy(planID, runtimedb.ModelCloud, runtimedb.MemoryDestination{}); !errors.Is(err, runtimedb.ErrPrivacy) {
		t.Fatalf("fixture is broken: plan %s is not local_only-restricted (err=%v)", planID, err)
	}
	return planID
}

// TestFilterDeliveryRedactsRestrictedPayloads is the delivery-boundary
// table: every kind that can carry restricted content, checked against
// every processing class disclosure_test.go's own tables use, with the
// SAME expectations — cloud and undeclared never see it, local_only
// does — and, unlike Manifest/Invoke, the delivery is never refused
// outright: it always comes back, with its payload replaced when it is
// withheld.
func TestFilterDeliveryRedactsRestrictedPayloads(t *testing.T) {
	for _, tc := range []struct {
		name          string
		class         agentapi.ProcessingClass
		wantDisclosed bool
	}{
		{"cloud never receives it", agentapi.ProcessingCloud, false},
		{"undeclared fails closed", "", false},
		{"local_only receives it", agentapi.ProcessingLocalOnly, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.class != "" {
				setProcessingClass(t, f, "joan", tc.class)
			}
			_, sessionID, handle := restrictedTurn(t, f)
			planID := restrictedPlan(t, f, sessionID)
			b, err := f.db.GetButler("joan")
			if err != nil {
				t.Fatal(err)
			}

			outcomePayload, err := json.Marshal(map[string]any{"plan": planID, "outcome": "approved"})
			if err != nil {
				t.Fatal(err)
			}
			turnPayload, err := json.Marshal(map[string]any{"handle": string(handle)})
			if err != nil {
				t.Fatal(err)
			}

			for _, row := range []struct {
				name    string
				kind    agentapi.DeliveryKind
				payload []byte
				sub     bool
			}{
				{"proposal_outcome", agentapi.DeliveryProposalOutcome, outcomePayload, false},
				{"turn", agentapi.DeliveryTurn, turnPayload, false},
				{"subscription", agentapi.DeliverySubscription, turnPayload, true},
				{"escalation", agentapi.DeliveryEscalation, turnPayload, false},
			} {
				t.Run(row.name, func(t *testing.T) {
					var id int64
					var err error
					if row.sub {
						id, err = f.db.RecordSubscriptionDelivery(b.Seq, "1", string(row.payload))
					} else {
						id, err = f.db.RecordAgentDelivery(b.Seq, row.kind, string(row.payload))
					}
					if err != nil {
						t.Fatalf("recording the delivery: %v", err)
					}
					stored, err := f.db.ListUnackedAgentDeliveries(b.Seq, 0, 50)
					if err != nil {
						t.Fatal(err)
					}
					var raw runtimedb.AgentDelivery
					found := false
					for _, r := range stored {
						if r.ID == id {
							raw = r
							found = true
						}
					}
					if !found {
						t.Fatalf("delivery %d not found among %+v", id, stored)
					}

					filtered := FilterDelivery(f.db, raw)
					if filtered.ID != raw.ID || filtered.Kind != raw.Kind || filtered.Cursor != raw.Cursor {
						t.Fatalf("FilterDelivery changed identity: %+v -> %+v", raw, filtered)
					}

					if tc.wantDisclosed {
						if filtered.Payload != raw.Payload {
							t.Fatalf("local_only payload was rewritten: %q -> %q", raw.Payload, filtered.Payload)
						}
						return
					}
					if filtered.Payload == raw.Payload {
						t.Fatalf("%s was not withheld for %s: %+v", row.name, tc.class, filtered)
					}
					var marker deliveryRefusalRow
					if err := json.Unmarshal([]byte(filtered.Payload), &marker); err != nil {
						t.Fatalf("withheld payload is not the refusal marker: %v (%q)", err, filtered.Payload)
					}
					if !marker.Withheld {
						t.Fatalf("withheld payload does not say withheld: %+v", marker)
					}
					if !strings.Contains(marker.Reason, runtimedb.ErrPrivacy.Error()) {
						t.Fatalf("withheld reason = %q, want runtimedb.ErrPrivacy's wording", marker.Reason)
					}
					if strings.Contains(filtered.Payload, string(handle)) || strings.Contains(filtered.Payload, planID) {
						t.Fatalf("the restricted reference itself leaked into the refusal: %q", filtered.Payload)
					}
				})
			}
		})
	}
}

// TestFilterDeliveryLeavesUnresolvableRowsAlone: a delivery whose
// payload names a plan or a task the filter cannot resolve (already
// expired from the ledger's own retention, or — as
// TestDeliveryClientIsByteIdenticalInProcessAndOverHTTP's own fixture
// does — simply never real) is left exactly as it was: there is no
// content left to check a restriction against, so there is nothing to
// withhold, the same "best effort" recordProposalOutcomeDelivery and
// reofferWaitingTasks already give an unresolvable reference elsewhere.
func TestFilterDeliveryLeavesUnresolvableRowsAlone(t *testing.T) {
	f := newFixture(t)
	setProcessingClass(t, f, "joan", agentapi.ProcessingCloud)
	b, err := f.db.GetButler("joan")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		kind    agentapi.DeliveryKind
		payload string
	}{
		{"malformed plan id", agentapi.DeliveryProposalOutcome, `{"plan":"p-1","outcome":"approved"}`},
		{"unknown task handle", agentapi.DeliveryTurn, `{"handle":"bt-999999.secret"}`},
		{"no handle at all", agentapi.DeliveryEscalation, `{"rule":"quiet hours","instruction":"say goodnight"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := f.db.RecordAgentDelivery(b.Seq, tc.kind, tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := f.db.ListUnackedAgentDeliveries(b.Seq, id-1, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("ListUnackedAgentDeliveries = %+v, want exactly the one just recorded", rows)
			}
			filtered := FilterDelivery(f.db, rows[0])
			if filtered.Payload != rows[0].Payload {
				t.Fatalf("an unresolvable reference was withheld anyway: %q -> %q", rows[0].Payload, filtered.Payload)
			}
		})
	}
}
