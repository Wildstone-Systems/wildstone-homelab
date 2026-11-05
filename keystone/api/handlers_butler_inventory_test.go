package api

import (
	"net/http"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// A Console inventory is a view for one authenticated Person. It is not an
// administrative dump with a Person-shaped credential in front of it.
func TestAgentInventoryIsScopedToTheCurrentPerson(t *testing.T) {
	h := newHarness(t)
	if err := h.s.DB.UpsertModel(runtimedb.Model{
		Name: "fixture-local", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "fixture",
		Readiness: runtimedb.ModelReady, Enabled: true,
	}, "fixture"); err != nil {
		t.Fatal(err)
	}
	mikaToken := h.console(runtimedb.RoleAdmin, "mika")
	samToken := h.console(runtimedb.RoleAdmin, "sam")

	for _, name := range []string{"mika-agent", "sam-agent"} {
		if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers", map[string]any{
			"name": name, "agentClass": runtimedb.AgentClassMember, "model": "fixture-local",
		}, nil); res.StatusCode != http.StatusCreated {
			t.Fatalf("create %s = %d", name, res.StatusCode)
		}
	}
	if err := h.s.DB.GrantButlerAccess("mika-agent", "mika", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.GrantButlerAccess("sam-agent", "sam", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.NarrowButlerTool("sam-agent", "remember", "fixture narrowing", "fixture"); err != nil {
		t.Fatal(err)
	}
	for _, person := range []string{"mika", "sam"} {
		if _, err := h.s.DB.RecordMemory(runtimedb.MemoryRecord{
			Scope: runtimedb.MemoryScopePersonal, Kind: runtimedb.MemoryFact,
			Content: person + " private inventory fixture", Owner: person, Author: person,
			Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: person,
			SourceKind: "test", SourceID: person,
		}); err != nil {
			t.Fatal(err)
		}
	}

	mikaSession := inventorySession(t, h, "mika", "mika-agent", "mika-private")
	samSession := inventorySession(t, h, "sam", "sam-agent", "sam-private")
	mikaTask := inventoryTask(t, h, "mika", mikaSession, "mika-private-task")
	samTask := inventoryTask(t, h, "sam", samSession, "sam-private-task")

	assertInventory := func(token, ownAgent, hiddenAgent, ownSession, hiddenSession, ownTask, hiddenTask string) {
		t.Helper()
		var roster struct {
			Butlers []struct {
				Butler         runtimedb.Butler `json:"butler"`
				ToolsAllowed   int              `json:"toolsAllowed"`
				ToolsAvailable int              `json:"toolsAvailable"`
			} `json:"butlers"`
		}
		if res := h.doToken(token, http.MethodGet, "/api/v1/butlers", nil, &roster); res.StatusCode != http.StatusOK {
			t.Fatalf("roster = %d", res.StatusCode)
		}
		if len(roster.Butlers) != 1 || roster.Butlers[0].Butler.Name != ownAgent {
			t.Fatalf("roster = %+v, want only %s", roster.Butlers, ownAgent)
		}
		if roster.Butlers[0].ToolsAvailable == 0 || roster.Butlers[0].ToolsAllowed > roster.Butlers[0].ToolsAvailable {
			t.Fatalf("person-bound tool counts are invalid: %+v", roster.Butlers[0])
		}
		var scope struct {
			Scope runtimedb.ButlerScopeSummary `json:"scope"`
		}
		if res := h.doToken(token, http.MethodGet, "/api/v1/butlers/"+ownAgent+"/scope", nil, &scope); res.StatusCode != http.StatusOK {
			t.Fatalf("own scope = %d", res.StatusCode)
		}
		if scope.Scope.MemoryCount != 1 {
			t.Fatalf("person-scoped memory count = %d, want 1", scope.Scope.MemoryCount)
		}
		if res := h.doToken(token, http.MethodGet, "/api/v1/butlers/"+ownAgent+"/tools", nil, nil); res.StatusCode != http.StatusOK {
			t.Fatalf("own tools = %d", res.StatusCode)
		}
		for _, path := range []string{
			"/api/v1/butlers/" + hiddenAgent + "/scope",
			"/api/v1/butlers/" + hiddenAgent + "/tools",
		} {
			if res := h.doToken(token, http.MethodGet, path, nil, nil); res.StatusCode != http.StatusNotFound {
				t.Fatalf("hidden drawer path %s = %d, want 404", path, res.StatusCode)
			}
		}

		var sessions []runtimedb.ButlerSession
		if res := h.doToken(token, http.MethodGet, "/api/v1/butler/sessions?butler="+ownAgent, nil, &sessions); res.StatusCode != http.StatusOK {
			t.Fatalf("own sessions = %d", res.StatusCode)
		}
		if len(sessions) != 1 || sessions[0].ID != ownSession {
			t.Fatalf("sessions = %+v, want only %s", sessions, ownSession)
		}
		var hiddenSessions []runtimedb.ButlerSession
		if res := h.doToken(token, http.MethodGet, "/api/v1/butler/sessions?butler="+hiddenAgent, nil, &hiddenSessions); res.StatusCode != http.StatusOK || len(hiddenSessions) != 0 {
			t.Fatalf("hidden sessions = %d %+v, want empty", res.StatusCode, hiddenSessions)
		}
		if res := h.doToken(token, http.MethodGet, "/api/v1/butler/sessions/"+hiddenSession, nil, nil); res.StatusCode != http.StatusNotFound {
			t.Fatalf("hidden session detail = %d, want 404", res.StatusCode)
		}

		var tasks struct {
			Tasks []runtimedb.ButlerTask `json:"tasks"`
		}
		if res := h.doToken(token, http.MethodGet, "/api/v1/butler/tasks", nil, &tasks); res.StatusCode != http.StatusOK {
			t.Fatalf("tasks = %d", res.StatusCode)
		}
		if len(tasks.Tasks) != 1 || tasks.Tasks[0].ID != ownTask {
			t.Fatalf("tasks = %+v, want only %s", tasks.Tasks, ownTask)
		}
		if res := h.doToken(token, http.MethodGet, "/api/v1/butler/tasks/"+hiddenTask, nil, nil); res.StatusCode != http.StatusNotFound {
			t.Fatalf("hidden task detail = %d, want 404", res.StatusCode)
		}
	}

	assertInventory(mikaToken, "mika-agent", "sam-agent", mikaSession, samSession, mikaTask, samTask)
	assertInventory(samToken, "sam-agent", "mika-agent", samSession, mikaSession, samTask, mikaTask)

	// No cached roster, drawer, session, or task survives current access
	// revocation. Re-granting is observed just as immediately.
	if err := h.s.DB.RevokeButlerAccess("mika-agent", "mika", "fixture"); err != nil {
		t.Fatal(err)
	}
	assertInventoryEmpty(t, h, mikaToken, "mika-agent", mikaSession, mikaTask)
	if err := h.s.DB.GrantButlerAccess("mika-agent", "mika", "fixture"); err != nil {
		t.Fatal(err)
	}
	assertInventory(mikaToken, "mika-agent", "sam-agent", mikaSession, samSession, mikaTask, samTask)
}

// Creation, duplication and retirement are one Console management lifecycle.
// Person scoping must not strand a new Agent before access can be configured,
// copy somebody else's access list, or erase the Retired restore path.
func TestPersonScopedInventoryPreservesAgentSetupAndRetirement(t *testing.T) {
	h := newHarness(t)
	if err := h.s.DB.UpsertModel(runtimedb.Model{
		Name: "fixture-local", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "fixture",
		Readiness: runtimedb.ModelReady, Enabled: true,
	}, "fixture"); err != nil {
		t.Fatal(err)
	}
	token := h.console(runtimedb.RoleAdmin, "mika")
	samToken := h.console(runtimedb.RoleAdmin, "sam")

	if res := h.doToken(token, http.MethodPost, "/api/v1/butlers", map[string]any{
		"name": "setup-agent", "agentClass": runtimedb.AgentClassMember, "model": "fixture-local",
	}, nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", res.StatusCode)
	}
	assertInventoryNames(t, h, token, "setup-agent")
	assertInventoryNames(t, h, samToken)
	if err := h.s.DB.GrantButlerAccess("setup-agent", "sam", "fixture"); err != nil {
		t.Fatal(err)
	}
	assertInventoryNames(t, h, samToken, "setup-agent")

	if res := h.doToken(token, http.MethodPost, "/api/v1/butlers/setup-agent/duplicate", map[string]any{
		"name": "setup-copy",
	}, nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("duplicate = %d", res.StatusCode)
	}
	assertInventoryNames(t, h, token, "setup-agent", "setup-copy")
	// A copy receives the acting Person's fresh setup grant, never the
	// source Agent's access list.
	assertInventoryNames(t, h, samToken, "setup-agent")

	if res := h.doToken(token, http.MethodDelete, "/api/v1/butlers/setup-agent", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("retire = %d", res.StatusCode)
	}
	assertInventoryNames(t, h, token, "setup-agent", "setup-copy")

	if err := h.s.DB.RevokeButlerAccess("setup-copy", "mika", "fixture"); err != nil {
		t.Fatal(err)
	}
	assertInventoryNames(t, h, token, "setup-agent")
}

func assertInventoryNames(t *testing.T, h *harness, token string, want ...string) {
	t.Helper()
	var roster struct {
		Butlers []struct {
			Butler runtimedb.Butler `json:"butler"`
		} `json:"butlers"`
	}
	if res := h.doToken(token, http.MethodGet, "/api/v1/butlers", nil, &roster); res.StatusCode != http.StatusOK {
		t.Fatalf("roster = %d", res.StatusCode)
	}
	got := make([]string, 0, len(roster.Butlers))
	for _, row := range roster.Butlers {
		got = append(got, row.Butler.Name)
	}
	if len(got) != len(want) {
		t.Fatalf("inventory = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("inventory = %v, want %v", got, want)
		}
	}
}

func inventorySession(t *testing.T, h *harness, person, agent, final string) string {
	t.Helper()
	id, err := h.s.DB.InsertButlerSession(runtimedb.ButlerSession{
		Principal: "fixture-runtime", ActingFor: person, Butler: agent,
		Surface: runtimedb.ButlerSurfaceFamily, Mode: runtimedb.ButlerModeFull, FinalText: final,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func inventoryTask(t *testing.T, h *harness, person, session, objective string) string {
	t.Helper()
	id, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: person, Owner: person,
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember,
		Audience: runtimedb.ButlerAudiencePrivate, ObjectiveKind: runtimedb.ObjectiveInformational,
		Objective: objective,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.SetButlerTaskSession(id, session); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertInventoryEmpty(t *testing.T, h *harness, token, agent, session, task string) {
	t.Helper()
	var roster struct {
		Butlers []runtimedb.Butler `json:"butlers"`
	}
	if res := h.doToken(token, http.MethodGet, "/api/v1/butlers", nil, &roster); res.StatusCode != http.StatusOK || len(roster.Butlers) != 0 {
		t.Fatalf("revoked roster = %d %+v, want empty", res.StatusCode, roster.Butlers)
	}
	for _, path := range []string{
		"/api/v1/butlers/" + agent + "/scope",
		"/api/v1/butlers/" + agent + "/tools",
	} {
		if res := h.doToken(token, http.MethodGet, path, nil, nil); res.StatusCode != http.StatusNotFound {
			t.Fatalf("revoked drawer path %s = %d, want 404", path, res.StatusCode)
		}
	}
	var sessions []runtimedb.ButlerSession
	if res := h.doToken(token, http.MethodGet, "/api/v1/butler/sessions?butler="+agent, nil, &sessions); res.StatusCode != http.StatusOK || len(sessions) != 0 {
		t.Fatalf("revoked sessions = %d %+v, want empty", res.StatusCode, sessions)
	}
	if res := h.doToken(token, http.MethodGet, "/api/v1/butler/sessions/"+session, nil, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("revoked session detail = %d, want 404", res.StatusCode)
	}
	var tasks struct {
		Tasks []runtimedb.ButlerTask `json:"tasks"`
	}
	if res := h.doToken(token, http.MethodGet, "/api/v1/butler/tasks", nil, &tasks); res.StatusCode != http.StatusOK || len(tasks.Tasks) != 0 {
		t.Fatalf("revoked tasks = %d %+v, want empty", res.StatusCode, tasks.Tasks)
	}
	if res := h.doToken(token, http.MethodGet, "/api/v1/butler/tasks/"+task, nil, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("revoked task detail = %d, want 404", res.StatusCode)
	}
}
