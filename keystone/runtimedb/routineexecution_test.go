package runtimedb

import (
	"testing"
	"time"
)

func TestRoutineSurvivesRemovalOfItsProposer(t *testing.T) {
	db, _, _ := secretFixture(t)
	plan, _ := approvedResourceRoutine(t, db, RoutineResources{}, AudienceHousehold)
	if _, err := db.AcquireCoordinator("routine-test"); err != nil {
		t.Fatal(err)
	}
	event, _, err := db.AppendHouseholdEvent(HouseholdEvent{
		EventID: "routine-trigger", Source: "test-publisher", Type: "person.left_home",
		OccurredAt: time.Now().UTC(), Subject: "sam",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, created, err := db.ClaimRoutineRun(plan, event)
	if err != nil || !created || first.PlanID != plan || first.TriggerEvent != event {
		t.Fatalf("first claim: %+v, created=%v, err=%v", first, created, err)
	}
	second, created, err := db.ClaimRoutineRun(plan, event)
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("retry made another run: %+v, created=%v, err=%v", second, created, err)
	}
	if err := db.DisableButlerMember("sam", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckRoutineRunBoundary(first.ID); err != nil {
		t.Fatalf("proposer removal stopped household-owned run: %v", err)
	}
	later, _, err := db.AppendHouseholdEvent(HouseholdEvent{EventID: "routine-after-proposer", Source: "test-publisher", Type: "person.left_home", OccurredAt: time.Now().UTC(), Subject: "mika"})
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := db.ClaimRoutineRun(plan, later); err != nil || !created {
		t.Fatalf("future run died with proposer: %v created=%v", err, created)
	}
}

func TestRoutineSurvivesRemovalOfItsLastApprovingOwner(t *testing.T) {
	db, _, _ := secretFixture(t)
	plan, _ := approvedResourceRoutine(t, db, RoutineResources{}, AudienceHousehold)
	if err := db.RecordButlerMember("julie", RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("julie", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireCoordinator("owner-survival-test"); err != nil {
		t.Fatal(err)
	}
	event, _, err := db.AppendHouseholdEvent(HouseholdEvent{EventID: "owner-survival", Source: "test-publisher", Type: "person.left_home", OccurredAt: time.Now().UTC(), Subject: "sam"})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := db.ClaimRoutineRun(plan, event)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DisableButlerMember("mika", "julie"); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckRoutineRunBoundary(run.ID); err != nil {
		t.Fatalf("removing last approving Owner stopped household routine: %v", err)
	}
	later, _, err := db.AppendHouseholdEvent(HouseholdEvent{EventID: "owner-survival-later", Source: "test-publisher", Type: "person.left_home", OccurredAt: time.Now().UTC(), Subject: "julie"})
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := db.ClaimRoutineRun(plan, later); err != nil || !created {
		t.Fatalf("future run died with approving Owner: %v created=%v", err, created)
	}
}

func TestRoutineEffectClaimDoesNotRepeatAfterAStop(t *testing.T) {
	db, _, _ := secretFixture(t)
	plan, _ := approvedResourceRoutine(t, db, RoutineResources{}, AudienceHousehold)
	if _, err := db.AcquireCoordinator("effect-test"); err != nil {
		t.Fatal(err)
	}
	event, _, err := db.AppendHouseholdEvent(HouseholdEvent{
		EventID: "effect-trigger", Source: "test-publisher", Type: "person.left_home",
		OccurredAt: time.Now().UTC(), Subject: "sam",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := db.ClaimRoutineRun(plan, event)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.ClaimRoutineEffect(run.ID); err != nil || !claimed {
		t.Fatalf("first effect claim: %v %v", claimed, err)
	}
	if err := db.DisableHouseholdRoutine("resource-run", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckRoutineRunBoundary(run.ID); err == nil {
		t.Fatal("disabled routine crossed next effect boundary")
	}
	if claimed, err := db.ClaimRoutineEffect(run.ID); err == nil || claimed {
		t.Fatalf("stopped run reclaimed effect: %v %v", claimed, err)
	}
	if err := db.RecordRoutineEffect(run.ID, "confirmed", "sent before stop"); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetRoutineRun(run.ID)
	if err != nil || got.EffectState != "confirmed" || got.EffectSummary != "sent before stop" {
		t.Fatalf("committed effect lost after stop: %+v %v", got, err)
	}
}

func TestHouseholdStopHaltsAllRoutineExecution(t *testing.T) {
	db, _, _ := secretFixture(t)
	plan, _ := approvedResourceRoutine(t, db, RoutineResources{}, AudienceHousehold)
	if _, err := db.AcquireCoordinator("household-stop-test"); err != nil {
		t.Fatal(err)
	}
	event, _, err := db.AppendHouseholdEvent(HouseholdEvent{
		EventID: "stop-trigger", Source: "test-publisher", Type: "person.left_home",
		OccurredAt: time.Now().UTC(), Subject: "sam",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := db.ClaimRoutineRun(plan, event)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.StopHouseholdExecution("sam", "incident"); err == nil {
		t.Fatal("Member stopped all household work")
	}
	if err := db.StopHouseholdExecution("mika", "unexpected messages"); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckRoutineRunBoundary(run.ID); err == nil {
		t.Fatal("routine crossed household stop")
	}
	if err := db.ResumeHouseholdExecution("sam"); err == nil {
		t.Fatal("Member released household stop")
	}
	if err := db.ResumeHouseholdExecution("mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckRoutineRunBoundary(run.ID); err != nil {
		t.Fatalf("Owner release did not restore eligible run: %v", err)
	}
}

func TestReviewedRoutineOwnerControlsFenceTheApprovedVersion(t *testing.T) {
	db, _, _ := secretFixture(t)
	plan, _ := approvedResourceRoutine(t, db, RoutineResources{}, AudienceHousehold)
	if _, err := db.AcquireCoordinator("routine-controls-test"); err != nil {
		t.Fatal(err)
	}
	event, _, err := db.AppendHouseholdEvent(HouseholdEvent{EventID: "controls-trigger", Source: "test-publisher", Type: "person.left_home", OccurredAt: time.Now().UTC(), Subject: "sam"})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := db.ClaimRoutineRun(plan, event)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ChangeReviewedRoutine("resource-run", "sam", "disable"); err == nil {
		t.Fatal("Member disabled reviewed routine")
	}
	if err := db.ChangeReviewedRoutine("resource-run", "mika", "disable"); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckRoutineRunBoundary(run.ID); err == nil {
		t.Fatal("disabled routine crossed boundary")
	}
	if err := db.ChangeReviewedRoutine("resource-run", "mika", "enable"); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckRoutineRunBoundary(run.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.ChangeReviewedRoutine("resource-run", "mika", "revoke"); err != nil {
		t.Fatal(err)
	}
	if err := db.ChangeReviewedRoutine("resource-run", "mika", "enable"); err == nil {
		t.Fatal("revoked version re-enabled")
	}
	if err := db.CheckRoutineRunBoundary(run.ID); err == nil {
		t.Fatal("revoked version crossed boundary")
	}
	if err := db.ChangeReviewedRoutine("resource-run", "mika", "delete"); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckRoutineRunBoundary(run.ID); err == nil {
		t.Fatal("deleted routine crossed boundary")
	}
	if err := db.ChangeReviewedRoutine("resource-run", "mika", "restore"); err != nil {
		t.Fatal(err)
	}
	if err := db.ChangeReviewedRoutine("resource-run", "mika", "enable"); err == nil {
		t.Fatal("restoring a deleted revoked routine revived its revoked version")
	}
}

func TestRevokedRoutineRequiresANewApprovedVersion(t *testing.T) {
	db, _, _ := secretFixture(t)
	oldPlan, _ := approvedResourceRoutine(t, db, RoutineResources{}, AudienceHousehold)
	if err := db.ChangeReviewedRoutine("resource-run", "mika", "revoke"); err != nil {
		t.Fatal(err)
	}
	if err := db.ChangeReviewedRoutine("resource-run", "mika", "enable"); err == nil {
		t.Fatal("revoked version re-enabled")
	}
	routine, err := db.GetHouseholdRoutine("resource-run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EditHouseholdRoutine("resource-run", routine.Document, "mika", "review after revocation"); err != nil {
		t.Fatal(err)
	}
	newPlan, err := db.SubmitRoutinePlan("resource-run", "mika", "", AudienceHousehold, "chat", routine.Document)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveRoutinePlan(newPlan, "mika"); err != nil {
		t.Fatal(err)
	}
	versions, err := db.ApprovedRoutineVersions()
	if err != nil || len(versions) != 1 || versions[0].PlanID != newPlan || newPlan == oldPlan {
		t.Fatalf("revocation did not require new approved Plan: %+v old=%s new=%s err=%v", versions, oldPlan, newPlan, err)
	}
}

func TestReassigningAnAgentRequiresReapproval(t *testing.T) {
	db := butlerFixture(t)
	newRoutine(t, db, "reassign-me")
	newButler(t, db, "new-agent", "kitchen-box")
	before, err := db.ApprovedRoutineVersions()
	if err != nil || len(before) != 1 {
		t.Fatalf("approved version missing: %+v %v", before, err)
	}
	if err := db.AssignRoutineButler("reassign-me", "new-agent", "mika"); err != nil {
		t.Fatal(err)
	}
	after, err := db.ApprovedRoutineVersions()
	if err != nil || len(after) != 0 {
		t.Fatalf("Agent reassignment inherited approval: %+v %v", after, err)
	}
}

func TestSoftDeletedRoutineDoesNotExecuteWhileRecoverable(t *testing.T) {
	db, _, _ := secretFixture(t)
	plan, _ := approvedResourceRoutine(t, db, RoutineResources{}, AudienceHousehold)
	if err := db.ChangeReviewedRoutine("resource-run", "mika", "delete"); err != nil {
		t.Fatal(err)
	}
	if versions, err := db.ApprovedRoutineVersions(); err != nil || len(versions) != 0 {
		t.Fatalf("soft-deleted version remained executable: %+v %v", versions, err)
	}
	routine, err := db.GetHouseholdRoutine("resource-run")
	if err != nil || routine.DeletedAt == "" {
		t.Fatalf("soft-deleted routine vanished: %+v %v", routine, err)
	}
	if err := db.ChangeReviewedRoutine("resource-run", "mika", "restore"); err != nil {
		t.Fatal(err)
	}
	if err := db.ChangeReviewedRoutine("resource-run", "mika", "enable"); err != nil {
		t.Fatal(err)
	}
	versions, err := db.ApprovedRoutineVersions()
	if err != nil || len(versions) != 1 || versions[0].PlanID != plan {
		t.Fatalf("restored exact version did not recover: %+v %v", versions, err)
	}
}
