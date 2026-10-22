package api

// B6's evidence (ADR-W031): the Console can create Butlers, switch a
// model immediately, and see at a glance which ones send material out
// of the house.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func butlerConsole(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	for _, r := range []runtimedb.Model{
		{Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "llama",
			Readiness: runtimedb.ModelReady, Enabled: true},
		{Name: "cloud", Kind: runtimedb.ModelCloud, Provider: "anthropic", ProviderModel: "sonnet",
			Readiness: runtimedb.ModelReady, Enabled: true},
	} {
		if err := h.s.DB.UpsertModel(r, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.s.DB.RecordButlerMember("sam", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AdmitButlerMember("sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	return h
}

// Every response naming a Butler names its model and whether that model
// leaves the house. A Console should never have to fetch that
// separately, because the screen that forgets is the one where it
// mattered.
func TestTheRosterSaysWhereEachButlersWordsGo(t *testing.T) {
	h := butlerConsole(t)

	var created map[string]any
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers", map[string]any{
		"name": "health", "directive": "look after the gym and sleep logs",
		"agentClass": "member", "model": "kitchen-box",
	}, &created)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("creating a butler = %d", res.StatusCode)
	}
	if created["leavesTheHouse"] != false {
		t.Fatalf("a local butler reported leavesTheHouse=%v", created["leavesTheHouse"])
	}

	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers", map[string]any{
		"name": "nutrition", "agentClass": "member", "model": "cloud",
	}, nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("creating the cloud butler = %d", res.StatusCode)
	}

	var list struct {
		Butlers []struct {
			Butler         runtimedb.Butler `json:"butler"`
			LeavesTheHouse bool             `json:"leavesTheHouse"`
			Reachable      bool             `json:"reachable"`
		} `json:"butlers"`
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butlers", nil, &list); res.StatusCode != http.StatusOK {
		t.Fatalf("listing butlers = %d", res.StatusCode)
	}
	if len(list.Butlers) != 2 {
		t.Fatalf("roster has %d butlers, want 2", len(list.Butlers))
	}
	for _, b := range list.Butlers {
		if b.Butler.Model == "" || b.Butler.ModelKind == "" {
			t.Fatalf("a roster row does not name its model: %+v", b)
		}
		want := b.Butler.ModelKind == runtimedb.ModelCloud
		if b.LeavesTheHouse != want {
			t.Fatalf("%s: leavesTheHouse=%v on a %s route", b.Butler.Name, b.LeavesTheHouse, b.Butler.ModelKind)
		}
	}
}

// A8: changing the model applies at once, and a local-to-cloud move
// says what happens to a conversation that is already open.
func TestSwitchingAModelAppliesAtOnceAndWarnsOnTheCrossing(t *testing.T) {
	h := butlerConsole(t)
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers", map[string]any{
		"name": "health", "agentClass": "member", "model": "kitchen-box",
	}, nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", res.StatusCode)
	}

	var out map[string]any
	res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/model",
		map[string]any{"model": "cloud"}, &out)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("switching the model = %d", res.StatusCode)
	}
	// It applied. No proposal, no pending anything.
	if b, _ := h.s.DB.GetButler("health"); b.Model != "cloud" {
		t.Fatalf("the switch did not apply: %q", b.Model)
	}
	if out["crossesToCloud"] != true {
		t.Fatal("a local-to-cloud switch did not report the crossing, so nothing could warn")
	}
	warning, _ := out["warning"].(string)
	if warning == "" {
		t.Fatal("the crossing carries no warning text")
	}
	if out["previousModel"] != "kitchen-box" {
		t.Fatalf("the response does not say what it was before: %v", out["previousModel"])
	}

	// Back the other way narrows where material can go and does not
	// warn — warning on it would train people through the one that
	// matters.
	var back map[string]any
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/model",
		map[string]any{"model": "kitchen-box"}, &back); res.StatusCode != http.StatusOK {
		t.Fatalf("switching back = %d", res.StatusCode)
	}
	if _, warned := back["crossesToCloud"]; warned {
		t.Fatal("a cloud-to-local switch warned")
	}
}

