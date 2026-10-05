package writes

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/chickenrat17/wildstone-homelab/keystone/operation"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace"
	"testing"
)

var template = dbtemplate.New(func(p string) error {
	db, err := runtimedb.Open(p)
	if err != nil {
		return err
	}
	return db.Close()
})

type fixture struct {
	db           *runtimedb.DB
	service      *Service
	settings     workspace.Settings
	task, dbPath string
	agent        int64
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func setup(t *testing.T) *fixture { return setupClass(t, runtimedb.AgentClassOwner) }
func setupClass(t *testing.T, class string) *fixture {
	role := runtimedb.RoleOwner
	surface := runtimedb.ButlerSurfaceAdmin
	connection := "console"
	if class == runtimedb.AgentClassMember {
		role = runtimedb.RoleMember
		surface = runtimedb.ButlerSurfaceFamily
		connection = "household_portal"
	}
	t.Helper()
	f := &fixture{dbPath: template.Path(t)}
	var err error
	f.db, err = runtimedb.Open(f.dbPath)
	must(t, err)
	t.Cleanup(func() { f.db.Close() })
	_, err = f.db.AcquireCoordinator("synthetic-writer")
	must(t, err)
	must(t, f.db.RecordButlerMember("fixture-writer", role, "", ""))
	must(t, f.db.AdmitButlerMember("fixture-writer", "root-admin"))
	must(t, f.db.UpsertModel(runtimedb.Model{Name: "local", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "fixture", Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"))
	f.agent, err = f.db.CreateButler(runtimedb.Butler{Name: "fixture-agent", Directive: "fixture", Surface: surface, AgentClass: class, Model: "local"}, "root-admin")
	must(t, err)
	must(t, f.db.GrantButlerAccess("fixture-agent", "fixture-writer", "root-admin"))
	must(t, f.db.GovernAgent(f.agent, "root-admin"))
	for _, tool := range []string{"workspace_write_begin", "workspace_write_chunk", "workspace_write_commit", "workspace_patch"} {
		must(t, f.db.AssignTool(f.agent, tool, "root-admin"))
	}
	person, err := f.db.GetButlerMember("fixture-writer")
	must(t, err)
	must(t, f.db.CreateWorkspace(runtimedb.Workspace{ID: "workspace", PersonID: person.PersonID, AgentID: f.agent}))
	f.task, err = f.db.InsertButlerTask(runtimedb.ButlerTask{Kind: runtimedb.ButlerTaskConversation, Author: "fixture-writer", Owner: "fixture-writer", Surface: surface, AgentClass: class, Audience: runtimedb.ButlerAudiencePrivate, OriginConnection: connection, Objective: "synthetic report", ObjectiveKind: runtimedb.ObjectiveInformational})
	must(t, err)
	must(t, f.db.SetTaskButler(f.task, "fixture-agent"))
	f.settings = workspace.Settings{Root: t.TempDir(), QuotaBytes: 1 << 30, MaxFileBytes: 8192, ScratchRetentionDays: 7, TrashRetentionDays: 30, Revision: "config"}
	f.service = New(f.db, func() (workspace.Settings, error) { return f.settings, nil })
	return f
}
func (f *fixture) ctx(t *testing.T, tool, key string) context.Context {
	t.Helper()
	ctx, p, err := f.db.BeginPrivacyTurn(context.Background(), runtimedb.ModelLocal, runtimedb.MemoryDestination{BuiltIn: true}, "")
	must(t, err)
	must(t, p.SetTask(f.task))
	return operation.With(ctx, operation.Identity{TaskID: f.task, Tool: tool, IdempotencyKey: key})
}
func decode(t *testing.T, out butler.ToolOutcome, dest any) {
	t.Helper()
	if out.Failed || out.Rejected {
		t.Fatalf("unexpected refusal: %+v", out)
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	must(t, json.Unmarshal([]byte(out.Content), &env))
	must(t, json.Unmarshal(env.Data, dest))
}
func (f *fixture) begin(t *testing.T, key, path string) string {
	t.Helper()
	var data struct {
		WriteID string `json:"writeId"`
	}
	decode(t, f.service.Call(f.ctx(t, "workspace_write_begin", key), "workspace_write_begin", butler.WorkspaceWriteCall{Scope: "private", Path: path}), &data)
	return data.WriteID
}
func TestBeginReplaysAndCurrentAuthority(t *testing.T) {
	f := setup(t)
	id := f.begin(t, "begin", "report.md")
	if f.begin(t, "begin", "report.md") != id {
		t.Fatal("begin duplicate")
	}
	out := f.service.Call(f.ctx(t, "workspace_write_begin", "begin"), "workspace_write_begin", butler.WorkspaceWriteCall{Scope: "private", Path: "other.md"})
	if !out.Failed {
		t.Fatal("changed begin key accepted")
	}
	if out = f.service.Call(f.ctx(t, "workspace_write_begin", "invalid"), "workspace_write_begin", butler.WorkspaceWriteCall{Scope: "private", Path: "script.exe"}); !out.Failed {
		t.Fatal("invalid format accepted")
	}
	if out = f.service.Call(f.ctx(t, "workspace_write_begin", ""), "workspace_write_begin", butler.WorkspaceWriteCall{Scope: "private", Path: "missing-key.md"}); !out.Rejected {
		t.Fatal("missing trusted key accepted")
	}
	must(t, f.db.RecordModelReadiness("local", runtimedb.ModelQAOnly, "synthetic QA mode"))
	if out = f.service.Call(f.ctx(t, "workspace_write_begin", "qa"), "workspace_write_begin", butler.WorkspaceWriteCall{Scope: "private", Path: "qa.md"}); !out.Rejected {
		t.Fatal("QA staged write accepted")
	}
	must(t, f.db.RecordModelReadiness("local", runtimedb.ModelReady, "synthetic ready mode"))
	must(t, f.db.RevokeButlerAccess("fixture-agent", "fixture-writer", "root-admin"))
	if out = f.service.Call(f.ctx(t, "workspace_write_commit", "commit"), "workspace_write_commit", butler.WorkspaceWriteCall{WriteID: id}); !out.Rejected {
		t.Fatal("revoked commit accepted")
	}
}
func TestCancelledTaskCannotResumeStaging(t *testing.T) {
	f := setup(t)
	id := f.begin(t, "begin", "report.md")
	must(t, f.db.TransitionButlerTask(f.task, runtimedb.ButlerTaskCancelled, "synthetic cancellation", "fixture-writer"))
	out := f.service.Call(f.ctx(t, "workspace_write_commit", "commit"), "workspace_write_commit", butler.WorkspaceWriteCall{WriteID: id})
	if !out.Rejected {
		t.Fatalf("cancelled commit accepted: %+v", out)
	}
	s, err := f.db.WorkspaceWriteSession("workspace", id)
	must(t, err)
	if s.State != "open" {
		t.Fatal("cancelled write changed journal")
	}
}

func TestWriteGrantRevocationForOwnerAndMember(t *testing.T) {
	for _, class := range []string{runtimedb.AgentClassOwner, runtimedb.AgentClassMember} {
		t.Run(class, func(t *testing.T) {
			f := setupClass(t, class)
			f.begin(t, "begin", "report.md")
			must(t, f.db.UnassignTool(f.agent, "workspace_write_begin", "root-admin"))
			out := f.service.Call(f.ctx(t, "workspace_write_begin", "revoked"), "workspace_write_begin", butler.WorkspaceWriteCall{Scope: "private", Path: "other.md"})
			if !out.Rejected {
				t.Fatalf("revoked class grant accepted: %+v", out)
			}
		})
	}
}

func TestWriteFenceAndSealInheritSourcesReadAfterBegin(t *testing.T) {
	f := setup(t)
	id := f.begin(t, "begin", "report.md")
	origin, err := f.db.InsertButlerTask(runtimedb.ButlerTask{Kind: runtimedb.ButlerTaskConversation, Author: "fixture-writer", Owner: "fixture-writer", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Audience: runtimedb.ButlerAudiencePrivate, OriginConnection: "console", Objective: "synthetic protected source"})
	must(t, err)
	must(t, f.db.SetTaskButler(origin, "fixture-agent"))
	task, err := f.db.GetButlerTask(origin)
	must(t, err)
	revision := fmt.Sprintf("%032x", 101)
	hash := fmt.Sprintf("%x", sha256.Sum256(nil))
	must(t, f.db.CreateWorkspaceArtifact(runtimedb.WorkspaceArtifact{ID: "protected-artifact", WorkspaceID: "workspace", Path: "protected.txt"}))
	must(t, f.db.CreateWorkspaceRevision(runtimedb.WorkspaceRevision{ID: revision, ArtifactID: "protected-artifact", TaskID: task.Seq, Hash: hash, SizeBytes: 0, MediaType: "text/plain", Privacy: "local-only"}))
	must(t, f.db.ReserveWorkspaceBytes(runtimedb.WorkspaceReservation{ID: "protected-reservation", WorkspaceID: "workspace", TaskID: task.Seq, SizeBytes: 0, Purpose: "publication"}))
	must(t, f.db.CreateWorkspacePublication(runtimedb.WorkspacePublication{ID: "protected-publication", WorkspaceID: "workspace", ArtifactID: "protected-artifact", RevisionID: revision, TaskID: task.Seq, ReservationID: "protected-reservation", EffectKey: "protected-effect"}))
	must(t, f.db.PrepareWorkspacePublication("workspace", "protected-publication"))
	must(t, f.db.MarkWorkspaceBytesPublished("workspace", "protected-publication"))
	must(t, f.db.CommitWorkspacePublication("workspace", "protected-publication", "protected-receipt"))
	access := f.db.BindWorkspaceAccess("fixture-writer", "fixture-agent", f.task, "console")
	_, err = f.db.ReadWorkspaceRevision(f.ctx(t, "workspace_write_commit", "read"), access, "workspace", "protected-artifact", revision)
	must(t, err)
	cloud, p, err := f.db.BeginPrivacyTurn(context.Background(), runtimedb.ModelCloud, runtimedb.MemoryDestination{BuiltIn: true}, "")
	must(t, err)
	must(t, p.SetTask(f.task))
	cloud = operation.With(cloud, operation.Identity{TaskID: f.task, Tool: "workspace_write_commit", IdempotencyKey: "cloud-commit"})
	out := f.service.Call(cloud, "workspace_write_commit", butler.WorkspaceWriteCall{WriteID: id})
	if !out.Rejected {
		t.Fatal("changed processing destination lost inherited source restriction")
	}
	session, err := f.db.WorkspaceWriteSession("workspace", id)
	must(t, err)
	if session.Privacy != "normal" {
		t.Fatal("fixture did not begin before source read")
	}
	_, err = f.db.CheckWorkspaceWriteSession(f.ctx(t, "workspace_write_commit", "local-commit"), access, session, "workspace_write_commit")
	must(t, err)
	// Isolate the seal's immutable policy capture without claiming published
	// filesystem evidence on this platform; confined-byte flows run on Linux.
	_, err = f.db.SealWorkspaceWrite(session, hash, "text/markdown", "local-commit", 0, 0)
	must(t, err)
	object, err := f.db.WorkspaceWriteObject("workspace", id)
	must(t, err)
	if object.Privacy != "local-only" {
		t.Fatal("seal discarded sources read after begin")
	}
}
