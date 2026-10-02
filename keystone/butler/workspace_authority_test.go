package butler

import (
	"context"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
)

// Pair with butlerstore.TestWorkspaceKindOnlyGatewayQueryResolvesPrivateScope:
// this exercises the actual manifest and dispatch builders; that test exercises
// their kind-only query against durable ownership and current access controls.
func TestWorkspaceCanonicalGatewaysCarryTrustedTaskScope(t *testing.T) {
	for _, family := range []bool{false, true} {
		name := "owner"
		if family {
			name = "member"
		}
		t.Run(name, func(t *testing.T) {
			allowed, calls, effects := true, 0, 0
			class, connection := string(AgentClassOwner), "console"
			if family {
				class, connection = string(AgentClassMember), "household_portal"
			}
			a := resourceAwareAuthorizer{rules: ResourceRules{"workspace": func(q ToolAuthorityQuery) (ToolDecision, error) {
				calls++
				if q.Caller != CallerPersonFacing || q.Person != "fixture-person" || q.TaskID != "bt-1" || q.Connection != connection || q.AgentClass != class || q.Resource.Kind != "workspace" || q.Resource.ID != "" {
					t.Fatalf("gateway lost trusted scope or inferred an input selector: %+v", q)
				}
				return ToolDecision{Allowed: allowed, Reason: "workspace access refused"}, nil
			}}}
			spec := provider.Tool{Name: "workspace_read", Description: "fixture read"}
			var offered func(bool) []provider.Tool
			var dispatch func(context.Context, string, map[string]any) (ToolOutcome, error)
			if family {
				// Use a wired fixture name; resource declaration is test-only.
				spec.Name = "get_household_events"
				gw := NewHouseholdGateway("fixture-person", HouseholdSeams{Events: func(context.Context, HouseholdLookup) ([]HouseholdFact, error) { return nil, nil }})
				gw.tools[spec.Name] = householdEntry{Spec: spec, Reaches: "workspace", Handler: func(context.Context, *HouseholdGateway, map[string]any) (ToolOutcome, error) {
					effects++
					return ToolOutcome{Content: "fixture result"}, nil
				}}
				gw.SetAuthorizer(a)
				gw.SetCaller(CallerPersonFacing, "fixture-person", connection)
				gw.SessionTask = "bt-1"
				offered, dispatch = gw.OfferedSpecs, gw.Dispatch
			} else {
				gw := NewToolGateway(nil, nil)
				gw.tools[spec.Name] = toolEntry{Spec: spec, Reaches: "workspace", Handler: func(context.Context, *ToolGateway, map[string]any) (ToolOutcome, error) {
					effects++
					return ToolOutcome{Content: "fixture result"}, nil
				}}
				gw.SetAuthorizer(a)
				gw.SetCaller(CallerPersonFacing, "fixture-person", connection)
				gw.SessionTask = "bt-1"
				offered, dispatch = gw.OfferedSpecs, gw.Dispatch
			}
			for _, admitted := range []bool{true, false} {
				allowed = admitted
				found := false
				for _, s := range offered(false) {
					if s.Name == spec.Name {
						found = true
					}
				}
				if found != admitted {
					t.Fatalf("manifest offered=%v, admitted=%v", found, admitted)
				}
				out, err := dispatch(context.Background(), spec.Name, map[string]any{"workspace_id": "foreign-workspace", "task_id": "bt-999", "person": "foreign-person"})
				if err != nil || out.Rejected == admitted {
					t.Fatalf("dispatch admitted=%v: %+v %v", admitted, out, err)
				}
			}
			if calls < 4 || effects != 1 {
				t.Fatalf("resource checks=%d, handler executions=%d", calls, effects)
			}
		})
	}
}
