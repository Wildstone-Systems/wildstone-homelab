package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func (h *harness) assistantSetupFor(t *testing.T, user, groups string) assistantSetupState {
	t.Helper()
	res := h.member(http.MethodGet, "/api/v1/assistant/agents", user, groups, nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("agents for %s: status %d", user, res.StatusCode)
	}
	var body struct {
		Agents []assistantAgent     `json:"agents"`
		Setup  *assistantSetupState `json:"setup"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Setup == nil {
		t.Fatalf("an empty assistant card for %s says nothing about why", user)
	}
	return *body.Setup
}

// A fresh install has the assistant widget and no assistant. The card
// says what is missing, and tells somebody who can fix it where: the
// Console's Butlers screen, linked. A member who cannot is told who can.
func TestAnEmptyAssistantSaysHowToSetItUp(t *testing.T) {
	h, _ := portalHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindRoute,
		Metadata: resource.Metadata{Name: "console", Owner: "capability/console", Package: "wildstone-console", Node: "n1"},
		Route:    &resource.RouteSpec{Host: "console.wildstone.internal", Upstream: "100.64.0.1:9441", Auth: "authelia_oidc", Classification: "admin_tailscale"},
	}); err != nil {
		t.Fatal(err)
	}
	h.person("mika", "kai")

	owner := h.assistantSetupFor(t, "mika", "assistant_users,"+ConsoleGroup)
	if !owner.Needed || !owner.CanConfigure || !strings.Contains(owner.Message, "Butlers") ||
		owner.Console != "https://console.wildstone.internal/" {
		t.Errorf("an owner with no assistant is told %+v", owner)
	}
	child := h.assistantSetupFor(t, "kai", "assistant_users")
	if !child.Needed || child.CanConfigure || child.Console != "" || !strings.Contains(child.Message, "Whoever looks after") {
		t.Errorf("a member who cannot configure it is told %+v", child)
	}

	// Once the household has one, an empty card means it was not shared.
	if err := h.s.DB.UpsertModel(runtimedb.Model{Name: "box", Kind: runtimedb.ModelLocal, Provider: "ollama",
		ProviderModel: "qwen", Readiness: runtimedb.ModelReady, Enabled: true}, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.CreateButler(runtimedb.Butler{Name: "sandy", Surface: runtimedb.ButlerSurfaceFamily,
		Profile: runtimedb.ProfileHousehold, Model: "box"}, "mika"); err != nil {
		t.Fatal(err)
	}
	if got := h.assistantSetupFor(t, "kai", "assistant_users"); got.Needed || !strings.Contains(got.Message, "shared") {
		t.Errorf("with an unshared assistant the card says %+v", got)
	}
}
