package agentgateway

// The Proposals family (ADR-W039 §3.2, Phase 3, #339): a Plan a
// runtime's propose_change call, or its own Propose call, produces is
// filed inside the gateway, attributed to the RUNTIME PRINCIPAL acting
// for the Person on the handle — never the credential the runtime holds
// on some other Agent's behalf, never applied here, and never
// approvable by the runtime that filed it.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/proposals"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// wireProposals gives a fixture's Gateway somewhere to file — the real
// pipeline, over the fixture's own Statefile and database — so
// propose_change and Propose stop returning the pre-Phase-3 placeholder
// and actually file a Plan.
func (f *fixture) wireProposals() {
	f.g.Proposals = proposals.Deps{DB: f.db, State: f.store, Policy: policy.NewEngine(nil)}
}

// grafanaRouteArgs is the same policy-compliant Route propose_change
// input api/handlers_butler_test.go's own full-flow test scripts, so a
// success here is a success under the real validation and policy
// pipeline, not a fixture's shortcut.
func grafanaRouteArgs() map[string]any {
	return map[string]any{
		"kind":  "Route",
		"node":  "node-a",
		"name":  "grafana",
		"owner": "capability/grafana",
		"spec": map[string]any{
			"host":           "grafana.example.com",
			"upstream":       "container/grafana:3000",
			"auth":           "authelia_oidc",
			"classification": "family_public",
		},
	}
}

// TestProposeChangeFilesUnderTheRuntimePrincipal is the core Phase 3
// evidence: an accepted propose_change, invoked as a bound runtime
// principal, appears in the Plan list attributed to THAT principal
// acting for the Person on the handle — not "butler", not the Agent's
// name — and is not applied.
func TestProposeChangeFilesUnderTheRuntimePrincipal(t *testing.T) {
	f := newFixture(t)
	f.wireProposals()
	f.bindRuntime(t, "runtime-alfred-1", "alfred")
	ctx := WithPrincipal(context.Background(), "runtime-alfred-1")
	_, h := f.openTask(t, "alfred", "mika")

	args, err := json.Marshal(grafanaRouteArgs())
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: h, Tool: "propose_change", Args: args})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outcome != agentapi.OutcomeOK {
		t.Fatalf("outcome = %s: %s", res.Outcome, res.Reason)
	}
	if res.Proposal == nil || res.Proposal.ID == "" {
		t.Fatalf("no proposal reference: %+v", res)
	}
	// The pre-Phase-3 placeholder was "<task>/step/<n>"; a real filed
	// Plan id is not shaped like the task's own id at all.
	if strings.Contains(res.Proposal.ID, h.TaskID()) {
		t.Fatalf("proposal id looks like the unfiled placeholder: %q", res.Proposal.ID)
	}

	rec, err := f.db.GetPlan(res.Proposal.ID)
	if err != nil {
		t.Fatalf("GetPlan(%s): %v", res.Proposal.ID, err)
	}
	if rec.State != runtimedb.PlanProposed {
		t.Fatalf("plan state = %s, want proposed", rec.State)
	}
	if rec.Principal != "runtime-alfred-1" {
		t.Fatalf("plan principal = %q, want the runtime principal", rec.Principal)
	}
	if rec.ActingFor != "mika" {
		t.Fatalf("plan actingFor = %q, want the person on the handle", rec.ActingFor)
	}

	// Never applied: the statefile has nothing in it yet.
	if refs, err := f.store.List(); err != nil {
		t.Fatal(err)
	} else if len(refs) != 0 {
		t.Fatalf("propose_change wrote to the statefile: %+v", refs)
	}

	// The post-turn handler must not re-file this call: ResourceYAML is
	// cleared once the gateway files it itself.
	calls := f.g.Records(h.TaskID())
	if len(calls) != 1 || calls[0].ResourceYAML != "" || calls[0].ProposalID != res.Proposal.ID {
		t.Fatalf("recorded call = %+v", calls)
	}
}

