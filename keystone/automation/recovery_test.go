package automation

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// TestReviewedRoutineRecoveryContract uses a real, parseable Automation and
// the supported draft -> exact Plan snapshot -> Owner approval workflow. The
// action provider is local and inert: it only records calls in memory.
//
// The one seeded interruption is at the production ClaimRoutineEffect
// boundary. That method is the durable checkpoint immediately before
// dispatch; the fixture intentionally performs no dispatch and records no
// result, exactly modelling a process stop at that boundary.
func TestReviewedRoutineRecoveryContract(t *testing.T) {
	path := migrated.Path(t)
	db, err := runtimedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("mika", "synthetic-owner"); err != nil {
		t.Fatal(err)
	}

	r := rule("recovery-fixture")
	r.Automation.Escalation = nil
	r.Automation.Action.Tool = "chat-test.send"
	r.Automation.Action.Params = map[string]string{"member": "mika", "message": "synthetic inert notice"}
	raw, err := resource.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := ParseDraft(string(raw)); err != nil || parsed.Metadata.Name != r.Metadata.Name {
		t.Fatalf("fixture is not a valid Automation draft: %v", err)
	}
	if _, err := db.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: r.Metadata.Name, Ownership: runtimedb.RoutineWildstone,
		Author: "mika", Document: string(raw),
	}); err != nil {
		t.Fatal(err)
	}
	plan, err := db.SubmitRoutinePlan(r.Metadata.Name, "mika", "", runtimedb.AudienceHousehold, "local inert action", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveRoutinePlan(plan, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireCoordinator("keystone-recovery"); err != nil {
		t.Fatal(err)
	}
	actions := &routineActions{available: true}
	engine := &Engine{DB: db, Location: london, Now: func() time.Time { return t0.Add(5 * time.Minute) }, ReviewedActions: actions}
	if err := engine.TickReviewed(context.Background()); err != nil { // adopt an empty stream
		t.Fatal(err)
	}

	completedEvent := emit(t, db, "recovery-completed", "person.left_home", "mika", t0)
	if err := engine.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(actions.calls) != 1 {
		t.Fatalf("initial execution dispatched %d times, want 1", len(actions.calls))
	}
	completed, _, err := db.ClaimRoutineRun(plan, completedEvent)
	if err != nil || completed.State != "completed" || completed.EffectState != "confirmed" {
		t.Fatalf("completed run = %+v, %v", completed, err)
	}

	// Restart all database-backed execution components over the same file.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = runtimedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireCoordinator("keystone-recovery"); err != nil {
		t.Fatal(err)
	}
	engine = &Engine{DB: db, Location: london, Now: func() time.Time { return t0.Add(5 * time.Minute) }, ReviewedActions: actions}
	if err := db.RecoverClaimedRoutineEffects(); err != nil {
		t.Fatal(err)
	}
	if err := engine.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(actions.calls) != 1 {
		t.Fatalf("restart repeated completed effect: calls=%d", len(actions.calls))
	}

	interruptedEvent := emit(t, db, "recovery-interrupted", "person.left_home", "mika", t0.Add(time.Minute))
	interrupted, created, err := db.ClaimRoutineRun(plan, interruptedEvent)
	if err != nil || !created {
		t.Fatalf("claim interrupted run = %+v created=%v err=%v", interrupted, created, err)
	}
	claimed, err := db.ClaimRoutineEffect(interrupted.ID)
	if err != nil || !claimed {
		t.Fatalf("claim effect = %v, %v", claimed, err)
	}
	// Stop after the durable pre-dispatch claim. No provider is called.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = runtimedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireCoordinator("keystone-recovery"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverClaimedRoutineEffects(); err != nil {
		t.Fatal(err)
	}
	interrupted, err = db.GetRoutineRun(interrupted.ID)
	if err != nil || interrupted.State != "uncertain" || interrupted.EffectState != "uncertain" {
		t.Fatalf("interrupted recovery = %+v, %v", interrupted, err)
	}
	engine = &Engine{DB: db, Location: london, Now: func() time.Time { return t0.Add(5 * time.Minute) }, ReviewedActions: actions}
	if err := engine.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(actions.calls) != 1 {
		t.Fatalf("uncertain effect was repeated: calls=%d", len(actions.calls))
	}

	// Revocation remains live product state, not a property cached by the
	// scheduler. It prevents both the interrupted version and future events.
	if err := db.DisableHouseholdRoutine(r.Metadata.Name, "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = runtimedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireCoordinator("keystone-recovery"); err != nil {
		t.Fatal(err)
	}
	versions, err := db.ApprovedRoutineVersions()
	if err != nil || len(versions) != 0 {
		t.Fatalf("disabled routine regained authority: %+v, %v", versions, err)
	}
	emit(t, db, "recovery-after-disable", "person.left_home", "mika", t0.Add(2*time.Minute))
	engine = &Engine{DB: db, Location: london, Now: func() time.Time { return t0.Add(5 * time.Minute) }, ReviewedActions: actions}
	if err := engine.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(actions.calls) != 1 {
		t.Fatalf("disabled routine dispatched after restart: calls=%d", len(actions.calls))
	}

	// The supported online snapshot and restore-alongside path preserves the
	// checkpoint, completed receipt state, uncertain history, and revocation.
	snapshot := filepath.Join(t.TempDir(), "runtime.snapshot.db")
	if err := db.BackupTo(snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := runtimedb.RestoreBackup(snapshot, filepath.Join(t.TempDir(), "restored.db"), "synthetic execution restore", t0)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	restoredCompleted, err := restored.GetRoutineRun(completed.ID)
	if err != nil || restoredCompleted.State != "completed" || restoredCompleted.EffectState != "confirmed" {
		t.Fatalf("restored completed run = %+v, %v", restoredCompleted, err)
	}
	restoredInterrupted, err := restored.GetRoutineRun(interrupted.ID)
	if err != nil || restoredInterrupted.State != "uncertain" || restoredInterrupted.EffectState != "uncertain" {
		t.Fatalf("restored interrupted run = %+v, %v", restoredInterrupted, err)
	}
	restoredRoutine, err := restored.GetHouseholdRoutine(r.Metadata.Name)
	if err != nil || restoredRoutine.Active() {
		t.Fatalf("restored routine revocation = %+v, %v", restoredRoutine, err)
	}
	cursor, initialized, err := restored.RoutineExecutionCursor()
	if err != nil || !initialized || cursor == 0 {
		t.Fatalf("restored cursor = %d initialized=%v err=%v", cursor, initialized, err)
	}
	if err := restored.CheckHouseholdExecution(); !errors.Is(err, runtimedb.ErrCoordinatorFenced) {
		t.Fatalf("restore-alongside execution = %v, want coordinator fence", err)
	}
}
