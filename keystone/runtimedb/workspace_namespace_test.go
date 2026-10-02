package runtimedb

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWorkspaceNamespaceAbortRechecksFenceAtTransactionBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*DB) error
		want   error
	}{
		{"promotion", func(db *DB) error { _, err := db.PromoteCoordinator("replacement"); return err }, ErrCoordinatorFenced},
		{"stop", func(db *DB) error {
			_, err := db.sql.Exec(`UPDATE household_execution_stop SET active=1 WHERE singleton=1`)
			return err
		}, ErrHouseholdStopped},
		{"quarantine", func(db *DB) error { return db.EnterRestoreQuarantine("test restore", time.Now()) }, ErrQuarantined},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, w, task := namespaceFixture(t)
			n, err := db.PrepareWorkspaceNamespace(WorkspaceNamespaceRequest{WorkspaceID: w.ID, TaskID: task, Operation: "workspace_mkdir", Path: "reserved", Key: "prepare"})
			if err != nil {
				t.Fatal(err)
			}
			began := false
			err = db.abortWorkspaceNamespace(w.ID, n.ID, func() (*sql.Tx, error) {
				// This runs after the initial execution check and before BEGIN,
				// deterministically reproducing the previously unguarded interval.
				began = true
				if err := tc.change(db); err != nil {
					t.Fatal(err)
				}
				return db.sql.Begin()
			})
			if !began || !errors.Is(err, tc.want) {
				t.Fatalf("abort boundary: began=%v error=%v, want %v", began, err, tc.want)
			}
			pending, err := db.WorkspaceNamespaceIntent(w.ID, n.ID)
			if err != nil || pending.State != "prepared" {
				t.Fatalf("reservation released after authority changed: %+v %v", pending, err)
			}
			if _, err := db.sql.Exec(`INSERT INTO workspace_artifacts(id,workspace_id,path,state,created_at) VALUES('blocked',?,'reserved/file.txt','pending',?)`, w.ID, now()); !errors.Is(namespaceError(err), ErrWorkspaceConflict) {
				t.Fatalf("prepared reservation no longer blocks descendant creation: %v", err)
			}
		})
	}
}

func namespaceFixture(t *testing.T) (*DB, Workspace, int64) {
	t.Helper()
	db, w, task := workspaceFixture(t)
	if _, err := db.AcquireCoordinator("namespace-test"); err != nil {
		t.Fatal(err)
	}
	return db, w, task
}

