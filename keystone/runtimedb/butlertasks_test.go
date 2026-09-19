package runtimedb

// P1's evidence (ADR-W030): existing conversations still read after the
// migration, invalid transitions and missing permission metadata fail
// closed, and no task can appear completed from a partial write.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTask(t *testing.T, db *DB, mutate func(*ButlerTask)) string {
	t.Helper()
	task := ButlerTask{
		Kind: ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: ButlerSurfaceAdmin, Profile: "operator", Audience: ButlerAudiencePrivate,
		Objective: "find out why n2 is offline", CompletionCriteria: "node reachable or cause named",
	}
	if mutate != nil {
		mutate(&task)
	}
	id, err := db.InsertButlerTask(task)
	if err != nil {
		t.Fatalf("InsertButlerTask: %v", err)
	}
	return id
}

func evidence() ButlerTaskEvidence {
	return ButlerTaskEvidence{
		Kind: ButlerEvidenceObservation, Description: "wardstoned on n2 answered Health",
		SourceID: "node:n2", PermissionLabel: "operator", Audience: ButlerAudiencePrivate,
	}
}

func TestButlerTaskRoundTrip(t *testing.T) {
	db := openWith(t, "mika")
	session := newSession(t, db, "looking now", []ButlerTurn{
		{Role: ButlerRoleUser, Text: "why is n2 offline?"},
		{Role: ButlerRoleAssistant, Text: "looking now"},
	})
	turn := 0
	id := newTask(t, db, func(task *ButlerTask) {
		task.SessionID = session
		task.OriginTurn = &turn
		task.GrantsJSON = `{"tools":["node_status"]}`
		task.DeadlineAt = time.Date(2026, 9, 9, 18, 0, 0, 0, time.UTC)
	})

	got, err := db.GetButlerTask(id)
	if err != nil {
		t.Fatalf("GetButlerTask: %v", err)
	}
	if got.State != ButlerTaskPending {
		t.Fatalf("state = %q, want pending", got.State)
	}
	if got.SessionID != session {
		t.Fatalf("sessionId = %q, want %q", got.SessionID, session)
	}
	if got.OriginTurn == nil || *got.OriginTurn != 0 {
		t.Fatalf("originTurn = %v, want 0", got.OriginTurn)
	}
	if got.EnvelopeVersion != TaskEnvelopeVersion {
		t.Fatalf("envelopeVersion = %d, want %d", got.EnvelopeVersion, TaskEnvelopeVersion)
	}
	if got.Retention != ButlerRetentionManual {
		t.Fatalf("retention = %q, want the manual default (ADR-W030 A10)", got.Retention)
	}
	if !got.DeadlineAt.Equal(time.Date(2026, 9, 9, 18, 0, 0, 0, time.UTC)) {
		t.Fatalf("deadlineAt = %v", got.DeadlineAt)
	}
	// The opening transition is written with the row, so history is
	// complete from the first state rather than the first change.
	hist, err := db.ListButlerTaskTransitions(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].ToState != string(ButlerTaskPending) {
		t.Fatalf("history = %+v", hist)
	}
}

// A task links to a conversation; the conversation is not rewritten to
// point back. Attribution recorded in 0003/0014 stays exactly as it was
// (ADR-W030 P1).
func TestButlerTaskDoesNotRewriteConversationAttribution(t *testing.T) {
	db := openWith(t, "mika")
	session := newSession(t, db, "answered", []ButlerTurn{
		{Role: ButlerRoleUser, Text: "status?"},
		{Role: ButlerRoleAssistant, Text: "answered"},
	})
	before, err := db.GetButlerSession(session)
	if err != nil {
		t.Fatal(err)
	}
	newTask(t, db, func(task *ButlerTask) { task.SessionID = session })

	after, err := db.GetButlerSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if after.Principal != before.Principal || after.ActingFor != before.ActingFor ||
		after.Surface != before.Surface || after.FinalText != before.FinalText ||
		len(after.Turns) != len(before.Turns) {
		t.Fatalf("creating a task changed the conversation:\nbefore %+v\nafter  %+v", before, after)
	}
}

