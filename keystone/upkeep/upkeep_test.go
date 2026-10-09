package upkeep

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/commitment"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

var chicago = mustLoc("America/Chicago")

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

func at(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 9, 0, 0, 0, chicago)
}

type fixture struct {
	t      *testing.T
	e      *Engine
	db     *runtimedb.DB
	state  *statefile.Store
	nowVal time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := runtimedb.Open(migrated.Into(t, filepath.Join(t.TempDir(), "runtime.db")))
	if err != nil {
		t.Fatalf("runtimedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	state, err := statefile.New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatalf("statefile.New: %v", err)
	}
	f := &fixture{t: t, db: db, state: state, nowVal: at(2026, time.June, 1)}
	f.e = &Engine{DB: db, State: state, Loc: chicago, Now: func() time.Time { return f.nowVal }}
	return f
}

func (f *fixture) at(when time.Time) { f.nowVal = when }

func (f *fixture) put(r resource.Resource) {
	f.t.Helper()
	if err := f.state.Put(r); err != nil {
		f.t.Fatalf("Put: %v", err)
	}
	if err := f.state.Commit("test", "", "declare "+r.Metadata.Name); err != nil {
		f.t.Fatalf("Commit: %v", err)
	}
}

func (f *fixture) meter(name string, spec resource.MeterSpec) {
	f.put(resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
		Metadata: resource.Metadata{Name: name, Owner: "household"}, Meter: &spec})
}

func (f *fixture) commitment(name string, spec resource.CommitmentSpec) {
	f.put(resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindCommitment,
		Metadata: resource.Metadata{Name: name, Owner: "household/kai"}, Commitment: &spec})
}

func (f *fixture) read(meter string, value float64, when time.Time) {
	f.t.Helper()
	if _, _, err := f.db.AppendMeterReading(runtimedb.MeterReading{
		Meter: meter, Value: value, At: when, RecordedBy: "mika",
		Source: runtimedb.ReadingByPerson}); err != nil {
		f.t.Fatalf("AppendMeterReading: %v", err)
	}
}

func (f *fixture) tick() []Change {
	f.t.Helper()
	changes, err := f.e.Tick()
	if err != nil {
		f.t.Fatalf("Tick: %v", err)
	}
	return changes
}

func checkup() resource.CommitmentSpec {
	return resource.CommitmentSpec{
		Description: "Kai's annual well-child visit",
		Rollover:    resource.RolloverFromSchedule,
		Anchor:      "2025-09-10", RequiresBooking: true,
		Due: []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "1yr", Lead: "60d"}},
	}
}

// The property the whole file is built around: a household told the same
// thing every morning stops reading its notifications.
func TestAQuietDaySaysNothingAndAStandingStateIsSaidOnce(t *testing.T) {
	f := newFixture(t)
	f.commitment("kai-checkup", checkup())

	// Long before anything is due.
	if got := f.tick(); len(got) != 0 {
		t.Errorf("a commitment that is not due announced %+v", got)
	}
	// Inside the lead window: news, once.
	f.at(at(2026, time.July, 15))
	got := f.tick()
	if len(got) != 1 || got[0].To != string(commitment.StateDueSoon) {
		t.Fatalf("changes = %+v, want one due_soon", got)
	}
	// Every morning for the next month: nothing.
	for _, day := range []time.Time{
		at(2026, time.July, 16), at(2026, time.August, 1), at(2026, time.August, 20),
	} {
		f.at(day)
		if again := f.tick(); len(again) != 0 {
			t.Errorf("on %s it said %+v again", day.Format("2 Jan"), again)
		}
	}
	// Crossing into overdue is news again.
	f.at(at(2026, time.September, 11))
	got = f.tick()
	if len(got) != 1 || got[0].To != string(commitment.StateOverdue) {
		t.Errorf("changes = %+v, want one overdue", got)
	}
	f.at(at(2026, time.October, 1))
	if again := f.tick(); len(again) != 0 {
		t.Errorf("overdue was announced twice: %+v", again)
	}
}

