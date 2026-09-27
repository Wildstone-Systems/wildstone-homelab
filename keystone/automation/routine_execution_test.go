package automation

import (
	"context"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

type routineActions struct {
	available      bool
	calls          []domestic.Call
	beforeDispatch func()
}

func (a *routineActions) Compatible(string, string) bool { return a.available }
func (a *routineActions) RoutineDestination(context.Context, string, string) (runtimedb.MemoryDestination, error) {
	return runtimedb.MemoryDestination{BuiltIn: true}, nil
}
func (a *routineActions) Dispatch(ctx context.Context, c domestic.Call) (domestic.Result, error) {
	if a.beforeDispatch != nil {
		a.beforeDispatch()
	}
	if err := domestic.CheckExecutionContext(ctx); err != nil {
		return domestic.Result{}, err
	}
	a.calls = append(a.calls, c)
	return domestic.Result{EffectConfirmed: true, Summary: "sent"}, nil
}

func assignReviewedTestAgent(t *testing.T, db *runtimedb.DB, routine string) {
	t.Helper()
	if err := db.UpsertModel(runtimedb.Model{Name: "routine-model", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "fixture", Readiness: runtimedb.ModelReady, Enabled: true}, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateButler(runtimedb.Butler{Name: "routine-agent", Directive: "routine", Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "routine-model"}, "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.AssignRoutineButler(routine, "routine-agent", "mika"); err != nil {
		t.Fatal(err)
	}
}

func TestFirstEventAfterAnEmptyReviewedTickIsProcessed(t *testing.T) {
	e, db := harness(t)
	if err := db.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	r := rule("first-event")
	r.Automation.Escalation = nil
	r.Automation.Action.Tool = "chat-test.send"
	r.Automation.Action.Params = map[string]string{"member": "mika", "message": "Hello"}
	raw, err := resource.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: "first-event", Ownership: runtimedb.RoutineWildstone, Author: "mika", Document: string(raw)}); err != nil {
		t.Fatal(err)
	}
	plan, err := db.SubmitRoutinePlan("first-event", "mika", "", runtimedb.AudienceHousehold, "chat", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveRoutinePlan(plan, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireCoordinator("first-event-test"); err != nil {
		t.Fatal(err)
	}
	actions := &routineActions{available: true}
	e.ReviewedActions = actions
	if err := e.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	emit(t, db, "first-event", "person.left_home", "mika", t0)
	if err := e.TickReviewed(context.Background()); err != nil || len(actions.calls) != 1 {
		t.Fatalf("first later event skipped: %v, calls=%d", err, len(actions.calls))
	}
}

func TestDeterministicStepsContinueWithoutTheAgent(t *testing.T) {
	e, db := harness(t)
	if err := db.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	r := rule("reviewed")
	r.Automation.Escalation = nil
	r.Automation.Action.Tool = "chat-test.send"
	r.Automation.Action.Params = map[string]string{"member": "mika", "message": "Did you have breakfast?"}
	raw, err := resource.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: "reviewed", Ownership: runtimedb.RoutineWildstone,
		Author: "mika", Document: string(raw),
	}); err != nil {
		t.Fatal(err)
	}
	assignReviewedTestAgent(t, db, "reviewed")
	plan, err := db.SubmitRoutinePlan("reviewed", "mika", "", runtimedb.AudienceHousehold, "notify household", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveRoutinePlan(plan, "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordModelReadiness("routine-model", runtimedb.ModelUnavailable, "offline"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireCoordinator("routine-executor-test"); err != nil {
		t.Fatal(err)
	}
	actions := &routineActions{available: true}
	e.ReviewedActions = actions
	emit(t, db, "old", "person.left_home", "mika", t0.Add(-time.Hour))
	if err := e.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		emit(t, db, id, "person.left_home", "mika", t0)
	}
	if err := e.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(actions.calls) != 2 {
		t.Fatalf("approved routine dispatched %d times, want two independent runs", len(actions.calls))
	}
	if err := e.TickReviewed(context.Background()); err != nil || len(actions.calls) != 2 {
		t.Fatalf("retry repeated an effect: %v, calls=%d", err, len(actions.calls))
	}
}