// Missing authority metadata is refused at write time: a row that cannot
// be permission-filtered later is worse than no row at all.
func TestButlerTaskRequiresAuthorityMetadata(t *testing.T) {
	db := open(t)
	base := ButlerTask{
		Kind: ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: ButlerSurfaceAdmin, Profile: "operator", Audience: ButlerAudiencePrivate,
		Objective: "check the disk",
	}
	for name, mutate := range map[string]func(*ButlerTask){
		"no owner":     func(x *ButlerTask) { x.Owner = "" },
		"no author":    func(x *ButlerTask) { x.Author = "" },
		"no surface":   func(x *ButlerTask) { x.Surface = "" },
		"bad surface":  func(x *ButlerTask) { x.Surface = "root" },
		"no profile":   func(x *ButlerTask) { x.Profile = "" },
		"no audience":  func(x *ButlerTask) { x.Audience = "" },
		"bad audience": func(x *ButlerTask) { x.Audience = "everyone" },
		"no objective": func(x *ButlerTask) { x.Objective = "" },
		"bad kind":     func(x *ButlerTask) { x.Kind = "errand" },
		"bad grants":   func(x *ButlerTask) { x.GrantsJSON = "{not json" },
	} {
		t.Run(name, func(t *testing.T) {
			task := base
			mutate(&task)
			if _, err := db.InsertButlerTask(task); err == nil {
				t.Fatal("InsertButlerTask accepted a task with missing or invalid authority metadata")
			}
		})
	}
}

func TestButlerTaskTransitionsAreClosed(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, nil)

	if err := db.TransitionButlerTask(id, ButlerTaskFailed, "no such node", "butler"); err == nil {
		t.Fatal("pending → failed was accepted; the table does not allow it")
	}
	if err := db.TransitionButlerTask(id, ButlerTaskRunning, "picked up", "butler"); err != nil {
		t.Fatalf("pending → running: %v", err)
	}
	if err := db.TransitionButlerTask(id, ButlerTaskPending, "", ""); err == nil {
		t.Fatal("running → pending was accepted; a task does not un-start")
	}
	if err := db.TransitionButlerTask(id, ButlerTaskCancelled, "user cancelled", "mika"); err != nil {
		t.Fatalf("running → cancelled: %v", err)
	}
	if err := db.TransitionButlerTask(id, ButlerTaskRunning, "", ""); err == nil {
		t.Fatal("cancelled → running was accepted; cancelled is terminal")
	}
	hist, err := db.ListButlerTaskTransitions(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 {
		t.Fatalf("history = %d rows, want created+running+cancelled: %+v", len(hist), hist)
	}
	if hist[2].Actor != "mika" || hist[2].Reason != "user cancelled" {
		t.Fatalf("terminal transition lost its actor/reason: %+v", hist[2])
	}
}

// Completion requires evidence, at both layers: the store refuses an
// empty set, and TransitionButlerTask cannot reach completed at all.
func TestButlerTaskCannotCompleteWithoutEvidence(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, nil)
	if err := db.TransitionButlerTask(id, ButlerTaskRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.TransitionButlerTask(id, ButlerTaskCompleted, "model said so", "butler"); err == nil {
		t.Fatal("TransitionButlerTask reached completed; completion must carry evidence")
	}
	if err := db.CompleteButlerTask(id, nil, "done", "butler"); err == nil {
		t.Fatal("CompleteButlerTask accepted an empty evidence set")
	}
	if got, _ := db.GetButlerTask(id); got.State != ButlerTaskRunning {
		t.Fatalf("state = %q after refused completions, want running", got.State)
	}

	if err := db.CompleteButlerTask(id, []ButlerTaskEvidence{evidence()}, "n2 is back", "butler"); err != nil {
		t.Fatalf("CompleteButlerTask: %v", err)
	}
	got, err := db.GetButlerTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ButlerTaskCompleted || got.ResultSummary != "n2 is back" {
		t.Fatalf("after completion: %+v", got)
	}
}

