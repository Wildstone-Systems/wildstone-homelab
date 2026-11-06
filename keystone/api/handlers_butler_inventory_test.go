package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerstore"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
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
		var allSessions []runtimedb.ButlerSession
		if res := h.doToken(token, http.MethodGet, "/api/v1/butler/sessions", nil, &allSessions); res.StatusCode != http.StatusOK {
			t.Fatalf("unfiltered sessions = %d", res.StatusCode)
		}
		if len(allSessions) != 1 || allSessions[0].ID != ownSession {
			t.Fatalf("unfiltered sessions = %+v, want only %s", allSessions, ownSession)
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

// Person and Agent predicates belong inside the task query, before its fixed
// page limit. Otherwise a busy second Person or a newly revoked Agent can make
// older visible work disappear from the inventory even though detail still
// admits it.
func TestPersonTaskInventoryScopesBeforeLimit(t *testing.T) {
	h := newHarness(t)
	if err := h.s.DB.UpsertModel(runtimedb.Model{
		Name: "fixture-local", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "fixture",
		Readiness: runtimedb.ModelReady, Enabled: true,
	}, "fixture"); err != nil {
		t.Fatal(err)
	}
	mikaToken := h.console(runtimedb.RoleAdmin, "mika")
	h.console(runtimedb.RoleAdmin, "sam")
	for _, name := range []string{"mika-visible", "mika-revoked", "sam-visible"} {
		if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers", map[string]any{
			"name": name, "agentClass": runtimedb.AgentClassMember, "model": "fixture-local",
		}, nil); res.StatusCode != http.StatusCreated {
			t.Fatalf("create %s = %d", name, res.StatusCode)
		}
	}
	for agent, person := range map[string]string{
		"mika-visible": "mika", "mika-revoked": "mika", "sam-visible": "sam",
	} {
		if err := h.s.DB.GrantButlerAccess(agent, person, "fixture"); err != nil {
			t.Fatal(err)
		}
	}

	mikaVisibleSession := inventorySession(t, h, "mika", "mika-visible", "visible")
	mikaRevokedSession := inventorySession(t, h, "mika", "mika-revoked", "revoked")
	samSession := inventorySession(t, h, "sam", "sam-visible", "other")
	visibleTask := inventoryTask(t, h, "mika", mikaVisibleSession, "older visible task")
	if err := h.s.DB.RevokeButlerAccess("mika-revoked", "mika", "fixture"); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 201; i++ {
		inventoryTask(t, h, "mika", mikaRevokedSession, fmt.Sprintf("newer revoked task %03d", i))
		inventoryTask(t, h, "sam", samSession, fmt.Sprintf("newer other-Person task %03d", i))
	}

	var tasks struct {
		Tasks []runtimedb.ButlerTask `json:"tasks"`
	}
	res := h.doToken(mikaToken, http.MethodGet, "/api/v1/butler/tasks", nil, &tasks)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("task inventory = %d", res.StatusCode)
	}
	if len(tasks.Tasks) != 1 || tasks.Tasks[0].ID != visibleTask {
		t.Fatalf("task inventory = %+v, want older visible task %s", tasks.Tasks, visibleTask)
	}
}

// Handles are labels, and distinct People may carry case-differing labels.
// Task detail must compare the stable identities stored on the task rather
// than folding those labels together.
func TestPersonTaskDetailUsesCanonicalIdentity(t *testing.T) {
	h := newHarness(t)
	lowerToken := h.console(runtimedb.RoleAdmin, "sam")
	upperToken := h.console(runtimedb.RoleAdmin, "Sam")

	lowerTask, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "sam", Owner: "sam",
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember,
		Audience: runtimedb.ButlerAudiencePrivate, ObjectiveKind: runtimedb.ObjectiveInformational,
		Objective: "lower-case Person private task",
	})
	if err != nil {
		t.Fatal(err)
	}
	upperTask, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "Sam", Owner: "Sam",
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember,
		Audience: runtimedb.ButlerAudiencePrivate, ObjectiveKind: runtimedb.ObjectiveInformational,
		Objective: "upper-case Person private task",
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		token string
		own   string
		other string
	}{
		{lowerToken, lowerTask, upperTask},
		{upperToken, upperTask, lowerTask},
	} {
		if res := h.doToken(tc.token, http.MethodGet, "/api/v1/butler/tasks/"+tc.own, nil, nil); res.StatusCode != http.StatusOK {
			t.Fatalf("own task detail = %d", res.StatusCode)
		}
		if res := h.doToken(tc.token, http.MethodGet, "/api/v1/butler/tasks/"+tc.other, nil, nil); res.StatusCode != http.StatusNotFound {
			t.Fatalf("case-distinct Person task detail = %d, want 404", res.StatusCode)
		}
	}
}