// Shaping a Butler is standing configuration: it needs manage, and a
// read token cannot do it.
func TestShapingAButlerNeedsManage(t *testing.T) {
	h := butlerConsole(t)
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers", map[string]any{
		"name": "health", "agentClass": "member", "model": "kitchen-box",
	}, nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", res.StatusCode)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/butlers", map[string]any{
			"name": "other", "agentClass": "member", "model": "cloud"}},
		{http.MethodPut, "/api/v1/butlers/health/model", map[string]any{"model": "cloud"}},
		{http.MethodPut, "/api/v1/butlers/health/access", map[string]any{"member": "sam", "allow": true}},
		{http.MethodPut, "/api/v1/butlers/health/tool", map[string]any{"tool": "send_message", "allow": false}},
		{http.MethodPut, "/api/v1/butlers/health/assignment", map[string]any{"tool": "recall", "assign": true}},
		{http.MethodDelete, "/api/v1/butlers/health", nil},
	} {
		res := h.do(runtimedb.RoleConcierge, c.method, c.path, c.body, nil)
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s with a read token = %d, want 403", c.method, c.path, res.StatusCode)
		}
	}
}

// The scope endpoint answers "what does this Butler see" in one call,
// so nobody assembles it from three screens and gets it wrong.
func TestButlerScopeAnswersInOneCall(t *testing.T) {
	h := butlerConsole(t)
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers", map[string]any{
		"name": "health", "agentClass": "member", "model": "kitchen-box",
	}, nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/access",
		map[string]any{"member": "sam", "allow": true}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("granting access = %d", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/tool",
		map[string]any{"tool": "send_message", "allow": false, "reason": "it only reads"}, nil,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("narrowing a tool = %d", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/address",
		map[string]any{"transport": "telegram", "externalId": "bot-health", "allow": true}, nil,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("adding an address = %d", res.StatusCode)
	}

	var out struct {
		Scope struct {
			Model     string            `json:"model"`
			ModelKind string            `json:"modelKind"`
			Narrowed  map[string]string `json:"narrowedTools"`
		} `json:"scope"`
		People    []string `json:"people"`
		Addresses []struct {
			Transport  string `json:"transport"`
			ExternalID string `json:"externalId"`
		} `json:"addresses"`
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butlers/health/scope", nil, &out); res.StatusCode != http.StatusOK {
		t.Fatalf("reading the scope = %d", res.StatusCode)
	}
	if out.Scope.Model != "kitchen-box" || out.Scope.ModelKind != runtimedb.ModelLocal {
		t.Fatalf("scope model = %q (%s)", out.Scope.Model, out.Scope.ModelKind)
	}
	if len(out.People) != 1 || out.People[0] != "sam" {
		t.Fatalf("scope people = %v", out.People)
	}
	if _, ok := out.Scope.Narrowed["send_message"]; !ok {
		t.Fatalf("scope does not report the narrowed tool: %+v", out.Scope.Narrowed)
	}
	if len(out.Addresses) != 1 || out.Addresses[0].ExternalID != "bot-health" {
		t.Fatalf("scope addresses = %+v", out.Addresses)
	}
}