func TestInferenceStepBlocksVisiblyWithoutTheAgent(t *testing.T) {
	e, db := harness(t)
	if err := db.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	r := rule("needs-agent")
	r.Automation.Action.Tool = "chat-test.send"
	r.Automation.Action.Params = map[string]string{"member": "mika", "message": "Did you have breakfast?"}
	raw, err := resource.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: "needs-agent", Ownership: runtimedb.RoutineWildstone,
		Author: "mika", Document: string(raw),
	}); err != nil {
		t.Fatal(err)
	}
	assignReviewedTestAgent(t, db, "needs-agent")
	plan, err := db.SubmitRoutinePlan("needs-agent", "mika", "", runtimedb.AudienceHousehold, "notify household", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveRoutinePlan(plan, "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordModelReadiness("routine-model", runtimedb.ModelUnavailable, "offline"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireCoordinator("inference-test"); err != nil {
		t.Fatal(err)
	}
	actions := &routineActions{available: true}
	e.ReviewedActions = actions
	emit(t, db, "old", "person.left_home", "mika", t0.Add(-time.Hour))
	if err := e.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	event := emit(t, db, "needs-inference", "person.left_home", "mika", t0)
	if err := e.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, _, err := db.ClaimRoutineRun(plan, event)
	if err != nil || run.State != "blocked" || run.Reason == "" || len(actions.calls) != 0 {
		t.Fatalf("missing Agent did not block visibly: run=%+v calls=%d err=%v", run, len(actions.calls), err)
	}
	visible, err := db.ListReviewedRoutineRuns("needs-agent", 10)
	if err != nil || len(visible) != 1 || visible[0].State != "blocked" || visible[0].Reason == "" {
		t.Fatalf("Owner cannot inspect blocked run: %+v %v", visible, err)
	}
}