func TestPersonSessionInventoryUsesCanonicalIdentityBeforeLimit(t *testing.T) {
	h := newHarness(t)
	if err := h.s.DB.UpsertModel(runtimedb.Model{
		Name: "fixture-local", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "fixture",
		Readiness: runtimedb.ModelReady, Enabled: true,
	}, "fixture"); err != nil {
		t.Fatal(err)
	}
	token := h.console(runtimedb.RoleAdmin, "mika")
	for _, name := range []string{"visible-agent", "revoked-agent"} {
		if res := h.doToken(token, http.MethodPost, "/api/v1/butlers", map[string]any{
			"name": name, "agentClass": runtimedb.AgentClassMember, "model": "fixture-local",
		}, nil); res.StatusCode != http.StatusCreated {
			t.Fatalf("create %s = %d", name, res.StatusCode)
		}
	}

	visibleSession := inventorySession(t, h, "mika", "visible-agent", "older visible session")
	for i := 0; i < 51; i++ {
		inventorySession(t, h, "mika", "revoked-agent", fmt.Sprintf("newer revoked session %02d", i))
	}
	if err := h.s.DB.RevokeButlerAccess("revoked-agent", "mika", "fixture"); err != nil {
		t.Fatal(err)
	}

	var sessions []runtimedb.ButlerSession
	if res := h.doToken(token, http.MethodGet, "/api/v1/butler/sessions", nil, &sessions); res.StatusCode != http.StatusOK {
		t.Fatalf("session inventory = %d", res.StatusCode)
	}
	if len(sessions) != 1 || sessions[0].ID != visibleSession {
		t.Fatalf("session inventory = %+v, want older visible session %s", sessions, visibleSession)
	}

	if err := h.s.DB.RenameButlerMemberHandle("mika", "mikhail"); err != nil {
		t.Fatal(err)
	}
	renamedToken, err := h.s.Principals.Mint(principalNameFor(t, runtimedb.RoleAdmin), "mikhail", 0)
	if err != nil {
		t.Fatal(err)
	}
	sessions = nil
	if res := h.doToken(renamedToken, http.MethodGet, "/api/v1/butler/sessions", nil, &sessions); res.StatusCode != http.StatusOK {
		t.Fatalf("renamed session inventory = %d", res.StatusCode)
	}
	if len(sessions) != 1 || sessions[0].ID != visibleSession {
		t.Fatalf("renamed session inventory = %+v, want %s", sessions, visibleSession)
	}
	if res := h.doToken(renamedToken, http.MethodGet, "/api/v1/butler/sessions/"+visibleSession, nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("renamed session detail = %d", res.StatusCode)
	}
}

