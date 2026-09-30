package publication

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace/storage"
)

var template = dbtemplate.New(func(p string) error {
	db, err := runtimedb.Open(p)
	if err != nil {
		return err
	}
	return db.Close()
})

type fixture struct {
	c        *Coordinator
	dbPath   string
	request  BeginRequest
	settings workspace.Settings
}

func setup(t *testing.T) fixture {
	t.Helper()
	dbPath := template.Path(t)
	db, err := runtimedb.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err = db.AcquireCoordinator("test-coordinator"); err != nil {
		t.Fatal(err)
	}
	if err = db.RecordButlerMember("writer", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err = db.AdmitButlerMember("writer", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err = db.UpsertModel(runtimedb.Model{Name: "local", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "model", Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateButler(runtimedb.Butler{Name: "agent", Directive: "assist", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Model: "local"}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	member, err := db.GetButlerMember("writer")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := db.GetButler("agent")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CreateWorkspace(runtimedb.Workspace{ID: "workspace", PersonID: member.PersonID, AgentID: agent.Seq}); err != nil {
		t.Fatal(err)
	}
	tid, err := db.InsertButlerTask(runtimedb.ButlerTask{Kind: runtimedb.ButlerTaskConversation, Author: "writer", Owner: "writer", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Audience: runtimedb.ButlerAudiencePrivate, ObjectiveKind: runtimedb.ObjectiveInformational, Objective: "write report", CompletionCriteria: "saved"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetTaskButler(tid, "agent"); err != nil {
		t.Fatal(err)
	}
	task, err := db.GetButlerTask(tid)
	if err != nil {
		t.Fatal(err)
	}
	settings := workspace.Settings{Root: t.TempDir(), QuotaBytes: 1 << 30, MaxFileBytes: 4096, ScratchRetentionDays: 7, TrashRetentionDays: 30, Revision: "config-one"}
	c := &Coordinator{DB: db, Settings: func() (workspace.Settings, error) { return settings, nil }, Authorize: func(ctx context.Context, s Session) error {
		if s.WorkspaceID != "workspace" || s.TaskID != task.Seq {
			return ErrContext
		}
		return nil
	}}
	return fixture{c: c, dbPath: dbPath, settings: settings, request: BeginRequest{WorkspaceID: "workspace", TaskID: task.Seq, Path: "report.md", Key: "begin", Privacy: "normal"}}
}
func begin(t *testing.T, f fixture, key, text string) Session {
	t.Helper()
	r := f.request
	r.Key = key
	s, err := f.c.Begin(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.c.Chunk(context.Background(), s.WorkspaceID, s.ID, 0, text, s.ID+":"+strconv.Itoa(0)); err != nil {
		t.Fatal(err)
	}
	return s
}
func commit(t *testing.T, c *Coordinator, s Session, key string) Receipt {
	t.Helper()
	r, err := c.Commit(context.Background(), s.WorkspaceID, s.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func reopen(t *testing.T, f *fixture) {
	t.Helper()
	if err := f.c.DB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := runtimedb.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err = db.AcquireCoordinator("test-coordinator"); err != nil {
		t.Fatal(err)
	}
	old := f.c
	f.c = &Coordinator{DB: db, Settings: old.Settings, Authorize: old.Authorize}
}
func TestWriteRetryReceiptVisibilityAndAuthority(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s := begin(t, f, "begin", "complete")
	same, err := f.c.Begin(ctx, f.request)
	if err != nil || same.ID != s.ID {
		t.Fatalf("begin retry: %v %v", same, err)
	}
	changed := f.request
	changed.Path = "other.md"
	if _, err = f.c.Begin(ctx, changed); !errors.Is(err, runtimedb.ErrWorkspaceConflict) {
		t.Fatalf("reused begin key: %v", err)
	}
	if _, err = f.c.Chunk(ctx, s.WorkspaceID, s.ID, 0, "changed", s.ID+":"+strconv.Itoa(0)); !errors.Is(err, runtimedb.ErrWorkspaceConflict) {
		t.Fatalf("chunk mismatch: %v", err)
	}
	if n, err := f.c.Chunk(ctx, s.WorkspaceID, s.ID, 0, "complete", s.ID+":"+strconv.Itoa(0)); err != nil || n != 1 {
		t.Fatalf("chunk retry: %d %v", n, err)
	}
	if _, err = f.c.DB.WorkspaceArtifact(s.WorkspaceID, s.ArtifactID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("staging visible: %v", err)
	}
	receipt := commit(t, f.c, s, "commit")
	reopen(t, &f)
	again := commit(t, f.c, s, "commit")
	if again != receipt {
		t.Fatalf("receipt changed: %+v %+v", receipt, again)
	}
	again = commit(t, f.c, s, "retry-key")
	if again != receipt {
		t.Fatal("session retry minted another receipt")
	}
	f.request.Path = "second.md"
	other := begin(t, f, "second", "second")
	if _, err = f.c.Commit(ctx, other.WorkspaceID, other.ID, "commit"); !errors.Is(err, runtimedb.ErrWorkspaceConflict) {
		t.Fatalf("commit key reused for another session: %v", err)
	}
	f.c.Authorize = func(context.Context, Session) error { return ErrContext }
	if _, err = f.c.Commit(ctx, s.WorkspaceID, s.ID, "commit"); !errors.Is(err, ErrContext) {
		t.Fatalf("receipt bypassed revocation: %v", err)
	}
}
func TestCrashBoundariesRetryOriginalReceipt(t *testing.T) {
	for _, point := range []string{"sealed", "assembled", "before_publish", "after_publish", "before_commit", "after_commit"} {
		t.Run(point, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			old := begin(t, f, "old", "prior")
			oldReceipt := commit(t, f.c, old, "old-commit")
			f.request.ArtifactID = old.ArtifactID
			f.request.ExpectedRevision = oldReceipt.Revision
			next := begin(t, f, "next", "replacement")
			crash := errors.New("injected crash")
			f.c.fault = func(p string) error {
				if p == point {
					return crash
				}
				return nil
			}
			if _, err := f.c.Commit(ctx, next.WorkspaceID, next.ID, "next-commit"); !errors.Is(err, crash) {
				t.Fatalf("crash missing: %v", err)
			}
			a, err := f.c.DB.WorkspaceArtifact(old.WorkspaceID, old.ArtifactID)
			if err != nil {
				t.Fatal(err)
			}
			if point != "after_commit" && a.CurrentRevision != oldReceipt.Revision {
				t.Fatal("uncommitted new revision visible")
			}
			if _, err = f.c.DB.WorkspaceRevision(old.WorkspaceID, oldReceipt.Revision); err != nil {
				t.Fatalf("old revision lost: %v", err)
			}
			reopen(t, &f)
			receipt := commit(t, f.c, next, "next-commit")
			again := commit(t, f.c, next, "next-commit")
			if receipt != again {
				t.Fatal("duplicate receipt")
			}
			if receipt.Revision != next.ID {
				t.Fatal("new object identity on retry")
			}
		})
	}
}
func TestConcurrentSameBaseOneCommit(t *testing.T) {
	f := setup(t)
	old := begin(t, f, "old", "old")
	r := commit(t, f.c, old, "old-commit")
	f.request.ArtifactID = old.ArtifactID
	f.request.ExpectedRevision = r.Revision
	a := begin(t, f, "a", "first")
	b := begin(t, f, "b", "second")
	// Independent coordinators prove the DB journal, not only local locks,
	// serializes compare/commit of a shared artifact.
	other := &Coordinator{DB: f.c.DB, Settings: f.c.Settings, Authorize: f.c.Authorize}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, s := range []Session{a, b} {
		wg.Add(1)
		go func(i int, s Session) {
			defer wg.Done()
			c := f.c
			if i == 1 {
				c = other
			}
			_, err := c.Commit(context.Background(), s.WorkspaceID, s.ID, "commit-"+strconv.Itoa(i))
			errs <- err
		}(i, s)
	}
	wg.Wait()
	close(errs)
	success, conflict := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, runtimedb.ErrWorkspaceConflict) {
			conflict++
		} else {
			t.Fatalf("unexpected commit failure: %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflicts=%d", success, conflict)
	}
	current, err := f.c.DB.WorkspaceArtifact(old.WorkspaceID, old.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := f.c.store(old, f.settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{r.Revision, current.CurrentRevision} {
		revision, err := f.c.DB.WorkspaceRevision(old.WorkspaceID, id)
		if err != nil {
			t.Fatal("retained revision lost", err)
		}
		bytes, err := st.Read(storage.Object{ID: revision.ID, SHA256: revision.Hash, Size: revision.SizeBytes})
		if err != nil {
			t.Fatal("retained bytes lost", err)
		}
		expected := "old"
		if id == a.ID {
			expected = "first"
		}
		if id == b.ID {
			expected = "second"
		}
		if string(bytes) != expected {
			t.Fatalf("metadata/byte mismatch: %q != %q", bytes, expected)
		}
	}
}
func TestStartupRollsBackUnsettledAndKeepsAccounting(t *testing.T) {
	for _, point := range []string{"sealed", "before_publish", "after_publish", "before_commit", "after_commit"} {
		t.Run(point, func(t *testing.T) {
			f := setup(t)
			old := begin(t, f, "old", "old")
			previous := commit(t, f.c, old, "old-commit")
			f.request.ArtifactID = old.ArtifactID
			f.request.ExpectedRevision = previous.Revision
			s := begin(t, f, "next", "next")
			f.c.fault = func(p string) error {
				if p == point {
					return errors.New("crash")
				}
				return nil
			}
			if _, err := f.c.Commit(context.Background(), s.WorkspaceID, s.ID, "next-commit"); err == nil {
				t.Fatal("missing fault")
			}
			reopen(t, &f)
			n, err := f.c.DB.RecoverWorkspaceWrites()
			if err != nil {
				t.Fatal(err)
			}
			if point == "after_commit" {
				if n != 0 {
					t.Fatal("settled effect rolled back")
				}
				commit(t, f.c, s, "next-commit")
				return
			}
			if n != 1 {
				t.Fatalf("rolled back %d", n)
			}
			n, err = f.c.DB.RecoverWorkspaceWrites()
			if err != nil || n != 0 {
				t.Fatalf("recovery retry: %d %v", n, err)
			}
			a, err := f.c.DB.WorkspaceArtifact(s.WorkspaceID, s.ArtifactID)
			if err != nil || a.CurrentRevision != previous.Revision {
				t.Fatalf("prior lost: %+v %v", a, err)
			}
			if _, err = f.c.DB.WorkspaceReceipt(s.WorkspaceID, s.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("invented receipt: %v", err)
			}
			q, err := f.c.DB.WorkspaceReservation(s.WorkspaceID, s.ID)
			if err != nil || q.State != "held" || q.SizeBytes != 4 {
				t.Fatalf("orphan bytes unaccounted: %+v %v", q, err)
			}
			if _, err = f.c.Commit(context.Background(), s.WorkspaceID, s.ID, "next-commit"); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("aborted effect resurrected: %v", err)
			}
		})
	}
}
func TestInterruptedChunksBoundsAndScratch(t *testing.T) {
	for _, point := range []string{"chunk_journaled", "chunk_staged"} {
		t.Run(point, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			s, err := f.c.Begin(ctx, f.request)
			if err != nil {
				t.Fatal(err)
			}
			f.c.fault = func(p string) error {
				if p == point {
					return errors.New("crash")
				}
				return nil
			}
			if _, err = f.c.Chunk(ctx, s.WorkspaceID, s.ID, 0, "part", s.ID+":"+strconv.Itoa(0)); err == nil {
				t.Fatal("missing fault")
			}
			chunks, err := f.c.DB.WorkspaceWriteChunks(s.ID)
			if err != nil || len(chunks) != 1 {
				t.Fatal(chunks, err)
			}
			q, err := f.c.DB.WorkspaceReservation(s.WorkspaceID, chunks[0].ReservationID)
			if err != nil || q.State != "held" || q.SizeBytes != 4 {
				t.Fatal(q, err)
			}
			reopen(t, &f)
			if n, err := f.c.DB.RecoverWorkspaceWrites(); err != nil || n != 0 {
				t.Fatalf("incomplete staging discarded: %d %v", n, err)
			}
			if _, err = f.c.Commit(ctx, s.WorkspaceID, s.ID, "commit"); err == nil {
				t.Fatal("partial chunk published")
			}
			if _, err = f.c.Chunk(ctx, s.WorkspaceID, s.ID, 1, "skip", s.ID+":"+strconv.Itoa(1)); err == nil {
				t.Fatal("pending chunk bypassed")
			}
			if _, err = f.c.Chunk(ctx, s.WorkspaceID, s.ID, 0, "part", s.ID+":"+strconv.Itoa(0)); err != nil {
				t.Fatal(err)
			}
			if _, err = f.c.Chunk(ctx, s.WorkspaceID, s.ID, 1, strings.Repeat("x", 1025), s.ID+":"+strconv.Itoa(1)); !errors.Is(err, ErrInput) {
				t.Fatalf("oversize chunk: %v", err)
			}
			commit(t, f.c, s, "commit")
		})
	}
}
func TestEmptyWriteAndConfigurationFence(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s, err := f.c.Begin(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, f.c, s, "empty")
	r, err := f.c.DB.WorkspaceRevision(s.WorkspaceID, s.ID)
	if err != nil || r.SizeBytes != 0 {
		t.Fatal(r, err)
	}
	settings := f.settings
	settings.Revision = "changed"
	f.c.Settings = func() (workspace.Settings, error) { return settings, nil }
	if _, err = f.c.Commit(ctx, s.WorkspaceID, s.ID, "empty"); !errors.Is(err, workspace.ErrSettings) {
		t.Fatalf("stale config accepted: %v", err)
	}
}

func TestRevocationAndEpochAtCommitBoundary(t *testing.T) {
	for _, kind := range []string{"authority", "epoch", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			s := begin(t, f, "begin", "report")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.c.fault = func(point string) error {
				if point == "before_commit" {
					switch kind {
					case "authority":
						f.c.Authorize = func(context.Context, Session) error { return ErrContext }
					case "epoch":
						_, err := f.c.DB.PromoteCoordinator("replacement")
						return err
					case "cancelled":
						cancel()
					}
				}
				return nil
			}
			if _, err := f.c.Commit(ctx, s.WorkspaceID, s.ID, "commit"); err == nil {
				t.Fatal("revoked effect committed")
			}
			if _, err := f.c.DB.WorkspaceArtifact(s.WorkspaceID, s.ArtifactID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("revoked artifact visible: %v", err)
			}
			if _, err := f.c.DB.WorkspaceReceipt(s.WorkspaceID, s.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("revoked receipt invented: %v", err)
			}
		})
	}
}
func TestAbortedCreateCanRetryWithoutDiscardingJournal(t *testing.T) {
	f := setup(t)
	s := begin(t, f, "begin", "report")
	f.c.fault = func(point string) error {
		if point == "after_publish" {
			return errors.New("crash")
		}
		return nil
	}
	if _, err := f.c.Commit(context.Background(), s.WorkspaceID, s.ID, "commit"); err == nil {
		t.Fatal("missing fault")
	}
	reopen(t, &f)
	if n, err := f.c.DB.RecoverWorkspaceWrites(); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	next := begin(t, f, "new-begin", "replacement")
	if next.ArtifactID != s.ArtifactID {
		t.Fatal("aborted artifact identity discarded")
	}
	commit(t, f.c, next, "new-commit")
	p, err := f.c.DB.WorkspacePublication(s.WorkspaceID, s.ID)
	if err != nil || p.State != "aborted" {
		t.Fatal(p, err)
	}
	q, err := f.c.DB.WorkspaceReservation(s.WorkspaceID, s.ID)
	if err != nil || q.State != "held" {
		t.Fatal("recovery bytes not charged", q, err)
	}
}
func TestScratchPinsAndFileCeiling(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s := begin(t, f, "begin", strings.Repeat("a", 1024))
	chunks, err := f.c.DB.WorkspaceWriteChunks(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.c.DB.ReleaseWorkspaceReservation(s.WorkspaceID, chunks[0].ReservationID); err == nil {
		t.Fatal("unresolved scratch reservation released")
	}
	for i := 1; i < 4; i++ {
		if _, err = f.c.Chunk(ctx, s.WorkspaceID, s.ID, i, strings.Repeat("b", 1024), s.ID+":"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = f.c.Chunk(ctx, s.WorkspaceID, s.ID, 4, "x", s.ID+":"+strconv.Itoa(4)); err == nil {
		t.Fatal("file ceiling bypassed")
	}
	chunks, err = f.c.DB.WorkspaceWriteChunks(s.ID)
	if err != nil || len(chunks) != 4 {
		t.Fatal("failed append left intent", chunks, err)
	}
	commit(t, f.c, s, "commit")
}
func TestMissingOrCorruptPublishedBytesCannotCommit(t *testing.T) {
	for _, kind := range []string{"missing", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			s := begin(t, f, "begin", "report")
			f.c.fault = func(point string) error {
				if point == "before_commit" {
					return errors.New("crash")
				}
				return nil
			}
			if _, err := f.c.Commit(context.Background(), s.WorkspaceID, s.ID, "commit"); err == nil {
				t.Fatal("missing fault")
			}
			w, err := f.c.DB.WorkspaceByID(s.WorkspaceID)
			if err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(f.settings.Root, hash([]byte(w.PersonID)), hash([]byte(strconv.FormatInt(w.AgentID, 10))), "revisions", s.ID)
			if kind == "missing" {
				err = os.Remove(name)
			} else {
				if err = os.Chmod(name, 0600); err == nil {
					err = os.WriteFile(name, []byte("broken"), 0600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			reopen(t, &f)
			if _, err = f.c.Commit(context.Background(), s.WorkspaceID, s.ID, "commit"); err == nil {
				t.Fatal("unverified bytes committed")
			}
			if _, err = f.c.DB.WorkspaceReceipt(s.WorkspaceID, s.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("invented receipt", err)
			}
			if _, err = f.c.DB.RecoverWorkspaceWrites(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStartupRecoveryRespectsQuarantineAndHouseholdStop(t *testing.T) {
	for _, kind := range []string{"quarantine", "stop"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			s := begin(t, f, "begin", "report")
			f.c.fault = func(point string) error {
				if point == "after_publish" {
					return errors.New("crash")
				}
				return nil
			}
			if _, err := f.c.Commit(context.Background(), s.WorkspaceID, s.ID, "commit"); err == nil {
				t.Fatal("missing fault")
			}
			var err error
			if kind == "quarantine" {
				err = f.c.DB.EnterRestoreQuarantine("test restore", time.Now())
			} else {
				err = f.c.DB.StopHouseholdExecution("writer", "test stop")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.c.DB.RecoverWorkspaceWrites(); err == nil {
				t.Fatal("recovery bypassed execution gate")
			}
			p, err := f.c.DB.WorkspacePublication(s.WorkspaceID, s.ID)
			if err != nil || p.State != "prepared" {
				t.Fatal("gated recovery changed journal", p, err)
			}
		})
	}
}

func TestChunkInvokeKeyBindingAndSettledReplay(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s := begin(t, f, "begin", "chunk")
	if _, err := f.c.Chunk(ctx, s.WorkspaceID, s.ID, 1, "next", s.ID+":0"); !errors.Is(err, runtimedb.ErrWorkspaceConflict) {
		t.Fatalf("chunk key reused for different index: %v", err)
	}
	f.request.Path = "other.md"
	other := begin(t, f, "other", "other")
	if _, err := f.c.Chunk(ctx, other.WorkspaceID, other.ID, 0, "other", s.ID+":0"); !errors.Is(err, runtimedb.ErrWorkspaceConflict) {
		t.Fatalf("chunk key reused for another session: %v", err)
	}
	if _, err := f.c.Chunk(ctx, s.WorkspaceID, s.ID, 1, "next", ""); !errors.Is(err, ErrInput) {
		t.Fatalf("missing chunk key: %v", err)
	}
	commit(t, f.c, s, "commit")
	if next, err := f.c.Chunk(ctx, s.WorkspaceID, s.ID, 0, "chunk", s.ID+":0"); err != nil || next != 1 {
		t.Fatalf("settled chunk replay: %d %v", next, err)
	}
	if _, err := f.c.Chunk(ctx, s.WorkspaceID, s.ID, 1, "next", "new-key"); !errors.Is(err, runtimedb.ErrWorkspaceConflict) {
		t.Fatalf("append to settled session: %v", err)
	}
}
