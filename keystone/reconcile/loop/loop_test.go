package loop

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"

	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

// okApplier answers every op with success — enough for Volume steps,
// whose apply and health gate are both EnsureVolume calls.
//
// Managed files are backed by a map rather than answered blankly. A stub
// that returns empty content for every ReadManagedFile makes the node
// prereqs (F34) look drifted on every tick, so the reconciler rewrites
// them forever and "converged means quiet" stops holding — an artifact of
// the fixture, not of the product, but one that would hide a real
// always-rewriting bug behind a test that expects churn.
type okApplier struct {
	ops   int
	files map[string][]byte
}

func (a *okApplier) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	a.ops++
	if a.files == nil {
		a.files = map[string][]byte{}
	}
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_WriteManagedFile:
		a.files[o.WriteManagedFile.GetPath()] = o.WriteManagedFile.GetContent()
	case *wsv1.Operation_ReadManagedFile:
		content, ok := a.files[o.ReadManagedFile.GetPath()]
		if !ok {
			return &wsv1.OpResult{Result: &wsv1.OpResult_Error{
				Error: &wsv1.OpError{Code: wsv1.ErrorCode_ERROR_CODE_NOT_FOUND},
			}}, nil
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_ReadManagedFile{
			ReadManagedFile: &wsv1.ReadManagedFileResult{Content: content},
		}}, nil
	}
	return &wsv1.OpResult{}, nil
}

func harness(t *testing.T) (*Loop, *statefile.Store, *runtimedb.DB, *okApplier) {
	t.Helper()
	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatalf("runtimedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	state, err := statefile.New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatalf("statefile.New: %v", err)
	}
	app := &okApplier{}
	l := &Loop{
		State: state,
		DB:    db,
		Rec:   &reconcile.Reconciler{Applier: app, ManagedPrefix: "/etc/wildstone"},
	}
	return l, state, db, app
}

func volume(name, node string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindVolume,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/test", Node: node},
		Volume:     &resource.VolumeSpec{Name: name},
	}
}

