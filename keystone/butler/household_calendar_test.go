package butler

import (
	"context"
	"strings"
	"testing"
)

// The calendar went unused on its first day: asked "can you see if the
// calendar shows any events?", a family Butler called get_household_events
// — the timeline — found nothing, and said the calendar was empty while
// three events sat on it. The actions were one list_household_actions
// call away, and the model never made it.
//
// So the actions a household has wired are named, with what each takes,
// in do_household_action's own description — where the model chooses —
// and the timeline says it is not a calendar.
func TestTheCalendarIsVisibleWhereTheModelChooses(t *testing.T) {
	seams, _ := householdSeams(t)
	seams.Vocabulary = func() (HouseholdVocabulary, error) {
		return HouseholdVocabulary{Actions: []HouseholdAction{
			{Tool: "calendar.get_agenda", Tier: "read", Usage: "What is on. params: date (today | tomorrow | YYYY-MM-DD)"},
			{Tool: "calendar.delete_event", Tier: "confirm", Usage: "Remove an event. params: title, date"},
			{Tool: "infra.restart_container", Tier: "high", Usage: "must never be offered"},
		}}, nil
	}
	gw := newTestHouseholdGateway("mika", seams)

	var do, events string
	for _, s := range gw.Specs() {
		switch s.Name {
		case "do_household_action":
			do = s.Description
		case "get_household_events":
			events = s.Description
		}
	}
	for _, want := range []string{
		"calendar.get_agenda: What is on. params: date (today | tomorrow | YYYY-MM-DD)",
		"calendar.delete_event: Remove an event",
	} {
		if !strings.Contains(do, want) {
			t.Errorf("do_household_action's description does not carry %q:\n%s", want, do)
		}
	}
	if strings.Contains(do, "infra.restart_container") {
		t.Error("an action above the family surface's ceiling was named in its tool description")
	}
	if !strings.Contains(events, "not a calendar") {
		t.Errorf("get_household_events does not say it is not the calendar:\n%s", events)
	}
	if !strings.Contains(HouseholdSystemPrompt, "calendar.get_agenda") {
		t.Error("the family prompt does not route calendar questions to the calendar actions")
	}

	out, err := gw.Execute(context.Background(), "list_household_actions", map[string]any{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "calendar.get_agenda (tier read): What is on.") {
		t.Errorf("list_household_actions promises each action's inputs and did not give them:\n%s", out.Content)
	}
}

// With nothing wired, the description is the plain one: no empty
// "Actions available" heading promising a list that is not there.
func TestNoActionsLeavesTheDescriptionAlone(t *testing.T) {
	seams, _ := householdSeams(t)
	seams.Vocabulary = func() (HouseholdVocabulary, error) { return HouseholdVocabulary{}, nil }
	for _, s := range newTestHouseholdGateway("mika", seams).Specs() {
		if s.Name == "do_household_action" && s.Description != householdDoSpec.Description {
			t.Errorf("with no actions the description changed:\n%s", s.Description)
		}
	}
}

// calendarGrantAuthorizer allows everything structural except the named
// tools, and records which tools it was asked about.
type calendarGrantAuthorizer struct {
	permitAll
	deny  map[string]bool
	asked []ToolAuthorityQuery
}

func (a *calendarGrantAuthorizer) AuthorizeTool(q ToolAuthorityQuery) (ToolDecision, error) {
	a.asked = append(a.asked, q)
	if a.deny[q.Tool] {
		return ToolDecision{Allowed: false, Reason: q.Tool + " has not been given"}, nil
	}
	return ToolDecision{Allowed: q.Structural}, nil
}

// ADR-W034 A5: an Owner can give an Agent read-only calendar access.
// A calendar action it was not given is neither offered nor run, an
// action it was given still runs, and non-calendar actions are not
// subject to the per-action check.
func TestCalendarActionsAreGrantedPerAction(t *testing.T) {
	seams, dispatched := householdSeams(t)
	seams.Vocabulary = func() (HouseholdVocabulary, error) {
		return HouseholdVocabulary{Actions: []HouseholdAction{
			{Tool: "calendar.get_agenda", Tier: "read"},
			{Tool: "calendar.create_event", Tier: "domestic"},
			{Tool: "calendar.delete_event", Tier: "confirm"},
			{Tool: "notifications.notify", Tier: "domestic"},
		}}, nil
	}
	auth := &calendarGrantAuthorizer{deny: map[string]bool{
		"calendar.create_event": true, "calendar.delete_event": true,
	}}
	gw := newTestHouseholdGateway("mika", seams)
	gw.SetAuthorizer(auth)
	ctx := context.Background()

	list, err := gw.Execute(ctx, "list_household_actions", map[string]any{}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"calendar.get_agenda", "notifications.notify"} {
		if !strings.Contains(list.Content, want) {
			t.Errorf("%s was not offered:\n%s", want, list.Content)
		}
	}
	for _, not := range []string{"calendar.create_event", "calendar.delete_event"} {
		if strings.Contains(list.Content, not) {
			t.Errorf("%s was offered to an Agent that was not given it:\n%s", not, list.Content)
		}
	}

	before := len(*dispatched)
	out, err := gw.Execute(ctx, "do_household_action", map[string]any{"tool": "calendar.delete_event"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Rejected || !strings.Contains(out.Content, "was not run") || len(*dispatched) != before {
		t.Fatalf("an ungranted calendar action ran: %+v (dispatched %d)", out, len(*dispatched)-before)
	}

	for _, tool := range []string{"calendar.get_agenda", "notifications.notify"} {
		out, err := gw.Execute(ctx, "do_household_action", map[string]any{"tool": tool}, false)
		if err != nil || out.Rejected {
			t.Fatalf("%s should run: %+v %v", tool, out, err)
		}
	}
	for _, q := range auth.asked {
		if q.Tool == "notifications.notify" {
			t.Error("a non-calendar action was put through the per-action check")
		}
		if strings.HasPrefix(q.Tool, "calendar.") && (q.Service != "calendar" || q.ToolClass != string(AgentClassMember) || q.OptIn || !q.Structural) {
			t.Errorf("calendar query mis-shaped: %+v", q)
		}
	}
}
