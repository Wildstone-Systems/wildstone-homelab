package automation

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

// Friday 2026-09-04 07:30 in Europe/London (06:30 UTC).
var london = mustLoc("Europe/London")
var t0 = time.Date(2026, 9, 4, 6, 30, 0, 0, time.UTC)

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

func rule(name string, conds ...resource.AutomationCondition) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindAutomation,
		Metadata: resource.Metadata{Name: name, Owner: "household/mika"},
		Automation: &resource.AutomationSpec{
			Description: "breakfast",
			Trigger:     resource.AutomationTrigger{EventType: "person.left_home", Subject: "mika"},
			Conditions:  conds,
			Escalation:  &resource.AutomationEscalation{Prompt: "ask"},
			Action:      resource.AutomationAction{Tool: "notifications.notify", Tier: "domestic"},
		},
	}
}

func left(seq int64, subject string, at time.Time) runtimedb.HouseholdEvent {
	return runtimedb.HouseholdEvent{Seq: seq, EventID: "ha-" + subject, Source: "publisher-home",
		Type: "person.left_home", Subject: subject, OccurredAt: at}
}

// fakeLookup scripts what the stream says.
type fakeLookup struct {
	count     int
	countArgs []any
	lastFired time.Time
	fired     bool
}

func (f *fakeLookup) Count(typ, subject string, set bool, from, to time.Time, exclude int64) (int, error) {
	f.countArgs = []any{typ, subject, set, from, to, exclude}
	return f.count, nil
}
func (f *fakeLookup) LastFired(rule, subject string, before time.Time) (time.Time, bool, error) {
	return f.lastFired, f.fired, nil
}

func TestMatches(t *testing.T) {
	r := rule("breakfast")
	if !Matches(r, left(1, "mika", t0)) {
		t.Fatal("subject match refused")
	}
	if Matches(r, left(1, "sam", t0)) {
		t.Fatal("other subject matched a subject-scoped rule")
	}
	any := rule("any")
	any.Automation.Trigger.Subject = ""
	if !Matches(any, left(1, "sam", t0)) {
		t.Fatal("unscoped rule refused a subject")
	}
	off := rule("off")
	off.Automation.Disabled = true
	if Matches(off, left(1, "mika", t0)) {
		t.Fatal("disabled rule matched")
	}
	// The loop guard: the engine's own records never trigger a rule.
	chain := rule("chain")
	chain.Automation.Trigger = resource.AutomationTrigger{EventType: TypeFired}
	if Matches(chain, runtimedb.HouseholdEvent{Type: TypeFired, Source: Source}) {
		t.Fatal("a firing triggered a rule")
	}
	other := left(1, "mika", t0)
	other.Type = "person.arrived_home"
	if Matches(r, other) {
		t.Fatal("wrong type matched")
	}
}

