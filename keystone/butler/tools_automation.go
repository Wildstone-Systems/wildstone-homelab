package butler

// Natural-language rule authoring (ADR-W024 H7): the two read tools that
// let the Butler turn "whenever I get home, ask whether I got the mail"
// into an Automation the runtime already understands.
//
// There is no second execution path here, and that is the whole design.
// The Butler does not gain a way to make things happen; it gains a way to
// WRITE DOWN what should happen, in the same schema a human would write,
// checked by the same validator and the same policy engine, dry-run
// through the same replay, and proposed through the same propose_change
// that produces a plan a human approves. Everything the Butler authors is
// a document until somebody signs it.
//
// get_automation_schema exists because a model that guesses at a closed
// vocabulary writes rules that get rejected. It reads the real
// vocabularies out of keystone/resource and the real event types and
// actions out of the deployment, so it cannot drift from what validation
// and the domestic gateway will actually accept.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// HouseholdAction is one action a rule may name on this deployment: a
// declared, registered tool and the tier it runs at.
type HouseholdAction struct {
	Tool string
	Tier string
	// Usage is what the action takes, in plain words, when its executor
	// describes itself; "" otherwise.
	Usage string
}

// HouseholdVocabulary is the deployment-specific half of the authoring
// schema — what publishers actually send and what the domestic gateway
// can actually run here. The closed grammar (condition kinds, tiers) is
// compile-time and read from keystone/resource.
type HouseholdVocabulary struct {
	EventTypes []string
	Actions    []HouseholdAction
}

var automationSchemaSpec = provider.Tool{
	Name: "get_automation_schema",
	Description: "Describe how to write a household Automation on THIS deployment: the closed " +
		"vocabularies for triggers, conditions, context and action tiers, plus the household " +
		"event types publishers actually send and the action tools that are actually wired up. " +
		"Call this BEFORE drafting a rule — the vocabularies are closed, and a rule that uses " +
		"a word outside them is rejected by validation rather than interpreted.",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
}

var dryRunAutomationSpec = provider.Tool{
	Name: "dry_run_automation",
	Description: "Replay a draft household rule against real recorded events and report what it " +
		"WOULD have done — how often it would have fired, and why it did or did not each time. " +
		"Writes nothing, sends nothing, and notifies nobody. Takes the same input as " +
		"propose_change. Always dry-run a rule before proposing it: a rule that never fires, or " +
		"fires every hour, is a rule the household should hear about before they approve it.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind":  map[string]any{"type": "string", "description": `Must be "Automation".`},
			"name":  map[string]any{"type": "string", "description": "Rule name, e.g. mail-check."},
			"owner": map[string]any{"type": "string", "description": "household, household/<member>, or capability/<name>."},
			"spec":  map[string]any{"type": "object", "description": "The rule: description, trigger, conditions, context, escalation, action."},
			"days":  map[string]any{"type": "integer", "description": "How many days of history to replay. Default 7."},
		},
		"required": []string{"kind", "name", "owner", "spec"},
	},
}

