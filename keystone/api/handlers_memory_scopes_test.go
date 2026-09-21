package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestConsoleRetirementTransfersOnlyLocalMemory(t *testing.T) {
	h := freshnessHarness(t)
	if err := h.s.DB.UpsertModel(runtimedb.Model{Name: "local", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "test", Readiness: runtimedb.ModelReady, Enabled: true}, "mika"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"source", "destination"} {
		if _, err := h.s.DB.CreateButler(runtimedb.Butler{Name: name, Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "local"}, "mika"); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]int64{}
	for _, scope := range []string{runtimedb.MemoryScopeAgentLocal, runtimedb.MemoryScopePersonal, runtimedb.MemoryScopeHousehold} {
		id, err := h.s.DB.RecordMemory(runtimedb.MemoryRecord{Scope: scope, Butler: "source", Kind: "fact", Content: "memory " + scope, Owner: "sandy", Author: "sandy", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "sandy", SourceKind: "conversation", SourceID: "test"})
		if err != nil {
			t.Fatal(err)
		}
		ids[scope] = id
	}
	cookie := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	code, out := h.sessionRequest(http.MethodDelete, "/api/v1/butlers/source?transferMemoryTo=destination", "", "mika", cookie)
	if code != http.StatusOK {
		t.Fatalf("retire: %d %v", code, out)
	}
	for scope, id := range ids {
		m, err := h.s.DB.GetMemory(id)
		if err != nil {
			t.Fatal(err)
		}
		want := "source"
		if scope == runtimedb.MemoryScopeAgentLocal {
			want = "destination"
		}
		if m.Butler != want || !m.Eligible() {
			t.Fatalf("%s after retirement: %+v", scope, m)
		}
	}
}

func TestConsoleMemoryRestrictionChangesReach(t *testing.T) {
	h := freshnessHarness(t)
	if err := h.s.DB.UpsertModel(runtimedb.Model{Name: "local", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "test", Readiness: runtimedb.ModelReady, Enabled: true}, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.CreateButler(runtimedb.Butler{Name: "agent", Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "local"}, "mika"); err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	code, out := h.sessionRequest(http.MethodPut, "/api/v1/butlers/agent/memory-restrictions", `{"scope":"household","restricted":true}`, "mika", cookie)
	if code != http.StatusOK {
		t.Fatalf("restriction: %d %v", code, out)
	}
	scopes, err := h.s.DB.AgentMemoryRestrictions("agent")
	if err != nil || len(scopes) != 1 {
		t.Fatalf("restriction lost: %v %v", scopes, err)
	}
}
