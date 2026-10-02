package butler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestWorkspaceReadToolsUseCanonicalAuthorityAndQA(t *testing.T) {
	for _, family := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner", true: "member"}[family], func(t *testing.T) {
			allowed, calls := true, 0
			seam := func(context.Context, string, WorkspaceReadCall) ToolOutcome {
				calls++
				return ToolOutcome{Content: `{"version":1,"data":{"items":[]}}`}
			}
			authority := resourceAwareAuthorizer{rules: ResourceRules{"workspace": func(q ToolAuthorityQuery) (ToolDecision, error) {
				if !q.OptIn || q.Resource.Kind != "workspace" || q.Person != "fixture-person" || q.TaskID != "bt-1" {
					t.Fatalf("lost workspace authority: %+v", q)
				}
				return ToolDecision{Allowed: allowed, Reason: "fixture refusal"}, nil
			}}}
			var gw Toolset
			if family {
				g := NewHouseholdGateway("fixture-person", HouseholdSeams{WorkspaceReads: seam})
				g.SetAuthorizer(authority)
				g.SetCaller(CallerPersonFacing, "fixture-person", "household_portal")
				g.SetSessionTask("bt-1")
				gw = g
			} else {
				g := NewToolGateway(nil, nil)
				g.WorkspaceReads = seam
				g.SetAuthorizer(authority)
				g.SetCaller(CallerPersonFacing, "fixture-person", "console")
				g.SetSessionTask("bt-1")
				gw = g
			}
			for _, spec := range gw.OfferedSpecs(true) {
				if strings.HasPrefix(spec.Name, "workspace_") {
					if spec.InputSchema["additionalProperties"] != false {
						t.Fatal("schema permits identity injection")
					}
				}
			}
			for _, spec := range workspaceReadSpecs {
				if ok, _ := gw.Permits(spec.Name, true); !ok {
					t.Fatalf("QA withheld %s", spec.Name)
				}
				out, err := gw.Execute(context.Background(), spec.Name, map[string]any{"scope": "private", "query": "fixture", "artifactId": "file", "revision": "revision"}, true)
				if err != nil || !out.Failed {
					t.Fatalf("unknown properties were accepted: %+v %v", out, err)
				}
			}
			out, err := gw.Execute(context.Background(), "workspace_list", map[string]any{"scope": "private"}, true)
			if err != nil || out.Failed || out.Rejected || !strings.Contains(out.Content, "<untrusted_data") {
				t.Fatalf("QA read failed: %+v %v", out, err)
			}
			if calls != 1 {
				t.Fatal("invalid inputs reached seam")
			}
			out, err = gw.Execute(context.Background(), "workspace_list", map[string]any{"scope": "shared"}, true)
			if err != nil || !out.Failed || calls != 1 {
				t.Fatal("unsupported shared scope reached seam")
			}
			for _, spec := range workspaceReadSpecs {
				if spec.Name == "workspace_read" {
					continue
				}
				properties := spec.InputSchema["properties"].(map[string]any)
				scopes := properties["scope"].(map[string]any)["enum"].([]string)
				if len(scopes) != 1 || scopes[0] != "private" {
					t.Fatal("schema advertises unsupported scope")
				}
			}
			allowed = false
			if ok, _ := gw.Permits("workspace_list", true); ok {
				t.Fatal("revoked read still offered")
			}
			out, err = gw.Execute(context.Background(), "workspace_list", map[string]any{"scope": "private"}, true)
			if err != nil || !out.Rejected || calls != 1 {
				t.Fatal("denied read reached seam")
			}
		})
	}
}

func TestWorkspaceStructuredOutputIsNeverTruncated(t *testing.T) {
	gw := NewToolGateway(nil, nil)
	gw.WorkspaceReads = func(context.Context, string, WorkspaceReadCall) ToolOutcome {
		data, _ := json.Marshal(map[string]any{"version": 1, "data": map[string]any{"text": strings.Repeat("synthetic text ", 80)}})
		return ToolOutcome{Content: string(data)}
	}
	gw.SetAuthorizer(resourceAwareAuthorizer{rules: ResourceRules{"workspace": func(ToolAuthorityQuery) (ToolDecision, error) { return ToolDecision{Allowed: true}, nil }}})
	gw.SetCaller(CallerPersonFacing, "fixture-person", "console")
	gw.SetSessionTask("bt-1")
	gw.MaxContentBytes = 700
	out, err := gw.Dispatch(context.Background(), "workspace_list", map[string]any{"scope": "private"})
	if err != nil || !out.Failed || strings.Contains(out.Content, `"text"`) {
		t.Fatalf("gateway truncated successful JSON: %+v %v", out, err)
	}
}