func handleGetAutomationSchema(_ context.Context, gw *ToolGateway, _ map[string]any) (ToolOutcome, error) {
	var b strings.Builder
	b.WriteString(`A household Automation is a rule the deterministic engine runs after a human
approves it. Write it as this document:

  kind: Automation
  metadata: { name: <kebab-name>, owner: household | household/<member> | capability/<name> }
  spec:
    description: <one sentence a reviewer reads before approving>
    trigger:   { eventType: <event type>, subject: <optional member/device> }
    conditions: [ ... ]        # ALL must hold; evaluated in order, in code
    context:   [ ... ]         # optional; only for rules that escalate
    escalation: { prompt: <what to ask the Butler for>, into: <action param> }
    action:    { tool: <capability.tool>, tier: <tier>, params: { ... } }

Rules of the schema, all enforced by validation:
`)
	fmt.Fprintf(&b, "  condition kinds: %s\n", strings.Join(resource.AutomationConditionKinds(), ", "))
	b.WriteString("    eventAbsent / eventPresent: eventType, window (e.g. 6h), sameSubject\n")
	b.WriteString("    timeOfDay: after and/or before, as HH:MM in the household's own zone\n")
	fmt.Fprintf(&b, "    weekday: days from %s\n", strings.Join(resource.AutomationWeekdays(), ", "))
	b.WriteString("    cooldown: window — how long after this rule last fired before it may fire again\n")
	fmt.Fprintf(&b, "  context kinds: %s (eventType, sameSubject, limit 1..%d)\n",
		strings.Join(resource.AutomationContextKinds(), ", "), resource.MaxAutomationContextLimit)
	fmt.Fprintf(&b, "  action tiers accepted by the schema: %s\n", strings.Join(resource.AutomationTiers(), ", "))
	b.WriteString("  BUT a household rule may only run read or domestic actions. Anything that would\n" +
		"  change infrastructure is a plan, not a rule, and policy rejects it.\n")
	fmt.Fprintf(&b, "  windows are Go durations (30m, 6h, 72h), at most %s\n", resource.MaxAutomationWindow)
	b.WriteString(`  params may reference the trigger: {{ .Subject }}, {{ .Time }}, {{ .Date }},
  {{ .EventType }}, {{ .Payload.<field> }}. A name that does not exist fails the action.
  escalation.into must name a param that already exists and already reads well:
  the Butler replaces that text, and it is what the household receives if no
  model answers. A rule with no escalation never involves a model at all.

`)

	if gw.AutomationVocabulary == nil {
		b.WriteString("This deployment's event types and action tools are not available to me, so " +
			"check with the household which events their integrations publish before drafting.\n")
		return ToolOutcome{Content: b.String()}, nil
	}
	vocab, err := gw.AutomationVocabulary()
	if err != nil {
		return ToolOutcome{}, err
	}
	types := append([]string(nil), vocab.EventTypes...)
	sort.Strings(types)
	if len(types) == 0 {
		b.WriteString("No package on this deployment declares any household event yet, so no trigger\n" +
			"would ever match. Say so rather than drafting a rule that cannot fire.\n")
	} else {
		fmt.Fprintf(&b, "Event types published on this deployment: %s\n", strings.Join(types, ", "))
	}
	if len(vocab.Actions) == 0 {
		b.WriteString("No household action is wired up on this deployment, so a rule has nothing to do.\n")
	} else {
		b.WriteString("Actions available here:\n")
		actions := append([]HouseholdAction(nil), vocab.Actions...)
		sort.Slice(actions, func(i, j int) bool { return actions[i].Tool < actions[j].Tool })
		for _, a := range actions {
			fmt.Fprintf(&b, "  %s (tier %s)\n", a.Tool, a.Tier)
		}
	}
	return ToolOutcome{Content: b.String()}, nil
}

func handleDryRunAutomation(ctx context.Context, gw *ToolGateway, input map[string]any) (ToolOutcome, error) {
	if gw.DryRunAutomation == nil {
		return ToolOutcome{Content: "No household automation engine is configured on this deployment, so a rule cannot be dry-run here."}, nil
	}
	res, err := resourceFromSpecDiffInput(input)
	if err != nil {
		return ToolOutcome{}, err
	}
	if res.Kind != resource.KindAutomation {
		return ToolOutcome{}, fmt.Errorf("dry_run_automation is for kind Automation, not %s", res.Kind)
	}
	doc, err := resource.Marshal(res)
	if err != nil {
		return ToolOutcome{}, err
	}
	days := intInput(input, "days")
	summary, err := gw.DryRunAutomation(ctx, string(doc), days)
	if err != nil {
		return ToolOutcome{}, err
	}
	return ToolOutcome{Content: summary}, nil
}

// intInput reads an integer tool-call argument. Providers hand numbers
// back as float64 through JSON, so both shapes are accepted.
func intInput(input map[string]any, key string) int {
	switch v := input[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}