func TestTheTimelineCarriesTheFact(t *testing.T) {
	f := newFixture(t)
	f.commitment("kai-checkup", checkup())
	f.at(at(2026, time.July, 15))
	f.tick()

	events, err := f.db.HouseholdEventsAfter(0, 0)
	if err != nil {
		t.Fatalf("HouseholdEventsAfter: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("%d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Type != TypeDueSoon || ev.Source != Source || ev.Entity != "kai-checkup" {
		t.Errorf("event = %+v", ev)
	}
	// The commitment is about Kai, so the fact is too — a stream consumer
	// filtering by person must be able to find it.
	if ev.Subject != "kai" {
		t.Errorf("subject = %q, want kai", ev.Subject)
	}
}

// Running the pass twice in a day must not record the fact twice, which
// is what a restart or an operator poking it looks like.
func TestRunningThePassTwiceRecordsOneFact(t *testing.T) {
	f := newFixture(t)
	f.commitment("kai-checkup", checkup())
	f.at(at(2026, time.July, 15))
	f.tick()
	f.tick()
	events, _ := f.db.HouseholdEventsAfter(0, 0)
	if len(events) != 1 {
		t.Errorf("%d events after two passes, want 1", len(events))
	}
}

func TestBookingAndMissingTheAppointment(t *testing.T) {
	f := newFixture(t)
	f.commitment("kai-checkup", checkup())
	f.at(at(2026, time.July, 15))
	f.tick() // due_soon

	// Booked: the state that separates knowing from happening.
	if _, err := f.db.RecordCommitmentBooking(runtimedb.CommitmentBooking{
		Commitment: "kai-checkup", At: at(2026, time.September, 15),
		BookedAt: at(2026, time.July, 20), BookedBy: "mika"}); err != nil {
		t.Fatalf("RecordCommitmentBooking: %v", err)
	}
	f.at(at(2026, time.August, 1))
	got := f.tick()
	if len(got) != 1 || got[0].To != string(commitment.StateScheduled) {
		t.Fatalf("changes = %+v, want scheduled", got)
	}

	// The appointment comes and goes with nothing recorded.
	f.at(at(2026, time.September, 16))
	got = f.tick()
	if len(got) != 1 || got[0].To != string(commitment.StateOverdue) {
		t.Errorf("changes = %+v, want overdue: the appointment was missed", got)
	}
}

func TestACancelledBookingIsNotAnAppointment(t *testing.T) {
	f := newFixture(t)
	f.commitment("kai-checkup", checkup())
	seq, err := f.db.RecordCommitmentBooking(runtimedb.CommitmentBooking{
		Commitment: "kai-checkup", At: at(2026, time.September, 15),
		BookedAt: at(2026, time.July, 20), BookedBy: "mika"})
	if err != nil {
		t.Fatalf("RecordCommitmentBooking: %v", err)
	}
	if err := f.db.CancelCommitmentBooking(seq, "mika"); err != nil {
		t.Fatalf("CancelCommitmentBooking: %v", err)
	}
	f.at(at(2026, time.August, 1))
	got := f.tick()
	if len(got) != 1 || got[0].To != string(commitment.StateDueSoon) {
		t.Errorf("changes = %+v, want due_soon: the appointment was cancelled", got)
	}
}

func TestCompletingOpensTheNextCycleAndSaysSo(t *testing.T) {
	f := newFixture(t)
	f.commitment("kai-checkup", checkup())
	f.at(at(2026, time.September, 11))
	f.tick() // overdue

	if _, _, err := f.db.RecordCommitmentCompletion(runtimedb.CommitmentCompletion{
		Commitment: "kai-checkup", At: at(2026, time.October, 9), RecordedBy: "mika"}); err != nil {
		t.Fatalf("RecordCommitmentCompletion: %v", err)
	}
	f.at(at(2026, time.October, 10))
	got := f.tick()
	if len(got) != 1 || got[0].Type != TypeCycleOpened {
		t.Fatalf("changes = %+v, want a cycle_opened", got)
	}
	// And it stays on the September schedule, not October's.
	standings, err := f.e.Evaluate()
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	due := standings[0].Occurrence.Rules[0].DueTime
	if due.Month() != time.September || due.Year() != 2027 {
		t.Errorf("next due %s, want September 2027", due.Format("2 Jan 2006"))
	}
}

// An unreadable meter is its own announcement: pending needs nothing and
// a stale odometer needs somebody to go and look.
func TestAnUnreadableMeterAsksRatherThanConcluding(t *testing.T) {
	f := newFixture(t)
	f.meter("odo", resource.MeterSpec{Description: "odometer", Unit: "miles", StaleAfter: "1080h"})
	f.commitment("f150-oil", resource.CommitmentSpec{
		Description: "F-150 oil", Rollover: resource.RolloverFromCompletion,
		Anchor: "2026-03-01", AnchorAt: map[string]float64{"odo": 79300},
		Due: []resource.CommitmentDue{{Meter: "odo", Every: "5000", Lead: "300"}},
	})
	f.read("odo", 81000, at(2026, time.March, 2))

	f.at(at(2026, time.June, 1)) // ninety days later: stale
	got := f.tick()
	if len(got) != 1 || got[0].Type != TypeUnresolved {
		t.Fatalf("changes = %+v, want an unresolved", got)
	}
	// Answered: back to an ordinary verdict.
	f.read("odo", 84100, at(2026, time.June, 1))
	got = f.tick()
	if len(got) != 1 || got[0].To != string(commitment.StateDueSoon) {
		t.Errorf("changes = %+v, want due_soon once the meter was read", got)
	}
}

func TestADisabledCommitmentSaysNothing(t *testing.T) {
	f := newFixture(t)
	spec := checkup()
	spec.Disabled = true
	f.commitment("kai-checkup", spec)
	f.at(at(2026, time.September, 11))
	if got := f.tick(); len(got) != 0 {
		t.Errorf("a paused commitment announced %+v", got)
	}
}

// One odometer feeds several commitments, and the pass must read it once.
func TestOneMeterServesSeveralCommitments(t *testing.T) {
	f := newFixture(t)
	f.meter("odo", resource.MeterSpec{Description: "odometer", Unit: "miles"})
	for _, c := range []struct {
		name  string
		every string
		start float64
	}{{"f150-oil", "5000", 79300}, {"f150-tyres", "40000", 79300}} {
		f.commitment(c.name, resource.CommitmentSpec{
			Description: c.name, Rollover: resource.RolloverFromCompletion,
			Anchor: "2026-03-01", AnchorAt: map[string]float64{"odo": c.start},
			Due: []resource.CommitmentDue{{Meter: "odo", Every: c.every, Lead: "300"}},
		})
	}
	f.read("odo", 84100, at(2026, time.June, 1))
	f.at(at(2026, time.June, 2))
	got := f.tick()
	if len(got) != 1 || got[0].Commitment != "f150-oil" {
		t.Errorf("changes = %+v, want only the oil due", got)
	}
}

// A commitment whose meter has been deleted must not stop the pass.
func TestADeletedMeterDoesNotSilenceEverythingElse(t *testing.T) {
	f := newFixture(t)
	f.commitment("f150-oil", resource.CommitmentSpec{
		Description: "F-150 oil", Rollover: resource.RolloverFromCompletion,
		Anchor: "2026-03-01", AnchorAt: map[string]float64{"gone": 79300},
		Due: []resource.CommitmentDue{{Meter: "gone", Every: "5000"}},
	})
	f.commitment("kai-checkup", checkup())
	f.at(at(2026, time.September, 11))

	got := f.tick()
	var kinds []string
	for _, c := range got {
		kinds = append(kinds, c.Commitment+":"+c.Type)
	}
	if len(got) != 2 {
		t.Fatalf("changes = %v, want both: one unresolved and one overdue", kinds)
	}
}

// A pass evaluating a past moment must not see an appointment nobody had
// arranged yet. Found by accident: the first version of the booking test
// wrote the row with the wall clock and the booking vanished, which was
// the replay guard working correctly against a fixture that described
// something impossible.
func TestAPassCannotSeeABookingMadeAfterTheMomentItIsEvaluating(t *testing.T) {
	f := newFixture(t)
	f.commitment("kai-checkup", checkup())
	if _, err := f.db.RecordCommitmentBooking(runtimedb.CommitmentBooking{
		Commitment: "kai-checkup", At: at(2026, time.September, 15),
		BookedAt: at(2026, time.August, 20), BookedBy: "mika"}); err != nil {
		t.Fatalf("RecordCommitmentBooking: %v", err)
	}
	// Ten days BEFORE it was arranged.
	f.at(at(2026, time.August, 10))
	got := f.tick()
	if len(got) != 1 || got[0].To != string(commitment.StateDueSoon) {
		t.Errorf("changes = %+v, want due_soon: on 10 August there was no appointment", got)
	}
	// And after it was arranged, there is one.
	f.at(at(2026, time.August, 21))
	got = f.tick()
	if len(got) != 1 || got[0].To != string(commitment.StateScheduled) {
		t.Errorf("changes = %+v, want scheduled", got)
	}
}

// An RFC3339 anchor is a moment, and the pass must not re-read its
// wall-clock fields in the household's timezone (Codex, #244).
//
// Chicago is UTC-5 in June, so an anchor of 12:00Z re-read as 12:00
// local is five hours late. A commitment due exactly one day after its
// anchor is therefore due at 12:00Z the next day, and a pass run at
// 16:00Z — four hours later — must already call it overdue. Under the
// bug the threshold sat at 17:00Z and the pass said nothing.
func TestAnInstantAnchorIsNotReinterpretedInTheHouseholdTimezone(t *testing.T) {
	f := newFixture(t)
	f.commitment("instant", resource.CommitmentSpec{
		Description: "an anchor that names a moment",
		Rollover:    resource.RolloverOnce,
		Anchor:      "2026-06-01T12:00:00Z",
		Due:         []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "1d"}},
	})
	f.at(time.Date(2026, time.June, 2, 16, 0, 0, 0, time.UTC))
	st := f.standings()["instant"]
	due := st.Occurrence.Rules[0].DueTime
	if want := time.Date(2026, time.June, 2, 12, 0, 0, 0, time.UTC); !due.Equal(want) {
		t.Errorf("dueTime = %s, want %s: the anchor was re-read in local time and moved %s",
			due.UTC(), want, due.Sub(want))
	}
	if got := st.Occurrence.State; got != commitment.StateOverdue {
		t.Errorf("state = %q, want overdue", got)
	}
}