func TestCapabilityRecoveryResumesTheSameReviewedOccurrence(t *testing.T) {
	e, db := harness(t)
	if err := db.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	r := rule("capability-recovery")
	r.Automation.Escalation = nil
	r.Automation.Action.Tool = "chat-test.send"
	r.Automation.Action.Params = map[string]string{"member": "mika", "message": "Dinner?"}
	raw, err := resource.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: "capability-recovery", Ownership: runtimedb.RoutineWildstone, Author: "mika", Document: string(raw)}); err != nil {
		t.Fatal(err)
	}
	plan, err := db.SubmitRoutinePlan("capability-recovery", "mika", "", runtimedb.AudienceHousehold, "chat", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveRoutinePlan(plan, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireCoordinator("capability-recovery-test"); err != nil {
		t.Fatal(err)
	}
	actions := &routineActions{available: false}
	e.ReviewedActions = actions
	emit(t, db, "old-capability", "person.left_home", "mika", t0.Add(-time.Hour))
	if err := e.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	event := emit(t, db, "missing-capability", "person.left_home", "mika", t0)
	if err := e.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, _, err := db.ClaimRoutineRun(plan, event)
	if err != nil || run.State != "blocked" || len(actions.calls) != 0 {
		t.Fatalf("capability gap: %+v calls=%d err=%v", run, len(actions.calls), err)
	}
	actions.available = true
	if err := e.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, err = db.GetRoutineRun(run.ID)
	if err != nil || run.State != "completed" || len(actions.calls) != 1 {
		t.Fatalf("restoration: %+v calls=%d err=%v", run, len(actions.calls), err)
	}
	if err := e.TickReviewed(context.Background()); err != nil || len(actions.calls) != 1 {
		t.Fatalf("replayed restored occurrence: %v calls=%d", err, len(actions.calls))
	}
}

func TestDeliveryToARevokedPersonPausesForOwnerResolution(t *testing.T) {
	e, db := harness(t)
	for _, person := range []struct{ name, role string }{{"mika", runtimedb.RoleOwner}, {"sam", runtimedb.RoleMember}} {
		if err := db.RecordButlerMember(person.name, person.role, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember(person.name, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	r := rule("recipient-hold")
	r.Automation.Escalation = nil
	r.Automation.Action.Tool = "chat-test.send"
	r.Automation.Action.Params = map[string]string{"member": "sam", "message": "Hello"}
	raw, err := resource.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: "recipient-hold", Ownership: runtimedb.RoutineWildstone, Author: "mika", Document: string(raw)}); err != nil {
		t.Fatal(err)
	}
	plan, err := db.SubmitRoutinePlan("recipient-hold", "mika", "", runtimedb.AudienceHousehold, "chat", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveRoutinePlan(plan, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireCoordinator("recipient-hold-test"); err != nil {
		t.Fatal(err)
	}
	actions := &routineActions{available: true}
	e.ReviewedActions = actions
	emit(t, db, "old-recipient", "person.left_home", "mika", t0.Add(-time.Hour))
	if err := e.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.DisableButlerMember("sam", "mika"); err != nil {
		t.Fatal(err)
	}
	event := emit(t, db, "revoked-recipient", "person.left_home", "mika", t0)
	if err := e.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, _, err := db.ClaimRoutineRun(plan, event)
	if err == nil || run.ID != 0 {
		t.Fatalf("paused routine still claimable: %+v %v", run, err)
	}
	routine, err := db.GetHouseholdRoutine("recipient-hold")
	if err != nil || routine.State != runtimedb.RoutineEnabled {
		t.Fatalf("recipient hold deleted or disabled routine: %+v %v", routine, err)
	}
	if len(actions.calls) != 0 {
		t.Fatalf("revoked recipient received %d calls", len(actions.calls))
	}
	if err := db.ChangeReviewedRoutine("recipient-hold", "mika", "enable"); err == nil {
		t.Fatal("Owner bypassed recipient resolution with enable")
	}
}

func TestInFlightRunHaltsAtItsNextBoundary(t *testing.T) {
	e, db := harness(t)
	if err := db.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	r := rule("stop-in-flight")
	r.Automation.Escalation = nil
	r.Automation.Action.Tool = "chat-test.send"
	r.Automation.Action.Params = map[string]string{"member": "mika", "message": "Hello"}
	raw, err := resource.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: "stop-in-flight", Ownership: runtimedb.RoutineWildstone, Author: "mika", Document: string(raw)}); err != nil {
		t.Fatal(err)
	}
	plan, err := db.SubmitRoutinePlan("stop-in-flight", "mika", "", runtimedb.AudienceHousehold, "chat", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveRoutinePlan(plan, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireCoordinator("stop-in-flight-test"); err != nil {
		t.Fatal(err)
	}
	actions := &routineActions{available: true}
	e.ReviewedActions = actions
	emit(t, db, "old-in-flight", "person.left_home", "mika", t0.Add(-time.Hour))
	if err := e.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	event := emit(t, db, "in-flight", "person.left_home", "mika", t0)
	actions.beforeDispatch = func() {
		if err := db.ChangeReviewedRoutine("stop-in-flight", "mika", "disable"); err != nil {
			t.Error(err)
		}
	}
	if err := e.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(actions.calls) != 0 {
		t.Fatalf("stopped routine dispatched %d times", len(actions.calls))
	}
	run, _, err := db.ClaimRoutineRun(plan, event)
	if err == nil || run.ID != 0 {
		t.Fatalf("disabled routine reclaimed occurrence: %+v %v", run, err)
	}
}

// A routine that sends to "whoever the event is about" meets an event
// about nobody (a timer set on the tablet by no linked profile). That run
// stops; the routine is not paused, and the next event that names
// somebody is delivered.
func TestAnEventNamingNobodyStopsThatRunOnly(t *testing.T) {
	e, db := harness(t)
	if err := db.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	r := rule("kitchen-timer")
	r.Automation.Escalation = nil
	r.Automation.Trigger = resource.AutomationTrigger{EventType: "pantry.timer_finished"}
	r.Automation.Action.Tool = "chat-test.send"
	r.Automation.Action.Params = map[string]string{"message": "Your kitchen timer is done."}
	raw, err := resource.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: "kitchen-timer", Ownership: runtimedb.RoutineWildstone, Author: "mika", Document: string(raw)}); err != nil {
		t.Fatal(err)
	}
	plan, err := db.SubmitRoutinePlan("kitchen-timer", "mika", "", runtimedb.AudienceHousehold, "chat", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveRoutinePlan(plan, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireCoordinator("kitchen-timer-test"); err != nil {
		t.Fatal(err)
	}
	actions := &routineActions{available: true}
	e.ReviewedActions = actions
	emit(t, db, "old-timer", "pantry.timer_finished", "mika", t0.Add(-time.Hour))
	if err := e.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}

	emit(t, db, "nobodys-timer", "pantry.timer_finished", "", t0)
	if err := e.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(actions.calls) != 0 {
		t.Fatalf("an event naming nobody was delivered: %+v", actions.calls)
	}
	routine, err := db.GetHouseholdRoutine("kitchen-timer")
	if err != nil || routine.State != runtimedb.RoutineEnabled {
		t.Fatalf("an event naming nobody changed the routine: %+v %v", routine, err)
	}

	emit(t, db, "mikas-timer", "pantry.timer_finished", "mika", t0.Add(time.Minute))
	if err := e.TickReviewed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(actions.calls) != 1 || actions.calls[0].Subject != "mika" {
		t.Fatalf("the next timer, set by mika, was not delivered to him: %+v", actions.calls)
	}
}
