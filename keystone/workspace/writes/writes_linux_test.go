//go:build linux

package writes

import (
	"context"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace/storage"
	"strconv"
	"strings"
	"testing"
)

type commitData struct {
	Receipt receipt `json:"receipt"`
	File    file    `json:"file"`
}

func chunk(t *testing.T, f *fixture, id string, index int, text, key string) {
	t.Helper()
	var data any
	decode(t, f.service.Call(f.ctx(t, "workspace_write_chunk", key), "workspace_write_chunk", butler.WorkspaceWriteCall{WriteID: id, Index: index, Text: text}), &data)
}
func commit(t *testing.T, f *fixture, id, key string) commitData {
	t.Helper()
	var data commitData
	decode(t, f.service.Call(f.ctx(t, "workspace_write_commit", key), "workspace_write_commit", butler.WorkspaceWriteCall{WriteID: id}), &data)
	return data
}
func body(t *testing.T, f *fixture, file file) string {
	t.Helper()
	w, err := f.db.WorkspaceByID("workspace")
	must(t, err)
	r, err := f.db.ReadWorkspaceRevision(f.ctx(t, "workspace_write_commit", "read"), f.db.BindWorkspaceAccess("fixture-writer", "fixture-agent", f.task, "console"), w.ID, string(file.ArtifactID), string(file.Revision))
	must(t, err)
	st, err := storage.New(f.settings, w.PersonID, strconv.FormatInt(w.AgentID, 10))
	must(t, err)
	data, err := st.Read(storage.Object{ID: r.ID, SHA256: r.Hash, Size: r.SizeBytes})
	must(t, err)
	return string(data)
}
func TestReportPublicationReplayCancellationAndStaleEdits(t *testing.T) {
	f := setup(t)
	id := f.begin(t, "begin", "report.md")
	chunk(t, f, id, 0, "# Report\n", "chunk-0")
	chunk(t, f, id, 1, "stable synthetic text\n", "chunk-1")
	chunk(t, f, id, 0, "# Report\n", "chunk-0")
	bad := f.service.Call(f.ctx(t, "workspace_write_chunk", "chunk-0"), "workspace_write_chunk", butler.WorkspaceWriteCall{WriteID: id, Index: 0, Text: "different"})
	if !bad.Failed {
		t.Fatal("changed replay chunk accepted")
	}
	first := commit(t, f, id, "commit")
	if body(t, f, first.File) != "# Report\nstable synthetic text\n" {
		t.Fatal("wrong assembled bytes")
	}
	// Reconstruct the adapter as a process restart would; durable receipts and
	// chunk identities, not in-memory result caches, govern retries.
	restart(t, f)
	replay := commit(t, f, id, "retry")
	if replay.Receipt.ID != first.Receipt.ID || replay.File.Revision != first.File.Revision {
		t.Fatal("duplicate revision/receipt")
	}
	collision := f.service.Call(f.ctx(t, "workspace_write_begin", "collision"), "workspace_write_begin", butler.WorkspaceWriteCall{Scope: "private", Path: "report.md"})
	if !collision.Failed {
		t.Fatal("create replaced prior file")
	}
	cancelled := f.begin(t, "cancel-begin", "cancel.md")
	ctx, cancel := context.WithCancel(f.ctx(t, "workspace_write_commit", "cancel-commit"))
	cancel()
	out := f.service.Call(ctx, "workspace_write_commit", butler.WorkspaceWriteCall{WriteID: cancelled})
	if !out.Failed {
		t.Fatal("cancelled context committed")
	}
	restart(t, f)
	commit(t, f, cancelled, "cancel-commit")
	base := string(first.File.Revision)
	args := butler.WorkspaceWriteCall{Scope: "private", Path: "report.md", ArtifactID: string(first.File.ArtifactID), ExpectedRevision: &base}
	var a, b struct {
		ID string `json:"writeId"`
	}
	decode(t, f.service.Call(f.ctx(t, "workspace_write_begin", "edit-a"), "workspace_write_begin", args), &a)
	decode(t, f.service.Call(f.ctx(t, "workspace_write_begin", "edit-b"), "workspace_write_begin", args), &b)
	chunk(t, f, a.ID, 0, "winner", "edit-a-chunk")
	chunk(t, f, b.ID, 0, "stale", "edit-b-chunk")
	winner := commit(t, f, a.ID, "edit-a-commit")
	out = f.service.Call(f.ctx(t, "workspace_write_commit", "edit-b-commit"), "workspace_write_commit", butler.WorkspaceWriteCall{WriteID: b.ID})
	if !out.Failed || !strings.Contains(out.Content, "conflict") {
		t.Fatalf("stale replaced winner: %+v", out)
	}
	if body(t, f, winner.File) != "winner" {
		t.Fatal("prior bytes changed")
	}
}
func TestPatchUniqueMatchAndArgumentReplay(t *testing.T) {
	f := setup(t)
	id := f.begin(t, "begin", "report.md")
	chunk(t, f, id, 0, "old synthetic text", "chunk")
	first := commit(t, f, id, "commit")
	args := butler.WorkspaceWriteCall{ArtifactID: string(first.File.ArtifactID), Revision: string(first.File.Revision), OldText: "old", NewText: "new"}
	var patched commitData
	decode(t, f.service.Call(f.ctx(t, "workspace_patch", "patch"), "workspace_patch", args), &patched)
	if body(t, f, patched.File) != "new synthetic text" {
		t.Fatal("patch bytes wrong")
	}
	restart(t, f)
	var retry commitData
	decode(t, f.service.Call(f.ctx(t, "workspace_patch", "patch"), "workspace_patch", args), &retry)
	if patched.Receipt.ID != retry.Receipt.ID {
		t.Fatal("patch retry duplicated effect")
	}
	args.NewText = "changed"
	out := f.service.Call(f.ctx(t, "workspace_patch", "patch"), "workspace_patch", args)
	if !out.Failed || !strings.Contains(out.Content, "conflict") {
		t.Fatal("patch key accepted different text")
	}
	args.Revision = string(patched.File.Revision)
	args.OldText = "absent"
	out = f.service.Call(f.ctx(t, "workspace_patch", "missing"), "workspace_patch", args)
	if !out.Failed {
		t.Fatal("missing patch match accepted")
	}
	if body(t, f, patched.File) != "new synthetic text" {
		t.Fatal("failed patch changed prior bytes")
	}
}

