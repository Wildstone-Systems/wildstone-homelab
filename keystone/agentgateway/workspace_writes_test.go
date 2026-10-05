package agentgateway

import (
	"context"
	"encoding/json"
	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace/writes"
	"strings"
	"testing"
)

func TestWorkspaceWriteInvokeTrustedKeysAndTypedErrors(t *testing.T) {
	f := newFixture(t)
	_, h := f.openTask(t, "alfred", "mika")
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
	if err = f.db.GovernAgent(agent.Seq, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err = f.db.AssignTool(agent.Seq, "workspace_write_begin", "root-admin"); err != nil {
		t.Fatal(err)
	}
	settings := workspace.Settings{Root: t.TempDir(), QuotaBytes: 1 << 30, MaxFileBytes: 8192, ScratchRetentionDays: 7, TrashRetentionDays: 30, Revision: "config"}
	service := writes.New(f.db, func() (workspace.Settings, error) { return settings, nil })
	f.g.Gateways = butlerlive.GatewayFunc(func(runtimedb.Butler, butlerlive.Turn) (butler.Toolset, error) {
		g := butler.NewToolGateway(f.store, nil)
		g.WorkspaceWrites = service.Call
		return g, nil
	})
	req := agentapi.InvokeRequest{Handle: h, Tool: "workspace_write_begin", Args: json.RawMessage(`{"scope":"private","path":"report.md","expectedRevision":null}`)}
	out, err := f.g.Invoke(context.Background(), req)
	if err != nil || out.Outcome != agentapi.OutcomeDenied {
		t.Fatalf("missing trusted key accepted: %+v %v", out, err)
	}
	req.IdempotencyKey = "synthetic-begin"
	first, err := f.g.Invoke(context.Background(), req)
	if err != nil || first.Outcome != agentapi.OutcomeOK {
		t.Fatalf("canonical begin failed: %+v %v", first, err)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(first.Content, redact.UntrustedOpenTag+"\n"), "\n"+redact.UntrustedCloseTag)
	var data struct {
		Data struct {
			ID string `json:"writeId"`
		} `json:"data"`
	}
	if err = json.Unmarshal([]byte(inner), &data); err != nil || len(data.Data.ID) != 32 {
		t.Fatal("server selector failed round trip")
	}
	retry, err := f.g.Invoke(context.Background(), req)
	if err != nil || retry.Content != first.Content || retry.ServedFromLedger {
		t.Fatal("begin retry bypassed durable authority")
	}
	req.Args = json.RawMessage(`{"scope":"private","path":"changed.md","expectedRevision":null}`)
	conflict, err := f.g.Invoke(context.Background(), req)
	if err != nil || conflict.Outcome != agentapi.OutcomeError {
		t.Fatalf("changed args not typed error: %+v %v", conflict, err)
	}
	req.Args = json.RawMessage(`{"scope":"private","path":"report.md","expectedRevision":null,"idempotencyKey":"model-key"}`)
	bad, err := f.g.Invoke(context.Background(), req)
	if err != nil || bad.Outcome != agentapi.OutcomeError {
		t.Fatal("model JSON supplied a trusted key")
	}
}