// The schema refuses the completion transition independently of the Go
// store, so a completed task written by any other route is impossible
// too. This is the partial-write case P1 has to rule out: a process that
// dies between "write the evidence" and "flip the state" must leave work
// to finish, never a completed task that never delivered.
func TestButlerTaskCompletionIsRefusedBySchemaWithoutEvidence(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, nil)
	seq, err := parseButlerTaskID(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE butler_tasks SET state = 'completed' WHERE seq = ?`, seq); err == nil {
		t.Fatal("a raw UPDATE completed a task with no evidence row")
	} else if !strings.Contains(err.Error(), "without evidence") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := db.sql.Exec(
		`INSERT INTO butler_tasks (created_at, updated_at, kind, state, author, owner, surface,
		     profile, audience, objective, completion_criteria, retention, envelope_version)
		 VALUES ('t','t','conversation','completed','a','a','admin','operator','aud_owner','o','c','manual',1)`,
	); err == nil {
		t.Fatal("a task was inserted already completed")
	}
}

// The interruption fixture: evidence is committed, the process dies
// before the state change. Reopening must find running work with its
// evidence intact — recoverable — rather than a completed task.
func TestButlerTaskInterruptedBetweenEvidenceAndCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Reopened by path rather than through openWith, so the cast is
	// recorded here.
	record(t, db, "sam", "mika")
	id := newTask(t, db, nil)
	if err := db.TransitionButlerTask(id, ButlerTaskRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendButlerTaskEvidence(id, evidence()); err != nil {
		t.Fatalf("AppendButlerTaskEvidence: %v", err)
	}
	// The crash: the transition never runs.
	db.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopening after the interruption: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.GetButlerTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ButlerTaskRunning {
		t.Fatalf("state = %q after an interrupted completion, want running", got.State)
	}
	ev, err := reopened.ListButlerTaskEvidence(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev[0].SourceID != "node:n2" {
		t.Fatalf("evidence did not survive: %+v", ev)
	}
	// And the resume can finish it without redoing the observation.
	if err := reopened.CompleteButlerTask(id, []ButlerTaskEvidence{evidence()}, "n2 is back", "butler"); err != nil {
		t.Fatalf("completing after resume: %v", err)
	}
}

func TestButlerEvidenceRequiresPermissionMetadata(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, nil)
	for name, mutate := range map[string]func(*ButlerTaskEvidence){
		"no permission label": func(e *ButlerTaskEvidence) { e.PermissionLabel = "" },
		"no audience":         func(e *ButlerTaskEvidence) { e.Audience = "" },
		"bad audience":        func(e *ButlerTaskEvidence) { e.Audience = "household" },
		"no description":      func(e *ButlerTaskEvidence) { e.Description = "" },
		"bad kind":            func(e *ButlerTaskEvidence) { e.Kind = "vibes" },
		"bad payload":         func(e *ButlerTaskEvidence) { e.PayloadJSON = "{" },
	} {
		t.Run(name, func(t *testing.T) {
			e := evidence()
			mutate(&e)
			if err := db.AppendButlerTaskEvidence(id, e); err == nil {
				t.Fatal("evidence with missing permission metadata was accepted")
			}
			// And it cannot sneak in through completion either.
			if err := db.CompleteButlerTask(id, []ButlerTaskEvidence{e}, "", ""); err == nil {
				t.Fatal("CompleteButlerTask accepted evidence with missing permission metadata")
			}
		})
	}
}

func TestButlerStepsAndAttemptsCheckpoint(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, nil)
	step, err := db.AppendButlerTaskStep(id, ButlerTaskStep{
		Kind: ButlerStepTool, Name: "node_status", ParamsJSON: `{"node":"n2"}`,
	})
	if err != nil {
		t.Fatalf("AppendButlerTaskStep: %v", err)
	}
	attempt, err := db.StartButlerAttempt(step)
	if err != nil {
		t.Fatalf("StartButlerAttempt: %v", err)
	}
	if attempt.Attempt != 1 || attempt.State != ButlerAttemptDispatched {
		t.Fatalf("attempt = %+v", attempt)
	}
	steps, err := db.ListButlerTaskSteps(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].State != ButlerStepRunning {
		t.Fatalf("steps = %+v", steps)
	}
	if err := db.FinishButlerAttempt(attempt.Seq, ButlerAttemptSucceeded, `{"online":false}`, ""); err != nil {
		t.Fatalf("FinishButlerAttempt: %v", err)
	}
	// A duplicate delivery must not reopen settled state.
	if err := db.FinishButlerAttempt(attempt.Seq, ButlerAttemptFailed, "{}", "timeout"); err == nil {
		t.Fatal("a second result was accepted for the same attempt")
	}
	if err := db.SetButlerStepState(step, ButlerStepVerified); err != nil {
		t.Fatal(err)
	}
	attempts, err := db.ListButlerTaskAttempts(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].State != ButlerAttemptSucceeded ||
		attempts[0].ResultJSON != `{"online":false}` {
		t.Fatalf("attempts = %+v", attempts)
	}
}

// A stall fences the in-flight attempt. When the abandoned provider
// finally answers, its result is refused rather than applied over the
// state the fencing settled (ADR-W030 A4).
func TestButlerAttemptFencedByNewerEpoch(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, nil)
	step, err := db.AppendButlerTaskStep(id, ButlerTaskStep{Kind: ButlerStepModel, Name: "answer"})
	if err != nil {
		t.Fatal(err)
	}
	stalled, err := db.StartButlerAttempt(step)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := db.FenceButlerTaskAttempts(id)
	if err != nil {
		t.Fatalf("FenceButlerTaskAttempts: %v", err)
	}
	if epoch != 1 {
		t.Fatalf("epoch = %d, want 1", epoch)
	}
	err = db.FinishButlerAttempt(stalled.Seq, ButlerAttemptSucceeded, `{"text":"late"}`, "")
	if !errors.Is(err, ErrAttemptFenced) {
		t.Fatalf("late result error = %v, want ErrAttemptFenced", err)
	}
	// The retry runs at the new epoch and lands normally.
	retry, err := db.StartButlerAttempt(step)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Epoch != 1 || retry.Attempt != 2 {
		t.Fatalf("retry = %+v", retry)
	}
	if err := db.FinishButlerAttempt(retry.Seq, ButlerAttemptSucceeded, `{"text":"n2 is offline"}`, ""); err != nil {
		t.Fatalf("finishing the retry: %v", err)
	}
	attempts, _ := db.ListButlerTaskAttempts(id)
	if len(attempts) != 2 || attempts[0].State != ButlerAttemptFenced {
		t.Fatalf("attempts = %+v", attempts)
	}
	// The lease only moves forward.
	if _, err := db.sql.Exec(`UPDATE butler_tasks SET lease_epoch = 0 WHERE seq = 1`); err == nil {
		t.Fatal("the lease epoch was rewound")
	}
}

func TestButlerTaskWaitsAreSingleAndClosed(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, nil)
	if err := db.TransitionButlerTask(id, ButlerTaskRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	resume := time.Date(2026, 9, 9, 7, 30, 0, 0, time.UTC)
	if err := db.OpenButlerTaskWait(id, ButlerTaskWait{
		Reason: ButlerWaitApproval, Detail: "plan-12", ResumeAt: resume,
	}); err != nil {
		t.Fatalf("OpenButlerTaskWait: %v", err)
	}
	got, _ := db.GetButlerTask(id)
	if got.State != ButlerTaskWaiting || !got.NextWakeupAt.Equal(resume) {
		t.Fatalf("task = %+v", got)
	}
	if err := db.OpenButlerTaskWait(id, ButlerTaskWait{Reason: ButlerWaitReply}); err == nil {
		t.Fatal("a second open wait was accepted; a task waits for one thing")
	}
	if err := db.OpenButlerTaskWait(id, ButlerTaskWait{Reason: "because"}); err == nil {
		t.Fatal("an unknown wait reason was accepted")
	}
	if err := db.ResolveButlerTaskWait(id, "mika", "approved"); err != nil {
		t.Fatalf("ResolveButlerTaskWait: %v", err)
	}
	got, _ = db.GetButlerTask(id)
	if got.State != ButlerTaskRunning || !got.NextWakeupAt.IsZero() {
		t.Fatalf("after resolve: %+v", got)
	}
	waits, err := db.ListButlerTaskWaits(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0].ResolvedBy != "mika" || waits[0].ResolvedAt.IsZero() {
		t.Fatalf("waits = %+v", waits)
	}
	if err := db.ResolveButlerTaskWait(id, "mika", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolving with no open wait = %v, want ErrNotFound", err)
	}
}

// Offboarding transfers operational ownership and preserves authorship
// (ADR-W030 A4). The store enforces the second half.
func TestButlerTaskAuthorshipIsImmutable(t *testing.T) {
	db := openWith(t, "sam", "mika")
	id := newTask(t, db, func(task *ButlerTask) {
		task.Author = "sam"
		task.Owner = "sam"
		task.Audience = ButlerAudienceShared
	})
	seq, _ := parseButlerTaskID(id)
	if _, err := db.sql.Exec(
		`UPDATE butler_tasks SET owner_person_id = (SELECT person_id FROM butler_members WHERE member = 'mika')
		 WHERE seq = ?`, seq); err != nil {
		t.Fatalf("transferring ownership: %v", err)
	}
	if _, err := db.sql.Exec(
		`UPDATE butler_tasks SET author_person_id = (SELECT person_id FROM butler_members WHERE member = 'mika')
		 WHERE seq = ?`, seq); err == nil {
		t.Fatal("authorship was rewritten")
	}
	got, _ := db.GetButlerTask(id)
	if got.Owner != "mika" || got.Author != "sam" {
		t.Fatalf("task = %+v", got)
	}
}

func TestButlerTraceRequiresPermissionMetadata(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, nil)
	base := ButlerTraceEvent{
		Type: TraceToolDispatched, PermissionLabel: "operator",
		Audience: ButlerAudiencePrivate, Redacted: true,
		SourceIDs: []string{"node:n2"}, ModelEndpoint: "ollama/llama3",
		PromptVersion: "p7", PolicyVersion: "pol3", ManifestVersion: "m2",
		PayloadJSON: `{"tool":"node_status"}`,
	}
	if err := db.AppendButlerTraceEvent(id, base); err != nil {
		t.Fatalf("AppendButlerTraceEvent: %v", err)
	}
	for name, mutate := range map[string]func(*ButlerTraceEvent){
		"no type":             func(e *ButlerTraceEvent) { e.Type = "" },
		"no permission label": func(e *ButlerTraceEvent) { e.PermissionLabel = "" },
		"no audience":         func(e *ButlerTraceEvent) { e.Audience = "" },
		"bad audience":        func(e *ButlerTraceEvent) { e.Audience = "public" },
		"comma in source id":  func(e *ButlerTraceEvent) { e.SourceIDs = []string{"a,b"} },
		"bad payload":         func(e *ButlerTraceEvent) { e.PayloadJSON = "nope" },
	} {
		t.Run(name, func(t *testing.T) {
			e := base
			mutate(&e)
			if err := db.AppendButlerTraceEvent(id, e); err == nil {
				t.Fatal("a trace event with missing permission metadata was accepted")
			}
		})
	}
	trace, err := db.ListButlerTrace(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(trace) != 1 {
		t.Fatalf("trace = %d rows, want the one valid event", len(trace))
	}
	got := trace[0]
	if got.PromptVersion != "p7" || got.PolicyVersion != "pol3" || got.ManifestVersion != "m2" {
		t.Fatalf("version metadata lost: %+v", got)
	}
	if len(got.SourceIDs) != 1 || got.SourceIDs[0] != "node:n2" || !got.Redacted {
		t.Fatalf("event = %+v", got)
	}
	if got.EnvelopeVersion != TaskEnvelopeVersion {
		t.Fatalf("envelopeVersion = %d", got.EnvelopeVersion)
	}
}

// The trace and the transition history are append-only. Retention may
// DELETE them; nothing may edit them after the fact (ADR-W030 A11).
func TestButlerTraceAndHistoryAreAppendOnly(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, nil)
	if err := db.AppendButlerTraceEvent(id, ButlerTraceEvent{
		Type: TraceOutcome, PermissionLabel: "operator", Audience: ButlerAudiencePrivate,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendButlerTaskEvidence(id, evidence()); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE butler_work_trace SET payload_json = '{"x":1}'`,
		`UPDATE butler_task_transitions SET to_state = 'completed'`,
		`UPDATE butler_task_evidence SET description = 'rewritten'`,
	} {
		if _, err := db.sql.Exec(stmt); err == nil {
			t.Fatalf("append-only row was edited by: %s", stmt)
		}
	}
	// Deletion stays available for retention and offboarding (P16).
	if _, err := db.sql.Exec(`DELETE FROM butler_work_trace`); err != nil {
		t.Fatalf("retention delete refused: %v", err)
	}
}