func TestWorkspaceNamespaceFreshMigrationInventory(t *testing.T) {
	db, _, _ := namespaceFixture(t)
	assertIntegrationSchema(t, db)
	for _, table := range []string{"workspace_directories", "workspace_namespace_intents"} {
		var count int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("missing namespace table %s: %d %v", table, count, err)
		}
	}
}
func namespaceMkdir(t *testing.T, db *DB, w Workspace, task int64, p, key string) WorkspaceNamespaceIntent {
	t.Helper()
	n, err := db.PrepareWorkspaceNamespace(WorkspaceNamespaceRequest{WorkspaceID: w.ID, TaskID: task, Operation: "workspace_mkdir", Path: p, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	n, err = db.CommitWorkspaceNamespace(w.ID, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func TestWorkspaceNamespaceDirectoryGenerationAndReceipts(t *testing.T) {
	db, w, task := namespaceFixture(t)
	n := namespaceMkdir(t, db, w, task, "reports", "create")
	d, err := db.WorkspaceDirectory(w.ID, "reports")
	if err != nil || d.Revision != n.DirectoryRevision {
		t.Fatalf("directory: %+v %v", d, err)
	}
	again := namespaceMkdir(t, db, w, task, "reports", "create")
	if again.ID != n.ID {
		t.Fatal("retry minted a receipt")
	}
	idempotent := namespaceMkdir(t, db, w, task, "reports", "verify")
	if idempotent.DirectoryID != n.DirectoryID || idempotent.DirectoryRevision != n.DirectoryRevision {
		t.Fatal("idempotent mkdir changed generation")
	}
	r := WorkspaceNamespaceRequest{WorkspaceID: w.ID, TaskID: task, Operation: "workspace_rmdir", Path: "reports", ExpectedDirectoryRevision: d.Revision, Key: "remove"}
	removal, err := db.PrepareWorkspaceNamespace(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.CommitWorkspaceNamespace(w.ID, removal.ID); err != nil {
		t.Fatal(err)
	}
	recreated := namespaceMkdir(t, db, w, task, "reports", "recreate")
	if recreated.DirectoryID == n.DirectoryID || recreated.DirectoryRevision == n.DirectoryRevision {
		t.Fatal("recreation reused identity/generation")
	}
	r.Key = "stale"
	if _, err = db.PrepareWorkspaceNamespace(r); !errors.Is(err, ErrWorkspaceConflict) {
		t.Fatalf("stale removal: %v", err)
	}
	// Settled retry retains original receipt without removing the replacement.
	if got, err := db.CommitWorkspaceNamespace(w.ID, removal.ID); err != nil || got.ID != removal.ID {
		t.Fatalf("settled retry: %v", err)
	}
	if _, err = db.WorkspaceDirectory(w.ID, "reports"); err != nil {
		t.Fatal("retry removed replacement")
	}
	for _, query := range []string{`UPDATE workspace_directories SET revision='bad'`, `DELETE FROM workspace_directories`, `UPDATE workspace_namespace_intents SET path='changed'`, `DELETE FROM workspace_namespace_intents`} {
		if _, err = db.sql.Exec(query); err == nil {
			t.Fatalf("mutable history: %s", query)
		}
	}
	r.Operation = "workspace_mkdir"
	r.Key = "create"
	r.Path = "other"
	r.ExpectedDirectoryRevision = ""
	if _, err = db.PrepareWorkspaceNamespace(r); !errors.Is(err, ErrWorkspaceConflict) {
		t.Fatalf("key reuse: %v", err)
	}
}
func TestWorkspaceNamespaceEmptyAndPathReservations(t *testing.T) {
	db, w, task := namespaceFixture(t)
	parent := namespaceMkdir(t, db, w, task, "a_%", "parent")
	namespaceMkdir(t, db, w, task, "a_%/child", "child")
	r := WorkspaceNamespaceRequest{WorkspaceID: w.ID, TaskID: task, Operation: "workspace_rmdir", Path: "a_%", ExpectedDirectoryRevision: parent.DirectoryRevision, Key: "nonempty"}
	if _, err := db.PrepareWorkspaceNamespace(r); !errors.Is(err, ErrWorkspaceConflict) {
		t.Fatalf("child directory: %v", err)
	}
	empty := namespaceMkdir(t, db, w, task, "empty", "empty")
	r.Path = "empty"
	r.ExpectedDirectoryRevision = empty.DirectoryRevision
	r.Key = "reserve"
	intent, err := db.PrepareWorkspaceNamespace(r)
	if err != nil {
		t.Fatal(err)
	}
	// The reservation blocks future file publication and descendant mkdir.
	if err = db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "blocked", WorkspaceID: w.ID, Path: "empty/report.txt"}); err == nil {
		t.Fatal("file entered reserved directory")
	}
	if _, err = db.PrepareWorkspaceNamespace(WorkspaceNamespaceRequest{WorkspaceID: w.ID, TaskID: task, Operation: "workspace_mkdir", Path: "empty/child", Key: "blocked"}); !errors.Is(err, ErrWorkspaceConflict) {
		t.Fatalf("child entered reservation: %v", err)
	}
	if err = db.AbortWorkspaceNamespace(w.ID, intent.ID); err != nil {
		t.Fatal(err)
	}
	if err = db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "pending", WorkspaceID: w.ID, Path: "empty/report.txt"}); err != nil {
		t.Fatal(err)
	}
	if err = db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "nested", WorkspaceID: w.ID, Path: "implicit/parent/report.txt"}); err != nil {
		t.Fatal(err)
	}
	if d, err := db.WorkspaceDirectory(w.ID, "implicit/parent"); err != nil || len(d.Revision) != 32 {
		t.Fatalf("implicit parent lacks identity: %+v %v", d, err)
	}
	r.Key = "pending-file"
	if _, err = db.PrepareWorkspaceNamespace(r); !errors.Is(err, ErrWorkspaceConflict) {
		t.Fatalf("pending file not empty: %v", err)
	}
	for _, p := range []string{"", "../outside", "root/../outside", "/absolute", "a\\b"} {
		if _, err = db.PrepareWorkspaceNamespace(WorkspaceNamespaceRequest{WorkspaceID: w.ID, TaskID: task, Operation: "workspace_mkdir", Path: p, Key: "bad-" + p}); !errors.Is(err, ErrWorkspaceConflict) {
			t.Fatalf("unsafe path %q: %v", p, err)
		}
	}
	for _, p := range []string{"a_%", "a_%/child", "empty"} {
		if err = db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "collision-" + p, WorkspaceID: w.ID, Path: p}); err == nil {
			t.Fatalf("directory replaced by file %s", p)
		}
	}
	if err = db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "ancestor", WorkspaceID: w.ID, Path: "empty"}); err == nil {
		t.Fatal("file replaced implicit ancestor")
	}
}
func TestWorkspaceNamespaceMovePreservesHistoryAndPinsSource(t *testing.T) {
	db, w, task := namespaceFixture(t)
	first := workspaceIntent(t, db, w, task, "report", "r1", "", 3)
	workspacePublish(t, db, first, "receipt-r1")
	second := workspaceIntent(t, db, w, task, "report", "r2", "r1", 4)
	workspacePublish(t, db, second, "receipt-r2")
	r := WorkspaceNamespaceRequest{WorkspaceID: w.ID, TaskID: task, Operation: "workspace_move", Path: "report.txt", Destination: "renamed.txt", ArtifactID: "report", ExpectedRevision: "r2", Key: "move"}
	n, err := db.PrepareWorkspaceNamespace(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`UPDATE workspace_artifacts SET path='bypass.txt' WHERE id='report'`, `UPDATE workspace_artifacts SET state='trashed',trashed_at='fixture' WHERE id='report'`} {
		if _, err = db.sql.Exec(query); err == nil {
			t.Fatal("source changed under intent")
		}
	}
	if err = db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "collision", WorkspaceID: w.ID, Path: "renamed.txt"}); err == nil {
		t.Fatal("destination reservation bypass")
	}
	if _, err = db.CommitWorkspaceNamespace(w.ID, n.ID); err != nil {
		t.Fatal(err)
	}
	a, err := db.WorkspaceArtifact(w.ID, "report")
	if err != nil || a.Path != "renamed.txt" || a.CurrentRevision != "r2" {
		t.Fatalf("move: %+v %v", a, err)
	}
	for _, id := range []string{"r1", "r2"} {
		if _, err = db.WorkspaceRevision(w.ID, id); err != nil {
			t.Fatalf("history lost: %v", err)
		}
	}
	if got, err := db.PrepareWorkspaceNamespace(r); err != nil || got.ID != n.ID {
		t.Fatalf("move replay: %v", err)
	}
	r.Key = "stale"
	r.Path = "renamed.txt"
	r.Destination = "other.txt"
	r.ExpectedRevision = "r1"
	if _, err = db.PrepareWorkspaceNamespace(r); !errors.Is(err, ErrWorkspaceConflict) {
		t.Fatalf("stale move: %v", err)
	}
	shared := Workspace{ID: "other-scope", PersonID: w.PersonID, AgentID: w.AgentID, Visibility: "shared"}
	if err = db.CreateWorkspace(shared); err != nil {
		t.Fatal(err)
	}
	r.WorkspaceID = shared.ID
	r.ExpectedRevision = "r2"
	if _, err = db.PrepareWorkspaceNamespace(r); !errors.Is(err, ErrWorkspaceDenied) {
		t.Fatalf("cross scope: %v", err)
	}
	stats, err := db.WorkspaceQuotaStats(w.ID)
	if err != nil || stats.CommittedBytes != 7 {
		t.Fatalf("move altered quota: %+v %v", stats, err)
	}
}
func TestWorkspaceNamespaceConcurrentDestinationHasOneWinner(t *testing.T) {
	db, w, task := namespaceFixture(t)
	for _, id := range []string{"a", "b"} {
		p := workspaceIntent(t, db, w, task, id, "revision-"+id, "", 1)
		workspacePublish(t, db, p, "receipt-"+id)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			n, err := db.PrepareWorkspaceNamespace(WorkspaceNamespaceRequest{WorkspaceID: w.ID, TaskID: task, Operation: "workspace_move", Path: id + ".txt", Destination: "winner.txt", ArtifactID: id, ExpectedRevision: "revision-" + id, Key: id})
			if err == nil {
				_, err = db.CommitWorkspaceNamespace(w.ID, n.ID)
			}
			results <- err
		}(id)
	}
	close(start)
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrWorkspaceConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}
func TestWorkspaceNamespacePopulatedUpgradeAndRestart(t *testing.T) {
	// Recreate a pre-0103 schema with existing nested, immutable file history.
	pathName := filepath.Join(t.TempDir(), "upgrade.db")
	old, err := openUnmigrated(pathName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { old.Close() }()
	if err = old.migrateThrough("migrations/0102_workspace_quota_policy.sql"); err != nil {
		t.Fatal(err)
	}
	record(t, old, "mika")
	if err = old.UpsertModel(Model{Name: "local", Kind: ModelLocal, Provider: "ollama", ProviderModel: "fixture", Readiness: ModelReady, Enabled: true}, "admin"); err != nil {
		t.Fatal(err)
	}
	agent, err := old.CreateButler(Butler{Name: "alfred", Directive: "fixture", Surface: ButlerSurfaceAdmin, AgentClass: AgentClassOwner, Model: "local"}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	member, err := old.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	scope := Workspace{ID: "upgrade-workspace", PersonID: member.PersonID, AgentID: agent}
	if err = old.CreateWorkspace(scope); err != nil {
		t.Fatal(err)
	}
	if err = old.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "upgrade-file", WorkspaceID: scope.ID, Path: "nested/child/report.txt"}); err != nil {
		t.Fatal(err)
	}
	id := newTask(t, old, nil)
	if err = old.SetTaskButler(id, "alfred"); err != nil {
		t.Fatal(err)
	}
	task, err := parseButlerTaskID(id)
	if err != nil {
		t.Fatal(err)
	}
	p := workspaceIntent(t, old, scope, task, "retained", "old-r1", "", 5)
	workspacePublish(t, old, p, "old-receipt")
	if _, err = old.sql.Exec(`UPDATE workspace_artifacts SET path='nested/child/retained.txt' WHERE id='retained'`); err != nil {
		t.Fatal(err)
	}
	if _, err = old.AcquireCoordinator("upgrade-test"); err != nil {
		t.Fatal(err)
	}
	staged := WorkspaceWriteSession{ID: strings.Repeat("e", 32), WorkspaceID: scope.ID, TaskID: task, ArtifactID: "upgrade-staging", Path: "staged/parent/report.txt", BeginKey: "upgrade-begin", RequestHash: strings.Repeat("f", 64), ConfigRevision: "config", MaxBytes: 1024, Privacy: "normal"}
	if _, err = old.BeginWorkspaceWrite(staged); err != nil {
		t.Fatal(err)
	}
	if err = old.migrate(); err != nil {
		t.Fatal(err)
	}
	assertIntegrationSchema(t, old)
	dirs, err := old.WorkspaceDirectories(scope.ID)
	if err != nil || len(dirs) != 4 || dirs[0].Path != "nested" || dirs[1].Path != "nested/child" || dirs[2].Path != "staged" || dirs[3].Path != "staged/parent" {
		t.Fatalf("upgrade: %+v %v", dirs, err)
	}
	if err = old.Close(); err != nil {
		t.Fatal(err)
	}
	old, err = Open(pathName)
	if err != nil {
		t.Fatal(err)
	}
	assertIntegrationSchema(t, old)
	after, err := old.WorkspaceDirectories(scope.ID)
	if err != nil || len(after) != 4 || after[0] != dirs[0] || after[1] != dirs[1] || after[2] != dirs[2] || after[3] != dirs[3] {
		t.Fatalf("restart identities: %+v %v", after, err)
	}
	if r, err := old.WorkspaceRevision(scope.ID, "old-r1"); err != nil || r.SizeBytes != 5 {
		t.Fatalf("upgrade lost history: %+v %v", r, err)
	}
	if s, err := old.WorkspaceWriteSession(scope.ID, staged.ID); err != nil || s.State != "open" || s.Path != staged.Path {
		t.Fatalf("upgrade lost staging: %+v %v", s, err)
	}
}

