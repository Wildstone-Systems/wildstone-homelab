package butler

import (
	"context"
	"strings"
	"testing"
)

func TestWorkspaceWritesAreOptInMutatingAndBounded(t *testing.T) {
	for _, family := range []bool{false, true} {
		calls := 0
		ready := true
		seam := func(context.Context, string, WorkspaceWriteCall) ToolOutcome {
			calls++
			return ToolOutcome{Content: `{"version":1,"data":{"writeId":"fixture"}}`, EffectConfirmed: true}
		}
		auth := resourceAwareAuthorizer{rules: ResourceRules{"workspace": func(q ToolAuthorityQuery) (ToolDecision, error) {
			if !q.OptIn || q.TaskID != "bt-1" {
				t.Fatal("lost write authority")
			}
			return ToolDecision{Allowed: true}, nil
		}}}
		var gw Toolset
		if family {
			g := NewHouseholdGateway("fixture", HouseholdSeams{WorkspaceWrites: seam, WorkspaceWriteReady: func() bool { return ready }})
			g.SetAuthorizer(auth)
			g.SetCaller(CallerPersonFacing, "fixture", "household_portal")
			g.SetSessionTask("bt-1")
			gw = g
		} else {
			g := NewToolGateway(nil, nil)
			g.WorkspaceWrites = seam
			g.WorkspaceWriteReady = func() bool { return ready }
			g.SetAuthorizer(auth)
			g.SetCaller(CallerPersonFacing, "fixture", "console")
			g.SetSessionTask("bt-1")
			gw = g
		}
		for _, spec := range workspaceWriteSpecs {
			if ok, _ := gw.Permits(spec.Name, true); ok {
				t.Fatal("QA permitted mutation")
			}
			for _, offered := range gw.OfferedSpecs(true) {
				if offered.Name == spec.Name {
					t.Fatal("QA offered write")
				}
			}
		}
		invalid := []map[string]any{{"scope": "shared", "path": "report.md", "expectedRevision": nil}, {"scope": "private", "path": "report.md", "expectedRevision": nil, "task": "bt-2"}, {"scope": "private", "path": "report.md"}, {"scope": "private", "path": "report.md", "expectedRevision": "", "artifactId": "file"}}
		for _, args := range invalid {
			out, err := gw.Execute(context.Background(), "workspace_write_begin", args, false)
			if err != nil || !out.Failed || calls != 0 {
				t.Fatalf("bad begin reached seam: %+v %v", out, err)
			}
		}
		for _, args := range []map[string]any{{"writeId": "fixture", "index": 0.5, "text": "fixture"}, {"writeId": "fixture", "index": 0, "text": strings.Repeat("世", 342)}, {"writeId": "fixture", "index": -1, "text": "fixture"}} {
			out, err := gw.Execute(context.Background(), "workspace_write_chunk", args, false)
			if err != nil || !out.Failed || calls != 0 {
				t.Fatal("bad chunk reached seam")
			}
		}
		out, err := gw.Execute(context.Background(), "workspace_write_begin", map[string]any{"scope": "private", "path": "report.md", "expectedRevision": nil}, false)
		if err != nil || out.Failed || out.Rejected || calls != 1 || !strings.Contains(out.Content, "<untrusted_data") {
			t.Fatalf("write dispatch failed: %+v %v", out, err)
		}
		ready = false
		if ok, _ := gw.Permits("workspace_write_begin", false); ok {
			t.Fatal("unready write still permitted")
		}
	}
}

func TestWorkspaceReadAndWriteReadinessRemainIndependent(t *testing.T) {
	for _, family := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner", true: "member"}[family], func(t *testing.T) {
			readReady, writeReady := true, true
			readCalls, writeCalls := 0, 0
			reads := func(context.Context, string, WorkspaceReadCall) ToolOutcome {
				readCalls++
				return ToolOutcome{Content: `{"version":1,"data":{"items":[]}}`}
			}
			writes := func(context.Context, string, WorkspaceWriteCall) ToolOutcome {
				writeCalls++
				return ToolOutcome{Content: `{"version":1,"data":{"writeId":"fixture"}}`, EffectConfirmed: true}
			}
			authority := resourceAwareAuthorizer{rules: ResourceRules{"workspace": func(ToolAuthorityQuery) (ToolDecision, error) { return ToolDecision{Allowed: true}, nil }}}
			var gw Toolset
			if family {
				g := NewHouseholdGateway("fixture", HouseholdSeams{WorkspaceReads: reads, WorkspaceReadReady: func() bool { return readReady }, WorkspaceWrites: writes, WorkspaceWriteReady: func() bool { return writeReady }})
				g.SetAuthorizer(authority)
				g.SetCaller(CallerPersonFacing, "fixture", "household_portal")
				g.SetSessionTask("bt-1")
				gw = g
			} else {
				g := NewToolGateway(nil, nil)
				g.WorkspaceReads = reads
				g.WorkspaceReadReady = func() bool { return readReady }
				g.WorkspaceWrites = writes
				g.WorkspaceWriteReady = func() bool { return writeReady }
				g.SetAuthorizer(authority)
				g.SetCaller(CallerPersonFacing, "fixture", "console")
				g.SetSessionTask("bt-1")
				gw = g
			}
			inputs := map[string]map[string]any{
				"workspace_list": {"scope": "private"}, "workspace_read": {"artifactId": "file", "revision": "revision"}, "workspace_search": {"scope": "private", "query": "fixture"},
				"workspace_write_begin": {"scope": "private", "path": "report.md", "expectedRevision": nil}, "workspace_write_chunk": {"writeId": "fixture", "index": 0, "text": "fixture"}, "workspace_write_commit": {"writeId": "fixture"}, "workspace_patch": {"artifactId": "file", "revision": "revision", "oldText": "old", "newText": "new"},
			}
			for _, ready := range [][2]bool{{false, true}, {true, false}, {true, true}, {false, false}} {
				readReady, writeReady = ready[0], ready[1]
				offered := map[string]bool{}
				for _, spec := range gw.OfferedSpecs(false) {
					offered[spec.Name] = true
				}
				for name, args := range inputs {
					want := readReady
					if workspaceWriteTool(name) {
						want = writeReady
					}
					permitted, _ := gw.Permits(name, false)
					if permitted != want || offered[name] != want {
						t.Fatalf("%s readiness coupled: permitted=%v offered=%v want=%v", name, permitted, offered[name], want)
					}
					before := readCalls + writeCalls
					out, err := gw.Execute(context.Background(), name, args, false)
					if err != nil || out.Rejected == want || out.Failed || (readCalls+writeCalls-before != map[bool]int{false: 0, true: 1}[want]) {
						t.Fatalf("%s readiness dispatch failed: %+v %v", name, out, err)
					}
				}
			}
			readReady, writeReady = true, true
			for name, args := range inputs {
				if workspaceWriteTool(name) {
					before := writeCalls
					out, err := gw.Execute(context.Background(), name, args, true)
					if err != nil || !out.Rejected || writeCalls != before {
						t.Fatalf("QA executed %s", name)
					}
				} else {
					if allowed, _ := gw.Permits(name, true); !allowed {
						t.Fatalf("QA withheld %s after write integration", name)
					}
				}
			}
		})
	}
}
