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
	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "runtime.db"))
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

	if _, err := f.db.RecordCommitmentCompletion(runtimedb.CommitmentCompletion{
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