func TestWorkspaceNamespaceWritePinsAndCommitFault(t *testing.T) {
	db, w, task := namespaceFixture(t)
	d := namespaceMkdir(t, db, w, task, "scratch", "scratch")
	s := WorkspaceWriteSession{ID: strings.Repeat("a", 32), WorkspaceID: w.ID, TaskID: task, ArtifactID: "writing", Path: "scratch/report.txt", BeginKey: "begin", RequestHash: strings.Repeat("b", 64), ConfigRevision: "config", MaxBytes: 1024, Privacy: "normal"}
	if _, err := db.BeginWorkspaceWrite(s); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PrepareWorkspaceNamespace(WorkspaceNamespaceRequest{WorkspaceID: w.ID, TaskID: task, Operation: "workspace_rmdir", Path: "scratch", ExpectedDirectoryRevision: d.DirectoryRevision, Key: "remove"}); !errors.Is(err, ErrWorkspaceConflict) {
		t.Fatalf("open write not empty: %v", err)
	}
	p := workspaceIntent(t, db, w, task, "source", "r1", "", 1)
	workspacePublish(t, db, p, "receipt")
	r := WorkspaceNamespaceRequest{WorkspaceID: w.ID, TaskID: task, Operation: "workspace_move", Path: "source.txt", Destination: "target.txt", ArtifactID: "source", ExpectedRevision: "r1", Key: "move"}
	n, err := db.PrepareWorkspaceNamespace(r)
	if err != nil {
		t.Fatal(err)
	}
	s.ID = strings.Repeat("c", 32)
	s.ArtifactID = "source"
	s.Path = "source.txt"
	s.BaseRevision = "r1"
	s.BeginKey = "edit"
	if _, err = db.BeginWorkspaceWrite(s); !errors.Is(namespaceError(err), ErrWorkspaceConflict) {
		t.Fatalf("edit raced reserved source: %v", err)
	}
	if err = db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "child", WorkspaceID: w.ID, Path: "target.txt/child.txt"}); err == nil {
		t.Fatal("descendant raced reserved destination")
	}
	if _, err = db.sql.Exec(`CREATE TRIGGER namespace_commit_fault BEFORE UPDATE OF path ON workspace_artifacts BEGIN SELECT RAISE(ABORT,'injected metadata fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.CommitWorkspaceNamespace(w.ID, n.ID); err == nil {
		t.Fatal("fault ignored")
	}
	if a, err := db.WorkspaceArtifact(w.ID, "source"); err != nil || a.Path != "source.txt" {
		t.Fatalf("source lost: %+v %v", a, err)
	}
	if pending, err := db.WorkspaceNamespaceIntent(w.ID, n.ID); err != nil || pending.State != "prepared" {
		t.Fatalf("receipt escaped rollback: %+v %v", pending, err)
	}
	if _, err = db.sql.Exec(`DROP TRIGGER namespace_commit_fault`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.CommitWorkspaceNamespace(w.ID, n.ID); err != nil {
		t.Fatal(err)
	}
	s.Path = "target.txt"
	if _, err = db.BeginWorkspaceWrite(s); err != nil {
		t.Fatal(err)
	}
	r.Path = "target.txt"
	r.Destination = "later.txt"
	r.Key = "during-edit"
	if _, err = db.PrepareWorkspaceNamespace(r); !errors.Is(err, ErrWorkspaceConflict) {
		t.Fatalf("move raced open edit: %v", err)
	}
	if _, err = db.PromoteCoordinator("replacement"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.CommitWorkspaceNamespace(w.ID, n.ID); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("receipt retry escaped fence: %v", err)
	}
	if err = db.AbortWorkspaceNamespace(w.ID, n.ID); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("abort escaped fence: %v", err)
	}
}

func TestWorkspaceNamespaceConcurrentRmdirAndCreate(t *testing.T) {
	db, w, task := namespaceFixture(t)
	d := namespaceMkdir(t, db, w, task, "race", "create-directory")
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		n, err := db.PrepareWorkspaceNamespace(WorkspaceNamespaceRequest{WorkspaceID: w.ID, TaskID: task, Operation: "workspace_rmdir", Path: "race", ExpectedDirectoryRevision: d.DirectoryRevision, Key: "remove"})
		if err == nil {
			_, err = db.CommitWorkspaceNamespace(w.ID, n.ID)
		}
		results <- err
	}()
	go func() {
		<-start
		results <- db.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "race-file", WorkspaceID: w.ID, Path: "race/report.txt"})
	}()
	close(start)
	first, second := <-results, <-results
	if first != nil && second != nil {
		t.Fatalf("remove/create outcomes: %v / %v", first, second)
	}
	var liveFiles, liveDirs int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM workspace_artifacts WHERE id='race-file'`).Scan(&liveFiles); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM workspace_directories WHERE workspace_id=? AND path='race' AND state='committed'`, w.ID).Scan(&liveDirs); err != nil {
		t.Fatal(err)
	}
	if liveFiles == 1 && liveDirs != 1 {
		t.Fatal("file created under removed directory")
	}
	// Both may succeed in the serial order remove, then create; the new parent
	// must receive a fresh token, never revive the removed directory identity.
	if first == nil && second == nil {
		current, err := db.WorkspaceDirectory(w.ID, "race")
		if err != nil || current.Revision == d.DirectoryRevision || current.ID == d.DirectoryID {
			t.Fatalf("recreate reused removed identity: %+v %v", current, err)
		}
	}
}

func TestWorkspaceNamespacePendingIntentSurvivesRestart(t *testing.T) {
	db, w, task := namespaceFixture(t)
	p := workspaceIntent(t, db, w, task, "restart", "restart-r1", "", 1)
	workspacePublish(t, db, p, "restart-receipt")
	r := WorkspaceNamespaceRequest{WorkspaceID: w.ID, TaskID: task, Operation: "workspace_move", Path: "restart.txt", Destination: "after.txt", ArtifactID: "restart", ExpectedRevision: "restart-r1", Key: "restart-move"}
	n, err := db.PrepareWorkspaceNamespace(r)
	if err != nil {
		t.Fatal(err)
	}
	filename := db.path
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err = reopened.AcquireCoordinator("namespace-test"); err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.PrepareWorkspaceNamespace(r); err != nil || got.ID != n.ID || got.State != "prepared" {
		t.Fatalf("pending intent changed: %+v %v", got, err)
	}
	if pending, err := reopened.PendingWorkspaceNamespaceIntents(); err != nil || len(pending) != 1 || pending[0].ID != n.ID {
		t.Fatalf("recovery inventory: %+v %v", pending, err)
	}
	if err = reopened.CreateWorkspaceArtifact(WorkspaceArtifact{ID: "restart-collision", WorkspaceID: w.ID, Path: "after.txt"}); err == nil {
		t.Fatal("restart lost destination reservation")
	}
	if _, err = reopened.CommitWorkspaceNamespace(w.ID, n.ID); err != nil {
		t.Fatal(err)
	}
	if a, err := reopened.WorkspaceArtifact(w.ID, "restart"); err != nil || a.Path != "after.txt" || a.CurrentRevision != "restart-r1" {
		t.Fatalf("reconciliation: %+v %v", a, err)
	}
}
