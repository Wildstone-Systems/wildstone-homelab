package reconcile

import (
	"context"
	"errors"
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
//	Container Create  → NO destructive reverse: an earlier absent
//	                    observation can be stale after a reconnect
//	Container Update  → NO destructive reverse: the old spec is gone and
//	                    stopping the desired replacement deepens the outage
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
//	ConfigFile any    → reverse: previous file content restored (the Route
//	                    machinery), and NO reload — nothing here knows
//	                    which container reads the file, so an app keeps
//	                    running its old config until it next restarts.
//	                    That is the level-triggered contract, not an
//	                    oversight: the file on disk is correct either way.
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
// reports the failed gate but must not make the outage worse with a
// StopContainer or RemoveContainer. Desired state remains for the next pass.
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
	if len(outcome.Steps) != 1 || !outcome.Steps[0].Applied || outcome.Steps[0].HealthOK || outcome.Steps[0].Reversed {
		t.Fatalf("step = %+v, want applied+gate-failed without a destructive reverse", outcome.Steps)
	}
	if s.count("StopContainer") != 0 || s.count("RemoveContainer") != 0 {
		t.Fatalf("reverse ops: stop=%d remove=%d, want 0/0 (calls: %v)",
			s.count("StopContainer"), s.count("RemoveContainer"), s.calls)
	}
}

// reconnectApplier reproduces #323's dangerous timing: observation says a
// managed container is absent, but by apply time the reconnected node has the
// already-existing, desired container. Ensure therefore reports Created=false,
// then the connection drops again while the health gate is in flight.
type reconnectApplier struct {
	files       map[string][]byte
	labels      map[string]string
	inspectCall int
	statsCall   int
	ensures     int
	stops       int
	removes     int
}

func (a *reconnectApplier) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	if a.files == nil {
		a.files = map[string][]byte{}
	}
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_ReadManagedFile:
		content, ok := a.files[o.ReadManagedFile.GetPath()]
		if !ok {
			return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{
				Code: wsv1.ErrorCode_ERROR_CODE_NOT_FOUND,
			}}}, nil
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_ReadManagedFile{ReadManagedFile: &wsv1.ReadManagedFileResult{Content: content}}}, nil
	case *wsv1.Operation_WriteManagedFile:
		a.files[o.WriteManagedFile.GetPath()] = o.WriteManagedFile.GetContent()
	case *wsv1.Operation_InspectContainer:
		a.inspectCall++
		if a.inspectCall == 1 {
			return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{
				Code: wsv1.ErrorCode_ERROR_CODE_NOT_FOUND,
			}}}, nil
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_InspectContainer{InspectContainer: &wsv1.InspectContainerResult{
			Running: true, Labels: a.labels,
		}}}, nil
	case *wsv1.Operation_EnsureContainer:
		a.ensures++
		a.labels = o.EnsureContainer.GetLabels()
		return &wsv1.OpResult{Result: &wsv1.OpResult_EnsureContainer{EnsureContainer: &wsv1.EnsureContainerResult{
			ContainerId: "existing", Created: false, Started: false,
		}}}, nil
	case *wsv1.Operation_ContainerStats:
		a.statsCall++
		if a.statsCall == 1 {
			return nil, errors.New("node disconnected")
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_ContainerStats{ContainerStats: &wsv1.ContainerStatsResult{}}}, nil
	case *wsv1.Operation_StopContainer:
		a.stops++
	case *wsv1.Operation_RemoveContainer:
		a.removes++
	}
	return &wsv1.OpResult{}, nil
}

func TestContainerReconnectRacePreservesExistingServiceAndNextPassConverges(t *testing.T) {
	a := &reconnectApplier{}
	r := fastReconciler(a)
	desired := []resource.Resource{containerResource("identity", testDigestImage)}

	first, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if first.Rolledback || first.Incident == "" || !first.Retryable || len(first.Steps) != 1 || !first.Steps[0].Applied {
		t.Fatalf("first outcome = %+v, want an applied operation with an inconclusive gate incident", first)
	}
	if a.stops != 0 || a.removes != 0 {
		t.Fatalf("reconnect race dispatched destructive rollback: stops=%d removes=%d", a.stops, a.removes)
	}

	second, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if !second.Applied || second.Incident != "" || second.Rolledback || len(second.Plan.Steps) != 0 {
		t.Fatalf("second outcome = %+v, want quiet convergence without a Statefile change", second)
	}
	if a.ensures != 1 || a.stops != 0 || a.removes != 0 {
		t.Fatalf("operations after convergence: ensures=%d stops=%d removes=%d", a.ensures, a.stops, a.removes)
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
	// Every Reconcile now re-asserts the two node-level managed files
	// (F34), so the dispatch list opens with their read/write pair. Those
	// are node prerequisites, not the volume path, and this assertion is
	// about the volume path: strip them by name — safe here because this
	// desired set contains no Route or OidcClient, so no other managed
	// file exists to write — and keep the negative on what remains.
	//
	// Deliberately narrowed rather than relaxed: the load-bearing claim
	// is that NO destructive op is reachable for a volume, and that is
	// still asserted exactly.
	prereqOps := map[string]int{"ReadManagedFile": 0, "WriteManagedFile": 0}
	nodePrereqCount := len(nodePrereqFiles(testManagedPrefix, nil))
	var volumePath []string
	for _, call := range s.calls {
		if _, ok := prereqOps[call]; ok {
			prereqOps[call]++
			continue
		}
		volumePath = append(volumePath, call)
	}
	if prereqOps["ReadManagedFile"] > nodePrereqCount || prereqOps["WriteManagedFile"] > nodePrereqCount {
		t.Fatalf("more managed-file ops than the %d node prereqs account for: %v", nodePrereqCount, s.calls)
	}
	for _, call := range volumePath {
		if call != "EnsureVolume" {
			t.Fatalf("volume path dispatched %q — only EnsureVolume may ever run for a volume (calls: %v)", call, s.calls)
		}
	}
}
