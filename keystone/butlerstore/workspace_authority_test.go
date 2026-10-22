package butlerstore

import (
	"errors"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func TestWorkspaceRuleCannotBeOverriddenOrBorrowRoutineAuthority(t *testing.T) {
	allow := func(butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
		return butler.ToolDecision{Allowed: true}, nil
	}
	a := &Authority{PersonAuthority: func(string) error { return nil }, Resources: butler.ResourceRules{"workspace": allow}, RoutineResources: butler.ResourceRules{"workspace": allow}}
	for _, caller := range []butler.CallerKind{butler.CallerRoutine, butler.CallerAdministrative, butler.CallerPersonFacing, ""} {
		q := butler.ToolAuthorityQuery{Caller: caller, Resource: butler.ResourceRef{Kind: "workspace", ID: "private-workspace"}}
		for _, check := range []func(butler.ToolAuthorityQuery) (butler.ToolDecision, error){a.AdmitResource, a.AuthorizeTool} {
			d, err := check(q)
			if err != nil || d.Allowed || d.Reason != runtimedb.ErrWorkspaceDenied.Error() {
				t.Fatalf("caller %q: %+v %v", caller, d, err)
			}
		}
	}
}

func TestWorkspaceKindOnlyGatewayQueryResolvesPrivateScope(t *testing.T) {
	db, a := authorityDB(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.SetButlerMemberRole("mika", runtimedb.RoleOwner, "root-admin"))
	must(db.UpsertModel(runtimedb.Model{Name: "workspace-model", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "fixture", Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"))
	agent, err := db.CreateButler(runtimedb.Butler{Name: "workspace-agent", Directive: "help", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Model: "workspace-model"}, "root-admin")
	must(err)
	a.Butler = "workspace-agent"
	must(db.GrantButlerAccess(a.Butler, "mika", "root-admin"))
	person, err := db.GetButlerMember("mika")
	must(err)
	must(db.CreateWorkspace(runtimedb.Workspace{ID: "private-workspace", PersonID: person.PersonID, AgentID: agent}))
	id, err := db.InsertButlerTask(runtimedb.ButlerTask{Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Audience: runtimedb.ButlerAudiencePrivate, OriginConnection: "console", Objective: "read own workspace"})
	must(err)
	must(db.SetTaskButler(id, a.Butler))
	// ToolGateway.query and HouseholdGateway.checkTool supply Reaches as a kind
	// only, for both offered manifests and dispatch. No model-supplied ID exists.
	q := butler.ToolAuthorityQuery{Caller: butler.CallerPersonFacing, Person: "mika", Connection: "console", TaskID: id, AgentClass: runtimedb.AgentClassOwner, Tool: "get_system_overview", Structural: true, Resource: butler.ResourceRef{Kind: "workspace"}}
	for _, check := range []func(butler.ToolAuthorityQuery) (butler.ToolDecision, error){a.AdmitResource, a.AuthorizeTool} {
		d, err := check(q)
		if err != nil || !d.Allowed {
			t.Fatalf("kind-only gateway query denied: %+v %v", d, err)
		}
	}
	must(db.SetToolControl(runtimedb.AgentClassOwner, q.Tool, runtimedb.ToolDisabled, "root-admin", "test"))
	if d, err := a.AuthorizeTool(q); err != nil || d.Allowed {
		t.Fatalf("workspace ownership bypassed tool control: %+v %v", d, err)
	}
	q.Resource.ID = "another-workspace"
	if d, err := a.AdmitResource(q); err != nil || d.Allowed {
		t.Fatalf("explicit foreign selector admitted: %+v %v", d, err)
	}
	q.Resource.ID = ""
	must(db.RevokeButlerAccess(a.Butler, "mika", "root-admin"))
	if d, err := a.AdmitResource(q); err != nil || d.Allowed {
		t.Fatalf("normalization resurrected revoked access: %+v %v", d, err)
	}
}

// Even a direct resolver caller must pass Person admission before any
// workspace lookup. A closed database makes an out-of-order lookup observable.
func TestWorkspaceAuthorizeToolChecksPersonBeforeResource(t *testing.T) {
	for _, id := range []string{"", "private-workspace"} {
		t.Run("selector="+id, func(t *testing.T) {
			db, a := authorityDB(t)
			a.Butler = "workspace-agent"
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			q := butler.ToolAuthorityQuery{Caller: butler.CallerPersonFacing, Person: "mika", TaskID: "bt-1", Connection: "console", Tool: "workspace_read", Resource: butler.ResourceRef{Kind: "workspace", ID: id}}
			var absent *Authority
			if got, err := absent.AuthorizeTool(q); err != nil || got.Allowed {
				t.Fatalf("absent admission allowed workspace: %+v %v", got, err)
			}
			calls := 0
			a.PersonAuthority = func(string) error { calls++; return errors.New("person is not admitted") }
			want, _ := a.AdmitPerson(q)
			calls = 0
			got, err := a.AuthorizeTool(q)
			if err != nil || got.Allowed || got.Reason != want.Reason || calls != 1 {
				t.Fatalf("workspace consulted before Person: %+v %v, admission calls=%d", got, err, calls)
			}
			a.PersonAuthority = nil
			want, _ = a.AdmitPerson(q)
			if got, err = a.AuthorizeTool(q); err != nil || got.Allowed || got.Reason != want.Reason {
				t.Fatalf("missing admission consulted workspace: %+v %v", got, err)
			}
			a.PersonAuthority = func(string) error { return nil }
			if got, err = a.AuthorizeTool(q); err == nil || got.Allowed {
				t.Fatalf("admitted Person bypassed resource lookup: %+v %v", got, err)
			}
		})
	}
}
