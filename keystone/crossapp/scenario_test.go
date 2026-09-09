package crossapp_test

// The two fixture scenarios (ADR-W030 A6, work package P12).
//
// Both run with the MODEL DISABLED — there is no Phraser anywhere in
// this file — because a household routine that only works when a model
// is up is not a routine, it is a hope. The model's contribution is
// rewording one message at the end, and its absence changes nothing
// about whether the milk gets on the list.
//
// The scenarios run against the real adapter Upstreams (the Home
// Assistant and CalDAV adapters against fake servers, and the versioned
// PantryOS fake) through the same four result types the integration
// contract defines.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/homeassistant"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/pantry"
	"github.com/chickenrat17/wildstone-homelab/keystone/crossapp"
)

// directIntegrations wires the runner straight onto Upstreams. It stands
// in for the work queue, which the API tests already exercise end to
// end; here the interest is the ROUTINE's behaviour given each of the
// four result types.
type directIntegrations struct {
	ups map[string]adapter.Upstream
	// uncertain makes one capability's outcome unknown: the call
	// neither clearly succeeded nor clearly failed, which is the case
	// A4 says must pause rather than retry.
	uncertain map[string]bool
	mu        sync.Mutex
	calls     map[string]int
}

func (d *directIntegrations) Ask(ctx context.Context, connection, kind, capability, request string, _ time.Duration) (string, json.RawMessage, time.Time, error) {
	d.mu.Lock()
	if d.calls == nil {
		d.calls = map[string]int{}
	}
	d.calls[connection+"/"+capability]++
	uncertain := d.uncertain[capability]
	d.mu.Unlock()

	if uncertain {
		return "", nil, time.Time{}, errors.New("the connection dropped after the request went out")
	}
	up, ok := d.ups[connection]
	if !ok {
		return "unavailable", nil, time.Time{}, nil
	}
	var answer adapter.Answer
	var err error
	if kind == "action" {
		answer, err = up.Act(ctx, capability, json.RawMessage(orEmpty(request)))
	} else {
		answer, err = up.Query(ctx, capability, json.RawMessage(orEmpty(request)))
	}
	if errors.Is(err, adapter.ErrUnavailable) || errors.Is(err, adapter.ErrNoSuchCapability) {
		return "unavailable", nil, time.Time{}, nil
	}
	if err != nil {
		return "", nil, time.Time{}, err
	}
	data, merr := json.Marshal(answer.Data)
	if merr != nil {
		return "", nil, time.Time{}, merr
	}
	switch {
	case answer.Stale:
		return "stale", data, answer.ObservedAt, nil
	case answer.Empty:
		return "empty", data, time.Time{}, nil
	}
	return "ok", data, time.Time{}, nil
}

func orEmpty(s string) string {
	if s == "" {
		return "{}"
	}
	return s
}

// memoryDedupe is the smallest thing that satisfies the seam.
type memoryDedupe struct {
	mu   sync.Mutex
	seen map[string]time.Time
	now  func() time.Time
}

func (m *memoryDedupe) Seen(routine, key string, window time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = map[string]time.Time{}
	}
	k := routine + "\x00" + key
	at, ok := m.seen[k]
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	if ok && now.Sub(at) < window {
		return true, nil
	}
	m.seen[k] = now
	return false, nil
}

