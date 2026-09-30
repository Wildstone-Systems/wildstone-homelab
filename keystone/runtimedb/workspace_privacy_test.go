package runtimedb

import (
	"context"
	"errors"
	"path/filepath"
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

func TestWorkspacePrivacyMigrationAndRunnerGaps(t *testing.T) {
	db, err := openUnmigrated(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.migrateThrough("migrations/0097_agent_workspaces.sql"); err != nil {
		t.Fatal(err)
	}
	if err = db.migrate(); err != nil {
		t.Fatal(err)
	}
	if err = db.migrate(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.sql.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name='migrations/0099_workspace_privacy.sql'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("migration count: %d %v", n, err)
	}
	// Synthetic runner fixtures prove the actual ordering behavior. These are
	// not a placeholder for #490's real 0098, whose ordered chain must be tested.
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
