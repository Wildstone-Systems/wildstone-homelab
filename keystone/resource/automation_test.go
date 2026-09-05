package resource

import (
	"strings"
	"testing"
)

// breakfastRule is the ADR-W022 H6 target as a resource: the shape every
// layer above has to carry.
func breakfastRule() Resource {
	return Resource{
		ApiVersion: ApiVersion,
		Kind:       KindAutomation,
		Metadata:   Metadata{Name: "breakfast-check", Owner: "household/mika"},
		Automation: &AutomationSpec{
			Description: "Ask about breakfast when leaving home without a meal logged",
			Trigger:     AutomationTrigger{EventType: "person.left_home", Subject: "mika"},
			Conditions: []AutomationCondition{
				{Kind: "eventAbsent", EventType: "pantry.meal_logged", Window: "6h", SameSubject: true},
				{Kind: "timeOfDay", After: "05:00", Before: "11:00"},
				{Kind: "weekday", Days: []string{"mon", "tue", "wed", "thu", "fri"}},
				{Kind: "cooldown", Window: "20h"},
			},
			Context: []AutomationContext{
				{Kind: "recentEvents", EventType: "pantry.meal_logged", SameSubject: true, Limit: 3},
			},
			Escalation: &AutomationEscalation{Prompt: "Ask, in one sentence, whether today matched the usual breakfast."},
			Action: AutomationAction{
				Tool: "notifications.notify", Tier: "domestic",
				Params: map[string]string{"topic": "{{ .Subject }}", "message": "Did you have breakfast?"},
			},
		},
	}
}

func TestAutomationYAMLRoundTrip(t *testing.T) {
	in := breakfastRule()
	data, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, data)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("Validate after round trip: %v", err)
	}
	if out.Automation == nil || out.Automation.Trigger.EventType != "person.left_home" ||
		len(out.Automation.Conditions) != 4 || out.Automation.Conditions[0].Window != "6h" ||
		out.Automation.Escalation == nil || out.Automation.Action.Params["topic"] != "{{ .Subject }}" {
		t.Fatalf("round trip lost fields:\n%s", data)
	}
	// Written the way an author writes it, with the closed vocabulary.
	doc := `apiVersion: wildstone.dev/v1
kind: Automation
metadata: { name: mail-check, owner: household }
spec:
  description: Ask whether the mail was collected on arriving home
  trigger: { eventType: person.arrived_home }
  conditions:
    - { kind: timeOfDay, after: "12:00" }
    - { kind: cooldown, window: 20h }
  action: { tool: notifications.notify, tier: domestic, params: { message: "Did you get the mail?" } }
`
	r, err := Unmarshal([]byte(doc))
	if err != nil {
		t.Fatalf("Unmarshal doc: %v", err)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate doc: %v", err)
	}
	if r.Automation.Disabled || r.Automation.Escalation != nil {
		t.Fatalf("defaults: %+v", r.Automation)
	}
	// Unknown keys are refused, as for every kind.
	if _, err := Unmarshal([]byte(strings.Replace(doc, "cooldown, window: 20h", "cooldown, window: 20h, retries: 3", 1))); err == nil {
		t.Fatal("unknown condition key accepted")
	}
}

