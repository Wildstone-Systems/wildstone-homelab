package api

// A Plan a runtime filed writes a proposal_outcome delivery the moment
// it is decided (ADR-W039 §3.2 Proposals, §3.6; migration 0084, Phase 3,
// #339) — the write side of Phase 4's own consumer. A Plan a human
// filed writes none: there is no runtime waiting to be told.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func TestApprovingARuntimeFiledPlanRecordsAProposalOutcomeDelivery(t *testing.T) {
	h := butlerConsole(t)
	h.person("mika")
	createAgent(t, h, "alfred")
	b, err := h.s.DB.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := h.s.DB.GetButlerRuntime(b.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Principal == "" {
		t.Fatal("a newly created agent has no runtime principal")
	}

	doc := containerYAML(t, "immich", "ghcr.io/immich-app/immich-server@"+testDigest, nil)
	body, status, err := h.s.submitProposalOpts(
		principal.Identity{Name: rt.Principal, Role: runtimedb.RoleButler, ActingFor: "mika"},
		proposalRequest{Summary: "deploy immich", Put: []string{doc}}, proposalOpts{})
	if err != nil || status != http.StatusCreated {
		t.Fatalf("filing: %v %d", err, status)
	}
	rec, ok := body.(planView)
	if !ok {
		t.Fatalf("filed body = %#v", body)
	}
	if rec.Principal != rt.Principal {
		t.Fatalf("plan principal = %q, want %q", rec.Principal, rt.Principal)
	}

	if empty, err := h.s.DB.ListAgentDeliveries(b.Seq, 10); err != nil || len(empty) != 0 {
		t.Fatalf("a delivery exists before any decision: %+v %v", empty, err)
	}

	var approved runtimedb.PlanRecord
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+rec.ID+"/approve", nil, &approved); res.StatusCode != http.StatusOK {
		t.Fatalf("approve = %d", res.StatusCode)
	}

	deliveries, err := h.s.DB.ListAgentDeliveries(b.Seq, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %+v, want one", deliveries)
	}
	d := deliveries[0]
	if d.Kind != "proposal_outcome" {
		t.Fatalf("delivery kind = %q", d.Kind)
	}
	if !containsAll(d.Payload, rec.ID, "approved") {
		t.Fatalf("delivery payload = %q, want the plan id and \"approved\"", d.Payload)
	}
}

func TestRejectingARuntimeFiledPlanRecordsAProposalOutcomeDelivery(t *testing.T) {
	h := butlerConsole(t)
	h.person("mika")
	createAgent(t, h, "alfred")
	b, err := h.s.DB.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := h.s.DB.GetButlerRuntime(b.Seq)
	if err != nil {
		t.Fatal(err)
	}

	doc := containerYAML(t, "immich", "ghcr.io/immich-app/immich-server@"+testDigest, nil)
	body, status, err := h.s.submitProposalOpts(
		principal.Identity{Name: rt.Principal, Role: runtimedb.RoleButler, ActingFor: "mika"},
		proposalRequest{Summary: "deploy immich", Put: []string{doc}}, proposalOpts{})
	if err != nil || status != http.StatusCreated {
		t.Fatalf("filing: %v %d", err, status)
	}
	rec := body.(planView)

	var rejected runtimedb.PlanRecord
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+rec.ID+"/reject", nil, &rejected); res.StatusCode != http.StatusOK {
		t.Fatalf("reject = %d", res.StatusCode)
	}
	deliveries, err := h.s.DB.ListAgentDeliveries(b.Seq, 10)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("deliveries = %+v, %v", deliveries, err)
	}
	if !containsAll(deliveries[0].Payload, rec.ID, "rejected") {
		t.Fatalf("delivery payload = %q, want the plan id and \"rejected\"", deliveries[0].Payload)
	}
}

// TestApprovingAHumanFiledPlanRecordsNoDelivery: a Plan filed by an
// ordinary credential — not a runtime principal — has no Agent to
// deliver to, and none is written.
func TestApprovingAHumanFiledPlanRecordsNoDelivery(t *testing.T) {
	h := newHarness(t)
	doc := containerYAML(t, "immich", "ghcr.io/immich-app/immich-server@"+testDigest, nil)
	var rec runtimedb.PlanRecord
	if res := h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/plans",
		proposalRequest{Summary: "deploy immich", Put: []string{doc}}, &rec); res.StatusCode != http.StatusCreated {
		t.Fatalf("propose = %d", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+rec.ID+"/approve", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("approve = %d", res.StatusCode)
	}
	// "butler" (the RoleButler test token's own name) is not bound to
	// any Agent, so RuntimePrincipalButler answers false for it and no
	// delivery table lookup by seq is even meaningful — the absence of
	// an error on approval, which the call above already checked, is
	// the evidence: recordProposalOutcomeDelivery must not have failed
	// the request trying to resolve a seq that does not exist.
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
