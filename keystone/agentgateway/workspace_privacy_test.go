package agentgateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestWorkspaceTaintBlocksGatewayLedgerReuseAndManifest(t *testing.T) {
	f := newFixture(t)
	taskID, h := f.openTask(t, "alfred", "mika")
	task, err := f.db.GetButlerTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	person, err := f.db.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := f.db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	w := runtimedb.Workspace{ID: "workspace", PersonID: person.PersonID, AgentID: agent.Seq}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.db.CreateWorkspace(w))
	must(f.db.CreateWorkspaceArtifact(runtimedb.WorkspaceArtifact{ID: "artifact", WorkspaceID: w.ID, Path: "report.txt"}))
	must(f.db.CreateWorkspaceRevision(runtimedb.WorkspaceRevision{ID: "revision", ArtifactID: "artifact", TaskID: task.Seq, Hash: strings.Repeat("a", 64), SizeBytes: 1, MediaType: "text/plain", Privacy: "local-only"}))
	must(f.db.ReserveWorkspaceBytes(runtimedb.WorkspaceReservation{ID: "reservation", WorkspaceID: w.ID, TaskID: task.Seq, SizeBytes: 1, Purpose: "publication"}))
	must(f.db.CreateWorkspacePublication(runtimedb.WorkspacePublication{ID: "publication", WorkspaceID: w.ID, ArtifactID: "artifact", RevisionID: "revision", TaskID: task.Seq, ReservationID: "reservation", EffectKey: "effect"}))
	must(f.db.PrepareWorkspacePublication(w.ID, "publication"))
	must(f.db.MarkWorkspaceBytesPublished(w.ID, "publication"))
	must(f.db.CommitWorkspacePublication(w.ID, "publication", "receipt"))
	setProcessingClass(t, f, "alfred", agentapi.ProcessingLocalOnly)
	req := agentapi.InvokeRequest{Handle: h, Tool: "get_system_overview", Args: json.RawMessage(`{}`)}
	first, err := f.g.Invoke(context.Background(), req)
	if err != nil || first.Outcome != agentapi.OutcomeOK {
		t.Fatalf("positive invocation: %+v %v", first, err)
	}
	ctx, p, err := f.db.BeginPrivacyTurn(context.Background(), runtimedb.ModelLocal, runtimedb.MemoryDestination{BuiltIn: true}, "")
	must(err)
	must(p.SetTask(taskID))
	_, err = f.db.ReadWorkspaceRevision(ctx, f.db.BindWorkspaceAccess("mika", "alfred", taskID, "console"), w.ID, "artifact", "revision")
	must(err)
	// No fresh read on the new call: only the persisted workspace edge connects
	// this task to the restriction. Cached results cannot bypass that context.
	setProcessingClass(t, f, "alfred", agentapi.ProcessingCloud)
	result, err := f.g.Invoke(context.Background(), req)
	if err != nil || result.Outcome != agentapi.OutcomeDenied || result.ServedFromLedger || result.Content != "" {
		t.Fatalf("cached result bypassed workspace taint: %+v %v", result, err)
	}
	if count := f.surface("alfred").count(req.Tool); count != 1 {
		t.Fatalf("denied retry dispatched %d times", count)
	}
	if _, err = f.g.Manifest(context.Background(), h); err == nil {
		t.Fatal("manifest lost persisted workspace source")
	}
}
