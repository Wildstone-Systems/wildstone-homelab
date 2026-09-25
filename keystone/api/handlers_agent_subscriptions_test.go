package api

// The Events family, end to end over HTTP (ADR-W039 §3.2, §3.6, §4 item
// 6; Phase 4, #341): a runtime proposes a subscription, an Owner
// approves it (never before), the automation engine's own tick matches
// it, and the runtime discovers the match by long-polling
// /api/v1/agent/deliveries and acknowledges it — idempotently, and
// tolerating an expired one.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestSubscriptionLifecycleOverHTTP(t *testing.T) {
	h, ledger := agentEnvHarness(t)
	createAgent(t, h, "alfred")
	cred := rotate(t, h, "alfred", nil)
	b, err := h.s.DB.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	_, handle := openAgentTask(t, h.s.DB, ledger, b)

	// Propose. Nothing is enabled yet.
	doc, err := json.Marshal(agentapi.SubscriptionProposal{
		MatchKind: "event_type",
		Match:     json.RawMessage(`{"eventType":"person.arrived_home","subject":"mika","sameSubject":true}`),
		Audience:  "aud_owner", Capabilities: []string{"peek"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ref agentapi.ProposalRef
	var errBody map[string]any
	if res := h.doToken(cred.Secret, http.MethodPost, "/api/v1/agent/propose", map[string]any{
		"handle": string(handle), "kind": agentapi.ProposableAgentSubscription, "document": json.RawMessage(doc),
	}, &errBody); res.StatusCode != http.StatusOK {
		t.Fatalf("propose = %d body=%v", res.StatusCode, errBody)
	} else {
		body, _ := json.Marshal(errBody)
		_ = json.Unmarshal(body, &ref)
	}
	if ref.ID == "" {
		t.Fatal("no plan id")
	}
	if subs, err := h.s.DB.ListAgentSubscriptions(b.Seq); err != nil || len(subs) != 0 {
		t.Fatalf("subscriptions before approval = %+v, %v; want none", subs, err)
	}

	// Approve, as the Owner.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+ref.ID+"/approve", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("approve = %d", res.StatusCode)
	}
	subs, err := h.s.DB.ListAgentSubscriptions(b.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || !subs[0].Enabled || subs[0].ApprovedVersion == 0 {
		t.Fatalf("subscriptions after approval = %+v, want exactly one enabled/approved", subs)
	}
	subID := subs[0].ID

	// Approval itself records a proposal_outcome delivery
	// (recordProposalOutcomeDelivery, Phase 3, #339); drain and ack it
	// so the "quiet poll" below is actually testing quiet.
	var afterApproval struct {
		Deliveries []agentapi.Delivery `json:"deliveries"`
	}
	if res := h.doToken(cred.Secret, http.MethodGet, "/api/v1/agent/deliveries?wait=0s", nil, &afterApproval); res.StatusCode != http.StatusOK {
		t.Fatalf("draining the approval delivery = %d", res.StatusCode)
	}
	for _, d := range afterApproval.Deliveries {
		if res := h.doToken(cred.Secret, http.MethodPost, "/api/v1/agent/deliveries/"+d.ID+"/ack", nil, nil); res.StatusCode != http.StatusOK {
			t.Fatalf("acking the approval delivery = %d", res.StatusCode)
		}
	}

	// A quiet long-poll: nothing has matched yet, so it blocks for
	// roughly its wait and returns empty rather than an error.
	start := time.Now()
	var quiet struct {
		Deliveries []agentapi.Delivery `json:"deliveries"`
	}
	if res := h.doToken(cred.Secret, http.MethodGet, "/api/v1/agent/deliveries?wait=300ms", nil, &quiet); res.StatusCode != http.StatusOK {
		t.Fatalf("quiet poll = %d", res.StatusCode)
	}
	if len(quiet.Deliveries) != 0 {
		t.Fatalf("quiet poll returned %+v, want none", quiet.Deliveries)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("quiet poll returned after %v, want it to have actually waited close to 300ms", elapsed)
	}

	// The engine's own tick matches the event — adopting the stream's
	// current end FIRST, exactly as automation.Engine's own first tick
	// does, so only the event emitted after that point is evaluated. A
	// marker event gives the adopt tick something to adopt: an empty
	// stream defers adopting at all (the same as automation.Engine's own
	// first tick), which would otherwise let the very next event —
	// this test's own arrival — be the one silently adopted over.
	if _, _, err := h.s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID: "marker", Source: "publisher-home", Type: "household.marker", OccurredAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	se := &automation.SubscriptionEngine{DB: h.s.DB, Ledger: ledger}
	if _, err := se.Tick(t.Context()); err != nil { // adopts the stream
		t.Fatal(err)
	}
	if _, _, err := h.s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID: "arrival-http-1", Source: "publisher-home", Type: "person.arrived_home",
		Subject: "mika", OccurredAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if out, err := se.Tick(t.Context()); err != nil {
		t.Fatal(err)
	} else if out.Matched != 1 {
		t.Fatalf("Matched = %d, want 1", out.Matched)
	}

	// Now the long-poll finds it immediately.
	var found struct {
		Deliveries []agentapi.Delivery `json:"deliveries"`
	}
	if res := h.doToken(cred.Secret, http.MethodGet, "/api/v1/agent/deliveries?wait=2s", nil, &found); res.StatusCode != http.StatusOK {
		t.Fatalf("poll = %d", res.StatusCode)
	}
	if len(found.Deliveries) != 1 || found.Deliveries[0].Kind != agentapi.DeliverySubscription || found.Deliveries[0].Handle == "" {
		t.Fatalf("deliveries = %+v, want one subscription delivery with a handle", found.Deliveries)
	}
	if found.Deliveries[0].Subscription != itoa(subID) {
		t.Errorf("delivery.subscription = %q, want %q", found.Deliveries[0].Subscription, itoa(subID))
	}
	deliveryID := found.Deliveries[0].ID

	// after= excludes it from a repeat poll, and acking is idempotent.
	var afterPoll struct {
		Deliveries []agentapi.Delivery `json:"deliveries"`
	}
	if res := h.doToken(cred.Secret, http.MethodGet, "/api/v1/agent/deliveries?after="+deliveryID+"&wait=0s", nil, &afterPoll); res.StatusCode != http.StatusOK {
		t.Fatalf("after-poll = %d", res.StatusCode)
	}
	if len(afterPoll.Deliveries) != 0 {
		t.Fatalf("after-poll = %+v, want none past the delivery just seen", afterPoll.Deliveries)
	}
	for i := 0; i < 2; i++ {
		if res := h.doToken(cred.Secret, http.MethodPost, "/api/v1/agent/deliveries/"+deliveryID+"/ack", nil, nil); res.StatusCode != http.StatusOK {
			t.Fatalf("ack #%d = %d", i+1, res.StatusCode)
		}
	}
	var afterAck struct {
		Deliveries []agentapi.Delivery `json:"deliveries"`
	}
	if res := h.doToken(cred.Secret, http.MethodGet, "/api/v1/agent/deliveries?wait=0s", nil, &afterAck); res.StatusCode != http.StatusOK {
		t.Fatalf("post-ack poll = %d", res.StatusCode)
	}
	if len(afterAck.Deliveries) != 0 {
		t.Fatalf("post-ack poll = %+v, an acked delivery must not be redelivered", afterAck.Deliveries)
	}

	// An expired delivery is skipped, not redelivered forever.
	expiredID, err := h.s.DB.RecordAgentDelivery(b.Seq, agentapi.DeliveryProposalOutcome, `{"plan":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.MarkExpiredAgentDeliveries(b.Seq, time.Now().Add(365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var afterExpiry struct {
		Deliveries []agentapi.Delivery `json:"deliveries"`
	}
	if res := h.doToken(cred.Secret, http.MethodGet, "/api/v1/agent/deliveries?wait=0s", nil, &afterExpiry); res.StatusCode != http.StatusOK {
		t.Fatalf("post-expiry poll = %d", res.StatusCode)
	}
	for _, d := range afterExpiry.Deliveries {
		if d.ID == itoa(expiredID) {
			t.Fatalf("an expired delivery was redelivered: %+v", d)
		}
	}
}
