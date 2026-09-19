package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// A control cannot add a capability, and the endpoint says so rather
// than letting an administrator believe they granted something.
func TestEnablingAToolAProfileLacksSaysSo(t *testing.T) {
	h := newHarness(t)
	var body struct {
		Note string `json:"note"`
	}
	res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butler/tools",
		map[string]any{"agentClass": "member", "tool": "propose_change", "state": "enabled"}, &body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT tools = %d", res.StatusCode)
	}
	if !strings.Contains(body.Note, "does nothing") {
		t.Fatalf("enabling a tool the household Butler lacks said: %q", body.Note)
	}
	// Enabling one it does have is quiet.
	body.Note = ""
	h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butler/tools",
		map[string]any{"agentClass": "member", "tool": "get_household_events", "state": "enabled"}, &body)
	if body.Note != "" {
		t.Fatalf("enabling a real household tool warned: %q", body.Note)
	}
}

// Asking needs read; deciding and enabling need manage. Approving a
// request does not enable anything.
func TestCapabilityRequestPermissionsAndSeparation(t *testing.T) {
	h := newHarness(t)

	var filed struct {
		Seq int64 `json:"seq"`
	}
	if res := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/butler/requests",
		map[string]any{
			"kind": "supported-tool", "agentClass": "owner", "tool": "get_calendar",
			"description": "to answer what is on today",
		}, &filed,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("an operator could not file a request: %d", res.StatusCode)
	}
	// Deciding is a manage act.
	if res := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/butler/requests/decide",
		map[string]any{"seq": filed.Seq, "state": "approved"}, nil,
	); res.StatusCode != http.StatusForbidden {
		t.Fatalf("an operator decided a request: %d", res.StatusCode)
	}
	var decided struct {
		Note string `json:"note"`
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/requests/decide",
		map[string]any{"seq": filed.Seq, "state": "approved", "note": "reasonable"}, &decided,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("decide = %d", res.StatusCode)
	}
	if !strings.Contains(decided.Note, "does not enable") {
		t.Fatalf("approving did not say it enables nothing: %q", decided.Note)
	}
	// The tool control is still absent — approving recorded a decision
	// and changed no authority.
	var controls struct {
		Controls []runtimedb.ToolControl `json:"controls"`
	}
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butler/tools", nil, &controls)
	for _, c := range controls.Controls {
		if c.Tool == "get_calendar" {
			t.Fatalf("approving a request wrote a tool control: %+v", c)
		}
	}
}

// A grant is a manage act, bound to real work, and revocable.
func TestGrantEndpointsAreBoundAndRevocable(t *testing.T) {
	h := newHarness(t)
	h.person("root-admin")
	taskID, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "root-admin", Owner: "root-admin",
		Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: "operator",
		Audience: runtimedb.ButlerAudiencePrivate, Objective: "work", CompletionCriteria: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"scope": "task", "scopeId": taskID, "agentClass": "owner",
		"tool": "restricted_tool", "minutes": 30, "reason": "one lookup",
	}
	if res := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/butler/grants", body, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("an operator issued a grant: %d", res.StatusCode)
	}
	var granted struct {
		Seq int64 `json:"seq"`
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/grants", body, &granted); res.StatusCode != http.StatusOK {
		t.Fatalf("grant = %d", res.StatusCode)
	}
	// Against a task that does not exist, it is refused rather than
	// becoming a permission nothing can expire.
	bad := map[string]any{"scope": "task", "scopeId": "bt-999", "agentClass": "owner", "tool": "x"}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/grants", bad, nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a grant against a missing task = %d, want 400", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/grants/revoke",
		map[string]any{"seq": granted.Seq}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d", res.StatusCode)
	}
	// Revoking twice is a 404: the second call and the queue disagree
	// about reality.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/grants/revoke",
		map[string]any{"seq": granted.Seq}, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("revoking twice = %d, want 404", res.StatusCode)
	}
}
