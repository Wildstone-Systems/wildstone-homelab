package api

// The runtime inventory (design doc §9 H9, #355): every live Agent's
// runtime, one call, correct health for a runtime that has never
// heartbeat as well as one that is in-process, retired Agents excluded,
// and the same read gate as the Butlers roster it augments.

import (
	"net/http"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func TestRuntimeInventoryListsEveryLiveAgentsRuntime(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health")
	createAgent(t, h, "nutrition")

	// health stays native (connected, in-process); nutrition is moved
	// to an external hermes runtime it has never presented, which is
	// unregistered until it does — the two states an inventory has to
	// tell apart at a glance.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/nutrition/runtime",
		map[string]any{"type": "hermes", "endpoint": "https://hermes-nutrition.internal/"}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("attaching hermes to nutrition = %d", res.StatusCode)
	}
	b, err := h.s.DB.GetButler("nutrition")
	if err != nil {
		t.Fatal(err)
	}
	// Offline rather than unregistered: nutrition once connected and
	// then went quiet, which is the state the strip most needs to
	// surface (sorted unhealthy-first on the Console side).
	if err := h.s.DB.SetRuntimeHealth(b.Seq, agentapi.HealthOffline); err != nil {
		t.Fatal(err)
	}

	var out []runtimeInventoryRow
	if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/runtimes", nil, &out); res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/runtimes = %d", res.StatusCode)
	}
	if len(out) != 2 {
		t.Fatalf("got %d row(s), want 2: %+v", len(out), out)
	}
	byName := map[string]runtimeInventoryRow{}
	for _, r := range out {
		byName[r.Butler] = r
	}
	healthRow, ok := byName["health"]
	if !ok {
		t.Fatalf("health is missing from the inventory: %+v", out)
	}
	if healthRow.Runtime.Type != agentapi.RuntimeNative || healthRow.Runtime.Health != agentapi.HealthConnected {
		t.Errorf("health = %+v, want native/connected", healthRow.Runtime)
	}
	if healthRow.AgentClass != runtimedb.AgentClassMember {
		t.Errorf("health's agentClass = %q, want %q", healthRow.AgentClass, runtimedb.AgentClassMember)
	}
	nutritionRow, ok := byName["nutrition"]
	if !ok {
		t.Fatalf("nutrition is missing from the inventory: %+v", out)
	}
	if nutritionRow.Runtime.Type != agentapi.RuntimeHermes || nutritionRow.Runtime.Health != agentapi.HealthOffline {
		t.Errorf("nutrition = %+v, want hermes/offline", nutritionRow.Runtime)
	}
	if nutritionRow.Runtime.Endpoint != "https://hermes-nutrition.internal/" {
		t.Errorf("nutrition endpoint = %q", nutritionRow.Runtime.Endpoint)
	}
}

func TestRuntimeInventoryExcludesARetiredAgent(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health")
	createAgent(t, h, "gone")
	if res := h.do(runtimedb.RoleAdmin, http.MethodDelete, "/api/v1/butlers/gone", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("retiring gone = %d", res.StatusCode)
	}

	var out []runtimeInventoryRow
	if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/runtimes", nil, &out); res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/runtimes = %d", res.StatusCode)
	}
	for _, r := range out {
		if r.Butler == "gone" {
			t.Fatalf("a retired agent appeared in the runtime inventory: %+v", out)
		}
	}
	if len(out) != 1 || out[0].Butler != "health" {
		t.Fatalf("got %+v, want only health", out)
	}
}

// Same gate as the roster it augments: a read, so a role that holds no
// read verb at all (a chat adapter, which holds converse only) is
// refused exactly as it is on GET /api/v1/butlers.
func TestRuntimeInventoryForbidsARoleWithoutRead(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health")
	if res := h.do("", http.MethodGet, "/api/v1/runtimes", nil, nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d, want 401", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleConcierge, http.MethodGet, "/api/v1/runtimes", nil, nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("concierge = %d, want 403", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/runtimes", nil, nil); res.StatusCode != http.StatusOK {
		t.Errorf("admin = %d, want 200", res.StatusCode)
	}
}
