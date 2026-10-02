package agentgateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace/reads"
)

func TestWorkspaceCanonicalInvokeMapsTypedFailuresAndRechecksGrants(t *testing.T) {
	f := newFixture(t)
	_, handle := f.openTask(t, "alfred", "mika")
	person, err := f.db.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := f.db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.CreateWorkspace(runtimedb.Workspace{ID: "workspace", PersonID: person.PersonID, AgentID: agent.Seq}); err != nil {
		t.Fatal(err)
	}
	settings := workspace.Settings{Root: t.TempDir(), QuotaBytes: 1 << 30, MaxFileBytes: 10 << 20, ScratchRetentionDays: 7, TrashRetentionDays: 30, Revision: "config"}
	service := &reads.Service{DB: f.db, Settings: func() (workspace.Settings, error) { return settings, nil }}
	calls := 0
	f.g.Gateways = butlerlive.GatewayFunc(func(b runtimedb.Butler, turn butlerlive.Turn) (butler.Toolset, error) {
		gw := butler.NewToolGateway(f.store, nil)
		gw.WorkspaceReads = func(ctx context.Context, tool string, call butler.WorkspaceReadCall) butler.ToolOutcome {
			calls++
			return service.Call(ctx, tool, call)
		}
		return gw, nil
	})
	if err = f.db.GovernAgent(agent.Seq, "root-admin"); err != nil {
		t.Fatal(err)
	}
	request := agentapi.InvokeRequest{Handle: handle, Tool: "workspace_list", Args: json.RawMessage(`{"scope":"private"}`)}
	denied, err := f.g.Invoke(context.Background(), request)
	if err != nil || denied.Outcome != agentapi.OutcomeDenied || calls != 0 {
		t.Fatalf("unassigned read reached seam: %+v %v calls=%d", denied, err, calls)
	}
	if err = f.db.AssignTool(agent.Seq, "workspace_list", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err = f.db.RecordModelReadiness("kitchen-box", runtimedb.ModelQAOnly, "synthetic QA fixture"); err != nil {
		t.Fatal(err)
	}
	manifest, err := f.g.Manifest(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, spec := range manifest.Tools {
		if spec.Name == "workspace_list" {
			found = true
			if !spec.ReadOnlyHint {
				t.Fatal("workspace read lacks QA hint")
			}
		}
	}
	if !found {
		t.Fatal("QA manifest omitted assigned read")
	}
	positive, err := f.g.Invoke(context.Background(), request)
	if err != nil || positive.Outcome != agentapi.OutcomeOK || !strings.Contains(positive.Content, `"items":[]`) {
		t.Fatalf("QA read failed: %+v %v", positive, err)
	}
	for _, token := range []string{"tampered-token", "expired-token"} {
		request.Args = json.RawMessage(`{"scope":"private","cursor":"` + token + `"}`)
		out, err := f.g.Invoke(context.Background(), request)
		if err != nil || out.Outcome != agentapi.OutcomeError || out.ServedFromLedger || !strings.Contains(out.Reason, "stale_cursor") {
			t.Fatalf("invalid cursor became success: %+v %v", out, err)
		}
	}
	before := calls
	if err = f.db.RevokeButlerAccess("alfred", "mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	out, err := f.g.Invoke(context.Background(), request)
	if err != nil || out.Outcome != agentapi.OutcomeDenied || calls != before {
		t.Fatalf("revoked cursor reached seam: %+v %v", out, err)
	}
}

func TestTypedWorkspaceFailurePreservesExistingOutcomeMappings(t *testing.T) {
	for _, tc := range []struct {
		out  butler.ToolOutcome
		want agentapi.Outcome
	}{
		{butler.ToolOutcome{Content: "ordinary successful result"}, agentapi.OutcomeOK},
		{butler.ToolOutcome{Content: "ordinary handler error text"}, agentapi.OutcomeOK},
		{butler.ToolOutcome{Rejected: true, Content: "ordinary refusal"}, agentapi.OutcomeDenied},
		{butler.ToolOutcome{Failed: true, Content: "stale_cursor: unavailable"}, agentapi.OutcomeError},
	} {
		got := resultFor("bt-1", 1, "fixture", tc.out)
		if got.Outcome != tc.want || got.Content != tc.out.Content {
			t.Fatalf("outcome mapping changed: %+v", got)
		}
	}
}

func TestWorkspaceMemberPortalQARead(t *testing.T) {
	f := newFixture(t)
	b, err := f.db.GetButler("joan")
	if err != nil {
		t.Fatal(err)
	}
	person, err := f.db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.db.InsertButlerTask(runtimedb.ButlerTask{Kind: runtimedb.ButlerTaskConversation, Author: "sam", Owner: "sam", Surface: b.Surface, AgentClass: b.AgentClass, Audience: runtimedb.ButlerAudiencePrivate, OriginConnection: "household_portal", Objective: "synthetic workspace read", ObjectiveKind: runtimedb.ObjectiveInformational})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.SetTaskButler(id, "joan"); err != nil {
		t.Fatal(err)
	}
	h, err := f.ledger.IssueHandle(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.CreateWorkspace(runtimedb.Workspace{ID: "member-workspace", PersonID: person.PersonID, AgentID: b.Seq}); err != nil {
		t.Fatal(err)
	}
	settings := workspace.Settings{Root: t.TempDir(), QuotaBytes: 1 << 30, MaxFileBytes: 10 << 20, ScratchRetentionDays: 7, TrashRetentionDays: 30, Revision: "config"}
	service := &reads.Service{DB: f.db, Settings: func() (workspace.Settings, error) { return settings, nil }}
	f.g.Gateways = butlerlive.GatewayFunc(func(_ runtimedb.Butler, turn butlerlive.Turn) (butler.Toolset, error) {
		return butler.NewHouseholdGateway("sam", butler.HouseholdSeams{WorkspaceReads: service.Call}), nil
	})
	if err = f.db.GovernAgent(b.Seq, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err = f.db.AssignTool(b.Seq, "workspace_list", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err = f.db.RecordModelReadiness(b.Model, runtimedb.ModelQAOnly, "synthetic QA fixture"); err != nil {
		t.Fatal(err)
	}
	manifest, err := f.g.Manifest(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, spec := range manifest.Tools {
		if spec.Name == "workspace_list" {
			found = true
		}
	}
	if !found {
		t.Fatal("member QA manifest omitted assigned scoped read")
	}
	out, err := f.g.Invoke(context.Background(), agentapi.InvokeRequest{Handle: h, Tool: "workspace_list", Args: json.RawMessage(`{"scope":"private"}`)})
	if err != nil || out.Outcome != agentapi.OutcomeOK {
		t.Fatalf("member portal read failed: %+v %v", out, err)
	}
}
