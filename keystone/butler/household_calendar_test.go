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
	gw := NewHouseholdGateway("mika", seams)

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
	for _, s := range NewHouseholdGateway("mika", seams).Specs() {
		if s.Name == "do_household_action" && s.Description != householdDoSpec.Description {
			t.Errorf("with no actions the description changed:\n%s", s.Description)
		}
	}
}
