package automation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// fakeEscalator records the packet and returns a scripted answer.
type fakeEscalator struct {
	reqs  []EscalationRequest
	reply string
	err   error
}

func (f *fakeEscalator) Escalate(_ context.Context, req EscalationRequest) (string, error) {
	f.reqs = append(f.reqs, req)
	return f.reply, f.err
}

// breakfastWithContext is the ADR-W022 §5 flow: leave home, no meal
// logged, ask an informed question phrased from the last few breakfasts.
func breakfastWithContext() resource.Resource {
	r := rule("breakfast-check",
		resource.AutomationCondition{Kind: "eventAbsent", EventType: "pantry.meal_logged", Window: "6h", SameSubject: true},
	)
	r.Automation.Context = []resource.AutomationContext{
		{Kind: "recentEvents", EventType: "pantry.meal_logged", SameSubject: true, Limit: 3},
	}
	r.Automation.Escalation = &resource.AutomationEscalation{
		Prompt: "Ask, in one sentence, whether today matched the usual breakfast.",
		Into:   "message",
	}
	return r
}

// TestEscalationReplacesOneParamAndNothingElse is the structural claim of
// H6: the model chooses words, never whether, what, or for whom.
func TestEscalationReplacesOneParamAndNothingElse(t *testing.T) {
	e, db := started(t, breakfastWithContext())
	acts := &fakeActions{}
	esc := &fakeEscalator{reply: "You usually have porridge about now — did you eat today?"}
	e.Actions, e.Escalator = acts, esc

	// Two breakfasts on earlier days, and today's departure with none.
	emit(t, db, "meal-1", "pantry.meal_logged", "mika", t0.Add(-48*time.Hour))
	emit(t, db, "meal-2", "pantry.meal_logged", "mika", t0.Add(-24*time.Hour))
	emit(t, db, "meal-sam", "pantry.meal_logged", "sam", t0.Add(-time.Hour))
	emit(t, db, "left-1", "person.left_home", "mika", t0)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(acts.calls) != 1 {
		t.Fatalf("dispatched %d times", len(acts.calls))
	}
	c := acts.calls[0]
	if c.Params["message"] != esc.reply {
		t.Fatalf("message = %q, want the Butler's wording", c.Params["message"])
	}
	// Everything the rule already settled is untouched.
	if c.Tool != "notifications.notify" || c.Tier != "domestic" || c.Params["topic"] != "household" ||
		c.Subject != "mika" || c.Rule != "breakfast-check" {
		t.Fatalf("escalation changed more than the wording: %+v", c)
	}
	if len(c.Params) != 2 {
		t.Fatalf("escalation introduced a param: %v", c.Params)
	}

	// The packet: the rule's instruction, its own wording as the
	// fallback, and only the context it declared.
	if len(esc.reqs) != 1 {
		t.Fatalf("escalated %d times", len(esc.reqs))
	}
	req := esc.reqs[0]
	if req.Rule != "breakfast-check" || !strings.HasPrefix(req.Instruction, "Ask, in one sentence") ||
		req.Fallback != "Did you have breakfast?" {
		t.Fatalf("request = %+v", req)
	}
	if len(req.Facts) != 1 {
		t.Fatalf("facts = %+v", req.Facts)
	}
	f := req.Facts[0]
	if f.Label != "The most recent pantry.meal_logged events for mika" {
		t.Fatalf("label = %q", f.Label)
	}
	// Targeted retrieval, not history: this subject's meals, newest
	// first, and nobody else's.
	lines := strings.Split(f.Content, "\n")
	if len(lines) != 2 {
		t.Fatalf("content has %d lines, want mika's two meals:\n%s", len(lines), f.Content)
	}
	if !strings.Contains(lines[0], "3 Sep") || !strings.Contains(lines[1], "2 Sep") {
		t.Fatalf("facts are not newest-first in the household's clock:\n%s", f.Content)
	}
	if strings.Contains(f.Content, "sam") {
		t.Fatalf("another member's meal leaked into the packet:\n%s", f.Content)
	}

	// Recorded, without repeating the wording that is already on the
	// acted record beside it.
	escalated, _ := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{Type: TypeEscalated})
	if len(escalated) != 1 || !strings.Contains(string(escalated[0].Payload), `"into":"message"`) ||
		!strings.Contains(string(escalated[0].Payload), `"facts":1`) {
		t.Fatalf("escalated record = %+v", escalated)
	}
	if strings.Contains(string(escalated[0].Payload), "porridge") {
		t.Fatalf("the escalation record duplicates the wording: %s", escalated[0].Payload)
	}
}