func (f *fixture) standings() map[string]Standing {
	f.t.Helper()
	list, err := f.e.Evaluate()
	if err != nil {
		f.t.Fatalf("Evaluate: %v", err)
	}
	m := map[string]Standing{}
	for _, st := range list {
		m[st.Name] = st
	}
	return m
}

// `satisfiedBy` was validated, generated a Console type, and did
// nothing else (Codex, #244). A household that declared it watched the
// commitment go overdue with the proof sitting in its own timeline.
func TestAnEventTheCommitmentNamedClosesItsCycle(t *testing.T) {
	f := newFixture(t)
	f.commitment("f150-oil", resource.CommitmentSpec{
		Description: "F-150 oil and filter", Rollover: resource.RolloverFromCompletion,
		Anchor: "2026-01-01",
		Due:    []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "6mo", Lead: "14d"}},
		SatisfiedBy: &resource.CommitmentSatisfiedBy{
			EventType: "vehicle.serviced", Entity: "f150",
		},
	})
	f.at(at(2026, time.July, 10)) // past due
	if got := f.standings()["f150-oil"].Occurrence.State; got != commitment.StateOverdue {
		t.Fatalf("state = %q, want overdue before the event", got)
	}

	f.event("vehicle.serviced", "f150", at(2026, time.July, 12))
	f.at(at(2026, time.July, 13))
	f.tick()
	st := f.standings()["f150-oil"]
	if st.Occurrence.State == commitment.StateOverdue {
		t.Errorf("the event did not close the cycle: %+v", st.Occurrence)
	}
	done, err := f.db.ListCommitmentCompletions("f150-oil")
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 {
		t.Fatalf("completions = %d, want 1", len(done))
	}
	if done[0].RecordedBy != harvestedBy {
		t.Errorf("recordedBy = %q, want %q: the inference is the engine's, not the publisher's",
			done[0].RecordedBy, harvestedBy)
	}
	if !done[0].At.Equal(at(2026, time.July, 12)) {
		t.Errorf("at = %s, want the moment the work was done", done[0].At)
	}

	// The pass runs every day and re-reads the stream. The same event
	// must not close a cycle again tomorrow.
	for _, day := range []time.Time{at(2026, time.July, 14), at(2026, time.July, 20)} {
		f.at(day)
		f.tick()
	}
	done, err = f.db.ListCommitmentCompletions("f150-oil")
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 {
		t.Errorf("completions = %d after three passes, want 1: the event was replayed", len(done))
	}
}