// The whole point of a durable ledger: what is running after a restart.
func TestButlerTasksInStateIsTheResumeQueue(t *testing.T) {
	db := openWith(t, "mika")
	running := newTask(t, db, nil)
	newTask(t, db, nil) // stays pending
	if err := db.TransitionButlerTask(running, ButlerTaskRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := db.ButlerTasksInState(ButlerTaskRunning)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != running {
		t.Fatalf("resume queue = %+v", got)
	}
	owned, err := db.ListButlerTasks(10, "mika")
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 2 {
		t.Fatalf("owner listing = %d, want 2", len(owned))
	}
	if none, err := db.ListButlerTasks(10, "sam"); err != nil || len(none) != 0 {
		t.Fatalf("listing another member's tasks = %+v, %v", none, err)
	}
}

// 0017 adds tables; it touches nothing that already existed. A
// conversation written before the migration reads back identically
// afterwards, and the household timeline's deterministic replay still
// works with an empty task table (ADR-W030 A4, P1).
func TestMigration0017LeavesExistingConversationsReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	db, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.migrateThrough("migrations/0016_observed_app_health.sql"); err != nil {
		t.Fatalf("migrating through 0016: %v", err)
	}
	id := insertSessionAtSchema(t, db, "n2 has been offline since 14:04.", []ButlerTurn{
		{Role: ButlerRoleUser, Text: "why is n2 offline?"},
		{Role: ButlerRoleAssistant, Text: "n2 has been offline since 14:04."},
	})
	db.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (applies 0017): %v", err)
	}
	defer db2.Close()
	sess, err := db2.GetButlerSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ActingFor != "mika" || sess.Surface != ButlerSurfaceAdmin ||
		sess.FinalText != "n2 has been offline since 14:04." || len(sess.Turns) != 2 {
		t.Fatalf("0017 changed an existing conversation: %+v", sess)
	}
	if sess.Turns[0].Role != ButlerRoleUser || sess.Turns[0].Text != "why is n2 offline?" {
		t.Fatalf("turn attribution changed: %+v", sess.Turns[0])
	}
	// No tasks exist for it, and nothing requires any.
	tasks, err := db2.ListButlerTasks(10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("migration invented tasks: %+v", tasks)
	}
	// And the conversation can still be continued.
	if err := db2.AppendButlerTurns(id, []ButlerTurn{
		{Role: ButlerRoleUser, Text: "and now?"},
		{Role: ButlerRoleAssistant, Text: "still offline."},
	}); err != nil {
		t.Fatalf("appending after 0017: %v", err)
	}
}