func restart(t *testing.T, f *fixture) {
	t.Helper()
	settings := f.service.Settings
	must(t, f.db.Close())
	var err error
	f.db, err = runtimedb.Open(f.dbPath)
	must(t, err)
	_, err = f.db.AcquireCoordinator("synthetic-writer")
	must(t, err)
	f.service = New(f.db, settings)
}
func TestCommitRechecksRevocationAtExecutionFence(t *testing.T) {
	f := setup(t)
	id := f.begin(t, "begin", "report.md")
	chunk(t, f, id, 0, "complete synthetic body", "chunk")
	original := f.service.coordinator.Authorize
	checks := 0
	f.service.coordinator.Authorize = func(ctx context.Context, s runtimedb.WorkspaceWriteSession) error {
		checks++
		if checks == 3 {
			must(t, f.db.UnassignTool(f.agent, "workspace_write_commit", "root-admin"))
		}
		return original(ctx, s)
	}
	out := f.service.Call(f.ctx(t, "workspace_write_commit", "commit"), "workspace_write_commit", butler.WorkspaceWriteCall{WriteID: id})
	if !out.Rejected || checks < 3 {
		t.Fatalf("revoked fence published: %+v checks=%d", out, checks)
	}
	session, err := f.db.WorkspaceWriteSession("workspace", id)
	must(t, err)
	if session.State != "open" {
		t.Fatal("fence denial sealed staging")
	}
}

func TestPatchOverlappingMatchesRefuseWithoutChangingBytes(t *testing.T) {
	f := setup(t)
	id := f.begin(t, "begin", "report.md")
	chunk(t, f, id, 0, "aaa", "chunk")
	original := commit(t, f, id, "commit")
	out := f.service.Call(f.ctx(t, "workspace_patch", "patch"), "workspace_patch", butler.WorkspaceWriteCall{ArtifactID: string(original.File.ArtifactID), Revision: string(original.File.Revision), OldText: "aa", NewText: "b"})
	if !out.Failed {
		t.Fatal("overlapping multiple matches accepted")
	}
	if body(t, f, original.File) != "aaa" {
		t.Fatal("refused patch changed bytes")
	}
}
