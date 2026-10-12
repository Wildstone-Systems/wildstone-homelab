package runtimedb

// Store-level evidence for scheduled subscriptions (0099, epic #326 S03,
// #456): proposal bounds and Person binding, exact-version approval, the
// no-overlap index, the checkpoint compare-and-swap, and restore.
// The scheduler's own timing is proved in keystone/automation.

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

var s0 = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

// scheduledFixture is one Agent, one Personal mail connection owned by
// sam, and an approved scheduled subscription reading it.
func scheduledFixture(t *testing.T) (*DB, AgentSubscription) {
	t.Helper()
	db := butlerFixture(t)
	butler, err := db.CreateButler(Butler{
		Name: "alfred", Surface: ButlerSurfaceAdmin, AgentClass: AgentClassOwner, Model: "kitchen-box",
	}, "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateConnection(IntegrationConnection{
		Name: "mail-sam", Adapter: "mail", Ownership: OwnershipPersonal, Owner: "sam", CreatedBy: "sam",
	}); err != nil {
		t.Fatal(err)
	}
	id, err := db.ProposeAgentSubscription(AgentSubscriptionSpec{
		Butler: butler, TriggerKind: SubscriptionTriggerSchedule, MatchKind: "mail.changes",
		Capabilities: []string{"peek"}, SourceConnection: "mail-sam",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveAgentSubscription(id, "mika"); err != nil {
		t.Fatal(err)
	}
	sub, err := db.GetAgentSubscription(id)
	if err != nil {
		t.Fatal(err)
	}
	return db, sub
}

func claimFor(t *testing.T, db *DB, sub AgentSubscription, at time.Time) (SubscriptionRun, SubscriptionCheckpoint, error) {
	t.Helper()
	conn, err := db.GetConnection(sub.SourceConnection)
	if err != nil {
		t.Fatal(err)
	}
	return db.ClaimSubscriptionRun(SubscriptionRunClaim{
		Subscription: sub.ID, Version: sub.Version, SourceConnection: sub.SourceConnection,
		ConnectionEpoch: conn.LeaseEpoch, PersonID: sub.PersonID, At: at,
	})
}

func TestRuntimeRevocationDisablesOnlyItsSubscriptionsAndFencesRuns(t *testing.T) {
	db, sub := scheduledFixture(t)
	if err := db.SetAgentSubscriptionCursor(sub.ID, 41); err != nil {
		t.Fatal(err)
	}
	run, beforeCheckpoint, err := claimFor(t, db, sub, s0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateButler(Butler{
		Name: "beatrice", Surface: ButlerSurfaceFamily, AgentClass: AgentClassMember, Model: "kitchen-box",
	}, "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := db.InsertAgentSubscription(other, "event_type", `{}`, "", []string{"peek"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveAgentSubscription(otherID, "mika"); err != nil {
		t.Fatal(err)
	}

	n, err := db.DisableAgentSubscriptionsForRuntime(sub.Butler, "Agent runtime revoked")
	if err != nil || n != 1 {
		t.Fatalf("DisableAgentSubscriptionsForRuntime = %d, %v; want 1, nil", n, err)
	}
	after, err := db.GetAgentSubscription(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Enabled || after.Cursor != 41 || after.ApprovedVersion != sub.ApprovedVersion || after.Version != sub.Version {
		t.Fatalf("revoked subscription = %+v; wanted disabled with cursor/version/approval retained", after)
	}
	gotRun, err := db.GetSubscriptionRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.State != SubscriptionRunFenced {
		t.Fatalf("in-flight run state = %s, want fenced", gotRun.State)
	}
	afterCheckpoint, err := db.GetSubscriptionCheckpoint(sub.ID, sub.SourceConnection)
	if err != nil {
		t.Fatal(err)
	}
	if afterCheckpoint.Version != beforeCheckpoint.Version || string(afterCheckpoint.Cursor) != string(beforeCheckpoint.Cursor) {
		t.Fatalf("checkpoint changed on runtime revoke: before=%+v after=%+v", beforeCheckpoint, afterCheckpoint)
	}
	otherSub, err := db.GetAgentSubscription(otherID)
	if err != nil {
		t.Fatal(err)
	}
	if !otherSub.Enabled {
		t.Fatal("revoking one Agent's runtime disabled another Agent's subscription")
	}
}

func TestScheduledSubscriptionProposalBoundsAndPersonBinding(t *testing.T) {
	db, sub := scheduledFixture(t)
	if sub.TriggerKind != SubscriptionTriggerSchedule || sub.Cadence != DefaultSubscriptionCadence {
		t.Fatalf("trigger/cadence = %s/%s, want schedule/%s", sub.TriggerKind, sub.Cadence, DefaultSubscriptionCadence)
	}
	sam, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	// Bound to the connection's owning Person, not the approving Owner.
	if sub.PersonID != sam.PersonID {
		t.Fatalf("person = %q, want sam's %q", sub.PersonID, sam.PersonID)
	}
	if !sub.Dispatchable() {
		t.Fatal("an approved subscription is not dispatchable")
	}
	if _, err := db.GetSubscriptionSchedule(sub.ID); err != nil {
		t.Fatalf("approval did not create a schedule: %v", err)
	}
	for _, c := range []time.Duration{time.Minute, 4*time.Minute + 59*time.Second, 25 * time.Hour} {
		if _, err := db.ProposeAgentSubscription(AgentSubscriptionSpec{
			Butler: sub.Butler, TriggerKind: SubscriptionTriggerSchedule, MatchKind: "mail.changes",
			Capabilities: []string{"peek"}, Cadence: c,
		}); err == nil {
			t.Errorf("cadence %s was accepted", c)
		}
	}
	for _, c := range []time.Duration{MinSubscriptionCadence, MaxSubscriptionCadence} {
		if _, err := db.ProposeAgentSubscription(AgentSubscriptionSpec{
			Butler: sub.Butler, TriggerKind: SubscriptionTriggerSchedule, MatchKind: "mail.changes",
			Capabilities: []string{"peek"}, Cadence: c,
		}); err != nil {
			t.Errorf("cadence %s was refused: %v", c, err)
		}
	}
	if _, err := db.ProposeAgentSubscription(AgentSubscriptionSpec{
		Butler: sub.Butler, MatchKind: "event_type", Capabilities: []string{"peek"}, SourceConnection: "mail-sam",
	}); err == nil {
		t.Error("an event subscription accepted a source")
	}
	if _, err := db.ProposeAgentSubscription(AgentSubscriptionSpec{
		Butler: sub.Butler, TriggerKind: SubscriptionTriggerSchedule, MatchKind: "mail.changes",
		Capabilities: []string{"peek"}, SourceConnection: "no-such-connection",
	}); err == nil {
		t.Error("a subscription accepted a source that is not a connection")
	}
	// A scheduled subscription is never one the event evaluator lists.
	events, err := db.ListEnabledAgentSubscriptions()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.ID == sub.ID {
			t.Fatal("the event evaluator lists a scheduled subscription")
		}
	}
}

func TestSubscriptionEditRequiresApprovalOfThatExactVersion(t *testing.T) {
	db, sub := scheduledFixture(t)
	run, _, err := claimFor(t, db, sub, s0)
	if err != nil {
		t.Fatal(err)
	}
	match := `{"folders":["INBOX","School"]}`
	v, err := db.ReviseAgentSubscription(sub.ID, AgentSubscriptionRevision{MatchJSON: &match}, "sam")
	if err != nil {
		t.Fatal(err)
	}
	if v != 2 {
		t.Fatalf("version = %d, want 2", v)
	}
	edited, _ := db.GetAgentSubscription(sub.ID)
	if edited.Dispatchable() {
		t.Fatal("an edited subscription is dispatchable before its new version is approved")
	}
	if _, enabled, _ := db.SubscriptionCapabilities(sub.ID); enabled {
		t.Fatal("a Task spawned under the old version still reads as enabled")
	}
	// The run in flight was fenced by the edit and commits nothing.
	if _, err := db.CompleteSubscriptionRun(run.ID, SubscriptionRunReport{
		Outcome: RunOutcomeOK, Checkpoint: json.RawMessage(`{"uid":9}`), At: s0.Add(time.Minute),
	}); !errors.Is(err, ErrSubscriptionRunFenced) {
		t.Fatalf("completing a run fenced by an edit = %v, want ErrSubscriptionRunFenced", err)
	}
	if cp, _ := db.GetSubscriptionCheckpoint(sub.ID, "mail-sam"); cp.Version != 0 {
		t.Fatalf("a fenced run committed checkpoint version %d", cp.Version)
	}
	if _, _, err := claimFor(t, db, edited, s0.Add(time.Hour)); !errors.Is(err, ErrSubscriptionNotDispatchable) {
		t.Fatalf("claim on an unapproved edit = %v", err)
	}
	// Approving a version that is no longer current enables nothing.
	if _, err := db.ReviseAgentSubscription(sub.ID, AgentSubscriptionRevision{MatchJSON: &match}, "sam"); err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveAgentSubscriptionVersion(sub.ID, 2, "mika"); err == nil {
		t.Fatal("approving version 2 succeeded after the row moved to version 3")
	}
	if err := db.ApproveAgentSubscriptionVersion(sub.ID, 3, "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveAgentSubscriptionVersion(sub.ID, 3, "mika"); err == nil {
		t.Fatal("the same version was approved twice")
	}
	approved, _ := db.GetAgentSubscription(sub.ID)
	if !approved.Dispatchable() || approved.ApprovedVersion != 3 {
		t.Fatalf("after approving v3: %+v", approved)
	}
}

func TestSubscriptionRunsNeverOverlap(t *testing.T) {
	db, sub := scheduledFixture(t)
	if _, _, err := claimFor(t, db, sub, s0); err != nil {
		t.Fatal(err)
	}
	// Due again a cadence later, but the first run is still in flight.
	if _, _, err := claimFor(t, db, sub, s0.Add(sub.Cadence)); !errors.Is(err, ErrSubscriptionRunInFlight) {
		t.Fatalf("second claim = %v, want ErrSubscriptionRunInFlight", err)
	}
}

func TestSubscriptionCheckpointIsCompareAndSwap(t *testing.T) {
	db, sub := scheduledFixture(t)
	run1, cp, err := claimFor(t, db, sub, s0)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Version != 0 || cp.Cursor != nil {
		t.Fatalf("first run was handed %+v, want no checkpoint", cp)
	}
	if _, err := db.CompleteSubscriptionRun(run1.ID, SubscriptionRunReport{
		Outcome: RunOutcomeOK, Checkpoint: json.RawMessage(`{"uid":10}`), At: s0.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	// A report for a finished run is refused and changes nothing.
	if _, err := db.CompleteSubscriptionRun(run1.ID, SubscriptionRunReport{
		Outcome: RunOutcomeOK, Checkpoint: json.RawMessage(`{"uid":99}`), At: s0.Add(2 * time.Minute),
	}); !errors.Is(err, ErrSubscriptionRunFenced) {
		t.Fatalf("double report = %v, want ErrSubscriptionRunFenced", err)
	}
	run2, cp, err := claimFor(t, db, sub, s0.Add(sub.Cadence))
	if err != nil {
		t.Fatal(err)
	}
	if cp.Version != 1 || string(cp.Cursor) != `{"uid":10}` {
		t.Fatalf("second run was handed %+v, want v1 {uid:10}", cp)
	}
	// Something moved the checkpoint under run2 (defence in depth: the
	// no-overlap index makes this unreachable through the API).
	if _, err := db.sql.Exec(`UPDATE agent_subscription_checkpoints SET version = 7`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CompleteSubscriptionRun(run2.ID, SubscriptionRunReport{
		Outcome: RunOutcomeOK, Checkpoint: json.RawMessage(`{"uid":20}`), At: s0.Add(sub.Cadence + time.Minute),
	}); !errors.Is(err, ErrCheckpointConflict) {
		t.Fatalf("stale-base commit = %v, want ErrCheckpointConflict", err)
	}
	got, _ := db.GetSubscriptionCheckpoint(sub.ID, "mail-sam")
	if string(got.Cursor) != `{"uid":10}` {
		t.Fatalf("a conflicting commit regressed or overwrote the checkpoint: %s", got.Cursor)
	}
	r2, _ := db.GetSubscriptionRun(run2.ID)
	if r2.State != SubscriptionRunFenced {
		t.Fatalf("conflicting run state = %s, want fenced", r2.State)
	}
}

func TestSubscriptionRunOutcomesAreDistinct(t *testing.T) {
	db, sub := scheduledFixture(t)
	run, _, err := claimFor(t, db, sub, s0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CompleteSubscriptionRun(run.ID, SubscriptionRunReport{
		Outcome: RunOutcomeUnavailable, Checkpoint: json.RawMessage(`{}`), At: s0,
	}); err == nil {
		t.Fatal("an unavailable run committed a checkpoint")
	}
	if _, err := db.CompleteSubscriptionRun(run.ID, SubscriptionRunReport{Outcome: "fine", At: s0}); err == nil {
		t.Fatal("an unknown outcome was accepted")
	}
	if _, err := db.CompleteSubscriptionRun(run.ID, SubscriptionRunReport{
		Outcome: RunOutcomeAuthFailed, Error: "LOGIN refused", At: s0.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	sched, _ := db.GetSubscriptionSchedule(sub.ID)
	if sched.Hold != SubscriptionHoldAuthFailed || sched.LastOutcome != RunOutcomeAuthFailed || sched.ConsecutiveFailures != 1 {
		t.Fatalf("after auth failure: %+v", sched)
	}
}

func TestSubscriptionRetryDelayIsBounded(t *testing.T) {
	for _, c := range []struct {
		cadence  time.Duration
		failures int
		want     time.Duration
	}{
		{15 * time.Minute, 1, time.Minute},
		{15 * time.Minute, 2, 2 * time.Minute},
		{15 * time.Minute, 4, 8 * time.Minute},
		{15 * time.Minute, 5, 15 * time.Minute},
		{15 * time.Minute, 500, 15 * time.Minute},
		{24 * time.Hour, 500, time.Hour},
		{5 * time.Minute, 3, 4 * time.Minute},
		{5 * time.Minute, 4, 5 * time.Minute},
	} {
		if got := SubscriptionRetryDelay(c.cadence, c.failures); got != c.want {
			t.Errorf("SubscriptionRetryDelay(%s, %d) = %s, want %s", c.cadence, c.failures, got, c.want)
		}
	}
}

// TestRestoreAbandonsInFlightRunsAndKeepsCheckpoint: a run in flight in
// the snapshot belonged to the lost machine. After restore it is
// abandoned (it can never report), and the checkpoint is exactly the
// committed one, so the first run after release re-reads rather than
// skips.
func TestRestoreAbandonsInFlightRunsAndKeepsCheckpoint(t *testing.T) {
	db, sub := scheduledFixture(t)
	run1, _, err := claimFor(t, db, sub, s0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CompleteSubscriptionRun(run1.ID, SubscriptionRunReport{
		Outcome: RunOutcomeOK, Checkpoint: json.RawMessage(`{"uid":10}`), At: s0.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	run2, _, err := claimFor(t, db, sub, s0.Add(sub.Cadence))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if err := db.BackupTo(snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreBackup(snapshot, filepath.Join(t.TempDir(), "restored.db"), "test restore", s0)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	r, err := restored.GetSubscriptionRun(run2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != SubscriptionRunAbandoned {
		t.Fatalf("restored in-flight run state = %s, want abandoned", r.State)
	}
	cp, _ := restored.GetSubscriptionCheckpoint(sub.ID, "mail-sam")
	if cp.Version != 1 || string(cp.Cursor) != `{"uid":10}` {
		t.Fatalf("restored checkpoint = %+v", cp)
	}
	// Quarantine holds new claims at the store, whatever the caller.
	if _, _, err := restored.ClaimSubscriptionRun(SubscriptionRunClaim{
		Subscription: sub.ID, Version: sub.Version, SourceConnection: sub.SourceConnection,
		ConnectionEpoch: 1, PersonID: sub.PersonID, At: s0.Add(time.Hour),
	}); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("claim under restore quarantine = %v, want ErrQuarantined", err)
	}
}
