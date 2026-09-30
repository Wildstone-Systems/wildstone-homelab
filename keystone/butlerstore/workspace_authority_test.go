package butlerstore

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestWorkspaceRuleCannotBeOverriddenOrBorrowRoutineAuthority(t *testing.T) {
	allow := func(butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
		return butler.ToolDecision{Allowed: true}, nil
	}
	a := &Authority{Resources: butler.ResourceRules{"workspace": allow}, RoutineResources: butler.ResourceRules{"workspace": allow}}
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
