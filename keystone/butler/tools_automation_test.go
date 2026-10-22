package butler

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGetAutomationSchemaDescribesTheRealVocabularies(t *testing.T) {
	gw := newTestToolGateway(newTestStore(t), nil)
	gw.AutomationVocabulary = func() (HouseholdVocabulary, error) {
		return HouseholdVocabulary{
			EventTypes: []string{"person.left_home", "person.arrived_home", "pantry.meal_logged"},
			Actions:    []HouseholdAction{{Tool: "notifications.notify", Tier: "domestic"}},
		}, nil
	}
	out, err := gw.Dispatch(context.Background(), "get_automation_schema", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The closed grammar, read from keystone/resource rather than
	// restated here — a description that drifts from the validator
	// teaches a model to write rules that get rejected.
	for _, want := range []string{
		"eventAbsent", "eventPresent", "timeOfDay", "weekday", "cooldown",
		"recentEvents", "domestic", "{{ .Subject }}", "escalation.into",
	} {
		if !strings.Contains(out.Content, want) {
			t.Errorf("schema omits %q", want)
		}
	}
	// And the deployment-specific half, sorted so it reads the same way
	// every time.
	if !strings.Contains(out.Content, "pantry.meal_logged, person.arrived_home, person.left_home") {
		t.Errorf("event types not listed in order:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "notifications.notify (tier domestic)") {
		t.Errorf("actions not listed:\n%s", out.Content)
	}
	// The one thing the schema must NOT do is imply a rule can carry an
	// infrastructure action.
	if !strings.Contains(out.Content, "only run read or domestic actions") {
		t.Errorf("schema does not state the tier limit:\n%s", out.Content)
	}
}

func TestGetAutomationSchemaWithoutADeployment(t *testing.T) {
	gw := newTestToolGateway(newTestStore(t), nil) // no vocabulary wired
	out, err := gw.Dispatch(context.Background(), "get_automation_schema", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "not available to me") {
		t.Fatalf("unwired schema did not say so:\n%s", out.Content)
	}

	// A deployment with nothing published and nothing wired says that
	// too, rather than inviting a rule that could never fire or act.
	gw.AutomationVocabulary = func() (HouseholdVocabulary, error) { return HouseholdVocabulary{}, nil }
	out, _ = gw.Dispatch(context.Background(), "get_automation_schema", nil)
	if !strings.Contains(out.Content, "declares any household event yet") ||
		!strings.Contains(out.Content, "No household action is wired") {
		t.Fatalf("empty deployment not reported:\n%s", out.Content)
	}

	gw.AutomationVocabulary = func() (HouseholdVocabulary, error) { return HouseholdVocabulary{}, errors.New("catalog unreadable") }
	out, _ = gw.Dispatch(context.Background(), "get_automation_schema", nil)
	if !strings.Contains(out.Content, "catalog unreadable") {
		t.Fatalf("vocabulary error not surfaced:\n%s", out.Content)
	}
}

func automationInput(name string) map[string]any {
	return map[string]any{
		"kind": "Automation", "name": name, "owner": "household/mika",
		"spec": map[string]any{
			"description": "Ask whether the mail was collected on arriving home",
			"trigger":     map[string]any{"eventType": "person.arrived_home", "subject": "mika"},
			"conditions":  []any{map[string]any{"kind": "cooldown", "window": "20h"}},
			"action": map[string]any{
				"tool": "notifications.notify", "tier": "domestic",
				"params": map[string]any{"topic": "household", "message": "Did you get the mail?"},
			},
		},
	}
}

func TestDryRunAutomation(t *testing.T) {
	gw := newTestToolGateway(newTestStore(t), nil)

	// Unwired: says so plainly rather than pretending a rule was checked.
	out, err := gw.Dispatch(context.Background(), "dry_run_automation", automationInput("mail-check"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "No household automation engine is configured") {
		t.Fatalf("unwired dry run:\n%s", out.Content)
	}

	var gotDoc string
	var gotDays int
	gw.DryRunAutomation = func(_ context.Context, doc string, days int) (string, error) {
		gotDoc, gotDays = doc, days
		return "Rule \"mail-check\" replayed: 3 matching event(s), 2 would have fired.", nil
	}
	in := automationInput("mail-check")
	in["days"] = float64(14) // as a provider hands numbers back
	out, err = gw.Dispatch(context.Background(), "dry_run_automation", in)
	if err != nil {
		t.Fatal(err)
	}
	if gotDays != 14 {
		t.Fatalf("days = %d, want 14", gotDays)
	}
	// The seam receives the rule as the same canonical document the
	// Statefile would hold, built by resource's own codec — not a
	// bespoke rendering that could accept what validation would not.
	if !strings.Contains(gotDoc, "kind: Automation") || !strings.Contains(gotDoc, "person.arrived_home") {
		t.Fatalf("document handed to the engine:\n%s", gotDoc)
	}
	// The result quotes household events back, so it is enveloped like
	// any other attacker-influenceable read.
	if !strings.Contains(out.Content, untrustedOpenTag) || !strings.Contains(out.Content, "2 would have fired") {
		t.Fatalf("dry run result:\n%s", out.Content)
	}

	// A draft that does not validate is rejected here, before any replay.
	bad := automationInput("mail-check")
	bad["spec"].(map[string]any)["trigger"] = map[string]any{"eventType": "arrived_home"}
	out, _ = gw.Dispatch(context.Background(), "dry_run_automation", bad)
	if !strings.Contains(out.Content, "error:") {
		t.Fatalf("an invalid draft was replayed anyway:\n%s", out.Content)
	}
	// And a different kind is not an automation, whatever it is called.
	wrong := map[string]any{
		"kind": "Volume", "name": "mail-check", "owner": "capability/test", "node": "n1",
		"spec": map[string]any{"name": "mail-check"},
	}
	out, _ = gw.Dispatch(context.Background(), "dry_run_automation", wrong)
	if !strings.Contains(out.Content, "is for kind Automation") {
		t.Fatalf("wrong kind accepted: %q", out.Content)
	}
}

// Authoring adds two READ tools and no new way to make anything happen.
// A QA-only session may describe and dry-run a rule; it still cannot
// propose one, which is the only step that produces a plan.
func TestAuthoringToolsAreReadOnly(t *testing.T) {
	gw := newTestToolGateway(newTestStore(t), nil)
	offered := map[string]bool{}
	for _, spec := range gw.ReadOnlyToolSpecs() {
		offered[spec.Name] = true
	}
	for _, want := range []string{"get_automation_schema", "dry_run_automation"} {
		if !offered[want] {
			t.Errorf("%s is withheld from a read-only session", want)
		}
		out, err := gw.DispatchReadOnly(context.Background(), want, automationInput("r"))
		if err != nil {
			t.Fatal(err)
		}
		if out.Rejected {
			t.Errorf("%s was refused in a read-only session", want)
		}
	}
	if offered["propose_change"] {
		t.Error("propose_change reached a read-only session")
	}
}
