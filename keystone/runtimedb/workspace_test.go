package runtimedb

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWorkspaceMigrationPreservesExistingToolAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	if err := db.migrateThrough("migrations/0096_backup_verification.sql"); err != nil {
		t.Fatal(err)
	}
	record(t, db, "mika")
	// Existing authority is deliberately populated before the workspace migration.
	if err := db.UpsertModel(Model{Name: "local", Kind: ModelLocal, Provider: "ollama", ProviderModel: "llama", Readiness: ModelReady, Enabled: true}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateButler(Butler{Name: "existing", Directive: "help", Surface: ButlerSurfaceFamily, AgentClass: AgentClassMember, Model: "local"}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO butler_tool_governance(butler,since,enrolled_by) SELECT seq,'2026-09-30T00:00:00Z','mika' FROM butlers WHERE name='existing'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO butler_tool_assignments(butler,tool,assigned_at,assigned_by) SELECT seq,'node_status','2026-09-30T00:00:00Z','mika' FROM butlers WHERE name='existing'`); err != nil {
		t.Fatal(err)
	}
	snapshot := func() []string {
		t.Helper()
		rows, err := db.sql.Query(`SELECT printf('%d|%s|%s|%s|%s|%s',butler,tool,assigned_at,assigned_by,revoked_at,revoked_by) FROM butler_tool_assignments ORDER BY seq`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()
	if err := db.migrate(); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("migration changed assignments: before=%v after=%v", before, after)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("reopen changed assignments: before=%v after=%v", before, after)
	}
}

func workspaceFixture(t *testing.T) (*DB, Workspace, int64) {
	t.Helper()
	db := agentFixture(t)
	member, err := db.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	w := Workspace{ID: "ws-a", PersonID: member.PersonID, AgentID: agent.Seq}
	if err := db.CreateWorkspace(w); err != nil {
		t.Fatal(err)
	}
	session, err := db.InsertButlerSession(ButlerSession{Principal: "mika", ActingFor: "mika", Butler: "alfred", Surface: ButlerSurfaceAdmin, Mode: ButlerModeFull})
	if err != nil {
		t.Fatal(err)
	}
	id := newTask(t, db, func(task *ButlerTask) { task.SessionID = session })
	if err := db.SetTaskButler(id, "alfred"); err != nil {
		t.Fatal(err)
	}
	task, err := parseButlerTaskID(id)
	if err != nil {
		t.Fatal(err)
	}
	return db, w, task
}

func workspaceIntent(t *testing.T, db *DB, w Workspace, task int64, artifact, revision, base string, size int64) WorkspacePublication {
	t.Helper()
	if base == "" {
		if err := db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: artifact, WorkspaceID: w.ID, Path: artifact + ".txt"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateWorkspaceRevision(WorkspaceRevision{ID: revision, ArtifactID: artifact, TaskID: task, Hash: strings.Repeat("a", 64), SizeBytes: size, MediaType: "text/plain", Privacy: "normal"}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveWorkspaceBytes(WorkspaceReservation{ID: "reserve-" + revision, WorkspaceID: w.ID, TaskID: task, SizeBytes: size, Purpose: "publication"}); err != nil {
		t.Fatal(err)
	}
	p := WorkspacePublication{ID: "publish-" + revision, WorkspaceID: w.ID, ArtifactID: artifact, RevisionID: revision, BaseRevision: base, TaskID: task, ReservationID: "reserve-" + revision, EffectKey: "effect-" + revision}
	if err := db.CreateWorkspacePublication(p); err != nil {
		t.Fatal(err)
	}
	return p
}
func workspacePublish(t *testing.T, db *DB, p WorkspacePublication, receipt string) {
	t.Helper()
	if err := db.PrepareWorkspacePublication(p.WorkspaceID, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkWorkspaceBytesPublished(p.WorkspaceID, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitWorkspacePublication(p.WorkspaceID, p.ID, receipt); err != nil {
		t.Fatal(err)
	}
}
func TestWorkspacePublicationVisibilityReceiptAndReopen(t *testing.T) {
	db, w, task := workspaceFixture(t)
	p := workspaceIntent(t, db, w, task, "report", "r1", "", 12)
	for _, advance := range []func() error{func() error { return nil }, func() error { return db.PrepareWorkspacePublication(w.ID, p.ID) }, func() error { return db.MarkWorkspaceBytesPublished(w.ID, p.ID) }} {
		if err := advance(); err != nil {
			t.Fatal(err)
		}
		if _, err := db.WorkspaceArtifact(w.ID, "report"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("pending artifact visible: %v", err)
		}
		if _, err := db.WorkspaceRevision(w.ID, "r1"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("pending revision visible: %v", err)
		}
	}
	if err := db.CommitWorkspacePublication(w.ID, p.ID, "receipt-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitWorkspacePublication(w.ID, p.ID, "receipt-1"); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	if err := db.CommitWorkspacePublication(w.ID, p.ID, "different-receipt"); err == nil {
		t.Fatal("receipt identity rewritten")
	}
	if _, err := db.WorkspaceArtifact("another-workspace", "report"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("wrong scope disclosed artifact: %v", err)
	}
	if err := db.RenameButlerMemberHandle("mika", "renamed"); err != nil {
		t.Fatal(err)
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
	got, err := reopened.Workspace(w.PersonID, w.AgentID)
	if err != nil || got.ID != w.ID {
		t.Fatalf("stable ownership lost: %+v %v", got, err)
	}
	artifact, err := reopened.WorkspaceArtifact(w.ID, "report")
	if err != nil || artifact.CurrentRevision != "r1" {
		t.Fatalf("reopen: %+v %v", artifact, err)
	}
	if err := reopened.CommitWorkspacePublication(w.ID, p.ID, "receipt-1"); err != nil {
		t.Fatalf("durable retry: %v", err)
	}
}
func TestWorkspaceCommitReceiptConflictRollsBackAndPreservesHistory(t *testing.T) {
	db, w, task := workspaceFixture(t)
	first := workspaceIntent(t, db, w, task, "report", "r1", "", 12)
	workspacePublish(t, db, first, "receipt-1")
	edit := workspaceIntent(t, db, w, task, "report", "r2", "r1", 15)
	if err := db.PrepareWorkspacePublication(w.ID, edit.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkWorkspaceBytesPublished(w.ID, edit.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitWorkspacePublication(w.ID, edit.ID, "receipt-1"); err == nil {
		t.Fatal("duplicate receipt accepted")
	}
	a, err := db.WorkspaceArtifact(w.ID, "report")
	if err != nil || a.CurrentRevision != "r1" {
		t.Fatalf("failed commit changed current: %+v %v", a, err)
	}
	p, err := db.WorkspacePublication(w.ID, edit.ID)
	if err != nil || p.State != "bytes_published" {
		t.Fatalf("journal not rolled back: %+v %v", p, err)
	}
	var state string
	if err := db.sql.QueryRow(`SELECT state FROM workspace_reservations WHERE id=?`, edit.ReservationID).Scan(&state); err != nil || state != "held" {
		t.Fatalf("reservation lost: %q %v", state, err)
	}
	if err := db.CommitWorkspacePublication(w.ID, edit.ID, "receipt-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.WorkspaceRevision(w.ID, "r1"); err != nil {
		t.Fatalf("old revision lost: %v", err)
	}
	if err := db.TrashWorkspaceArtifact(w.ID, "report", "r1"); err == nil {
		t.Fatal("stale trash accepted")
	}
	if err := db.TrashWorkspaceArtifact(w.ID, "report", "r2"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.WorkspaceArtifact(w.ID, "report"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("trash visible: %v", err)
	}
	if _, err := db.WorkspaceRevision(w.ID, "r2"); err != nil {
		t.Fatalf("trash history lost: %v", err)
	}
	if err := db.RestoreWorkspaceArtifact(w.ID, "report", "r2", "restored.txt"); err != nil {
		t.Fatal(err)
	}
}
func TestWorkspaceOwnershipAndForeignKeysRejectOrphans(t *testing.T) {
	db, w, task := workspaceFixture(t)
	p := workspaceIntent(t, db, w, task, "report", "r1", "", 1)
	for name, query := range map[string]string{
		"workspace person":   `UPDATE agent_workspaces SET person_id='missing' WHERE id='ws-a'`,
		"artifact owner":     `UPDATE workspace_artifacts SET workspace_id='other' WHERE id='report'`,
		"revision immutable": `UPDATE workspace_revisions SET hash='` + strings.Repeat("b", 64) + `' WHERE id='r1'`,
		"revision orphan":    `INSERT INTO workspace_revisions(id,artifact_id,task_id,hash,size_bytes,media_type,privacy,created_at) SELECT 'orphan','missing',task_id,hash,size_bytes,media_type,privacy,created_at FROM workspace_revisions WHERE id='r1'`,
		"current pending":    `UPDATE workspace_artifacts SET state='committed',current_revision='r1' WHERE id='report'`,
		"early receipt":      `INSERT INTO workspace_receipts VALUES('early','publish-r1','now')`,
		"skip journal":       `UPDATE workspace_publications SET state='committed' WHERE id='publish-r1'`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.sql.Exec(query); err == nil {
				t.Fatal("invalid SQL mutation accepted")
			} else if !strings.Contains(err.Error(), "constraint failed") {
				t.Fatalf("expected enforced invariant, got unrelated error: %v", err)
			}
		})
	}
	other := newTask(t, db, func(task *ButlerTask) { task.Owner = "sam"; task.Author = "sam" })
	otherTask, err := parseButlerTaskID(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveWorkspaceBytes(WorkspaceReservation{ID: "wrong-owner", WorkspaceID: w.ID, TaskID: otherTask, SizeBytes: 1, Purpose: "scratch"}); err == nil {
		t.Fatal("another Person's task reserved workspace")
	}
	if err := db.ReleaseWorkspaceReservation(w.ID, p.ReservationID); err == nil {
		t.Fatal("unresolved publication reservation released")
	}
	if err := db.AbortWorkspacePublication(w.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.WorkspaceRevision(w.ID, "r1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("aborted revision visible: %v", err)
	}
}
func TestWorkspaceQuotaConcurrentReservationsAndRetainedTrash(t *testing.T) {
	db, w, task := workspaceFixture(t)
	p := workspaceIntent(t, db, w, task, "report", "r1", "", 12)
	workspacePublish(t, db, p, "receipt")
	if err := db.TrashWorkspaceArtifact(w.ID, "report", "r1"); err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveWorkspaceBytes(WorkspaceReservation{ID: "full", WorkspaceID: w.ID, TaskID: task, SizeBytes: 1 << 30, Purpose: "scratch"}); err == nil {
		t.Fatal("trash excluded from quota")
	}
	const reserve = (1 << 30) - 12
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, id := range []string{"race-a", "race-b"} {
		go func(id string) {
			<-start
			results <- db.ReserveWorkspaceBytes(WorkspaceReservation{ID: id, WorkspaceID: w.ID, TaskID: task, SizeBytes: reserve, Purpose: "scratch"})
		}(id)
	}
	close(start)
	successes := 0
	for range 2 {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent full reservations successes=%d want 1", successes)
	}
}

func TestWorkspaceProvenanceSealsPrivacyAndBaseCannotWiden(t *testing.T) {
	db, w, task := workspaceFixture(t)
	if err := db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "protected", WorkspaceID: w.ID, Path: "secret.txt"}); err != nil {
		t.Fatal(err)
	}
	r := WorkspaceRevision{ID: "protected-r1", ArtifactID: "protected", TaskID: task, Hash: strings.Repeat("a", 64), SizeBytes: 3, MediaType: "text/plain", Privacy: "local-only", TrustedOnly: true}
	if err := db.CreateWorkspaceRevision(r); err != nil {
		t.Fatal(err)
	}
	edge := WorkspaceProvenance{ID: "edge", RevisionID: r.ID, SourceID: "task-source", SourceVersion: "v1", Privacy: "local-only", TrustedOnly: true, Audience: w.PersonID}
	if err := db.RecordWorkspaceProvenance(edge); err != nil {
		t.Fatal(err)
	}
	q := WorkspaceReservation{ID: "protected-q", WorkspaceID: w.ID, TaskID: task, SizeBytes: 3, Purpose: "publication"}
	if err := db.ReserveWorkspaceBytes(q); err != nil {
		t.Fatal(err)
	}
	p := WorkspacePublication{ID: "protected-p", WorkspaceID: w.ID, ArtifactID: r.ArtifactID, RevisionID: r.ID, TaskID: task, ReservationID: q.ID, EffectKey: "protected-effect"}
	if err := db.CreateWorkspacePublication(p); err != nil {
		t.Fatal(err)
	}
	if err := db.PrepareWorkspacePublication(w.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	edge.ID = "late-edge"
	if err := db.RecordWorkspaceProvenance(edge); err == nil {
		t.Fatal("prepared revision accepted new provenance")
	}
	if _, err := db.sql.Exec(`DELETE FROM workspace_provenance WHERE id='edge'`); err == nil {
		t.Fatal("sealed provenance deleted")
	}
	if err := db.MarkWorkspaceBytesPublished(w.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitWorkspacePublication(w.ID, p.ID, "protected-receipt"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, privacy string
		trusted     bool
	}{{"drop-local", "normal", true}, {"drop-trusted", "local-only", false}, {"drop-both", "normal", false}} {
		t.Run(tc.id, func(t *testing.T) {
			r.ID = tc.id
			r.Privacy = tc.privacy
			r.TrustedOnly = tc.trusted
			if err := db.CreateWorkspaceRevision(r); err != nil {
				t.Fatal(err)
			}
			q.ID = "q-" + tc.id
			if err := db.ReserveWorkspaceBytes(q); err != nil {
				t.Fatal(err)
			}
			p.ID = "p-" + tc.id
			p.RevisionID = r.ID
			p.BaseRevision = "protected-r1"
			p.ReservationID = q.ID
			p.EffectKey = tc.id
			if err := db.CreateWorkspacePublication(p); err == nil {
				t.Fatal("edit widened base privacy without provenance edge")
			}
			edge.ID = "edge-" + tc.id
			edge.RevisionID = r.ID
			edge.SourceRevision = "protected-r1"
			edge.SourceID = ""
			edge.SourceVersion = ""
			edge.Privacy = "normal"
			edge.TrustedOnly = false
			if err := db.RecordWorkspaceProvenance(edge); err == nil {
				t.Fatal("derived edge widened protected source")
			}
		})
	}
	got, err := db.WorkspaceRevision(w.ID, "protected-r1")
	if err != nil || got.Privacy != "local-only" || !got.TrustedOnly {
		t.Fatalf("privacy did not roundtrip: %+v %v", got, err)
	}
}
func TestWorkspaceTaskReassignmentRefusesPublicationAndAllowsRecovery(t *testing.T) {
	db, w, task := workspaceFixture(t)
	p := workspaceIntent(t, db, w, task, "report", "r1", "", 1)
	if err := db.PrepareWorkspacePublication(w.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkWorkspaceBytesPublished(w.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTaskButler(butlerTaskID(task), "hobson"); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitWorkspacePublication(w.ID, p.ID, "receipt"); err == nil {
		t.Fatal("reassigned task committed old Agent workspace")
	}
	if err := db.AbortWorkspacePublication(w.ID, p.ID); err != nil {
		t.Fatalf("recovery after reassignment: %v", err)
	}
	var state string
	if err := db.sql.QueryRow(`SELECT state FROM workspace_reservations WHERE id=?`, p.ReservationID).Scan(&state); err != nil || state != "released" {
		t.Fatalf("abort reservation=%q %v", state, err)
	}
}
func TestWorkspaceConcurrentEditsHaveOneIntentAndStaleBaseRefused(t *testing.T) {
	db, w, task := workspaceFixture(t)
	p := workspaceIntent(t, db, w, task, "report", "r1", "", 1)
	workspacePublish(t, db, p, "receipt-1")
	for _, id := range []string{"r2", "r3"} {
		if err := db.CreateWorkspaceRevision(WorkspaceRevision{ID: id, ArtifactID: "report", TaskID: task, Hash: strings.Repeat("a", 64), SizeBytes: 1, MediaType: "text/plain", Privacy: "normal"}); err != nil {
			t.Fatal(err)
		}
		if err := db.ReserveWorkspaceBytes(WorkspaceReservation{ID: "q-" + id, WorkspaceID: w.ID, TaskID: task, SizeBytes: 1, Purpose: "publication"}); err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		p   WorkspacePublication
		err error
	}
	out := make(chan result, 2)
	start := make(chan struct{})
	for _, id := range []string{"r2", "r3"} {
		go func(id string) {
			<-start
			p := WorkspacePublication{ID: "p-" + id, WorkspaceID: w.ID, ArtifactID: "report", RevisionID: id, BaseRevision: "r1", TaskID: task, ReservationID: "q-" + id, EffectKey: id}
			out <- result{p, db.CreateWorkspacePublication(p)}
		}(id)
	}
	close(start)
	var winner, loser WorkspacePublication
	success := 0
	for range 2 {
		r := <-out
		if r.err == nil {
			success++
			winner = r.p
		} else {
			loser = r.p
		}
	}
	if success != 1 {
		t.Fatalf("concurrent edits admitted %d intents", success)
	}
	workspacePublish(t, db, winner, "winner-receipt")
	if err := db.CreateWorkspacePublication(loser); err == nil {
		t.Fatal("stale base accepted after competing commit")
	}
	if _, err := db.WorkspaceRevision(w.ID, "r1"); err != nil {
		t.Fatalf("base history lost: %v", err)
	}
}

func TestWorkspaceQuotaSpansPeopleAndVisibility(t *testing.T) {
	db, w, task := workspaceFixture(t)
	shared := w
	shared.ID = "shared"
	shared.Visibility = "shared"
	if err := db.CreateWorkspace(shared); err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveWorkspaceBytes(WorkspaceReservation{ID: "private-charge", WorkspaceID: w.ID, TaskID: task, SizeBytes: 1 << 29, Purpose: "scratch"}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveWorkspaceBytes(WorkspaceReservation{ID: "shared-charge", WorkspaceID: shared.ID, TaskID: task, SizeBytes: 1 << 29, Purpose: "scratch"}); err != nil {
		t.Fatal(err)
	}
	sam, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	other := Workspace{ID: "sam-workspace", PersonID: sam.PersonID, AgentID: w.AgentID}
	if err := db.CreateWorkspace(other); err != nil {
		t.Fatal(err)
	}
	otherTaskID := newTask(t, db, func(task *ButlerTask) { task.Owner = "sam"; task.Author = "sam" })
	if err := db.SetTaskButler(otherTaskID, "alfred"); err != nil {
		t.Fatal(err)
	}
	otherTask, err := parseButlerTaskID(otherTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveWorkspaceBytes(WorkspaceReservation{ID: "overquota", WorkspaceID: other.ID, TaskID: otherTask, SizeBytes: 1, Purpose: "scratch"}); err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("Agent quota not enforced across people: %v", err)
	}
	if err := db.ReleaseWorkspaceReservation(w.ID, "private-charge"); err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveWorkspaceBytes(WorkspaceReservation{ID: "after-release", WorkspaceID: other.ID, TaskID: otherTask, SizeBytes: 1, Purpose: "scratch"}); err != nil {
		t.Fatal(err)
	}
	got, err := db.Workspace(w.PersonID, w.AgentID)
	if err != nil || got.Visibility != "private" {
		t.Fatalf("private workspace lookup: %+v %v", got, err)
	}
}

func TestWorkspaceRetainedForeignKeysAndCrossArtifactRevision(t *testing.T) {
	db, w, task := workspaceFixture(t)
	a := workspaceIntent(t, db, w, task, "a", "a1", "", 1)
	workspacePublish(t, db, a, "receipt-a")
	b := workspaceIntent(t, db, w, task, "b", "b1", "", 1)
	workspacePublish(t, db, b, "receipt-b")
	for _, query := range []string{`DELETE FROM workspace_revisions WHERE id='a1'`, `DELETE FROM workspace_artifacts WHERE id='a'`, `DELETE FROM agent_workspaces WHERE id='ws-a'`} {
		if _, err := db.sql.Exec(query); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			t.Fatalf("retained reference deletion: query=%s error=%v", query, err)
		}
	}
	if _, err := db.sql.Exec(`UPDATE workspace_artifacts SET current_revision='b1' WHERE id='a'`); err == nil || !strings.Contains(err.Error(), "constraint failed") {
		t.Fatalf("cross-artifact current revision accepted or wrong failure: %v", err)
	}
}
