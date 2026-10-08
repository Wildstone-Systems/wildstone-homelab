package runtimedb

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func workspacePrivacyTask(t *testing.T, db *DB) (string, string) {
	t.Helper()
	session, err := db.InsertButlerSession(ButlerSession{Principal: "mika", ActingFor: "mika", Butler: "alfred", Surface: ButlerSurfaceAdmin, Mode: ButlerModeFull})
	if err != nil {
		t.Fatal(err)
	}
	id := newTask(t, db, func(task *ButlerTask) {
		task.AgentClass = AgentClassOwner
		task.OriginConnection = "console"
		task.SessionID = session
	})
	if err = db.SetTaskButler(id, "alfred"); err != nil {
		t.Fatal(err)
	}
	return id, session
}
func workspacePrivacyTurn(t *testing.T, db *DB, task, model string, d MemoryDestination) (context.Context, *PrivacyTurn, *WorkspaceAccess) {
	t.Helper()
	ctx, p, err := db.BeginPrivacyTurn(context.Background(), model, d, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = p.SetTask(task); err != nil {
		t.Fatal(err)
	}
	return ctx, p, db.BindWorkspaceAccess("mika", "alfred", task, "console")
}
func workspaceFinishRevision(t *testing.T, db *DB, w Workspace, task int64, artifact, revision, base string) {
	t.Helper()
	reservation := "reserve-" + revision
	if err := db.ReserveWorkspaceBytes(WorkspaceReservation{ID: reservation, WorkspaceID: w.ID, TaskID: task, SizeBytes: 1, Purpose: "publication"}); err != nil {
		t.Fatal(err)
	}
	p := WorkspacePublication{ID: "publish-" + revision, WorkspaceID: w.ID, ArtifactID: artifact, RevisionID: revision, BaseRevision: base, TaskID: task, ReservationID: reservation, EffectKey: "effect-" + revision}
	if err := db.CreateWorkspacePublication(p); err != nil {
		t.Fatal(err)
	}
	workspacePublish(t, db, p, "receipt-"+revision)
}
func workspaceNewRevision(id, artifact string) WorkspaceRevision {
	return WorkspaceRevision{ID: id, ArtifactID: artifact, Hash: strings.Repeat("a", 64), SizeBytes: 1, MediaType: "text/plain"}
}

func TestWorkspacePrivacyInheritanceReadResumeAndTightening(t *testing.T) {
	db, w, origin := workspaceAuthorityFixture(t)
	sourceTask, _ := parseButlerTaskID(origin)
	workspacePublish(t, db, workspaceIntent(t, db, w, sourceTask, "source", "source-r1", "", 1), "source-receipt")
	task, session := workspacePrivacyTask(t, db)
	ctx, guard, access := workspacePrivacyTurn(t, db, task, ModelLocal, MemoryDestination{BuiltIn: true})
	if _, err := db.ReadWorkspaceRevision(ctx, access, w.ID, "source", "source-r1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "derived", WorkspaceID: w.ID, Path: "derived.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateWorkspaceRevisionForTask(ctx, access, w.ID, "", workspaceNewRevision("derived-r1", "derived")); err != nil {
		t.Fatal(err)
	}
	taskSeq, _ := parseButlerTaskID(task)
	workspaceFinishRevision(t, db, w, taskSeq, "derived", "derived-r1", "")
	// A human privacy update is represented by the existing policy table. No
	// tool/API for changing these marks is introduced by this foundation slice.
	if _, err := db.sql.Exec(`INSERT INTO content_privacy(object,local_only,trusted_only) VALUES('wa:source',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := guard.Check(); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckSessionPrivacy(session, ModelCloud, MemoryDestination{BuiltIn: true}); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("session lost source taint: %v", err)
	}
	for _, destination := range []struct {
		model string
		d     MemoryDestination
	}{
		{ModelCloud, MemoryDestination{BuiltIn: true}}, {ModelLocal, MemoryDestination{}},
	} {
		next, _ := workspacePrivacyTask(t, db)
		c, _, a := workspacePrivacyTurn(t, db, next, destination.model, destination.d)
		got, err := db.ReadWorkspaceRevision(c, a, w.ID, "derived", "derived-r1")
		if !errors.Is(err, ErrPrivacy) || got.ID != "" {
			t.Fatalf("inherited read disclosed metadata: %+v %v", got, err)
		}
		seq, _ := parseButlerTaskID(next)
		var count int
		if err = db.sql.QueryRow(`SELECT COUNT(*) FROM workspace_task_sources WHERE task=?`, seq).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed read mutated provenance: %d %v", count, err)
		}
	}
	next, _ := workspacePrivacyTask(t, db)
	c, _, a := workspacePrivacyTurn(t, db, next, ModelLocal, MemoryDestination{BuiltIn: true})
	if err := db.CreateWorkspaceRevisionForTask(c, a, w.ID, "derived-r1", workspaceNewRevision("derived-r2", "derived")); err != nil {
		t.Fatal(err)
	}
	var privacy string
	var trusted bool
	if err := db.sql.QueryRow(`SELECT privacy,trusted_only FROM workspace_revisions WHERE id='derived-r2'`).Scan(&privacy, &trusted); err != nil || privacy != "local-only" || !trusted {
		t.Fatalf("base inheritance: %s %v %v", privacy, trusted, err)
	}
	path := db.path
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = reopened.CheckSessionPrivacy(session, ModelCloud, MemoryDestination{BuiltIn: true}); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("restart lost provenance: %v", err)
	}
	_, resumed, _ := workspacePrivacyTurn(t, reopened, task, ModelCloud, MemoryDestination{BuiltIn: true})
	if err = resumed.Check(); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("resumed task lost provenance: %v", err)
	}
}

func TestWorkspacePrivacyMissingContextAndFailuresLeaveNoRevision(t *testing.T) {
	db, w, id := workspaceAuthorityFixture(t)
	a := db.BindWorkspaceAccess("mika", "alfred", id, "console")
	if err := db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "draft", WorkspaceID: w.ID, Path: "draft.txt"}); err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{context.Background(), func() context.Context {
		c, _, err := db.BeginPrivacyTurn(context.Background(), ModelLocal, MemoryDestination{BuiltIn: true}, "")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}()} {
		if err := db.CreateWorkspaceRevisionForTask(ctx, a, w.ID, "", workspaceNewRevision("missing-context", "draft")); !errors.Is(err, ErrWorkspaceContext) {
			t.Fatal(err)
		}
	}
	ctx, _, a := workspacePrivacyTurn(t, db, id, ModelCloud, MemoryDestination{BuiltIn: true})
	seq, err := db.RecordMemory(MemoryRecord{Scope: MemoryScopePersonal, Kind: MemoryFact, Content: "synthetic restricted source", Owner: "mika", Author: "mika", Butler: "alfred", Audience: ButlerAudiencePrivate, PermissionLabel: "household", SourceKind: "conversation", SourceID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetMemoryPrivacy("mika", seq, PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err = TrackMemoryPrivacy(ctx, seq, nil); err != nil {
		t.Fatal(err)
	}
	if err = db.CreateWorkspaceRevisionForTask(ctx, a, w.ID, "", workspaceNewRevision("denied", "draft")); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("restricted write: %v", err)
	}
	var count int
	if err = db.sql.QueryRow(`SELECT COUNT(*) FROM workspace_revisions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed write left revision: %d %v", count, err)
	}
	if err = db.sql.QueryRow(`SELECT COUNT(*) FROM task_memory_sources`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed write left evidence: %d %v", count, err)
	}
}

// A migration that lands after a later one was already applied (two
// branches numbering independently) still runs; the runner does not
// treat a gap as "already past it".
func TestMigrationRunnerAppliesALateGapMigration(t *testing.T) {
	var err error
	runner := runnerDB(t)
	fs := migrationSet(map[string]string{"0001_base.sql": "CREATE TABLE ordering(n INTEGER); INSERT INTO ordering VALUES(1);", "0003_later.sql": "INSERT INTO ordering VALUES(3);"})
	if err = runner.migrateFrom(fs, ""); err != nil {
		t.Fatal(err)
	}
	fs["migrations/0002_middle.sql"] = migrationSet(map[string]string{"0002_middle.sql": "INSERT INTO ordering VALUES(2);"})["migrations/0002_middle.sql"]
	if err = runner.migrateFrom(fs, ""); err != nil {
		t.Fatal(err)
	}
	var order string
	if err = runner.sql.QueryRow(`SELECT group_concat(n,',') FROM ordering`).Scan(&order); err != nil || order != "1,3,2" {
		t.Fatalf("runner gap behavior: %s %v", order, err)
	}
}

func TestWorkspacePrivacyMemorySourcesAndAtomicFailure(t *testing.T) {
	db, w, id := workspaceAuthorityFixture(t)
	ctx, guard, a := workspacePrivacyTurn(t, db, id, ModelLocal, MemoryDestination{BuiltIn: true})
	memory, err := db.RecordMemory(MemoryRecord{Scope: MemoryScopePersonal, Kind: MemoryFact, Content: "synthetic input", Owner: "mika", Author: "mika", Butler: "alfred", Audience: ButlerAudiencePrivate, PermissionLabel: "household", SourceKind: "conversation", SourceID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err = TrackMemoryPrivacy(ctx, memory, nil); err != nil {
		t.Fatal(err)
	}
	if err = db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "output", WorkspaceID: w.ID, Path: "output.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.sql.Exec(`CREATE TRIGGER reject_test_revision BEFORE INSERT ON workspace_revisions BEGIN SELECT RAISE(ABORT,'injected metadata failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err = db.CreateWorkspaceRevisionForTask(ctx, a, w.ID, "", workspaceNewRevision("output-r1", "output")); err == nil {
		t.Fatal("injected failure ignored")
	}
	var count int
	if err = db.sql.QueryRow(`SELECT COUNT(*) FROM task_memory_sources`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial evidence survived: %d %v", count, err)
	}
	if _, err = db.sql.Exec(`DROP TRIGGER reject_test_revision`); err != nil {
		t.Fatal(err)
	}
	if err = db.CreateWorkspaceRevisionForTask(ctx, a, w.ID, "", workspaceNewRevision("output-r1", "output")); err != nil {
		t.Fatal(err)
	}
	task, _ := parseButlerTaskID(id)
	workspaceFinishRevision(t, db, w, task, "output", "output-r1", "")
	if err = db.SetMemoryPrivacy("mika", memory, PrivacyPolicy{LocalOnly: true, TrustedOnly: true}); err != nil {
		t.Fatal(err)
	}
	next, _ := workspacePrivacyTask(t, db)
	cloud, _, reader := workspacePrivacyTurn(t, db, next, ModelCloud, MemoryDestination{BuiltIn: true})
	if got, err := db.ReadWorkspaceRevision(cloud, reader, w.ID, "output", "output-r1"); !errors.Is(err, ErrPrivacy) || got.ID != "" {
		t.Fatalf("later source tightening lost: %+v %v", got, err)
	}
	if err = guard.Check(); err != nil {
		t.Fatal(err)
	}
	if err = CheckPrivacyDelivery(ctx, MemoryDestination{}); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("output disclosure lost provenance: %v", err)
	}
	// A source that is no longer authorized cannot be laundered by a local route.
	if _, err = db.sql.Exec(`UPDATE butler_memory SET forgotten_at='2026-09-30T00:00:00Z' WHERE seq=?`, memory); err != nil {
		t.Fatal(err)
	}
	local, _, reader := workspacePrivacyTurn(t, db, next, ModelLocal, MemoryDestination{BuiltIn: true})
	if got, err := db.ReadWorkspaceRevision(local, reader, w.ID, "output", "output-r1"); !errors.Is(err, ErrWorkspaceDenied) || got.ID != "" {
		t.Fatalf("forgotten source disclosed: %+v %v", got, err)
	}
}

func TestWorkspacePrivacyReadFailureAndUnknownSourceFailClosed(t *testing.T) {
	db, w, id := workspaceAuthorityFixture(t)
	task, _ := parseButlerTaskID(id)
	workspacePublish(t, db, workspaceIntent(t, db, w, task, "source", "r1", "", 1), "receipt")
	next, _ := workspacePrivacyTask(t, db)
	ctx, _, a := workspacePrivacyTurn(t, db, next, ModelLocal, MemoryDestination{BuiltIn: true})
	if _, err := db.sql.Exec(`CREATE TRIGGER reject_test_read BEFORE INSERT ON workspace_task_sources BEGIN SELECT RAISE(ABORT,'injected source persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	if got, err := db.ReadWorkspaceRevision(ctx, a, w.ID, "source", "r1"); err == nil || got.ID != "" {
		t.Fatalf("failed provenance commit disclosed metadata: %+v %v", got, err)
	}
	if _, err := db.sql.Exec(`DROP TRIGGER reject_test_read`); err != nil {
		t.Fatal(err)
	}
	p := workspaceIntent(t, db, w, task, "unknown", "r2", "", 1)
	if err := db.RecordWorkspaceProvenance(WorkspaceProvenance{ID: "unresolved", RevisionID: "r2", SourceID: "unrecognized-source", SourceVersion: "v1", Privacy: "normal", Audience: "private"}); err != nil {
		t.Fatal(err)
	}
	workspacePublish(t, db, p, "receipt-unknown")
	if got, err := db.ReadWorkspaceRevision(ctx, a, w.ID, "unknown", "r2"); !errors.Is(err, ErrWorkspaceDenied) || got.ID != "" {
		t.Fatalf("unknown source treated as public: %+v %v", got, err)
	}
}

func TestWorkspacePrivacyQAReadOnlyAndFrozenInputs(t *testing.T) {
	db, w, id := workspaceAuthorityFixture(t)
	task, _ := parseButlerTaskID(id)
	workspacePublish(t, db, workspaceIntent(t, db, w, task, "source", "r1", "", 1), "receipt")
	next, _ := workspacePrivacyTask(t, db)
	ctx, _, a := workspacePrivacyTurn(t, db, next, ModelLocal, MemoryDestination{BuiltIn: true})
	if _, err := db.ReadWorkspaceRevision(ctx, a, w.ID, "source", "r1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "copy", WorkspaceID: w.ID, Path: "copy.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateWorkspaceRevisionForTask(ctx, a, w.ID, "", workspaceNewRevision("copy-r1", "copy")); err != nil {
		t.Fatal(err)
	}
	// Source snapshots pin the earlier revision independently of mutable task
	// bookkeeping. A future cleanup cannot erase a derivative's restriction.
	if _, err := db.sql.Exec(`DELETE FROM workspace_task_sources`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DELETE FROM workspace_session_sources`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO content_privacy(object,local_only) VALUES('wa:source',1)`); err != nil {
		t.Fatal(err)
	}
	policy, err := db.contentPrivacy("wr:copy-r1")
	if err != nil || !policy.LocalOnly {
		t.Fatalf("snapshot lost input: %+v %v", policy, err)
	}
	if _, err := db.sql.Exec(`UPDATE models SET readiness='qa-only'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReadWorkspaceRevision(ctx, a, w.ID, "source", "r1"); err != nil {
		t.Fatalf("QA read refused: %v", err)
	}
	if err := db.CreateWorkspaceRevisionForTask(ctx, a, w.ID, "", workspaceNewRevision("qa-write", "copy")); !errors.Is(err, ErrWorkspaceDenied) {
		t.Fatalf("QA mutation admitted: %v", err)
	}
}

func TestWorkspacePrivacyBaseEditTracksTaskAndSession(t *testing.T) {
	db, w, origin := workspaceAuthorityFixture(t)
	sourceTask, _ := parseButlerTaskID(origin)
	workspacePublish(t, db, workspaceIntent(t, db, w, sourceTask, "base", "base-r1", "", 1), "base-receipt")
	if _, err := db.sql.Exec(`INSERT INTO content_privacy(object,local_only,trusted_only) VALUES('wa:base',1,1)`); err != nil {
		t.Fatal(err)
	}
	task, session := workspacePrivacyTask(t, db)
	ctx, _, access := workspacePrivacyTurn(t, db, task, ModelLocal, MemoryDestination{BuiltIn: true})
	// An explicit-base edit has no preceding read to record its input. A failed
	// revision insert must roll back both the task source and its session trigger.
	if _, err := db.sql.Exec(`CREATE TRIGGER reject_base_edit BEFORE INSERT ON workspace_revisions BEGIN SELECT RAISE(ABORT,'injected edit failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateWorkspaceRevisionForTask(ctx, access, w.ID, "base-r1", workspaceNewRevision("base-r2", "base")); err == nil {
		t.Fatal("injected failure ignored")
	}
	for _, table := range []string{"workspace_task_sources", "workspace_session_sources"} {
		var n int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("failed edit left %s: %d %v", table, n, err)
		}
	}
	if _, err := db.sql.Exec(`DROP TRIGGER reject_base_edit`); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateWorkspaceRevisionForTask(ctx, access, w.ID, "base-r1", workspaceNewRevision("base-r2", "base")); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct {
		model       string
		destination MemoryDestination
	}{{ModelCloud, MemoryDestination{BuiltIn: true}}, {ModelLocal, MemoryDestination{}}} {
		if err := db.CheckTaskPrivacy(task, route.model, route.destination); !errors.Is(err, ErrPrivacy) {
			t.Fatalf("explicit-base edit lost task policy: %v", err)
		}
		if err := db.CheckSessionPrivacy(session, route.model, route.destination); !errors.Is(err, ErrPrivacy) {
			t.Fatalf("explicit-base edit lost session policy: %v", err)
		}
		_, resumed, _ := workspacePrivacyTurn(t, db, task, route.model, route.destination)
		if err := resumed.Check(); !errors.Is(err, ErrPrivacy) {
			t.Fatalf("resumed edit lost base policy: %v", err)
		}
	}
	if err := CheckPrivacyDelivery(ctx, MemoryDestination{}); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("edit delivery lost base policy: %v", err)
	}
}

func TestWorkspacePrivacyPreservesSourceTaskCeiling(t *testing.T) {
	db, w, origin := workspaceAuthorityFixture(t)
	sourceTask, _ := parseButlerTaskID(origin)
	workspacePublish(t, db, workspaceIntent(t, db, w, sourceTask, "owner-source", "owner-r1", "", 1), "owner-receipt")
	history, session := workspacePrivacyTask(t, db)
	historyCtx, historyGuard, historyAccess := workspacePrivacyTurn(t, db, history, ModelLocal, MemoryDestination{BuiltIn: true})
	if _, err := db.ReadWorkspaceRevision(historyCtx, historyAccess, w.ID, "owner-source", "owner-r1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetAgentClass("alfred", AgentClassMember, "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantButlerAccess("alfred", "mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	for name, check := range map[string]func() error{
		"task":     func() error { return db.CheckTaskPrivacy(history, ModelLocal, MemoryDestination{BuiltIn: true}) },
		"session":  func() error { return db.CheckSessionPrivacy(session, ModelLocal, MemoryDestination{BuiltIn: true}) },
		"delivery": func() error { return CheckPrivacyDelivery(historyCtx, MemoryDestination{BuiltIn: true}) },
		"resume":   historyGuard.Check,
	} {
		if err := check(); !errors.Is(err, ErrPrivacy) {
			t.Fatalf("%s replayed Owner source: %v", name, err)
		}
	}
	task, err := db.InsertButlerTask(ButlerTask{Kind: ButlerTaskConversation, Author: "mika", Owner: "mika", AgentClass: AgentClassMember, Surface: ButlerSurfaceFamily, Audience: ButlerAudiencePrivate, OriginConnection: "household_portal", Objective: "read own member files"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetTaskButler(task, "alfred"); err != nil {
		t.Fatal(err)
	}
	if err = db.CheckWorkspaceTask("mika", "alfred", task, "household_portal", w.ID); err != nil {
		t.Fatal(err)
	}
	ctx, guard, err := db.BeginPrivacyTurn(context.Background(), ModelLocal, MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = guard.SetTask(task); err != nil {
		t.Fatal(err)
	}
	access := db.BindWorkspaceAccess("mika", "alfred", task, "household_portal")
	currentTask, _ := parseButlerTaskID(task)
	derived := workspaceIntent(t, db, w, currentTask, "member-derived", "member-derived-r1", "", 1)
	if err = db.RecordWorkspaceProvenance(WorkspaceProvenance{ID: "ceiling-edge", RevisionID: derived.RevisionID, SourceRevision: "owner-r1", Privacy: "normal", Audience: "private"}); err != nil {
		t.Fatal(err)
	}
	workspacePublish(t, db, derived, "derived-receipt")
	for _, target := range []struct{ artifact, revision string }{{"owner-source", "owner-r1"}, {"member-derived", "member-derived-r1"}} {
		if got, err := db.ReadWorkspaceRevision(ctx, access, w.ID, target.artifact, target.revision); !errors.Is(err, ErrWorkspaceDenied) || got.ID != "" {
			t.Fatalf("source ceiling disclosed metadata: %+v %v", got, err)
		}
		if err := db.CreateWorkspaceRevisionForTask(ctx, access, w.ID, target.revision, workspaceNewRevision(target.revision+"-edit", target.artifact)); !errors.Is(err, ErrWorkspaceDenied) {
			t.Fatalf("source ceiling allowed edit: %v", err)
		}
	}
	var n int
	if err = db.sql.QueryRow(`SELECT COUNT(*) FROM workspace_task_sources WHERE task=?`, currentTask).Scan(&n); err != nil || n != 0 {
		t.Fatalf("denied ceiling left inputs: %d %v", n, err)
	}
	if err = db.sql.QueryRow(`SELECT COUNT(*) FROM workspace_revisions WHERE id LIKE '%-edit'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("denied ceiling left revision: %d %v", n, err)
	}
	// The deliberately forged derivative above now taints its writer too.
	// Prove the same-ceiling positive case on fresh work with no Owner input.
	task, err = db.InsertButlerTask(ButlerTask{Kind: ButlerTaskConversation, Author: "mika", Owner: "mika", AgentClass: AgentClassMember, Surface: ButlerSurfaceFamily, Audience: ButlerAudiencePrivate, OriginConnection: "household_portal", Objective: "read fresh member source"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetTaskButler(task, "alfred"); err != nil {
		t.Fatal(err)
	}
	currentTask, _ = parseButlerTaskID(task)
	ctx, guard, err = db.BeginPrivacyTurn(context.Background(), ModelLocal, MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = guard.SetTask(task); err != nil {
		t.Fatal(err)
	}
	access = db.BindWorkspaceAccess("mika", "alfred", task, "household_portal")
	workspacePublish(t, db, workspaceIntent(t, db, w, currentTask, "member-source", "member-r1", "", 1), "member-receipt")
	if _, err = db.ReadWorkspaceRevision(ctx, access, w.ID, "member-source", "member-r1"); err != nil {
		t.Fatalf("same-ceiling source refused: %v", err)
	}
}

func TestWorkspacePrivacyAcceptsLegacyPersonAudience(t *testing.T) {
	db, w, origin := workspaceAuthorityFixture(t)
	seq, _ := parseButlerTaskID(origin)
	workspacePublish(t, db, workspaceIntent(t, db, w, seq, "legacy-source", "legacy-source-r1", "", 1), "legacy-source-receipt")
	other, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, audience string
		allowed        bool
	}{{"own", w.PersonID, true}, {"foreign", other.PersonID, false}} {
		t.Run(tc.name, func(t *testing.T) {
			artifact, revision := "legacy-"+tc.name, "legacy-"+tc.name+"-r1"
			publication := workspaceIntent(t, db, w, seq, artifact, revision, "", 1)
			// Existing 0097 callers store the stable Person ID, not a private alias.
			if err := db.RecordWorkspaceProvenance(WorkspaceProvenance{ID: tc.name + "-edge", RevisionID: revision, SourceRevision: "legacy-source-r1", Privacy: "normal", Audience: tc.audience}); err != nil {
				t.Fatal(err)
			}
			workspacePublish(t, db, publication, tc.name+"-receipt")
			task, _ := workspacePrivacyTask(t, db)
			ctx, _, access := workspacePrivacyTurn(t, db, task, ModelLocal, MemoryDestination{BuiltIn: true})
			got, err := db.ReadWorkspaceRevision(ctx, access, w.ID, artifact, revision)
			if tc.allowed {
				if err != nil || got.ID != revision {
					t.Fatalf("valid legacy audience refused: %+v %v", got, err)
				}
				if err = db.CreateWorkspaceRevisionForTask(ctx, access, w.ID, revision, workspaceNewRevision(revision+"-edit", artifact)); err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrWorkspaceDenied) || got.ID != "" {
				t.Fatalf("foreign audience admitted: %+v %v", got, err)
			}
		})
	}
}

func TestWorkspacePrivacyMemoryDisclosureAuthority(t *testing.T) {
	for _, tc := range []struct{ name, mutation string }{
		{"compatible", ""},
		{"builtin source", ""},
		{"builtin verified", ""},
		{"verified chat", "UPDATE connection_disclosure SET retention='none'"},
		{"missing classification", "DELETE FROM connection_disclosure"},
		{"reach", "UPDATE connection_disclosure SET reach='local'"},
		{"encryption", "UPDATE connection_disclosure SET encryption='end_to_end'"},
		{"retention", "UPDATE connection_disclosure SET retention='none'"},
		{"surface", "UPDATE connection_disclosure SET surface='trusted_ui'"},
		{"unknown", "UPDATE connection_disclosure SET retention='unknown'"},
		{"revoked origin", "UPDATE butler_addresses SET revoked_at='revoked'"},
		{"forgotten", "UPDATE butler_memory SET forgotten_at='forgotten'"},
		{"audience", "INSERT INTO memory_constraints(memory,audience,owner_person_id) SELECT m.seq,'aud_owner',p.person_id FROM butler_memory m CROSS JOIN butler_members p WHERE p.member='sam'"},
		{"scope", "INSERT INTO memory_agent_restrictions(agent,scope) SELECT seq,'personal' FROM butlers WHERE name='alfred'"},
		{"agent access", "UPDATE butler_access SET revoked_at='revoked'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, w, id := workspaceAuthorityFixture(t)
			at := connect(t, db, "alfred", "telegram", ButlerSurfaceAdmin)
			if _, err := db.sql.Exec(`UPDATE connection_disclosure SET retention='provider' WHERE connection=?`, at.Seq); err != nil {
				t.Fatal(err)
			}
			origin := newTask(t, db, func(task *ButlerTask) {
				task.AgentClass = AgentClassOwner
				task.OriginConnection = at.Name()
				task.OriginConnectionRef = at.Seq
				if strings.HasPrefix(tc.name, "builtin") {
					task.OriginConnection = "console"
					task.OriginConnectionRef = 0
				}
			})
			memory, err := db.RecordMemory(MemoryRecord{Scope: MemoryScopePersonal, Kind: MemoryFact, Content: "synthetic disclosure input", Owner: "mika", Author: "mika", Butler: "alfred", Audience: ButlerAudiencePrivate, PermissionLabel: "household", SourceKind: "conversation", SourceID: origin})
			if err != nil {
				t.Fatal(err)
			}
			ctx, _, access := workspacePrivacyTurn(t, db, id, ModelLocal, MemoryDestination{BuiltIn: true})
			writerCtx := ctx
			if err = TrackMemoryPrivacy(ctx, memory, nil); err != nil {
				t.Fatal(err)
			}
			if err = db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "output", WorkspaceID: w.ID, Path: "output.txt"}); err != nil {
				t.Fatal(err)
			}
			if err = db.CreateWorkspaceRevisionForTask(ctx, access, w.ID, "", workspaceNewRevision("output-r1", "output")); err != nil {
				t.Fatal(err)
			}
			seq, _ := parseButlerTaskID(id)
			workspaceFinishRevision(t, db, w, seq, "output", "output-r1", "")
			// Publish a second generation through an explicit base without a read.
			derived, _ := workspacePrivacyTask(t, db)
			derivedCtx, _, derivedAccess := workspacePrivacyTurn(t, db, derived, ModelLocal, MemoryDestination{BuiltIn: true})
			if err = db.CreateWorkspaceRevisionForTask(derivedCtx, derivedAccess, w.ID, "output-r1", workspaceNewRevision("output-derived", "output")); err != nil {
				t.Fatal(err)
			}
			derivedSeq, _ := parseButlerTaskID(derived)
			workspaceFinishRevision(t, db, w, derivedSeq, "output", "output-derived", "output-r1")
			// Capture a successful builtin read before tightening. All resumed
			// and delivery paths must recheck live source authority themselves.
			history, historySession := workspacePrivacyTask(t, db)
			historyCtx, _, historyAccess := workspacePrivacyTurn(t, db, history, ModelLocal, MemoryDestination{BuiltIn: true})
			if _, err = db.ReadWorkspaceRevision(historyCtx, historyAccess, w.ID, "output", "output-derived"); err != nil {
				t.Fatal(err)
			}
			if tc.mutation != "" {
				if _, err = db.sql.Exec(tc.mutation); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "verified chat" || tc.name == "builtin verified" {
				bind(t, db, at, "mika", "synthetic-account")
			}
			d := MemoryDestination{ConnectionRef: at.Seq}
			hits, err := db.RetrieveMemory(MemoryQuery{Reader: "mika", Butler: "alfred", Kind: MemoryFact, ReadContext: db.BindMemoryRead("mika", "alfred", d)})
			if err != nil {
				t.Fatal(err)
			}
			want := (tc.mutation == "" && tc.name != "builtin source") || tc.name == "verified chat" || tc.name == "builtin verified"
			if (len(hits) > 0) != want {
				t.Fatalf("canonical recall got %d, allowed=%v", len(hits), want)
			}
			_, resumed, _ := workspacePrivacyTurn(t, db, history, ModelLocal, d)
			for name, check := range map[string]func() error{
				"writer task":     func() error { return db.CheckTaskPrivacy(id, ModelLocal, d) },
				"writer delivery": func() error { return CheckPrivacyDelivery(writerCtx, d) },
				"task":            func() error { return db.CheckTaskPrivacy(history, ModelLocal, d) },
				"session":         func() error { return db.CheckSessionPrivacy(historySession, ModelLocal, d) },
				"delivery":        func() error { return CheckPrivacyDelivery(historyCtx, d) },
				"resume":          resumed.Check,
			} {
				if err := check(); (err == nil) != want {
					t.Fatalf("%s authority allowed=%v: %v", name, want, err)
				}
			}
			next, _ := workspacePrivacyTask(t, db)
			ctx, _, access = workspacePrivacyTurn(t, db, next, ModelLocal, d)
			for _, revision := range []string{"output-r1", "output-derived"} {
				got, err := db.ReadWorkspaceRevision(ctx, access, w.ID, "output", revision)
				if want {
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, ErrWorkspaceDenied) || got.ID != "" {
					t.Fatalf("derivative disclosed metadata: %+v %v", got, err)
				}
			}
			err = db.CreateWorkspaceRevisionForTask(ctx, access, w.ID, "output-derived", workspaceNewRevision("output-r2", "output"))
			if want {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, ErrWorkspaceDenied) {
					t.Fatalf("edit authority: %v", err)
				}
				var n int
				nextSeq, _ := parseButlerTaskID(next)
				if err = db.sql.QueryRow(`SELECT (SELECT count(*) FROM workspace_task_sources WHERE task=?)+(SELECT count(*) FROM workspace_revisions WHERE id='output-r2')`, nextSeq).Scan(&n); err != nil || n != 0 {
					t.Fatalf("denial persisted effects: %d %v", n, err)
				}
			}
		})
	}
}