// The tools endpoint answers with the gateway's own registry, resolved
// through this Butler's authority.
//
// The Console cannot assemble either half. A tool list written in the
// client is a second copy of the registry that drifts the first time a
// gateway changes, and deriving availability means re-implementing the
// effective-authority order in TypeScript — where getting it wrong
// shows a tool as on that a dispatch would refuse.
func TestButlerToolsComeFromTheGatewayNotTheClient(t *testing.T) {
	h := butlerConsole(t)
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers", map[string]any{
		"name": "health", "agentClass": "member", "model": "kitchen-box",
	}, nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", res.StatusCode)
	}

	type toolsView struct {
		Butler     string `json:"butler"`
		AgentClass string `json:"agentClass"`
		Surface    string `json:"surface"`
		Verdict    string `json:"modelVerdict"`
		Tools      []struct {
			Name      string `json:"name"`
			Summary   string `json:"summary"`
			Available bool   `json:"available"`
			Reason    string `json:"reason"`
		} `json:"tools"`
		Narrowed map[string]string `json:"narrowedTools"`
	}
	read := func() toolsView {
		t.Helper()
		var out toolsView
		if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butlers/health/tools", nil, &out); res.StatusCode != http.StatusOK {
			t.Fatalf("reading the tools = %d", res.StatusCode)
		}
		return out
	}

	before := read()
	if before.AgentClass != runtimedb.AgentClassMember || before.Surface != string(runtimedb.ButlerSurfaceFamily) {
		t.Fatalf("tools view = %+v", before)
	}
	// The family gateway's own registry, not a list this package keeps.
	// A tool added or removed there must show up here without anybody
	// editing a second copy.
	want := map[string]bool{"get_household_events": false, "do_household_action": false}
	for _, tool := range before.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
			if tool.Summary == "" {
				t.Fatalf("%s came back with no summary, so the screen has nothing to render", tool.Name)
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("the household surface's %s is missing from the manifest: %+v", name, before.Tools)
		}
	}

	// Narrowing is reported as a narrowing, separately from the tool
	// being unavailable. Only the first is something a screen can undo,
	// and a client that cannot tell them apart offers a button that
	// does nothing.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/tool",
		map[string]any{"tool": "do_household_action", "allow": false, "reason": "it only reads"}, nil,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("narrowing = %d", res.StatusCode)
	}
	narrowed := read()
	if narrowed.Narrowed["do_household_action"] != "it only reads" {
		t.Fatalf("the narrowing reason did not survive: %+v", narrowed.Narrowed)
	}
	for _, tool := range narrowed.Tools {
		if tool.Name == "do_household_action" && tool.Available {
			t.Fatalf("a narrowed tool is still reported available: %+v", tool)
		}
	}

	// Giving it back returns it to whatever the profile allows, and the
	// narrowing stops being reported at all.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/tool",
		map[string]any{"tool": "do_household_action", "allow": true}, nil,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("widening = %d", res.StatusCode)
	}
	if back := read(); back.Narrowed["do_household_action"] != "" {
		t.Fatalf("the narrowing outlived its removal: %+v", back.Narrowed)
	}
}

// A governed Agent can be given a tool after it was created, and the
// Tools view says which of its missing tools assigning would fix
// (#389). Before this, assignments were written only at creation, and
// an unassigned tool looked the same as one nothing here provides.
func TestAToolCanBeAssignedToAnExistingAgent(t *testing.T) {
	h := butlerConsole(t)
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers", map[string]any{
		"name": "health", "agentClass": "member", "model": "kitchen-box",
	}, nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", res.StatusCode)
	}
	type toolView struct {
		Name       string `json:"name"`
		Available  bool   `json:"available"`
		Reason     string `json:"reason"`
		Unassigned bool   `json:"unassigned"`
	}
	read := func() (bool, map[string]toolView) {
		t.Helper()
		var out struct {
			Governed bool       `json:"governed"`
			Tools    []toolView `json:"tools"`
		}
		if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butlers/health/tools", nil, &out); res.StatusCode != http.StatusOK {
			t.Fatalf("reading the tools = %d", res.StatusCode)
		}
		byName := map[string]toolView{}
		for _, tv := range out.Tools {
			byName[tv.Name] = tv
		}
		return out.Governed, byName
	}
	assign := func(tool string, on bool) int {
		t.Helper()
		return h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/assignment",
			map[string]any{"tool": tool, "assign": on}, nil).StatusCode
	}

	governed, tools := read()
	if !governed {
		t.Fatal("a newly created Agent is not governed")
	}
	const tool = "recall"
	if tv, ok := tools[tool]; !ok || !tv.Available {
		t.Fatalf("%s before = %+v, want seeded and available", tool, tv)
	}

	if code := assign(tool, false); code != http.StatusOK {
		t.Fatalf("unassigning = %d", code)
	}
	_, tools = read()
	if tv := tools[tool]; tv.Available || !tv.Unassigned || !strings.Contains(tv.Reason, "has not been given") {
		t.Fatalf("%s after unassigning = %+v, want unavailable, unassigned, and said so", tool, tv)
	}

	if code := assign(tool, true); code != http.StatusOK {
		t.Fatalf("assigning = %d", code)
	}
	_, tools = read()
	if tv := tools[tool]; !tv.Available || tv.Unassigned {
		t.Fatalf("%s after assigning = %+v, want available again", tool, tv)
	}

	// A name this Agent's class does not have is refused, not recorded
	// as an assignment covering nothing.
	if code := assign("no_such_tool", true); code != http.StatusBadRequest {
		t.Fatalf("assigning an unknown tool = %d, want 400", code)
	}
}