func TestEvaluateConditions(t *testing.T) {
	ev := left(7, "mika", t0)
	for name, tc := range map[string]struct {
		cond   resource.AutomationCondition
		look   *fakeLookup
		fired  bool
		reason string
	}{
		"absent holds":   {resource.AutomationCondition{Kind: "eventAbsent", EventType: "pantry.meal_logged", Window: "6h", SameSubject: true}, &fakeLookup{count: 0}, true, ""},
		"absent fails":   {resource.AutomationCondition{Kind: "eventAbsent", EventType: "pantry.meal_logged", Window: "6h", SameSubject: true}, &fakeLookup{count: 2}, false, "2 pantry.meal_logged for mika in the last 6h"},
		"present holds":  {resource.AutomationCondition{Kind: "eventPresent", EventType: "calendar.school_day", Window: "24h"}, &fakeLookup{count: 1}, true, ""},
		"present fails":  {resource.AutomationCondition{Kind: "eventPresent", EventType: "calendar.school_day", Window: "24h"}, &fakeLookup{count: 0}, false, "no calendar.school_day in the last 24h"},
		"time inside":    {resource.AutomationCondition{Kind: "timeOfDay", After: "05:00", Before: "11:00"}, &fakeLookup{}, true, ""},
		"time outside":   {resource.AutomationCondition{Kind: "timeOfDay", After: "08:00", Before: "11:00"}, &fakeLookup{}, false, "07:30 local is outside 08:00–11:00"},
		"time after":     {resource.AutomationCondition{Kind: "timeOfDay", After: "07:30"}, &fakeLookup{}, true, ""},
		"time before":    {resource.AutomationCondition{Kind: "timeOfDay", Before: "07:30"}, &fakeLookup{}, false, "07:30 local is outside before 07:30"},
		"time overnight": {resource.AutomationCondition{Kind: "timeOfDay", After: "22:00", Before: "08:00"}, &fakeLookup{}, true, ""},
		"weekday holds":  {resource.AutomationCondition{Kind: "weekday", Days: []string{"mon", "fri"}}, &fakeLookup{}, true, ""},
		"weekday fails":  {resource.AutomationCondition{Kind: "weekday", Days: []string{"sat", "sun"}}, &fakeLookup{}, false, "fri is not one of sat,sun"},
		"cooldown never": {resource.AutomationCondition{Kind: "cooldown", Window: "20h"}, &fakeLookup{}, true, ""},
		"cooldown old":   {resource.AutomationCondition{Kind: "cooldown", Window: "20h"}, &fakeLookup{fired: true, lastFired: t0.Add(-25 * time.Hour)}, true, ""},
		"cooldown hot":   {resource.AutomationCondition{Kind: "cooldown", Window: "20h"}, &fakeLookup{fired: true, lastFired: t0.Add(-3 * time.Hour)}, false, "fired 3h0m0s ago for mika, within the 20h cooldown"},
	} {
		t.Run(name, func(t *testing.T) {
			d, err := Evaluate(rule("breakfast", tc.cond), ev, tc.look, london)
			if err != nil {
				t.Fatal(err)
			}
			if d.Fired != tc.fired {
				t.Fatalf("fired = %v (%s), want %v", d.Fired, d.Reason, tc.fired)
			}
			if !tc.fired && !strings.Contains(d.Reason, tc.reason) {
				t.Fatalf("reason = %q, want it to contain %q", d.Reason, tc.reason)
			}
			if tc.fired && d.Reason != "all conditions held" {
				t.Fatalf("fired reason = %q", d.Reason)
			}
		})
	}

	// The window is anchored on the trigger, scoped to its subject when
	// asked, and excludes the trigger itself.
	look := &fakeLookup{}
	Evaluate(rule("b", resource.AutomationCondition{Kind: "eventAbsent", EventType: "pantry.meal_logged", Window: "6h", SameSubject: true}), ev, look, london) //nolint:errcheck
	want := []any{"pantry.meal_logged", "mika", true, t0.Add(-6 * time.Hour), t0, int64(7)}
	for i := range want {
		if look.countArgs[i] != want[i] {
			t.Fatalf("count args = %v, want %v", look.countArgs, want)
		}
	}
	// Conditions stop at the first failure, in order; the decision carries
	// what a firing would do.
	d, _ := Evaluate(rule("b",
		resource.AutomationCondition{Kind: "weekday", Days: []string{"sat"}},
		resource.AutomationCondition{Kind: "timeOfDay", After: "23:00"},
	), ev, look, london)
	if d.Fired || !strings.HasPrefix(d.Reason, "conditions[0] weekday") {
		t.Fatalf("order: %+v", d)
	}
	if d.Action != "notifications.notify" || d.Tier != "domestic" || !d.Escalates || d.Rule != "b" || d.EventSeq != 7 {
		t.Fatalf("decision = %+v", d)
	}
	// nil location is UTC: 06:30, not 07:30.
	d, _ = Evaluate(rule("b", resource.AutomationCondition{Kind: "timeOfDay", After: "07:00"}), ev, look, nil)
	if d.Fired {
		t.Fatal("nil location did not evaluate in UTC")
	}
}

func harness(t *testing.T, rules ...resource.Resource) (*Engine, *runtimedb.DB) {
	t.Helper()
	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	state, err := statefile.New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if err := state.Put(r); err != nil {
			t.Fatal(err)
		}
	}
	if len(rules) > 0 {
		if err := state.Commit("test", "seed", "seed"); err != nil {
			t.Fatal(err)
		}
	}
	now := t0.Add(5 * time.Minute)
	return &Engine{DB: db, State: state, Location: london, Now: func() time.Time { return now }}, db
}

