package automation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// fakeActions records dispatches and can fail on demand.
type fakeActions struct {
	calls []domestic.Call
	err   error
}

func (f *fakeActions) Dispatch(_ context.Context, c domestic.Call) (domestic.Result, error) {
	f.calls = append(f.calls, c)
	if f.err != nil {
		return domestic.Result{}, f.err
	}
	return domestic.Result{Summary: "ok"}, nil
}

func TestRenderParams(t *testing.T) {
	ev := runtimedb.HouseholdEvent{
		EventID: "ha-1", Type: "person.arrived_home", Subject: "mika",
		CorrelationID: "corr-1", OccurredAt: t0,
		Payload: json.RawMessage(`{"zone":"home","confidence":0.9}`),
	}
	got, err := RenderParams(map[string]string{
		"topic":   "household-{{ .Subject }}",
		"message": "{{ .Rule }} at {{ .Time }} on {{ .Date }}: {{ .EventType }} ({{ .Payload.zone }})",
		"plain":   "no templates here",
	}, ev, "mail-check", london)
	if err != nil {
		t.Fatalf("RenderParams: %v", err)
	}
	if got["topic"] != "household-mika" {
		t.Fatalf("topic = %q", got["topic"])
	}
	if got["message"] != "mail-check at 07:30 on 2026-09-04: person.arrived_home (home)" {
		t.Fatalf("message = %q", got["message"])
	}
	if got["plain"] != "no templates here" {
		t.Fatalf("plain = %q", got["plain"])
	}

	// A typo must fail the action, not send "<no value>" to a person.
	if _, err := RenderParams(map[string]string{"m": "{{ .Payload.typo }}"}, ev, "r", london); err == nil {
		t.Fatal("a missing payload key rendered without error")
	}
	if _, err := RenderParams(map[string]string{"m": "{{ .Nope }}"}, ev, "r", london); err == nil {
		t.Fatal("an unknown field rendered without error")
	}
	if _, err := RenderParams(map[string]string{"m": "{{ unclosed"}, ev, "r", london); err == nil {
		t.Fatal("a malformed template rendered without error")
	}
	// An oversize expansion is refused rather than sent.
	big := runtimedb.HouseholdEvent{EventID: "x", Type: "a.b", OccurredAt: t0,
		Payload: json.RawMessage(`{"blob":"` + strings.Repeat("x", maxRenderedParamBytes+1) + `"}`)}
	if _, err := RenderParams(map[string]string{"m": "{{ .Payload.blob }}"}, big, "r", london); err == nil {
		t.Fatal("an oversize render was accepted")
	}
	// No params is no work.
	if out, err := RenderParams(nil, ev, "r", london); err != nil || out != nil {
		t.Fatalf("nil params = %v, %v", out, err)
	}
}

// started returns a harness whose engine has already adopted the stream,
// so the test's first real event is evaluated rather than skipped as
// history. The marker it adopts on is seq 1; the first real event is 2.
func started(t *testing.T, rules ...resource.Resource) (*Engine, *runtimedb.DB) {
	t.Helper()
	e, db := harness(t, rules...)
	emit(t, db, "adopt-marker", "household.marker", "", t0.Add(-72*time.Hour))
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e, db
}

// mailRule is the ADR-W022 A8 example: a rule with no escalation, so H4
// can close the loop without a model in it.
func mailRule() resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindAutomation,
		Metadata: resource.Metadata{Name: "mail-check", Owner: "household/mika"},
		Automation: &resource.AutomationSpec{
			Description: "Ask whether the mail was collected on arriving home",
			Trigger:     resource.AutomationTrigger{EventType: "person.arrived_home", Subject: "mika"},
			Conditions:  []resource.AutomationCondition{{Kind: "cooldown", Window: "20h"}},
			Action: resource.AutomationAction{
				Tool: "notifications.notify", Tier: "domestic",
				Params: map[string]string{
					"topic": "household-{{ .Subject }}", "message": "Did you get the mail?", "replies": "Yes,No",
				},
			},
		},
	}
}

