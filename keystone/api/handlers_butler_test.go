package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// scriptedProvider replays a fixed sequence of ChatResponses — the same
// technique the butler package's own scripted-provider tests use, letting
// the full ask pipeline run with zero network.
type scriptedProvider struct {
	turns []*provider.ChatResponse
	i     int
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Chat(_ context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	if p.i >= len(p.turns) {
		return nil, fmt.Errorf("scripted provider exhausted after %d turns", len(p.turns))
	}
	t := p.turns[p.i]
	p.i++
	return t, nil
}

// proposeGrafanaTurns scripts one session: the model proposes a valid,
// policy-compliant Route, then answers.
func proposeGrafanaTurns() []*provider.ChatResponse {
	return []*provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{{
			ID:   "tc-1",
			Name: "propose_change",
			Input: map[string]any{
				"kind":  "Route",
				"name":  "grafana",
				"owner": "capability/grafana",
				"spec": map[string]any{
					"host":           "grafana.example.com",
					"upstream":       "container/grafana:3000",
					"auth":           "authelia_oidc",
					"classification": "family_public",
				},
			},
		}}},
		{Text: "I've proposed the grafana route for your approval."},
	}
}

func (h *harness) wireButler(turns []*provider.ChatResponse, mode runtimedb.ButlerSessionMode) {
	h.s.ButlerHost = &ButlerHost{
		Provider: &scriptedProvider{turns: turns},
		Gateway:  butler.NewToolGateway(h.s.State, nil),
		Mode:     mode,
	}
}

func TestButlerAskUnconfigured503(t *testing.T) {
	h := newHarness(t)
	resp := h.do(runtimedb.RoleOperator, "POST", "/api/v1/butler/ask", map[string]string{"message": "hi"}, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestButlerAskFullFlow(t *testing.T) {
	h := newHarness(t)
	h.wireButler(proposeGrafanaTurns(), runtimedb.ButlerModeFull)

	var ask askResponse
	resp := h.do(runtimedb.RoleOperator, "POST", "/api/v1/butler/ask", map[string]string{"message": "expose grafana"}, &ask)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ask.SessionID != "bs-1" || ask.Mode != "full" || ask.Capped {
		t.Fatalf("ask = %+v", ask)
	}
	if !strings.Contains(ask.FinalText, "grafana") {
		t.Fatalf("final text = %q", ask.FinalText)
	}
	if len(ask.PlanIDs) != 1 || len(ask.ProposalErrors) != 0 {
		t.Fatalf("planIds = %v, proposalErrors = %v", ask.PlanIDs, ask.ProposalErrors)
	}

	// ADR-W017 A3: the proposal is a REAL plan record, in state proposed,
	// as principal "butler" acting-for the caller — a human approve verb is
	// still the only way forward.
	var rec runtimedb.PlanRecord
	if resp := h.do(runtimedb.RoleAdmin, "GET", "/api/v1/plans/"+ask.PlanIDs[0], nil, &rec); resp.StatusCode != http.StatusOK {
		t.Fatalf("get plan: %d", resp.StatusCode)
	}
	if rec.State != runtimedb.PlanProposed {
		t.Fatalf("plan state = %s, want proposed", rec.State)
	}
	if rec.Principal != "butler" || rec.ActingFor != "op" {
		t.Fatalf("plan principal = %s acting for %q, want butler acting for op", rec.Principal, rec.ActingFor)
	}

	// The statefile itself is untouched until a human approves.
	refs, err := h.s.State.List()
	if err != nil {
		t.Fatalf("State.List: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("ask wrote to the statefile before approval: %+v", refs)
	}

	// ADR-W017 A1: the session is persisted, principal-bound, transcript
	// on the detail read but not the list.
	var list []runtimedb.ButlerSession
	if resp := h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions", nil, &list); resp.StatusCode != http.StatusOK {
		t.Fatalf("list sessions: %d", resp.StatusCode)
	}
	if len(list) != 1 || list[0].ID != "bs-1" || list[0].ActingFor != "op" || list[0].TranscriptJSON != "" {
		t.Fatalf("session list = %+v", list)
	}
	var sess runtimedb.ButlerSession
	if resp := h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions/bs-1", nil, &sess); resp.StatusCode != http.StatusOK {
		t.Fatalf("get session: %d", resp.StatusCode)
	}
	if !strings.Contains(sess.TranscriptJSON, "propose_change") || !strings.Contains(sess.TranscriptJSON, ask.PlanIDs[0]) {
		t.Fatalf("transcript missing tool call / plan id: %q", sess.TranscriptJSON)
	}
}

func TestButlerAskQAOnlyRefusesPropose(t *testing.T) {
	h := newHarness(t)
	h.wireButler(proposeGrafanaTurns(), runtimedb.ButlerModeQAOnly)

	var ask askResponse
	resp := h.do(runtimedb.RoleOperator, "POST", "/api/v1/butler/ask", map[string]string{"message": "expose grafana"}, &ask)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ask.Mode != "qa-only" || len(ask.PlanIDs) != 0 {
		t.Fatalf("QA-only session produced plans: %+v", ask)
	}
	var plans []runtimedb.PlanRecord
	h.do(runtimedb.RoleAdmin, "GET", "/api/v1/plans", nil, &plans)
	if len(plans) != 0 {
		t.Fatalf("QA-only session left plan records: %+v", plans)
	}
}

func TestButlerAskInputValidation(t *testing.T) {
	h := newHarness(t)
	h.wireButler(nil, runtimedb.ButlerModeFull)

	if resp := h.do(runtimedb.RoleOperator, "POST", "/api/v1/butler/ask", map[string]string{"message": ""}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty message: %d, want 400", resp.StatusCode)
	}
	huge := map[string]string{"message": strings.Repeat("x", maxAskBytes+1)}
	if resp := h.do(runtimedb.RoleOperator, "POST", "/api/v1/butler/ask", huge, nil); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized message: %d, want 413", resp.StatusCode)
	}
}

func TestButlerSessionGet404(t *testing.T) {
	h := newHarness(t)
	if resp := h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions/bs-42", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