// An event closes the cycle from the best number the household has, and
// records that it was a guess.
//
// The stricter first version refused a stale reading outright, which
// meant a `vehicle.serviced` could not close the cycle at all and the
// commitment sat overdue forever with the truck already serviced. The
// error runs the safe way — a stale reading is behind where the meter
// stood, so the next change comes due early — and the completion says
// which number was taken rather than given.
func TestAnEventClosesFromTheBestNumberAndSaysItGuessed(t *testing.T) {
	f := newFixture(t)
	f.meter("f150-odometer", resource.MeterSpec{Description: "Odometer", Unit: "miles", StaleAfter: "168h"})
	f.commitment("f150-oil", resource.CommitmentSpec{
		Description: "F-150 oil and filter", Rollover: resource.RolloverFromCompletion,
		Anchor: "2026-01-01",
		Due:    []resource.CommitmentDue{{Meter: "f150-odometer", Every: "5000", Lead: "300"}},
		SatisfiedBy: &resource.CommitmentSatisfiedBy{
			EventType: "vehicle.serviced", Entity: "f150",
		},
	})
	f.read("f150-odometer", 80000, at(2026, time.January, 1))
	f.event("vehicle.serviced", "f150", at(2026, time.July, 12))
	f.at(at(2026, time.July, 13))
	f.tick()
	done := f.completions("f150-oil")
	if len(done) != 1 {
		t.Fatalf("completions = %d, want 1: the truck was serviced and the household knows it", len(done))
	}
	if done[0].Positions["f150-odometer"] != 80000 {
		t.Errorf("position = %v, want the best reading it had", done[0].Positions["f150-odometer"])
	}
	if len(done[0].Approximate) != 1 || done[0].Approximate[0] != "f150-odometer" {
		t.Errorf("approximate = %v, want the odometer named", done[0].Approximate)
	}
}