func eventTypes(t *testing.T, db *runtimedb.DB) []string {
	t.Helper()
	events, err := db.EventsAfter(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	return types
}

func TestQuietTickDoesNothing(t *testing.T) {
	l, _, db, app := harness(t)
	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if out.Failed != "" || out.Repaired || len(out.PlanIDs) != 0 {
		t.Fatalf("quiet tick = %+v", out)
	}
	if app.ops != 0 {
		t.Fatalf("quiet tick dispatched %d ops", app.ops)
	}
	if types := eventTypes(t, db); len(types) != 0 {
		t.Fatalf("quiet tick emitted events: %v (ADR-W008 A5: no heartbeats)", types)
	}
}

func TestPlanTickAppliesAndResolvesRecords(t *testing.T) {
	l, state, db, _ := harness(t)
	if err := state.Put(volume("data", "n1")); err != nil {
		t.Fatal(err)
	}
	if err := state.Commit("test", "plan-1", "add volume"); err != nil {
		t.Fatal(err)
	}
	// Two approved plans describing the same target tree resolve together
	// (ADR-W008 A2).
	for range 2 {
		id, err := db.InsertPlan("op", "", "add volume", "low", `{"x":1}`)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.TransitionPlan(id, runtimedb.PlanApproved, "op"); err != nil {
			t.Fatal(err)
		}
	}

	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if out.Failed != "" || len(out.PlanIDs) != 2 {
		t.Fatalf("plan tick = %+v", out)
	}
	for _, id := range out.PlanIDs {
		rec, err := db.GetPlan(id)
		if err != nil || rec.State != runtimedb.PlanApplied {
			t.Fatalf("plan %s = %+v, %v; want applied", id, rec, err)
		}
	}
	obs, err := db.ListObserved()
	if err != nil || len(obs) != 1 || !obs[0].Healthy || obs[0].Node != "n1" {
		t.Fatalf("observed = %+v, %v", obs, err)
	}
	types := strings.Join(eventTypes(t, db), ",")
	if !strings.Contains(types, "reconcile.applied") {
		t.Fatalf("events = %v", types)
	}
}

func TestDriftTickRepairsLowRisk(t *testing.T) {
	l, state, db, _ := harness(t)
	// Desired volume, never created: a Low-risk Create the drift tick
	// auto-applies with no plan record involved.
	if err := state.Put(volume("photos", "n1")); err != nil {
		t.Fatal(err)
	}
	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !out.Repaired || out.Failed != "" {
		t.Fatalf("drift tick = %+v", out)
	}
	types := strings.Join(eventTypes(t, db), ",")
	if !strings.Contains(types, "drift.repaired") {
		t.Fatalf("events = %v", types)
	}
	// Second tick: converged, quiet.
	out, err = l.Tick(t.Context())
	if err != nil || out.Repaired {
		t.Fatalf("second tick = %+v, %v", out, err)
	}
}

func TestMediumHighDriftNeedsApprovalAndFailureSurfaces(t *testing.T) {
	l, state, db, _ := harness(t)
	// Create the volume so the engine's managed set observes it...
	if err := state.Put(volume("data", "n1")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	// ...then remove it from desired: the diff is now a High-risk Volume
	// Delete, which a drift tick must NOT auto-apply.
	if err := state.Delete(resource.KindVolume, "data"); err != nil {
		t.Fatal(err)
	}
	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if out.DriftProposed == "" || out.Failed != "" {
		t.Fatalf("drift tick = %+v, want a proposed drift plan", out)
	}
	rec, err := db.GetPlan(out.DriftProposed)
	if err != nil || rec.Principal != "system/drift" || rec.State != runtimedb.PlanProposed {
		t.Fatalf("drift plan = %+v, %v", rec, err)
	}
	// A second drift tick does not flood the plan list.
	out2, err := l.Tick(t.Context())
	if err != nil || out2.DriftProposed != "" {
		t.Fatalf("second drift tick proposed again: %+v, %v", out2, err)
	}

	// Approving the drift plan authorizes Medium/High on the next tick —
	// which for a Volume Delete hits the engine's unconditional apply-time
	// refusal (no delete op exists; Keystone never auto-deletes data), so
	// the plan must land in failed with the refusal surfaced, and desired
	// state must NOT be reverted.
	if err := db.TransitionPlan(out.DriftProposed, runtimedb.PlanApproved, "mika"); err != nil {
		t.Fatal(err)
	}
	out3, err := l.Tick(t.Context())
	if err != nil {
		t.Fatalf("plan tick: %v", err)
	}
	if out3.Failed == "" || !strings.Contains(out3.Failed, "refusing to apply a Volume Delete") {
		t.Fatalf("plan tick = %+v, want the volume-delete refusal", out3)
	}
	rec, _ = db.GetPlan(out.DriftProposed)
	if rec.State != runtimedb.PlanFailed {
		t.Fatalf("drift plan after failed apply = %+v", rec)
	}
	types := strings.Join(eventTypes(t, db), ",")
	for _, want := range []string{"drift.pending-approval", "reconcile.failed"} {
		if !strings.Contains(types, want) {
			t.Fatalf("events missing %s: %v", want, types)
		}
	}
	// Observed reflects the failed resource.
	obs, _ := db.ListObserved()
	found := false
	for _, o := range obs {
		if o.Kind == "Volume" && o.Name == "data" && !o.Healthy && strings.Contains(o.Detail, "refusing") {
			found = true
		}
	}
	if !found {
		t.Fatalf("observed missing unhealthy volume: %+v", obs)
	}
}

func TestOfflineNode(t *testing.T) {
	l, state, db, app := harness(t)
	l.Connected = func(string) bool { return false }
	container := resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindContainer,
		Metadata:   resource.Metadata{Name: "app", Owner: "capability/test", Node: "n1"},
		Container: &resource.ContainerSpec{
			Image: "reg.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	if err := state.Put(container); err != nil {
		t.Fatal(err)
	}

	// Drift tick: quiet skip, no ops, one skipped-offline event.
	out, err := l.Tick(t.Context())
	if err != nil || out.Failed != "" {
		t.Fatalf("offline drift tick = %+v, %v", out, err)
	}
	if app.ops != 0 {
		t.Fatalf("offline tick dispatched %d ops", app.ops)
	}
	if types := strings.Join(eventTypes(t, db), ","); !strings.Contains(types, "reconcile.skipped-offline") {
		t.Fatalf("events = %v", types)
	}

	// Plan tick: an approved change that cannot land is a loud failure.
	id, err := db.InsertPlan("op", "", "deploy app", "low", `{"x":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TransitionPlan(id, runtimedb.PlanApproved, "op"); err != nil {
		t.Fatal(err)
	}
	out, err = l.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Failed, "not connected") {
		t.Fatalf("offline plan tick = %+v", out)
	}
	rec, _ := db.GetPlan(id)
	if rec.State != runtimedb.PlanFailed {
		t.Fatalf("plan = %+v, want failed", rec)
	}
}

// TestBackupPlansAreNotLoopWork: plan records driven by the backup engine
// (principal system/backup) pass through 'approved' on their way to
// 'applying' under the engine's own control — the loop must never pick
// one up, or the two would double-drive its state machine (ADR-W011 A4).
func TestBackupPlansAreNotLoopWork(t *testing.T) {
	l, _, db, _ := harness(t)
	id, err := db.InsertPlan("system/backup", "", "backup run: photos", "low", `{"backupPlan":"photos"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TransitionPlan(id, runtimedb.PlanApproved, "system/backup"); err != nil {
		t.Fatal(err)
	}

	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(out.PlanIDs) != 0 {
		t.Fatalf("loop picked up backup-engine plans: %v", out.PlanIDs)
	}
	rec, err := db.GetPlan(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != runtimedb.PlanApproved {
		t.Fatalf("backup plan record state = %s, loop must leave it alone", rec.State)
	}
}

// An approved plan whose only content is a household rule (ADR-W022 A1)
// resolves to applied with no agent involved, and the rule is recorded
// as observed-healthy at no node.
func TestPlanTickWithAutomationOnlyResolves(t *testing.T) {
	l, state, db, app := harness(t)
	rule := resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindAutomation,
		Metadata:   resource.Metadata{Name: "breakfast-check", Owner: "household/mika"},
		Automation: &resource.AutomationSpec{
			Description: "breakfast",
			Trigger:     resource.AutomationTrigger{EventType: "person.left_home"},
			Action:      resource.AutomationAction{Tool: "notifications.notify", Tier: "domestic"},
		},
	}
	if err := state.Put(rule); err != nil {
		t.Fatal(err)
	}
	if err := state.Commit("test", "plan-1", "add rule"); err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertPlan("butler", "mika", "add rule", "medium", `{"x":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TransitionPlan(id, runtimedb.PlanApproved, "mika"); err != nil {
		t.Fatal(err)
	}
	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if out.Failed != "" || len(out.PlanIDs) != 1 {
		t.Fatalf("plan tick = %+v", out)
	}
	rec, err := db.GetPlan(id)
	if err != nil || rec.State != runtimedb.PlanApplied {
		t.Fatalf("plan = %+v, %v; want applied", rec, err)
	}
	if app.ops != 0 {
		t.Fatalf("%d agent op(s) dispatched for a rule that is applied nowhere", app.ops)
	}
	obs, err := db.ListObserved()
	if err != nil || len(obs) != 1 || !obs[0].Healthy || obs[0].Kind != "Automation" || obs[0].Node != "" {
		t.Fatalf("observed = %+v, %v", obs, err)
	}
}