func emit(t *testing.T, db *runtimedb.DB, id, typ, subject string, at time.Time) int64 {
	t.Helper()
	seq, _, err := db.AppendHouseholdEvent(runtimedb.HouseholdEvent{EventID: id, Source: "publisher-home", Type: typ, Subject: subject, OccurredAt: at})
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func TestTickRecordsDecisionsIdempotently(t *testing.T) {
	breakfast := rule("breakfast-check",
		resource.AutomationCondition{Kind: "eventAbsent", EventType: "pantry.meal_logged", Window: "6h", SameSubject: true},
		resource.AutomationCondition{Kind: "cooldown", Window: "20h"},
	)
	off := rule("paused")
	off.Automation.Disabled = true
	e, db := harness(t, breakfast, off)

	// History that predates the engine is adopted, not evaluated.
	emit(t, db, "old-1", "person.left_home", "mika", t0.Add(-48*time.Hour))
	out, err := e.Tick(context.Background())
	if err != nil || !out.Adopted || out.Evaluated != 0 || out.Cursor != 1 {
		t.Fatalf("first tick = %+v, %v", out, err)
	}

	// Sam ate; Mika did not. Both leave.
	emit(t, db, "meal-sam", "pantry.meal_logged", "sam", t0.Add(-time.Hour))
	emit(t, db, "left-mika", "person.left_home", "mika", t0)
	emit(t, db, "left-sam", "person.left_home", "sam", t0) // not the rule's subject
	out, err = e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Evaluated != 1 || out.Fired != 1 || out.Suppressed != 0 || out.Cursor != 4 {
		t.Fatalf("second tick = %+v", out)
	}
	fired, _ := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{Type: TypeFired})
	if len(fired) != 1 || fired[0].Source != Source || fired[0].Entity != "breakfast-check" || fired[0].Subject != "mika" ||
		fired[0].EventID != "breakfast-check@3" || fired[0].CorrelationID != "left-mika" || !fired[0].OccurredAt.Equal(t0) {
		t.Fatalf("fired record = %+v", fired)
	}
	var payload map[string]any
	json.Unmarshal(fired[0].Payload, &payload) //nolint:errcheck
	if payload["action"] != "notifications.notify" || payload["escalates"] != true || payload["trigger"] != float64(3) {
		t.Fatalf("payload = %v", payload)
	}

	// A quiet tick is quiet; rewinding the cursor and re-ticking records
	// nothing twice (the decision id is deterministic).
	out, _ = e.Tick(context.Background())
	if out.Evaluated != 0 {
		t.Fatalf("quiet tick evaluated %d", out.Evaluated)
	}
	if err := db.SetAutomationCursor(1); err != nil {
		t.Fatal(err)
	}
	out, _ = e.Tick(context.Background())
	if out.Evaluated != 1 {
		t.Fatalf("re-tick evaluated %d", out.Evaluated)
	}
	if all, _ := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{Type: TypeFired}); len(all) != 1 {
		t.Fatalf("re-tick wrote %d decision(s), want the original 1", len(all))
	}
	// This rule escalates and no escalator is wired, so the firing is
	// followed by exactly one escalation_pending — also idempotent.
	pending, _ := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{Type: TypeEscalationPending})
	if len(pending) != 1 || pending[0].Entity != "breakfast-check" || pending[0].CorrelationID != "left-mika" {
		t.Fatalf("escalation_pending = %+v", pending)
	}

	// Mika leaves again two hours later: the cooldown suppresses, with
	// the reason on the record, and the firing itself never triggers.
	emit(t, db, "left-mika-2", "person.left_home", "mika", t0.Add(2*time.Hour))
	e.Now = func() time.Time { return t0.Add(2*time.Hour + time.Minute) }
	out, _ = e.Tick(context.Background())
	if out.Evaluated != 1 || out.Suppressed != 1 {
		t.Fatalf("cooldown tick = %+v", out)
	}
	sup, _ := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{Type: TypeSuppressed})
	if len(sup) != 1 || !strings.Contains(string(sup[0].Payload), "within the 20h cooldown") {
		t.Fatalf("suppressed = %+v", sup)
	}

	// A trigger older than the freshness bound is recorded as stale, not
	// acted on: catching up after an outage must not ask stale questions.
	emit(t, db, "left-mika-3", "person.left_home", "mika", t0.Add(2*time.Hour+30*time.Minute))
	e.Now = func() time.Time { return t0.Add(10 * time.Hour) }
	out, _ = e.Tick(context.Background())
	if out.Suppressed != 1 || !strings.HasPrefix(out.Decisions[0].Reason, "stale:") {
		t.Fatalf("stale tick = %+v", out)
	}
}

func TestReplayIsSideEffectFree(t *testing.T) {
	e, db := harness(t) // no approved rules at all: a draft is replayed
	// Three mornings: ate, skipped, skipped again within a day.
	days := []struct {
		day  time.Duration
		meal bool
	}{{0, true}, {24 * time.Hour, false}, {26 * time.Hour, false}}
	for i, d := range days {
		at := t0.Add(d.day)
		if d.meal {
			emit(t, db, "meal-"+string(rune('a'+i)), "pantry.meal_logged", "mika", at.Add(-time.Hour))
		}
		emit(t, db, "left-"+string(rune('a'+i)), "person.left_home", "mika", at)
	}
	draft := rule("draft",
		resource.AutomationCondition{Kind: "eventAbsent", EventType: "pantry.meal_logged", Window: "6h", SameSubject: true},
		resource.AutomationCondition{Kind: "cooldown", Window: "20h"},
	)
	decisions, err := e.Replay(context.Background(), []resource.Resource{draft}, t0.Add(-time.Hour), t0.Add(48*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 3 {
		t.Fatalf("decisions = %+v", decisions)
	}
	if decisions[0].Fired || !strings.Contains(decisions[0].Reason, "1 pantry.meal_logged") {
		t.Fatalf("day one (ate) = %+v", decisions[0])
	}
	if !decisions[1].Fired {
		t.Fatalf("day two (skipped) = %+v", decisions[1])
	}
	if decisions[2].Fired || !strings.Contains(decisions[2].Reason, "cooldown") {
		t.Fatalf("day two again (cooldown, simulated in-memory) = %+v", decisions[2])
	}
	if written, _ := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{Source: Source}); len(written) != 0 {
		t.Fatalf("replay wrote %d decision(s)", len(written))
	}
	if cursor, _ := db.AutomationCursor(); cursor != 0 {
		t.Fatalf("replay moved the cursor to %d", cursor)
	}
	// A window that excludes everything is an empty list, not nil.
	none, _ := e.Replay(context.Background(), []resource.Resource{draft}, t0.Add(100*time.Hour), t0.Add(200*time.Hour), 0)
	if none == nil || len(none) != 0 {
		t.Fatalf("empty replay = %v", none)
	}
}