// A meter nobody has EVER read is the one case left that refuses. There
// is no number to be roughly right with, and inventing one is the
// failure the whole package exists to prevent.
func TestAnEventCannotCloseACycleWithNoNumberAtAll(t *testing.T) {
	f := newFixture(t)
	f.meter("f150-odometer", resource.MeterSpec{Description: "Odometer", Unit: "miles"})
	f.commitment("f150-oil", resource.CommitmentSpec{
		Description: "F-150 oil and filter", Rollover: resource.RolloverFromCompletion,
		Anchor: "2026-01-01",
		Due:    []resource.CommitmentDue{{Meter: "f150-odometer", Every: "5000", Lead: "300"}},
		SatisfiedBy: &resource.CommitmentSatisfiedBy{
			EventType: "vehicle.serviced", Entity: "f150",
		},
	})
	f.event("vehicle.serviced", "f150", at(2026, time.July, 12))
	f.at(at(2026, time.July, 13))
	f.tick()
	if done := f.completions("f150-oil"); len(done) != 0 {
		t.Fatalf("a cycle was closed against an odometer nobody has ever read: %+v", done)
	}
}

// When the household WAS keeping the meter current, the event closes
// the cycle and the position it records is the one the meter stood at
// when the work was done.
func TestAnEventClosesACounterCycleItCanMeasure(t *testing.T) {
	f := newFixture(t)
	f.meter("f150-odometer", resource.MeterSpec{Description: "Odometer", Unit: "miles", StaleAfter: "168h"})
	f.commitment("f150-oil", resource.CommitmentSpec{
		Description: "F-150 oil and filter", Rollover: resource.RolloverFromCompletion,
		Anchor: "2026-01-01",
		Due:    []resource.CommitmentDue{{Meter: "f150-odometer", Every: "5000", Lead: "300"}},
		SatisfiedBy: &resource.CommitmentSatisfiedBy{
			EventType: "vehicle.serviced", Entity: "f150",
		},
	})
	f.read("f150-odometer", 80000, at(2026, time.January, 1))
	f.read("f150-odometer", 84900, at(2026, time.July, 10))
	f.event("vehicle.serviced", "f150", at(2026, time.July, 12))
	// A reading after the service, to prove the completion takes the one
	// from before it rather than the newest one lying around.
	f.read("f150-odometer", 85400, at(2026, time.July, 14))
	f.at(at(2026, time.July, 15))
	f.tick()
	done := f.completions("f150-oil")
	if len(done) != 1 {
		t.Fatalf("completions = %d, want 1", len(done))
	}
	if got := done[0].Positions["f150-odometer"]; got != 84900 {
		t.Errorf("position = %v, want 84900: the odometer as it stood when the truck was serviced", got)
	}
}

