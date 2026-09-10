package api

// B6's evidence (ADR-W031): the Console can create Butlers, switch a
// model immediately, and see at a glance which ones send material out
// of the house.

import (
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func butlerConsole(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	for _, r := range []runtimedb.ModelRoute{
		{Name: "kitchen-box", Kind: runtimedb.RouteLocal, Provider: "ollama", Model: "llama",
			Readiness: runtimedb.RouteReady, Enabled: true},
		{Name: "cloud", Kind: runtimedb.RouteCloud, Provider: "anthropic", Model: "sonnet",
			Readiness: runtimedb.RouteReady, Enabled: true},
	} {
		if err := h.s.DB.UpsertModelRoute(r, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.s.DB.RecordButlerMember("sam", runtimedb.ProfileHousehold, ""); err != nil {
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
		"surface": "family", "profile": "household", "route": "kitchen-box",
	}, &created)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("creating a butler = %d", res.StatusCode)
	}
	if created["leavesTheHouse"] != false {
		t.Fatalf("a local butler reported leavesTheHouse=%v", created["leavesTheHouse"])
	}

	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers", map[string]any{
		"name": "nutrition", "surface": "family", "profile": "household", "route": "cloud",
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
		if b.Butler.Route == "" || b.Butler.RouteKind == "" {
			t.Fatalf("a roster row does not name its model: %+v", b)
		}
		want := b.Butler.RouteKind == runtimedb.RouteCloud
		if b.LeavesTheHouse != want {
			t.Fatalf("%s: leavesTheHouse=%v on a %s route", b.Butler.Name, b.LeavesTheHouse, b.Butler.RouteKind)
		}
	}
}

// A8: changing the model applies at once, and a local-to-cloud move
// says what happens to a conversation that is already open.
func TestSwitchingAModelAppliesAtOnceAndWarnsOnTheCrossing(t *testing.T) {
	h := butlerConsole(t)
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers", map[string]any{
		"name": "health", "surface": "family", "profile": "household", "route": "kitchen-box",
	}, nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", res.StatusCode)
	}

	var out map[string]any
	res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/route",
		map[string]any{"route": "cloud"}, &out)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("switching the model = %d", res.StatusCode)
	}
	// It applied. No proposal, no pending anything.
	if b, _ := h.s.DB.GetButler("health"); b.Route != "cloud" {
		t.Fatalf("the switch did not apply: %q", b.Route)
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
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/route",
		map[string]any{"route": "kitchen-box"}, &back); res.StatusCode != http.StatusOK {
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
		"name": "health", "surface": "family", "profile": "household", "route": "kitchen-box",
	}, nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", res.StatusCode)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/butlers", map[string]any{
			"name": "other", "surface": "family", "profile": "household", "route": "cloud"}},
		{http.MethodPut, "/api/v1/butlers/health/route", map[string]any{"route": "cloud"}},
		{http.MethodPut, "/api/v1/butlers/health/access", map[string]any{"member": "sam", "allow": true}},
		{http.MethodPut, "/api/v1/butlers/health/tool", map[string]any{"tool": "send_message", "allow": false}},
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
		"name": "health", "surface": "family", "profile": "household", "route": "kitchen-box",
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
			RouteKind string            `json:"routeKind"`
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
	if out.Scope.Model != "kitchen-box" || out.Scope.RouteKind != runtimedb.RouteLocal {
		t.Fatalf("scope model = %q (%s)", out.Scope.Model, out.Scope.RouteKind)
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
		"name": "health", "surface": "family", "profile": "household", "route": "kitchen-box",
	}, nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", res.StatusCode)
	}

	type toolsView struct {
		Butler  string `json:"butler"`
		Profile string `json:"profile"`
		Surface string `json:"surface"`
		Verdict string `json:"modelVerdict"`
		Tools   []struct {
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
	if before.Profile != runtimedb.ProfileHousehold || before.Surface != string(runtimedb.ButlerSurfaceFamily) {
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

// An operator Butler is answered with the operator surface's tools, and
// a household one is not. The ceiling is what the Butler was created
// against (A3), so this endpoint must never let a screen render the
// wrong surface's list beside the right surface's name.
func TestButlerToolsAreTheSurfacesOwn(t *testing.T) {
	h := butlerConsole(t)
	for _, b := range []map[string]any{
		{"name": "health", "surface": "family", "profile": "household", "route": "kitchen-box"},
		{"name": "ops", "surface": "admin", "profile": "operator", "route": "kitchen-box"},
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
	if operator["get_household_events"] {
		t.Fatalf("an operator Butler was offered the family surface's reads: %v", operator)
	}
}