func TestFiredRuleActsOnceThroughTheGateway(t *testing.T) {
	e, db := started(t, mailRule())
	acts := &fakeActions{}
	e.Actions = acts

	emit(t, db, "arrived-1", "person.arrived_home", "mika", t0)
	out, err := e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Fired != 1 {
		t.Fatalf("tick = %+v", out)
	}
	if len(acts.calls) != 1 {
		t.Fatalf("dispatched %d times", len(acts.calls))
	}
	c := acts.calls[0]
	if c.Tool != "notifications.notify" || c.Tier != "domestic" || c.Rule != "mail-check" ||
		c.Subject != "mika" || c.CorrelationID != "arrived-1" || c.TriggerType != "person.arrived_home" {
		t.Fatalf("call = %+v", c)
	}
	// Params reached the gateway rendered, not as templates.
	if c.Params["topic"] != "household-mika" || c.Params["replies"] != "Yes,No" {
		t.Fatalf("params = %v", c.Params)
	}

	// The claim is on the stream, correlated to the trigger.
	acted, _ := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{Type: TypeActed})
	if len(acted) != 1 || acted[0].Entity != "mail-check" || acted[0].CorrelationID != "arrived-1" ||
		acted[0].EventID != "mail-check@2:act" {
		t.Fatalf("acted = %+v", acted)
	}
	var payload map[string]any
	json.Unmarshal(acted[0].Payload, &payload) //nolint:errcheck
	if payload["tool"] != "notifications.notify" || payload["tier"] != "domestic" {
		t.Fatalf("acted payload = %v", payload)
	}

	// Re-evaluating the same trigger must not dispatch again: the claim
	// is written before the call precisely so a crash cannot double-send.
	if err := db.SetAutomationCursor(1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(acts.calls) != 1 {
		t.Fatalf("re-tick dispatched again: %d calls", len(acts.calls))
	}
}

func TestActionFailuresAreRecorded(t *testing.T) {
	e, db := started(t, mailRule())
	e.Actions = &fakeActions{err: errors.New("ntfy returned 507")}

	emit(t, db, "arrived-1", "person.arrived_home", "mika", t0)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	failed, _ := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{Type: TypeActionFailed})
	if len(failed) != 1 || !strings.Contains(string(failed[0].Payload), "ntfy returned 507") ||
		!strings.Contains(string(failed[0].Payload), `"stage":"dispatch"`) {
		t.Fatalf("failure record = %+v", failed)
	}
	// The claim stands beside it: the action WAS dispatched, and then it
	// failed. Both facts are on the stream.
	if acted, _ := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{Type: TypeActed}); len(acted) != 1 {
		t.Fatalf("acted = %+v", acted)
	}

	// A render failure never reaches the gateway at all.
	bad := mailRule()
	bad.Automation.Action.Params["message"] = "{{ .Payload.nope }}"
	e2, db2 := started(t, bad)
	acts := &fakeActions{}
	e2.Actions = acts
	emit(t, db2, "arrived-1", "person.arrived_home", "mika", t0)
	if _, err := e2.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(acts.calls) != 0 {
		t.Fatalf("a rule that could not render its params was dispatched: %+v", acts.calls)
	}
	failed, _ = db2.QueryHouseholdEvents(runtimedb.HouseholdQuery{Type: TypeActionFailed})
	if len(failed) != 1 || !strings.Contains(string(failed[0].Payload), `"stage":"render"`) {
		t.Fatalf("render failure = %+v", failed)
	}
	if acted, _ := db2.QueryHouseholdEvents(runtimedb.HouseholdQuery{Type: TypeActed}); len(acted) != 0 {
		t.Fatalf("a render failure claimed an action: %+v", acted)
	}
}

// TestEscalationSkippedActsWithTheRulesOwnWording: an escalating rule on
// a control plane with no Butler is not a broken rule. It acts with the
// wording its author wrote — the deterministic backbone doing its job —
// and says on the stream why the model had no part in it.
func TestEscalationSkippedActsWithTheRulesOwnWording(t *testing.T) {
	e, db := started(t, rule("breakfast-check"))
	acts := &fakeActions{}
	e.Actions = acts // no Escalator

	emit(t, db, "left-1", "person.left_home", "mika", t0)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(acts.calls) != 1 {
		t.Fatalf("an escalating rule did not act without its Butler: %+v", acts.calls)
	}
	if got := acts.calls[0].Params["message"]; got != "Did you have breakfast?" {
		t.Fatalf("message = %q, want the rule's own wording", got)
	}
	skipped, _ := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{Type: TypeEscalationSkipped})
	if len(skipped) != 1 || skipped[0].Entity != "breakfast-check" ||
		!strings.Contains(string(skipped[0].Payload), `"into":"message"`) {
		t.Fatalf("skipped = %+v", skipped)
	}
}

func TestNoGatewayLeavesDecisionsUnacted(t *testing.T) {
	e, db := started(t, mailRule()) // Actions nil
	emit(t, db, "arrived-1", "person.arrived_home", "mika", t0)
	out, err := e.Tick(context.Background())
	if err != nil || out.Fired != 1 {
		t.Fatalf("tick = %+v, %v", out, err)
	}
	for _, typ := range []string{TypeActed, TypeActionFailed, TypeEscalationSkipped} {
		if got, _ := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{Type: typ}); len(got) != 0 {
			t.Fatalf("%s recorded with no gateway wired: %+v", typ, got)
		}
	}
}