func (f *fixture) completions(name string) []runtimedb.CommitmentCompletion {
	f.t.Helper()
	done, err := f.db.ListCommitmentCompletions(name)
	if err != nil {
		f.t.Fatalf("ListCommitmentCompletions: %v", err)
	}
	return done
}

func (f *fixture) event(eventType, entity string, when time.Time) {
	f.t.Helper()
	if _, _, err := f.db.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID:    eventType + "@" + entity + "@" + when.Format(time.RFC3339),
		OccurredAt: when, Type: eventType, Entity: entity,
		Source: "publisher/garage", SchemaVersion: 1,
	}); err != nil {
		f.t.Fatalf("AppendHouseholdEvent: %v", err)
	}
}

// A one-off's cycle never reopens, so an event arriving after it was
// answered has nothing to close.
//
// Deduping by event id is not enough here: a SECOND `dental.visit` is a
// different event with a different id, and without this the engine
// would record a second completion against a commitment that only ever
// had one thing to do.
func TestASecondEventCannotCloseAOneOffTwice(t *testing.T) {
	f := newFixture(t)
	f.commitment("kai-dentist", resource.CommitmentSpec{
		Description: "The dentist, on the third", Rollover: resource.RolloverOnce,
		Anchor: "2026-01-01",
		Due:    []resource.CommitmentDue{{Meter: resource.MeterClock, At: "2026-03-03"}},
		SatisfiedBy: &resource.CommitmentSatisfiedBy{
			EventType: "dental.visit", Subject: "kai",
		},
	})
	f.eventFor("dental.visit", "kai", at(2026, time.March, 3))
	f.at(at(2026, time.March, 4))
	f.tick()
	if done := f.completions("kai-dentist"); len(done) != 1 {
		t.Fatalf("completions = %d, want 1", len(done))
	}
	if got := f.standings()["kai-dentist"].Occurrence.State; got != commitment.StateSatisfied {
		t.Fatalf("state = %q, want satisfied", got)
	}

	// Kai goes to the dentist again in September. That is not this
	// commitment coming round again — it has no next cycle.
	f.eventFor("dental.visit", "kai", at(2026, time.September, 9))
	f.at(at(2026, time.September, 10))
	f.tick()
	if done := f.completions("kai-dentist"); len(done) != 1 {
		t.Errorf("completions = %d, want 1: a one-off was closed twice", len(done))
	}
}