// An operator (Owner) Butler is answered with the operator surface's
// tools AND the household's, and a household (Member) one with the
// household's alone. ADR-W033 B3: Member tools may be granted to either
// class, Owner tools only to Owner Agents — so the list may widen in one
// direction and never the other.
func TestButlerToolsAreTheSurfacesOwn(t *testing.T) {
	h := butlerConsole(t)
	for _, b := range []map[string]any{
		{"name": "health", "agentClass": "member", "model": "kitchen-box"},
		{"name": "ops", "agentClass": "owner", "model": "kitchen-box"},
	} {
		if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers", b, nil); res.StatusCode != http.StatusCreated {
			t.Fatalf("create %v = %d", b["name"], res.StatusCode)
		}
	}
	names := func(butler string) map[string]bool {
		t.Helper()
		var out struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butlers/"+butler+"/tools", nil, &out); res.StatusCode != http.StatusOK {
			t.Fatalf("reading %s's tools = %d", butler, res.StatusCode)
		}
		set := map[string]bool{}
		for _, tool := range out.Tools {
			set[tool.Name] = true
		}
		return set
	}
	household, operator := names("health"), names("ops")
	if !operator["propose_change"] {
		t.Fatalf("the operator surface lost propose_change: %v", operator)
	}
	if household["propose_change"] {
		t.Fatalf("a household Butler was offered propose_change, which its gateway does not contain: %v", household)
	}
	if !household["get_household_events"] {
		t.Fatalf("the household surface lost get_household_events: %v", household)
	}
	for _, tool := range []string{"get_household_events", "do_household_action", "list_household_actions"} {
		if !operator[tool] {
			t.Fatalf("an operator Butler is not offered the household tool %s it holds (ADR-W033 B3): %v", tool, operator)
		}
	}
	for _, tool := range []string{"get_system_overview", "diagnose", "get_container_logs"} {
		if household[tool] {
			t.Fatalf("a household Butler was offered the operator tool %s: %v", tool, household)
		}
	}
}

// A model in use cannot be deleted, and a retired Butler counts as use.
//
// Its conversations are still evidence, and the question they exist to
// answer is "which model saw this" — so the row they name has to
// outlive them. Deleting it would turn an answer into a dangling id.
func TestAModelInUseCannotBeDeleted(t *testing.T) {
	h := butlerConsole(t)
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers", map[string]any{
		"name": "health", "agentClass": "member", "model": "kitchen-box",
	}, nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodDelete, "/api/v1/butler/models/kitchen-box", nil, nil); res.StatusCode != http.StatusConflict {
		t.Fatalf("deleting a model in use = %d, want 409", res.StatusCode)
	}
	// Retiring the Butler does not release it.
	if res := h.do(runtimedb.RoleAdmin, http.MethodDelete, "/api/v1/butlers/health", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("retire = %d", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodDelete, "/api/v1/butler/models/kitchen-box", nil, nil); res.StatusCode != http.StatusConflict {
		t.Fatalf("deleting a retired butler's model = %d, want 409 — its sessions still name it", res.StatusCode)
	}
	// One nothing was ever assigned to goes.
	if res := h.do(runtimedb.RoleAdmin, http.MethodDelete, "/api/v1/butler/models/cloud", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("deleting an unused model = %d", res.StatusCode)
	}
}
