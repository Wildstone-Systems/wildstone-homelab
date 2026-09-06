package api

// Natural-language rule authoring, end to end (ADR-W024 H7).
//
// The claim under test is "the AI writes rules the engine already
// understands; no second execution path". So the test does not stop at a
// plausible-looking document: it drives a real session through the real
// ask endpoint, has a human approve the plan that comes out, and then
// lets the deterministic engine run what the model wrote — unchanged,
// through the same code path a hand-written rule takes.

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
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
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

func TestButlerAuthorsARuleTheEngineThenRuns(t *testing.T) {
	h := newHarness(t)
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
	actions := &domestic.Gateway{}
	actions.Register(&domestic.Notifier{BaseURL: ntfy.URL})
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
	}
	if resp := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/butler/ask",
		map[string]string{"message": "Whenever I get home, ask whether I got the mail."}, &ask); resp.StatusCode != http.StatusOK {
		t.Fatalf("ask = %d", resp.StatusCode)
	}
	if len(ask.Plans) != 1 {
		t.Fatalf("the session produced %d plan(s), want 1", len(ask.Plans))
	}
	// It dry-ran before proposing, against the real history.
	if len(dryRunDocs) != 1 || !strings.Contains(dryRunDocs[0], "person.arrived_home") {
		t.Fatalf("dry runs = %+v", dryRunDocs)
	}

	// What landed is a proposal, not a rule: the Butler holds no approve
	// verb, and authoring did not give it one.
	rec, err := h.s.DB.GetPlan(ask.Plans[0])
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != runtimedb.PlanProposed || rec.Principal != "butler" || rec.ActingFor != "op" {
		t.Fatalf("plan = %+v", rec)
	}
	if resp := h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/plans/"+rec.ID+"/approve", nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the butler approved its own rule: %d", resp.StatusCode)
	}
	// Nothing runs until a human signs it.
	if _, err := h.s.Automation.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if published.Topic != "" {
		t.Fatalf("an unapproved rule acted: %+v", published)
	}

	if resp := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/plans/"+rec.ID+"/approve", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("approve = %d", resp.StatusCode)
	}

	// The rule the model wrote is now an ordinary Statefile resource,
	// indistinguishable from a hand-written one.
	var view resourceView
	if resp := h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/resources/Automation/mail-check", nil, &view); resp.StatusCode != http.StatusOK {
		t.Fatalf("get rule = %d", resp.StatusCode)
	}
	stored, err := resource.Unmarshal([]byte(view.YAML))
	if err != nil {
		t.Fatalf("the stored rule does not parse: %v\n%s", err, view.YAML)
	}
	if stored.Automation.Trigger.EventType != "person.arrived_home" ||
		stored.Automation.Action.Params["message"] != "Did you get the mail?" {
		t.Fatalf("stored rule = %+v", stored.Automation)
	}

	// And the deterministic engine runs it — the same engine, the same
	// path, no second execution route for a rule an AI happened to write.
	emit("arrived-now", now.Add(-time.Minute))
	out, err := h.s.Automation.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.Fired != 1 {
		t.Fatalf("tick = %+v", out)
	}
	if published.Topic != "household-mika" || published.Message != "Did you get the mail?" {
		t.Fatalf("published = %+v", published)
	}
}
