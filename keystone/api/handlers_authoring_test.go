package api

// Legacy natural-language Automation proposals must migrate to exact-version
// routine review; dry-run authoring remains available without activating a rule.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/household"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// mailRuleSpec is what the model produces from "whenever I get home, ask
// whether I got the mail" — the same object shape propose_change takes.
func mailRuleSpec() map[string]any {
	return map[string]any{
		"description": "Ask whether the mail was collected on arriving home",
		"trigger":     map[string]any{"eventType": "person.arrived_home", "subject": "mika"},
		"conditions": []any{
			map[string]any{"kind": "cooldown", "window": "20h"},
		},
		"action": map[string]any{
			"tool": "notifications.notify", "tier": "domestic",
			"params": map[string]any{"topic": "household-{{ .Subject }}", "message": "Did you get the mail?"},
		},
	}
}

func authoringTurns() []*provider.ChatResponse {
	rule := map[string]any{
		"kind": "Automation", "name": "mail-check", "owner": "household/mika", "spec": mailRuleSpec(),
	}
	dryRun := map[string]any{}
	for k, v := range rule {
		dryRun[k] = v
	}
	dryRun["days"] = float64(7)
	return []*provider.ChatResponse{
		// Learn the vocabulary before guessing at it.
		{ToolCalls: []provider.ToolCall{{ID: "t1", Name: "get_automation_schema", Input: map[string]any{}}}},
		// Check what it would have done before asking anyone to approve it.
		{ToolCalls: []provider.ToolCall{{ID: "t2", Name: "dry_run_automation", Input: dryRun}}},
		// Then propose it, like any other resource.
		{ToolCalls: []provider.ToolCall{{ID: "t3", Name: "propose_change", Input: rule}}},
		{Text: "I've drafted a mail-check rule and proposed it — it would have fired twice last week. Approve it when you're happy."},
	}
}

func TestButlerLegacyAutomationProposalRequiresRoutineReview(t *testing.T) {
	h := newHarness(t)
	// A butler turn is had by a person, so the caller acts for one.
	asksAsRoleOperator := h.console(runtimedb.RoleOperator, "mika")
	h.addPublisher("publisher-home")

	// A real domestic gateway over a fake ntfy, so the rule the model
	// wrote can actually act at the end.
	var published struct {
		Topic   string `json:"topic"`
		Message string `json:"message"`
	}
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &published) //nolint:errcheck
		w.Write([]byte(`{"id":"x"}`))    //nolint:errcheck
	}))
	defer ntfy.Close()
	ownExecutionFixture(t, h)
	actions := &domestic.Gateway{Authority: h.s.DB}
	actions.Register(&domestic.Notifier{Authority: h.s.DB, BaseURL: ntfy.URL})
	h.s.Automation.Actions = actions

	// The authoring seams, wired as cmd/keystone wires them.
	gw := butler.NewToolGateway(h.s.State, nil)
	gw.AutomationVocabulary = func() (butler.HouseholdVocabulary, error) {
		return butler.HouseholdVocabulary{
			EventTypes: []string{"person.arrived_home", "person.left_home"},
			Actions:    []butler.HouseholdAction{{Tool: "notifications.notify", Tier: "domestic"}},
		}, nil
	}
	var dryRunDocs []string
	gw.DryRunAutomation = func(ctx context.Context, doc string, days int) (string, error) {
		dryRunDocs = append(dryRunDocs, doc)
		rule, err := automation.ParseDraft(doc)
		if err != nil {
			return "", err
		}
		to := time.Now()
		replay, err := h.s.Automation.ReplayDraft(ctx, rule, h.s.Policy, "butler", "butler", to.AddDate(0, 0, -days), to, 0)
		if err != nil {
			return "", err
		}
		return replay.Summary(20), nil
	}
	sp := &scriptedProvider{turns: authoringTurns()}
	h.s.ButlerHost = &ButlerHost{Provider: sp, Gateway: gw, Mode: runtimedb.ButlerModeFull}

	// Two earlier arrivals, so the dry run has something to report.
	emit := func(id string, at time.Time) {
		t.Helper()
		env := household.Envelope{EventID: id, OccurredAt: at, Type: "person.arrived_home", Subject: "mika"}
		if resp := h.do(runtimedb.RolePublisher, http.MethodPost, "/api/v1/household/events", env, nil); resp.StatusCode != http.StatusCreated {
			t.Fatalf("emit %s = %d", id, resp.StatusCode)
		}
	}
	now := time.Now()
	emit("arrived-old-1", now.Add(-72*time.Hour))
	emit("arrived-old-2", now.Add(-48*time.Hour))

	// "Whenever I get home, ask whether I got the mail."
	var ask struct {
		Text    string   `json:"text"`
		Session string   `json:"sessionId"`
		Plans   []string `json:"planIds"`
		Errors  []string `json:"proposalErrors"`
	}
	if resp := h.doToken(asksAsRoleOperator, "POST", "/api/v1/butler/ask",
		map[string]string{"message": "Whenever I get home, ask whether I got the mail."}, &ask); resp.StatusCode != http.StatusOK {
		t.Fatalf("ask = %d", resp.StatusCode)
	}
	if len(ask.Plans) != 0 || !strings.Contains(strings.Join(ask.Errors, " "), "exact-version routine Plan") {
		t.Fatalf("legacy proposal was not refused clearly: %+v", ask)
	}
	if len(dryRunDocs) != 1 || !strings.Contains(dryRunDocs[0], "person.arrived_home") {
		t.Fatalf("dry runs = %+v", dryRunDocs)
	}
	plans, err := h.s.DB.ListPlans(10)
	if err != nil || len(plans) != 0 {
		t.Fatalf("legacy authoring persisted a Plan: %v %v", plans, err)
	}
	if _, err := h.s.Automation.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if published.Topic != "" {
		t.Fatalf("unreviewed rule acted: %+v", published)
	}
}