func (f *fixture) eventFor(eventType, subject string, when time.Time) {
	f.t.Helper()
	if _, _, err := f.db.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID:    eventType + "@" + subject + "@" + when.Format(time.RFC3339),
		OccurredAt: when, Type: eventType, Subject: subject,
		Source: "publisher/clinic", SchemaVersion: 1,
	}); err != nil {
		f.t.Fatalf("AppendHouseholdEvent: %v", err)
	}
}

// An event must not close the cycle that the work it describes OPENED.
//
// The harvest window ran from the open cycle's OpenedAt, which for a
// fromCompletion or fromSchedule cycle IS the moment the last
// completion landed. So a member who taps Done and a publisher who
// emits `health.visit_completed` minutes later — the same visit,
// recorded twice by two different doors — closed two cycles, and the
// commitment skipped a year.
func TestAnEventDoesNotCloseTheCycleItsOwnWorkOpened(t *testing.T) {
	f := newFixture(t)
	spec := checkup()
	spec.SatisfiedBy = &resource.CommitmentSatisfiedBy{EventType: "health.visit_completed", Subject: "kai"}
	f.commitment("kai-checkup", spec)

	f.at(at(2026, time.September, 20))
	f.tick()
	before := f.standings()["kai-checkup"].Occurrence.Cycle

	// The visit happens. A parent taps Done, and the clinic's publisher
	// emits the same fact a few minutes later.
	done := at(2026, time.September, 21)
	if _, _, err := f.db.RecordCommitmentCompletion(runtimedb.CommitmentCompletion{
		Commitment: "kai-checkup", At: done, RecordedBy: "mika"}); err != nil {
		t.Fatal(err)
	}
	f.eventFor("health.visit_completed", "kai", done.Add(5*time.Minute))
	f.at(at(2026, time.September, 22))
	f.tick()

	if got := f.completions("kai-checkup"); len(got) != 1 {
		t.Errorf("completions = %d, want 1: the visit was recorded twice by two doors "+
			"and closed two cycles", len(got))
		for _, c := range got {
			t.Logf("  %+v", c)
		}
	}
	if got := f.standings()["kai-checkup"].Occurrence.Cycle; got != before+1 {
		t.Errorf("cycle = %d, want %d: one visit advanced the commitment more than one cycle", got, before+1)
	}
}

// A commitment written for somebody keeps its standing when they are
// renamed (#372, ADR-W035 A21): who it is about, who may answer it and who
// hears about it all follow the Person, and read as their current name.
func TestACommitmentFollowsItsPeopleThroughARename(t *testing.T) {
	f := newFixture(t)
	for _, h := range []string{"kai", "mika"} {
		if err := f.db.RecordButlerMember(h, runtimedb.RoleMember, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	mika, _ := f.db.ResolvePersonRef("mika")
	spec := checkup()
	spec.MayComplete = []string{"household/mika", "household/" + mika, "household/nobody", "group/parents"}
	spec.Notify = []string{"household/mika"}
	f.commitment("kai-checkup", spec)
	for from, to := range map[string]string{"kai": "kaito", "mika": "micah"} {
		if err := f.db.RenameButlerMemberHandle(from, to); err != nil {
			t.Fatal(err)
		}
	}
	standings, err := f.e.Evaluate()
	if err != nil || len(standings) != 1 {
		t.Fatalf("Evaluate = %v, %v", standings, err)
	}
	st := standings[0]
	if st.Owner != "household/kaito" {
		t.Errorf("owner = %q, want the renamed subject", st.Owner)
	}
	want := []string{"household/micah", "household/micah", "household/nobody", "group/parents"}
	if len(st.MayComplete) != len(want) {
		t.Fatalf("mayComplete = %v", st.MayComplete)
	}
	for i := range want {
		if st.MayComplete[i] != want[i] {
			t.Errorf("mayComplete[%d] = %q, want %q", i, st.MayComplete[i], want[i])
		}
	}
	if len(st.Notify) != 1 || st.Notify[0] != "household/micah" {
		t.Errorf("notify = %v", st.Notify)
	}
}
