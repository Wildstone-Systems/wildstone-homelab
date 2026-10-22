package crossapp_test

// Found in review of the ADR-W030 Part C branch: a step skipped by its
// own guard did not mark its result empty, so the NEXT guard saw
// nothing wrong and the chain ran on data nobody had fetched.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/crossapp"
)

// emptyThen answers "empty" for the named capability and "ok" for
// anything else, so a test can decide exactly where a chain breaks.
type emptyThen struct {
	empty  string
	asked  []string
	answer json.RawMessage
}

func (e *emptyThen) Ask(_ context.Context, _, _, capability, _ string, _ time.Duration) (string, json.RawMessage, time.Time, error) {
	e.asked = append(e.asked, capability)
	if capability == e.empty {
		return "empty", json.RawMessage(`[]`), time.Time{}, nil
	}
	return "ok", e.answer, time.Time{}, nil
}

// "If the calendar has nothing, do not read the pantry; if the pantry
// read produced nothing, do not send the message." Skipping the middle
// step has to carry through to the last one, or the household gets a
// message about a list that was never looked at.
func TestASkippedStepIsEmptyForTheStepsAfterIt(t *testing.T) {
	ask := &emptyThen{empty: "list_events", answer: json.RawMessage(`{"items":["milk"]}`)}
	runner := &crossapp.Runner{Integrations: ask}

	run, err := runner.Execute(context.Background(), crossapp.Routine{
		Name: "breakfast", Trigger: "person.left_home",
		Steps: []crossapp.Step{
			{Kind: crossapp.StepQuery, Connection: "calendar", Capability: "list_events", As: "events"},
			{Kind: crossapp.StepQuery, Connection: "pantry", Capability: "list_low", As: "low", When: "events"},
			{Kind: crossapp.StepNotify, Text: "you are low on things", When: "low"},
		},
	}, crossapp.Trigger{Type: "person.left_home"})
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range ask.asked {
		if c == "list_low" {
			t.Fatal("the guarded read ran even though the step it depends on was empty")
		}
	}
	if len(run.Messages) != 0 {
		t.Fatalf("a message went out about data that was never read: %q", run.Messages)
	}
	if len(run.Steps) != 3 {
		t.Fatalf("the run recorded %d steps, want all three so the household can see what was skipped", len(run.Steps))
	}
	for _, s := range run.Steps[1:] {
		if !s.Skipped {
			t.Fatalf("step %+v was not recorded as skipped", s)
		}
	}
	if run.State != crossapp.RunCompleted {
		t.Fatalf("run state = %q, want completed — skipping is not failing", run.State)
	}
}