// TestProposeChangeFilesForTheNativeInProcessCaller covers the other
// half of "the runtime principal from ctx, or the native runtime
// principal for in-process": a call with no principal in ctx at all —
// the native runner talking to its own in-process Gateway — files under
// the Agent's own current runtime principal, resolved from its runtime
// record rather than left unattributed.
func TestProposeChangeFilesForTheNativeInProcessCaller(t *testing.T) {
	f := newFixture(t)
	f.wireProposals()
	// alfred's native runtime principal, exactly as Phase 1 mints one at
	// Butler creation — bound here the same way api.mintRuntimePrincipal
	// does, so this test does not depend on that HTTP-layer code.
	f.bindRuntime(t, "runtime-alfred-1", "alfred")
	if err := f.db.SetButlerRuntime(mustButlerSeq(t, f, "alfred"), runtimedb.ButlerRuntime{
		Type: agentapi.RuntimeNative, Principal: "runtime-alfred-1",
	}, "mika"); err != nil {
		t.Fatal(err)
	}
	_, h := f.openTask(t, "alfred", "mika")

	args, err := json.Marshal(grafanaRouteArgs())
	if err != nil {
		t.Fatal(err)
	}
	// No WithPrincipal: an in-process caller under the process's own
	// trust, exactly as the native runner calls its Gateway.
	res, err := f.g.Invoke(context.Background(), agentapi.InvokeRequest{Handle: h, Tool: "propose_change", Args: args})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outcome != agentapi.OutcomeOK || res.Proposal == nil {
		t.Fatalf("outcome = %+v", res)
	}
	rec, err := f.db.GetPlan(res.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Principal != "runtime-alfred-1" {
		t.Fatalf("plan principal = %q, want the agent's own runtime principal", rec.Principal)
	}
}

// TestProposeChangeUnfiledWithoutProposalsWired pins the fallback: a
// gateway built without Proposals wired (every pre-Phase-3 fixture,
// including this package's own newFixture by default) behaves exactly
// as before — a placeholder reference, ResourceYAML kept on the record
// for a caller to file after the turn.
func TestProposeChangeUnfiledWithoutProposalsWired(t *testing.T) {
	f := newFixture(t)
	f.bindRuntime(t, "runtime-alfred-1", "alfred")
	ctx := WithPrincipal(context.Background(), "runtime-alfred-1")
	_, h := f.openTask(t, "alfred", "mika")

	args, err := json.Marshal(grafanaRouteArgs())
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: h, Tool: "propose_change", Args: args})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outcome != agentapi.OutcomeOK || res.Proposal == nil {
		t.Fatalf("outcome = %+v", res)
	}
	calls := f.g.Records(h.TaskID())
	if len(calls) != 1 || calls[0].ResourceYAML == "" || calls[0].ProposalID != "" {
		t.Fatalf("an unwired gateway filed the proposal itself: %+v", calls)
	}
}

// TestProposeFilesADocumentDirectly covers the standalone Propose RPC
// (as opposed to invoking propose_change): a runtime naming a resource
// document directly files exactly as propose_change does.
func TestProposeFilesADocumentDirectly(t *testing.T) {
	f := newFixture(t)
	f.wireProposals()
	f.bindRuntime(t, "runtime-alfred-1", "alfred")
	ctx := WithPrincipal(context.Background(), "runtime-alfred-1")
	_, h := f.openTask(t, "alfred", "mika")

	doc, err := json.Marshal(map[string]any{
		"apiVersion": resource.ApiVersion,
		"kind":       "Route",
		"metadata":   map[string]any{"name": "grafana", "owner": "capability/grafana", "node": "node-a"},
		"spec": map[string]any{
			"host": "grafana.example.com", "upstream": "container/grafana:3000",
			"auth": "authelia_oidc", "classification": "family_public",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.g.Propose(ctx, agentapi.ProposeRequest{Handle: h, Kind: "Route", Document: doc, Rationale: "expose grafana"})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	rec, err := f.db.GetPlan(ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Principal != "runtime-alfred-1" || rec.ActingFor != "mika" || rec.State != runtimedb.PlanProposed {
		t.Fatalf("plan = %+v", rec)
	}
}

// mustButlerSeq is a small local helper: the fixture exposes handles and
// task-opening but not a raw Butler lookup by name.
func mustButlerSeq(t *testing.T, f *fixture, name string) int64 {
	t.Helper()
	b, err := f.db.GetButler(name)
	if err != nil {
		t.Fatal(err)
	}
	return b.Seq
}