func TestEscalationFailureFallsBackAndSaysSo(t *testing.T) {
	e, db := started(t, breakfastWithContext())
	acts := &fakeActions{}
	e.Actions = acts
	e.Escalator = &fakeEscalator{err: errors.New("provider unreachable")}

	emit(t, db, "left-1", "person.left_home", "mika", t0)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(acts.calls) != 1 || acts.calls[0].Params["message"] != "Did you have breakfast?" {
		t.Fatalf("a failed escalation did not fall back: %+v", acts.calls)
	}
	failed, _ := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{Type: TypeEscalationFailed})
	if len(failed) != 1 || !strings.Contains(string(failed[0].Payload), "provider unreachable") ||
		!strings.Contains(string(failed[0].Payload), "own wording") {
		t.Fatalf("failure record = %+v", failed)
	}

	// An escalator that answers with nothing usable is a failure too,
	// not a silent send of the empty string.
	e2, db2 := started(t, breakfastWithContext())
	acts2 := &fakeActions{}
	e2.Actions, e2.Escalator = acts2, &fakeEscalator{reply: "   \n\t "}
	emit(t, db2, "left-1", "person.left_home", "mika", t0)
	if _, err := e2.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(acts2.calls) != 1 || acts2.calls[0].Params["message"] != "Did you have breakfast?" {
		t.Fatalf("a blank answer was sent: %+v", acts2.calls)
	}
	if got, _ := db2.QueryHouseholdEvents(runtimedb.HouseholdQuery{Type: TypeEscalationFailed}); len(got) != 1 {
		t.Fatalf("a blank answer was not recorded as a failure: %+v", got)
	}
}

// TestEscalatedTextIsBoundedAtTheSeam: an Escalator is an interface, so
// the engine bounds what comes back rather than trusting whatever is
// wired into it — the Butler's own bound is the other side of the same
// wall, not a substitute for this one.
func TestEscalatedTextIsBoundedAtTheSeam(t *testing.T) {
	e, db := started(t, breakfastWithContext())
	acts := &fakeActions{}
	e.Actions = acts
	e.Escalator = &fakeEscalator{reply: "line one\nline two\x00\x07  padded   " + strings.Repeat("very long ", 400)}

	emit(t, db, "left-1", "person.left_home", "mika", t0)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	msg := acts.calls[0].Params["message"]
	if len(msg) > resource.MaxAutomationParamBytes {
		t.Fatalf("message is %d bytes, over the %d bound", len(msg), resource.MaxAutomationParamBytes)
	}
	if strings.ContainsAny(msg, "\x00\x07\n") {
		t.Fatalf("control characters reached the action: %q", msg)
	}
	if !strings.HasPrefix(msg, "line one line two padded") {
		t.Fatalf("message = %q", msg)
	}
}

// A rule with no context providers still escalates — the packet just
// carries no facts, and the Butler is told so rather than left to guess.
func TestEscalationWithoutContextProviders(t *testing.T) {
	r := breakfastWithContext()
	r.Automation.Context = nil
	e, db := started(t, r)
	esc := &fakeEscalator{reply: "Did you manage breakfast?"}
	e.Actions, e.Escalator = &fakeActions{}, esc

	emit(t, db, "left-1", "person.left_home", "mika", t0)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(esc.reqs) != 1 || len(esc.reqs[0].Facts) != 0 {
		t.Fatalf("request = %+v", esc.reqs)
	}
}

// A non-escalating rule never reaches the Butler at all, however it is
// configured. This is what "a rule with no escalation never involves a
// model" means in code.
func TestNonEscalatingRuleNeverAsks(t *testing.T) {
	e, db := started(t, mailRule())
	esc := &fakeEscalator{reply: "should not be used"}
	e.Actions, e.Escalator = &fakeActions{}, esc

	emit(t, db, "arrived-1", "person.arrived_home", "mika", t0)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(esc.reqs) != 0 {
		t.Fatalf("a rule with no escalation asked the Butler: %+v", esc.reqs)
	}
	for _, typ := range []string{TypeEscalated, TypeEscalationFailed, TypeEscalationSkipped} {
		if got, _ := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{Type: typ}); len(got) != 0 {
			t.Fatalf("%s recorded for a non-escalating rule", typ)
		}
	}
}
