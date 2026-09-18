package butlerstore

// P7's evidence (ADR-W030): disable and revocation win over cached
// manifests and saved work, the household's structural ceiling cannot
// be widened, a missing tool can be requested but not used until
// someone enables it, and grants expire at task end, cancellation, or
// deadline.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

func authorityDB(t *testing.T) (*runtimedb.DB, *Authority) {
	t.Helper()
	db, err := runtimedb.Open(migrated.Path(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, &Authority{DB: db}
}

// A grant cannot conjure a tool the surface does not have. This is the
// household ceiling: the family gateway's map holds no operator tool, so
// there is nothing for a control or a grant to switch on.
func TestStructuralCeilingCannotBeWidened(t *testing.T) {
	db, auth := authorityDB(t)

	// Even with the switch explicitly ON and a live grant, a
	// structurally absent tool is refused.
	if err := db.SetToolControl(runtimedb.ProfileHousehold, "propose_change",
		runtimedb.ToolEnabled, "root-admin", "please"); err != nil {
		t.Fatal(err)
	}
	d, err := auth.AuthorizeTool(butler.ToolAuthorityQuery{
		Profile: runtimedb.ProfileHousehold, Tool: "propose_change", Structural: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("an explicitly enabled control widened the household's structural ceiling")
	}
	if !strings.Contains(d.Reason, "the tool is absent") {
		t.Fatalf("reason = %q; it should say the tool is absent, not that it is off", d.Reason)
	}
	// And it is requestable: asking is not having (A7).
	if !d.Requestable {
		t.Fatal("a structurally absent tool was not reported as something you could ask for")
	}

	// The gateway proves it end to end: the family surface does not
	// register the tool at all, so the call never reaches the resolver.
	seams := butler.HouseholdSeams{}
	gw := butler.NewHouseholdGateway("sam", seams)
	gw.Authorizer = auth
	if _, err := gw.Dispatch(context.Background(), "propose_change", nil); err == nil {
		t.Fatal("the family gateway dispatched an operator tool")
	}
}

// Disable wins over a cached manifest and over saved work: it is
// checked before grants, and it refuses at the dispatch boundary even
// when the model was offered the tool a minute ago.
func TestDisableWinsOverManifestsAndGrants(t *testing.T) {
	db, auth := authorityDB(t)
	// A real Statefile, because a gateway with no store reports its
	// Statefile-backed tools as unwired — which is a different reason
	// for "unavailable" than the one this test is about.
	store, err := statefile.New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatalf("statefile.New: %v", err)
	}
	gw := butler.NewToolGateway(store, nil)
	gw.Authorizer = auth

	// Offered while enabled.
	before := gw.Capabilities(false)
	if !toolAvailable(before, "get_system_overview") {
		t.Fatal("a permitted tool was not offered")
	}

	if err := db.SetToolControl(runtimedb.ProfileOperator, "get_system_overview",
		runtimedb.ToolDisabled, "root-admin", "noisy"); err != nil {
		t.Fatal(err)
	}
	// The manifest agrees with the enforcement point, so a description
	// and a refusal cannot disagree about the same tool.
	after := gw.Capabilities(false)
	if toolAvailable(after, "get_system_overview") {
		t.Fatal("the manifest still offers a disabled tool")
	}
	if before.Fingerprint() == after.Fingerprint() {
		t.Fatal("disabling a tool did not change the manifest fingerprint")
	}

	// And a model holding the OLD manifest is still refused.
	out, err := gw.Dispatch(context.Background(), "get_system_overview", nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !out.Rejected || !strings.Contains(out.Content, "turned that off") {
		t.Fatalf("a disabled tool ran or refused unclearly: %+v", out)
	}

	// A grant does not override a disable. The order of the checks is
	// the guarantee.
	id := newAuthTask(t, db)
	if _, err := db.GrantTool(runtimedb.ToolGrant{
		Scope: runtimedb.GrantScopeTask, ScopeID: id, Profile: runtimedb.ProfileOperator,
		Tool: "get_system_overview", GrantedBy: "root-admin",
	}, time.Hour); err != nil {
		t.Fatal(err)
	}
	d, err := auth.AuthorizeTool(butler.ToolAuthorityQuery{
		Profile: runtimedb.ProfileOperator, Tool: "get_system_overview",
		Structural: true, GrantRequired: true, TaskID: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("a grant overrode an administrator's disable")
	}
}

func toolAvailable(m butler.CapabilityManifest, name string) bool {
	for _, t := range m.Tools {
		if t.Name == name {
			return t.Available
		}
	}
	return false
}

func newAuthTask(t *testing.T, db *runtimedb.DB) string {
	t.Helper()
	id, err := db.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: runtimedb.ButlerSurfaceAdmin, Profile: runtimedb.ProfileOperator,
		Audience: runtimedb.ButlerAudiencePrivate, Objective: "work", CompletionCriteria: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A grant lives and dies with its work: at the task's end, at
// cancellation, and at its own deadline, whichever comes first.
func TestGrantsExpireWithTheirWork(t *testing.T) {
	query := func(id string) butler.ToolAuthorityQuery {
		return butler.ToolAuthorityQuery{
			Profile: runtimedb.ProfileOperator, Tool: "restricted_tool",
			Structural: true, GrantRequired: true, TaskID: id,
		}
	}

	t.Run("no grant means no", func(t *testing.T) {
		db, auth := authorityDB(t)
		id := newAuthTask(t, db)
		d, err := auth.AuthorizeTool(query(id))
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed || !d.Requestable {
			t.Fatalf("a tool needing a grant ran without one: %+v", d)
		}
	})

	t.Run("a live grant allows it", func(t *testing.T) {
		db, auth := authorityDB(t)
		id := newAuthTask(t, db)
		seq, err := db.GrantTool(runtimedb.ToolGrant{
			Scope: runtimedb.GrantScopeTask, ScopeID: id, Profile: runtimedb.ProfileOperator,
			Tool: "restricted_tool", GrantedBy: "root-admin", Reason: "one lookup",
		}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		d, err := auth.AuthorizeTool(query(id))
		if err != nil {
			t.Fatal(err)
		}
		if !d.Allowed || d.GrantSeq != seq {
			t.Fatalf("a live grant did not allow the call: %+v", d)
		}
	})

	t.Run("cancellation ends it", func(t *testing.T) {
		db, auth := authorityDB(t)
		id := newAuthTask(t, db)
		if _, err := db.GrantTool(runtimedb.ToolGrant{
			Scope: runtimedb.GrantScopeTask, ScopeID: id, Profile: runtimedb.ProfileOperator,
			Tool: "restricted_tool", GrantedBy: "root-admin",
		}, time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := db.TransitionButlerTask(id, runtimedb.ButlerTaskCancelled, "user cancelled", "mika"); err != nil {
			t.Fatal(err)
		}
		d, _ := auth.AuthorizeTool(query(id))
		if d.Allowed {
			t.Fatal("a grant survived its task's cancellation")
		}
	})

	t.Run("task completion ends it", func(t *testing.T) {
		db, auth := authorityDB(t)
		id := newAuthTask(t, db)
		if _, err := db.GrantTool(runtimedb.ToolGrant{
			Scope: runtimedb.GrantScopeTask, ScopeID: id, Profile: runtimedb.ProfileOperator,
			Tool: "restricted_tool", GrantedBy: "root-admin",
		}, time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := db.TransitionButlerTask(id, runtimedb.ButlerTaskRunning, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteButlerTask(id, []runtimedb.ButlerTaskEvidence{{
			Kind: runtimedb.ButlerEvidenceDelivery, Description: "answered",
			PermissionLabel: "operator", Audience: runtimedb.ButlerAudiencePrivate,
		}}, "done", "butler"); err != nil {
			t.Fatal(err)
		}
		d, _ := auth.AuthorizeTool(query(id))
		if d.Allowed {
			t.Fatal("a grant survived its task's completion")
		}
	})

	t.Run("the deadline ends it even while the task runs", func(t *testing.T) {
		db, auth := authorityDB(t)
		id := newAuthTask(t, db)
		if _, err := db.GrantTool(runtimedb.ToolGrant{
			Scope: runtimedb.GrantScopeTask, ScopeID: id, Profile: runtimedb.ProfileOperator,
			Tool: "restricted_tool", GrantedBy: "root-admin",
		}, time.Nanosecond); err != nil {
			t.Fatal(err)
		}
		d, _ := auth.AuthorizeTool(query(id))
		if d.Allowed {
			t.Fatal("an expired grant still allowed the call")
		}
	})

	t.Run("explicit revocation ends it", func(t *testing.T) {
		db, auth := authorityDB(t)
		id := newAuthTask(t, db)
		if _, err := db.GrantTool(runtimedb.ToolGrant{
			Scope: runtimedb.GrantScopeTask, ScopeID: id, Profile: runtimedb.ProfileOperator,
			Tool: "restricted_tool", GrantedBy: "root-admin",
		}, time.Hour); err != nil {
			t.Fatal(err)
		}
		n, err := db.RevokeGrantsForScope(runtimedb.GrantScopeTask, id, "root-admin")
		if err != nil || n != 1 {
			t.Fatalf("RevokeGrantsForScope = %d, %v", n, err)
		}
		d, _ := auth.AuthorizeTool(query(id))
		if d.Allowed {
			t.Fatal("a revoked grant still allowed the call")
		}
	})

	t.Run("a grant is bounded and needs real work", func(t *testing.T) {
		db, _ := authorityDB(t)
		if _, err := db.GrantTool(runtimedb.ToolGrant{
			Scope: runtimedb.GrantScopeTask, ScopeID: "bt-999", Profile: runtimedb.ProfileOperator,
			Tool: "restricted_tool", GrantedBy: "root-admin",
		}, time.Hour); err == nil {
			t.Fatal("a grant was issued against a task that does not exist")
		}
		id := newAuthTask(t, db)
		if _, err := db.GrantTool(runtimedb.ToolGrant{
			Scope: runtimedb.GrantScopeTask, ScopeID: id, Profile: runtimedb.ProfileOperator,
			Tool: "restricted_tool", GrantedBy: "root-admin",
		}, 72*time.Hour); err == nil {
			t.Fatal("an unbounded grant window was accepted")
		}
	})
}

// A missing tool can be requested, and the request grants nothing —
// approving it and enabling the tool are two separate acts.
func TestCapabilityRequestGrantsNothing(t *testing.T) {
	db, auth := authorityDB(t)

	seq, err := db.FileCapabilityRequest(runtimedb.CapabilityRequest{
		Kind: runtimedb.RequestSupportedTool, RequestedBy: "sam",
		Profile: runtimedb.ProfileHousehold, Tool: "get_calendar",
		Description: "so I can answer questions about what is on today",
	})
	if err != nil {
		t.Fatalf("FileCapabilityRequest: %v", err)
	}
	// A request needs someone it is for: the Butler asks on a person's
	// behalf and has no wishes of its own.
	if _, err := db.FileCapabilityRequest(runtimedb.CapabilityRequest{
		Kind: runtimedb.RequestSupportedTool, Profile: runtimedb.ProfileHousehold,
		Tool: "get_calendar", Description: "please",
	}); err == nil {
		t.Fatal("an unattributed capability request was accepted")
	}

	open, err := db.ListCapabilityRequests(runtimedb.RequestOpen)
	if err != nil || len(open) != 1 {
		t.Fatalf("open requests = %+v, %v", open, err)
	}

	// Approving does not enable it.
	if err := db.DecideCapabilityRequest(seq, runtimedb.RequestApproved, "root-admin", "good idea"); err != nil {
		t.Fatal(err)
	}
	d, err := auth.AuthorizeTool(butler.ToolAuthorityQuery{
		Profile: runtimedb.ProfileHousehold, Tool: "get_calendar", Structural: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("approving a request enabled the tool")
	}

	// Enabling is a separate, visible act — and even then it cannot
	// widen the structural ceiling, only switch on something the surface
	// already has.
	if err := db.SetToolControl(runtimedb.ProfileHousehold, "get_calendar",
		runtimedb.ToolEnabled, "root-admin", ""); err != nil {
		t.Fatal(err)
	}
	if d, _ := auth.AuthorizeTool(butler.ToolAuthorityQuery{
		Profile: runtimedb.ProfileHousehold, Tool: "get_calendar", Structural: false,
	}); d.Allowed {
		t.Fatal("enabling a control conjured a tool the surface does not have")
	}
	if d, _ := auth.AuthorizeTool(butler.ToolAuthorityQuery{
		Profile: runtimedb.ProfileHousehold, Tool: "get_calendar", Structural: true,
	}); !d.Allowed {
		t.Fatal("a tool that exists and was enabled is still refused")
	}
}

// A resolver that cannot answer has not said yes.
type brokenAuthorizer struct{}

func (brokenAuthorizer) AuthorizeTool(butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	return butler.ToolDecision{}, context.DeadlineExceeded
}

func TestAuthorityFailureRefusesTheDispatch(t *testing.T) {
	store, err := statefile.New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatal(err)
	}
	gw := butler.NewToolGateway(store, nil)
	gw.Authorizer = brokenAuthorizer{}
	out, derr := gw.Dispatch(context.Background(), "get_system_overview", nil)
	if derr != nil {
		t.Fatalf("Dispatch: %v", derr)
	}
	if !out.Rejected {
		t.Fatalf("a dispatch ran while authority could not be checked: %+v", out)
	}
}