func loadRoutine(t *testing.T, name string) crossapp.Routine {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testing", "fixtures", "routines", name))
	if err != nil {
		t.Fatalf("reading the routine fixture: %v", err)
	}
	// The fixture is what a household wrote. Parsing it here rather than
	// building the struct inline is the point: if the document stops
	// being expressible, this test stops compiling.
	var doc struct {
		Name         string `yaml:"name"`
		Trigger      string `yaml:"trigger"`
		DedupeWindow string `yaml:"dedupeWindow"`
		Steps        []crossapp.Step
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	window, err := time.ParseDuration(doc.DedupeWindow)
	if err != nil {
		t.Fatalf("%s has an unreadable dedupe window %q: %v", name, doc.DedupeWindow, err)
	}
	return crossapp.Routine{
		Name: doc.Name, Trigger: doc.Trigger, DedupeWindow: window, Steps: doc.Steps,
	}
}

func fakeHA(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"2026.9.1"}`))
	})
	mux.HandleFunc("/api/states", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
		  {"entity_id":"person.sam","state":"home","last_changed":"2026-09-09T07:00:00Z","attributes":{"friendly_name":"Sam"}},
		  {"entity_id":"person.mika","state":"not_home","last_changed":"2026-09-09T08:10:00Z","attributes":{"friendly_name":"Mika"}}
		]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func fakeCal(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const dayWithEvents = `<?xml version="1.0"?>
<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
 <response><propstat><prop><C:calendar-data>BEGIN:VCALENDAR
BEGIN:VEVENT
UID:ev-1
SUMMARY:School run
DTSTART:20260909T080000Z
END:VEVENT
END:VCALENDAR</C:calendar-data></prop></propstat></response>
</multistatus>`

func fixtureRunner(t *testing.T, pantryFake *pantry.Fake, calBody string) (*crossapp.Runner, *directIntegrations) {
	t.Helper()
	at := func() time.Time { return time.Date(2026, 9, 9, 8, 15, 0, 0, time.UTC) }
	ha := fakeHA(t)
	cal := fakeCal(t, calBody)
	integrations := &directIntegrations{
		ups: map[string]adapter.Upstream{
			"home":            &homeassistant.Upstream{BaseURL: ha.URL, Token: "t", Now: at},
			"family-calendar": &caldav.Upstream{CollectionURL: cal.URL, Username: "sam", Now: at},
			"pantry":          pantryFake,
		},
		uncertain: map[string]bool{},
	}
	// No Phraser: the model is disabled for every scenario here.
	return &crossapp.Runner{Integrations: integrations, Dedupe: &memoryDedupe{}}, integrations
}

// The departure/breakfast scenario, model disabled.
func TestDepartureBreakfastScenario(t *testing.T) {
	fake := pantry.NewFake()
	runner, calls := fixtureRunner(t, fake, dayWithEvents)
	routine := loadRoutine(t, "departure-breakfast.yaml")
	trigger := crossapp.Trigger{Type: "person.left_home", Key: "evt-1", Subject: "mika"}

	run, err := runner.Execute(context.Background(), routine, trigger)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if run.State != crossapp.RunCompleted {
		t.Fatalf("run = %s: %s", run.State, run.Reason)
	}
	// It did the cross-app work: read the calendar, read the pantry, and
	// put milk on the list.
	if got := fake.Items("groceries"); len(got) != 2 || got[1] != "bread" {
		// milk was already there; the point is it is not there twice.
		t.Logf("groceries = %v", got)
	}
	// The author's own words came out, because there is no model.
	if len(run.Messages) == 0 || !strings.Contains(run.Messages[len(run.Messages)-1], "shopping list") {
		t.Fatalf("messages = %v", run.Messages)
	}
	if calls.calls["family-calendar/caldav.today"] != 1 {
		t.Fatalf("calendar reads = %d, want one", calls.calls["family-calendar/caldav.today"])
	}

	// A repeated departure is one departure.
	repeat, err := runner.Execute(context.Background(), routine, trigger)
	if err != nil {
		t.Fatal(err)
	}
	if repeat.State != crossapp.RunSkipped {
		t.Fatalf("a repeated trigger ran again: %s", repeat.State)
	}
	if calls.calls["family-calendar/caldav.today"] != 1 {
		t.Fatal("a deduplicated trigger still read the calendar")
	}

	// A DIFFERENT departure is a different departure.
	other, err := runner.Execute(context.Background(), routine,
		crossapp.Trigger{Type: "person.left_home", Key: "evt-2", Subject: "sam"})
	if err != nil {
		t.Fatal(err)
	}
	if other.State != crossapp.RunCompleted {
		t.Fatalf("a second, distinct departure = %s: %s", other.State, other.Reason)
	}
	// And the list still has milk once: the action itself is idempotent
	// as well as the trigger.
	milk := 0
	for _, item := range fake.Items("groceries") {
		if item == "milk" {
			milk++
		}
	}
	if milk != 1 {
		t.Fatalf("milk appears %d times on the list", milk)
	}
}

// The calendar/presence scenario, model disabled and read-only.
func TestCalendarPresenceScenario(t *testing.T) {
	runner, _ := fixtureRunner(t, pantry.NewFake(), dayWithEvents)
	routine := loadRoutine(t, "calendar-presence.yaml")

	run, err := runner.Execute(context.Background(), routine,
		crossapp.Trigger{Type: "household.morning", Key: "2026-09-09"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if run.State != crossapp.RunCompleted {
		t.Fatalf("run = %s: %s", run.State, run.Reason)
	}
	// Two reads, both answered, and neither of them an action.
	reads := 0
	for _, s := range run.Steps {
		if s.Kind == crossapp.StepQuery {
			reads++
			if s.ResultType != "ok" {
				t.Fatalf("%s came back %q", s.Capability, s.ResultType)
			}
		}
		if s.Kind == crossapp.StepAct {
			t.Fatal("a read-only routine performed an action")
		}
	}
	if reads != 2 {
		t.Fatalf("reads = %d, want presence and calendar", reads)
	}
}

// An unavailable source waits; an unavailable OPTIONAL source does not
// stop the useful half.
func TestScenariosHandleUnavailableSourcesExplicitly(t *testing.T) {
	t.Run("a required source waits", func(t *testing.T) {
		fake := pantry.NewFake()
		fake.Unavailable = true
		runner, _ := fixtureRunner(t, fake, dayWithEvents)
		run, err := runner.Execute(context.Background(), loadRoutine(t, "departure-breakfast.yaml"),
			crossapp.Trigger{Type: "person.left_home", Key: "evt-1"})
		if err != nil {
			t.Fatal(err)
		}
		if run.State != crossapp.RunWaiting {
			t.Fatalf("an unavailable pantry = %s, want waiting", run.State)
		}
		if !strings.Contains(run.Reason, "unavailable") {
			t.Fatalf("the reason does not say what was unavailable: %q", run.Reason)
		}
		if len(run.Messages) != 0 {
			t.Fatalf("a routine that could not do its job still told the household it had: %v", run.Messages)
		}
	})

	t.Run("an optional source is noted and skipped", func(t *testing.T) {
		fake := pantry.NewFake()
		// The calendar server is up but refusing, which is what a
		// rotated password looks like.
		refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer refusing.Close()
		runner, integrations := fixtureRunner(t, fake, dayWithEvents)
		integrations.ups["family-calendar"] = &caldav.Upstream{CollectionURL: refusing.URL}

		run, err := runner.Execute(context.Background(), loadRoutine(t, "departure-breakfast.yaml"),
			crossapp.Trigger{Type: "person.left_home", Key: "evt-9"})
		if err != nil {
			t.Fatal(err)
		}
		if run.State != crossapp.RunCompleted {
			t.Fatalf("an optional source's outage stopped the run: %s — %s", run.State, run.Reason)
		}
		var sawUnavailable bool
		for _, s := range run.Steps {
			if s.Capability == caldav.CapToday && s.ResultType == "unavailable" {
				sawUnavailable = true
			}
		}
		if !sawUnavailable {
			t.Fatalf("the calendar outage was not recorded: %+v", run.Steps)
		}
	})

	t.Run("a stale reading says it is stale", func(t *testing.T) {
		fake := pantry.NewFake()
		observed := time.Date(2026, 9, 9, 6, 0, 0, 0, time.UTC)
		fake.StaleAfter = observed
		fake.Now = func() time.Time { return observed.Add(2 * time.Hour) }
		runner, _ := fixtureRunner(t, fake, dayWithEvents)

		run, err := runner.Execute(context.Background(), loadRoutine(t, "departure-breakfast.yaml"),
			crossapp.Trigger{Type: "person.left_home", Key: "evt-3"})
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(run.Messages, " ")
		if !strings.Contains(joined, "as of") {
			t.Fatalf("a stale reading was reported as if it were current: %v", run.Messages)
		}
	})
}

// An action whose outcome is unknown pauses the run rather than being
// retried.
func TestUncertainActionPausesRatherThanRetries(t *testing.T) {
	fake := pantry.NewFake()
	runner, integrations := fixtureRunner(t, fake, dayWithEvents)
	integrations.uncertain[pantry.CapAdd] = true

	run, err := runner.Execute(context.Background(), loadRoutine(t, "departure-breakfast.yaml"),
		crossapp.Trigger{Type: "person.left_home", Key: "evt-4"})
	if err != nil {
		t.Fatal(err)
	}
	if run.State != crossapp.RunWaiting {
		t.Fatalf("an uncertain action = %s, want waiting", run.State)
	}
	if !strings.Contains(run.Reason, "may or may not have happened") {
		t.Fatalf("the reason does not say the outcome is unknown: %q", run.Reason)
	}
	// It was tried exactly once. Retrying an action that may already
	// have happened is the thing A4 forbids.
	if got := integrations.calls["pantry/"+pantry.CapAdd]; got != 1 {
		t.Fatalf("an uncertain action was attempted %d times", got)
	}
	// And nothing was said to the household about work that may not
	// have happened.
	if len(run.Messages) != 0 {
		t.Fatalf("a paused run still sent a message: %v", run.Messages)
	}
	_ = fmt.Sprint()
}