func TestPersonSessionInventoryKeepsOwnUnstampedHistory(t *testing.T) {
	h := newHarness(t)
	id, err := h.s.DB.InsertButlerSession(runtimedb.ButlerSession{
		Principal: "legacy-runtime", ActingFor: "mika",
		Surface: runtimedb.ButlerSurfaceFamily, Mode: runtimedb.ButlerModeFull, FinalText: "legacy",
	})
	if err != nil {
		t.Fatal(err)
	}
	var sessions []runtimedb.ButlerSession
	res := h.doToken(h.console(runtimedb.RoleAdmin, "mika"), http.MethodGet, "/api/v1/butler/sessions", nil, &sessions)
	if res.StatusCode != http.StatusOK || len(sessions) != 1 || sessions[0].ID != id {
		t.Fatalf("legacy session inventory = %d %+v, want %s", res.StatusCode, sessions, id)
	}

	// Until ADR-W032 A4's normalization migration exists, case-distinct
	// handles may be different canonical People. Their unstamped legacy
	// rows therefore use an exact fallback on the Person-scoped Console
	// path, even though the older family portal remains label-only and
	// case-insensitive for compatibility.
	caseDistinctID, err := h.s.DB.InsertButlerSession(runtimedb.ButlerSession{
		Principal: "legacy-runtime", ActingFor: "Mika",
		Surface: runtimedb.ButlerSurfaceFamily, Mode: runtimedb.ButlerModeFull, FinalText: "case distinct",
	})
	if err != nil {
		t.Fatal(err)
	}
	caseDistinctToken := h.console(runtimedb.RoleAdmin, "Mika")
	sessions = nil
	res = h.doToken(caseDistinctToken, http.MethodGet, "/api/v1/butler/sessions", nil, &sessions)
	if res.StatusCode != http.StatusOK || len(sessions) != 1 || sessions[0].ID != caseDistinctID {
		t.Fatalf("case-distinct legacy inventory = %d %+v, want only %s", res.StatusCode, sessions, caseDistinctID)
	}
	if res := h.doToken(caseDistinctToken, http.MethodGet, "/api/v1/butler/sessions/"+id, nil, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("case-distinct legacy detail = %d, want 404", res.StatusCode)
	}
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
	if res := h.doToken(token, http.MethodGet, "/api/v1/butler/sessions", nil, &sessions); res.StatusCode != http.StatusOK || len(sessions) != 0 {
		t.Fatalf("revoked unfiltered sessions = %d %+v, want empty", res.StatusCode, sessions)
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

// The roster's "N of M" denominator is what this deployment wired, not
// the structural ceiling: integrations nobody set up must not read as
// tools somebody switched off.
func TestAgentRosterToolCountExcludesUnwiredTools(t *testing.T) {
	h := newHarness(t)
	if err := h.s.DB.UpsertModel(runtimedb.Model{
		Name: "fixture-local", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "fixture",
		Readiness: runtimedb.ModelReady, Enabled: true,
	}, "fixture"); err != nil {
		t.Fatal(err)
	}
	token := h.console(runtimedb.RoleAdmin, "mika")
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers", map[string]any{
		"name": "mika-agent", "agentClass": runtimedb.AgentClassMember, "model": "fixture-local",
	}, nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", res.StatusCode)
	}
	if err := h.s.DB.GrantButlerAccess("mika-agent", "mika", "fixture"); err != nil {
		t.Fatal(err)
	}
	b, err := h.s.DB.GetButler("mika-agent")
	if err != nil {
		t.Fatal(err)
	}

	// The fixture must leave something unwired, or this proves nothing.
	unwired := 0
	for _, tool := range h.s.administrativeButlerManifest(b, &butlerstore.Authority{DB: h.s.DB, Butler: b.Name}, !b.CarriesActionAuthority()).Tools {
		if !tool.Available && !tool.Unassigned {
			unwired++
		}
	}
	if unwired == 0 {
		t.Skip("harness wires every integration; no unwired tool to exclude")
	}

	var roster struct {
		Butlers []struct {
			ToolsAllowed   int `json:"toolsAllowed"`
			ToolsAvailable int `json:"toolsAvailable"`
		} `json:"butlers"`
	}
	if res := h.doToken(token, http.MethodGet, "/api/v1/butlers", nil, &roster); res.StatusCode != http.StatusOK || len(roster.Butlers) != 1 {
		t.Fatalf("roster = %d %+v", res.StatusCode, roster.Butlers)
	}
	row := roster.Butlers[0]

	wired := h.s.availableTools(b.AgentClass, !b.CarriesActionAuthority())
	want := len(wired)
	ceiling := 0
	for _, tool := range h.s.butlerManifestFor(b, principal.Identity{ActingFor: "mika"}).Tools {
		ceiling++
		if tool.Available && !wired[tool.Name] {
			want++
		}
	}
	if row.ToolsAvailable != want {
		t.Fatalf("toolsAvailable = %d, want %d wired (+ Person-bound); structural ceiling is %d", row.ToolsAvailable, want, ceiling)
	}
	if row.ToolsAvailable >= ceiling || row.ToolsAllowed > row.ToolsAvailable {
		t.Fatalf("counts %+v must exclude %d unwired tools from a ceiling of %d", row, unwired, ceiling)
	}
}