func TestAutomationValidation(t *testing.T) {
	good := breakfastRule()
	if err := good.Validate(); err != nil {
		t.Fatalf("well-formed rule: %v", err)
	}
	for name, tc := range map[string]struct {
		mut   func(*Resource)
		field string
	}{
		"node set":           {func(r *Resource) { r.Metadata.Node = "n1" }, "metadata.node"},
		"owner system":       {func(r *Resource) { r.Metadata.Owner = "system" }, "metadata.owner"},
		"owner bare cap":     {func(r *Resource) { r.Metadata.Owner = "photos" }, "metadata.owner"},
		"owner bad member":   {func(r *Resource) { r.Metadata.Owner = "household/Mika!" }, "metadata.owner"},
		"no description":     {func(r *Resource) { r.Automation.Description = "  " }, "spec.description"},
		"flat trigger":       {func(r *Resource) { r.Automation.Trigger.EventType = "left_home" }, "spec.trigger.eventType"},
		"subject with space": {func(r *Resource) { r.Automation.Trigger.Subject = "mi key" }, "spec.trigger.subject"},
		"unknown condition":  {func(r *Resource) { r.Automation.Conditions[0].Kind = "moonPhase" }, "spec.conditions[0].kind"},
		"absent no window":   {func(r *Resource) { r.Automation.Conditions[0].Window = "" }, "spec.conditions[0].window"},
		"absent bad window":  {func(r *Resource) { r.Automation.Conditions[0].Window = "6 hours" }, "spec.conditions[0].window"},
		"absent huge window": {func(r *Resource) { r.Automation.Conditions[0].Window = "1000h" }, "spec.conditions[0].window"},
		"absent neg window":  {func(r *Resource) { r.Automation.Conditions[0].Window = "-1h" }, "spec.conditions[0].window"},
		"absent bad type":    {func(r *Resource) { r.Automation.Conditions[0].EventType = "Meal" }, "spec.conditions[0].eventType"},
		"absent stray field": {func(r *Resource) { r.Automation.Conditions[0].After = "05:00" }, "spec.conditions[0]"},
		"time neither bound": {func(r *Resource) { r.Automation.Conditions[1].After, r.Automation.Conditions[1].Before = "", "" }, "spec.conditions[1]"},
		"time bad clock":     {func(r *Resource) { r.Automation.Conditions[1].After = "25:00" }, "spec.conditions[1].after"},
		"time stray field":   {func(r *Resource) { r.Automation.Conditions[1].Window = "1h" }, "spec.conditions[1]"},
		"weekday none":       {func(r *Resource) { r.Automation.Conditions[2].Days = nil }, "spec.conditions[2].days"},
		"weekday bad":        {func(r *Resource) { r.Automation.Conditions[2].Days = []string{"monday"} }, "spec.conditions[2].days"},
		"weekday dup":        {func(r *Resource) { r.Automation.Conditions[2].Days = []string{"mon", "mon"} }, "spec.conditions[2].days"},
		"cooldown no window": {func(r *Resource) { r.Automation.Conditions[3].Window = "" }, "spec.conditions[3].window"},
		"cooldown stray":     {func(r *Resource) { r.Automation.Conditions[3].SameSubject = true }, "spec.conditions[3]"},
		"context unknown":    {func(r *Resource) { r.Automation.Context[0].Kind = "wholeHistory" }, "spec.context[0].kind"},
		"context bad type":   {func(r *Resource) { r.Automation.Context[0].EventType = "" }, "spec.context[0].eventType"},
		"context limit zero": {func(r *Resource) { r.Automation.Context[0].Limit = 0 }, "spec.context[0].limit"},
		"context limit big":  {func(r *Resource) { r.Automation.Context[0].Limit = MaxAutomationContextLimit + 1 }, "spec.context[0].limit"},
		"escalation empty":   {func(r *Resource) { r.Automation.Escalation.Prompt = "" }, "spec.escalation.prompt"},
		// An escalation must name a param that exists and already
		// carries serviceable wording: it is both what a reviewer sees
		// and what the household receives when no model answers.
		"into unknown param":   {func(r *Resource) { r.Automation.Escalation.Into = "nope" }, "spec.escalation.into"},
		"into blank param":     {func(r *Resource) { r.Automation.Action.Params["message"] = "  " }, "spec.escalation.into"},
		"into missing default": {func(r *Resource) { delete(r.Automation.Action.Params, "message") }, "spec.escalation.into"},
		"into bad identifier":  {func(r *Resource) { r.Automation.Escalation.Into = "a b" }, "spec.escalation.into"},
		"escalation long":      {func(r *Resource) { r.Automation.Escalation.Prompt = strings.Repeat("x", MaxAutomationPromptBytes+1) }, "spec.escalation.prompt"},
		"tool no capability":   {func(r *Resource) { r.Automation.Action.Tool = "notify" }, "spec.action.tool"},
		"tool bad chars":       {func(r *Resource) { r.Automation.Action.Tool = "Notifications.Notify" }, "spec.action.tool"},
		"tier unknown":         {func(r *Resource) { r.Automation.Action.Tier = "critical" }, "spec.action.tier"},
		"tier empty":           {func(r *Resource) { r.Automation.Action.Tier = "" }, "spec.action.tier"},
		"param key spaced":     {func(r *Resource) { r.Automation.Action.Params["bad key"] = "x" }, "spec.action.params"},
		"param value long": {func(r *Resource) {
			r.Automation.Action.Params["message"] = strings.Repeat("m", MaxAutomationParamBytes+1)
		}, "spec.action.params.message"},
		"container spec too": {func(r *Resource) { r.Container = &ContainerSpec{} }, "spec"},
	} {
		t.Run(name, func(t *testing.T) {
			r := breakfastRule()
			tc.mut(&r)
			err := r.Validate()
			ve, ok := err.(*ValidationError)
			if !ok {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if ve.Field != tc.field {
				t.Fatalf("field = %q (%s), want %q", ve.Field, ve.Msg, tc.field)
			}
		})
	}
	// The schema accepts the whole tier vocabulary; which tiers a rule
	// may carry is policy's call (ADR-W022 A5), the Route.auth precedent.
	for _, tier := range []string{"read", "low", "medium", "high", "domestic"} {
		r := breakfastRule()
		r.Automation.Action.Tier = tier
		if err := r.Validate(); err != nil {
			t.Errorf("tier %s refused by the schema: %v", tier, err)
		}
	}
	// Into defaults to "message", and naming another existing param is
	// fine — the Butler replaces a param, it never introduces one.
	named := breakfastRule()
	named.Automation.Action.Params["headline"] = "Breakfast"
	named.Automation.Escalation.Into = "headline"
	if err := named.Validate(); err != nil {
		t.Fatalf("named into: %v", err)
	}
	if named.Automation.EscalationParam() != "headline" {
		t.Fatalf("EscalationParam = %q", named.Automation.EscalationParam())
	}
	if breakfastRule().Automation.EscalationParam() != DefaultEscalationParam {
		t.Fatal("an escalation with no into does not default to message")
	}
	noEscalation := breakfastRule()
	noEscalation.Automation.Escalation = nil
	if noEscalation.Automation.EscalationParam() != "" {
		t.Fatal("a rule with no escalation reported an escalation param")
	}

	// A rule with no conditions, context, or escalation is the minimum.
	r := breakfastRule()
	r.Automation.Conditions, r.Automation.Context, r.Automation.Escalation = nil, nil, nil
	if err := r.Validate(); err != nil {
		t.Fatalf("minimal rule: %v", err)
	}
	if !NodeScoped(KindVolume) || NodeScoped(KindAutomation) {
		t.Fatal("Automation must not be node-scoped")
	}
}
