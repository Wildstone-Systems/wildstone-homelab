package reconcile

import (
	"context"
	"sync"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// This file is the ADR-W008 R2d reverse-coverage audit: living
// documentation, enforced by test, of what the engine does when a step's
// health gate fails, per kind × action. The current matrix:
//
//	Container Create  → reverse: StopContainer + RemoveContainer
//	Container Update  → reverse: StopContainer (old spec is gone; stop is
//	                    the only safe move without a spec archive)
//	Container Delete  → no gate (delete has no post-condition to probe)
//	Volume    Create  → NO reverse: no volume-delete op exists on the
//	                    surface, and deleting data is exactly what the
//	                    engine must never do on its own (ADR-W004 A4)
//	Volume    Delete  → refused at apply unconditionally (same reason);
//	                    pinned by TestMediumHighDrift... in reconcile/loop
//	Route     any     → reverse: previous fragment content restored +
//	                    reload; pinned by the existing rollback test in
//	                    reconcile_test.go
//	OidcClient any    → reverse: previous fragment restored (Route
//	                    machinery); batched Authelia reload still at most
//	                    once per Reconcile
//	DnsRecord any     → no imperative reverse: provider apply is
//	                    last-writer-wins and the level-triggered loop
//	                    re-converges; failures surface as Incident
//	LdapGroup any     → declared-only, never dispatches, nothing to reverse
//
// The tests below pin the rows that were previously only prose: the
// Container reverses, and — the load-bearing negative — that a Volume
// gate failure dispatches NOTHING destructive.

// scriptedApplier answers ops in-process with per-op-name scripts: a
// call sequence log for assertions, and failure injection by op name
// after N successes.
type scriptedApplier struct {
	mu    sync.Mutex
	calls []string
	// failAfter[op] = N: the op succeeds N times, then every later call
	// fails with a RUNTIME error. Absent = always succeed.
	failAfter map[string]int
	seen      map[string]int
}

func opName(op *wsv1.Operation) string {
	switch op.GetOp().(type) {
	case *wsv1.Operation_EnsureContainer:
		return "EnsureContainer"
	case *wsv1.Operation_StopContainer:
		return "StopContainer"
	case *wsv1.Operation_RemoveContainer:
		return "RemoveContainer"
	case *wsv1.Operation_ContainerStats:
		return "ContainerStats"
	case *wsv1.Operation_InspectContainer:
		return "InspectContainer"
	case *wsv1.Operation_EnsureVolume:
		return "EnsureVolume"
	case *wsv1.Operation_WriteManagedFile:
		return "WriteManagedFile"
	case *wsv1.Operation_ReadManagedFile:
		return "ReadManagedFile"
	case *wsv1.Operation_ProbeHttp:
		return "ProbeHttp"
	default:
		return "Other"
	}
}

func (s *scriptedApplier) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	name := opName(op)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]int{}
	}
	s.calls = append(s.calls, name)
	s.seen[name]++
	// Inspect/stats always answer NOT_FOUND: nothing "exists" in this
	// scripted world, which makes observe plan a Create and makes the
	// post-apply ContainerStats gate fail — exactly the shape the
	// rollback audits need.
	if name == "InspectContainer" || name == "ContainerStats" {
		return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{
			Code: wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, Message: "scripted: absent",
		}}}, nil
	}
	if limit, ok := s.failAfter[name]; ok && s.seen[name] > limit {
		return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{
			Code: wsv1.ErrorCode_ERROR_CODE_RUNTIME, Message: "scripted failure",
		}}}, nil
	}
	return &wsv1.OpResult{}, nil
}

func (s *scriptedApplier) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[name]
}

func fastReconciler(a Applier) *Reconciler {
	return &Reconciler{
		Applier:            a,
		ManagedPrefix:      "/etc/wildstone",
		Approve:            func(*plan.Plan) bool { return true },
		MaxApplyRetries:    1,
		HealthGateAttempts: 1,
	}
}

// TestReverseAudit_ContainerCreateGateFailure: EnsureContainer succeeds,
// the ContainerStats gate never passes (scripted NOT_FOUND) → the engine
// must reverse with StopContainer + RemoveContainer and report Rolledback.
func TestReverseAudit_ContainerCreateGateFailure(t *testing.T) {
	s := &scriptedApplier{}
	r := fastReconciler(s)
	desired := []resource.Resource{containerResource("app", testDigestImage)}
	outcome, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !outcome.Rolledback || outcome.Applied {
		t.Fatalf("outcome = %+v, want Rolledback", outcome)
	}
	if len(outcome.Steps) != 1 || !outcome.Steps[0].Applied || outcome.Steps[0].HealthOK || !outcome.Steps[0].Reversed {
		t.Fatalf("step = %+v, want applied+gate-failed+reversed", outcome.Steps)
	}
	if s.count("StopContainer") != 1 || s.count("RemoveContainer") != 1 {
		t.Fatalf("reverse ops: stop=%d remove=%d, want 1/1 (calls: %v)",
			s.count("StopContainer"), s.count("RemoveContainer"), s.calls)
	}
}

// TestReverseAudit_VolumeGateFailureNeverDeletes: the first EnsureVolume
// (apply) succeeds, the second (gate) fails → Rolledback with NO reverse:
// the audit's load-bearing negative is that nothing beyond EnsureVolume
// is ever dispatched for a volume — there is no destructive op to reach.
func TestReverseAudit_VolumeGateFailureNeverDeletes(t *testing.T) {
	s := &scriptedApplier{failAfter: map[string]int{"EnsureVolume": 1}}
	r := fastReconciler(s)
	desired := []resource.Resource{volumeResource("data")}
	outcome, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !outcome.Rolledback || outcome.Applied {
		t.Fatalf("outcome = %+v, want Rolledback", outcome)
	}
	if len(outcome.Steps) != 1 || outcome.Steps[0].Reversed {
		t.Fatalf("step = %+v, want gate-failed WITHOUT reverse", outcome.Steps)
	}
	for _, call := range s.calls {
		if call != "EnsureVolume" {
			t.Fatalf("volume path dispatched %q — only EnsureVolume may ever run for a volume (calls: %v)", call, s.calls)
		}
	}
}
