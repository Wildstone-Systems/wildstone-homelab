package commitment

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The three things that prompted ADR-W035, walked through a year.
//
// These are acceptance scenarios rather than unit tests: each one is a
// story with dates, and what is being checked is that the sequence of
// states a household would actually see is the right one. Run them with
// `go test -v -run Scenario` to read the timeline.

// step is one moment in a story.
type step struct {
	on      time.Time
	note    string
	reading float64 // 0 = no new reading
	done    *Completion
	booking *Booking
	want    State
	// wantUnresolved, when set, is a meter the occurrence should be
	// asking about.
	wantUnresolved string
}

// play runs a story, logging what a household would see and checking it.
func play(t *testing.T, c Commitment, meterName string, meter Meter, steps []step) {
	t.Helper()
	var history []Completion
	var booking *Booking
	t.Logf("%-12s %-11s %-9s %s", "DATE", "STATE", "TRIGGER", "WHAT THE HOUSEHOLD SEES")
	for _, s := range steps {
		if s.reading != 0 {
			meter.Readings = append(meter.Readings, Reading{At: s.on, Value: s.reading, By: "mika"})
		}
		if s.booking != nil {
			booking = s.booking
		}
		// A completion is recorded and THEN the day is evaluated, which
		// is the order the ledger writes them in: the cycle it closed is
		// already closed by the time anything asks where things stand.
		if s.done != nil {
			history = append(history, *s.done)
			booking = nil
		}
		in := Input{Commitment: c, Now: s.on, History: history, Booking: booking, Loc: chicago}
		if meterName != "" {
			in.Meters = map[string]Meter{meterName: meter}
		}
		occ, err := Evaluate(in)
		if err != nil {
			t.Fatalf("%s: %v", s.on.Format("2 Jan"), err)
		}
		extra := ""
		if len(occ.Unresolved) > 0 {
			extra = " [asking about " + strings.Join(occ.Unresolved, ", ") + "]"
		}
		if occ.Projection != nil {
			extra += fmt.Sprintf(" [projected %s, from %s]",
				occ.Projection.At.Format("2 Jan"), occ.Projection.Source)
		}
		t.Logf("%-12s %-11s %-9s %s%s", s.on.Format("2 Jan 2006"), occ.State, occ.Trigger, occ.Reason, extra)
		if s.note != "" {
			t.Logf("%-12s %s", "", "  -> "+s.note)
		}
		if occ.State != s.want {
			t.Errorf("%s: state = %q, want %q", s.on.Format("2 Jan 2006"), occ.State, s.want)
		}
		if s.wantUnresolved != "" {
			found := false
			for _, m := range occ.Unresolved {
				found = found || m == s.wantUnresolved
			}
			if !found {
				t.Errorf("%s: unresolved = %v, want it to name %q", s.on.Format("2 Jan 2006"), occ.Unresolved, s.wantUnresolved)
			}
		}
	}
}

// TestScenarioTheCheckupNobodyBooked is the one that started this: the
// practice was going to call, nobody was tracking that they hadn't, and
// a year went by.
func TestScenarioTheCheckupNobodyBooked(t *testing.T) {
	c := Commitment{
		Name:            "kai-annual-checkup",
		Anchor:          at(2025, time.September, 10), // last year's visit
		Rollover:        FromSchedule,                 // stays in September
		RequiresBooking: true,
		Due:             []DueRule{{Meter: MeterClock, Every: iv(t, "1yr"), Lead: iv(t, "60d")}},
	}
	play(t, c, "", Meter{}, []step{
		{on: at(2026, time.March, 1), want: StatePending,
			note: "nothing to do for months"},
		{on: at(2026, time.July, 12), want: StateDueSoon,
			note: "sixty days out: starts asking, and will not stop for an acknowledgement"},
		{on: at(2026, time.August, 15), want: StateDueSoon,
			note: "still nothing booked — this is the state that used to be invisible"},
		{on: at(2026, time.September, 11), want: StateOverdue,
			note: "the anniversary passed with no appointment"},
		{on: at(2026, time.September, 20), want: StateScheduled,
			booking: &Booking{At: at(2026, time.October, 6), By: "mika"},
			note:    "booked for 6 October — late, and the record says so"},
		{on: at(2026, time.October, 7), want: StateOverdue,
			note: "the appointment came and went with nothing recorded: back to overdue"},
		{on: at(2026, time.October, 9), want: StatePending,
			done: &Completion{At: at(2026, time.October, 9), By: "mika"},
			note: "seen. next cycle opens on the September schedule, not this October"},
		{on: at(2027, time.January, 1), want: StatePending},
		{on: at(2027, time.July, 12), want: StateDueSoon,
			note: "sixty days before 10 September 2027 — the slip did not move the schedule"},
	})
}

// TestScenarioTheTruckThatWentOverOnMiles is the case a calendar cannot
// express and a capability is not needed for.
func TestScenarioTheTruckThatWentOverOnMiles(t *testing.T) {
	c := Commitment{
		Name:     "f150-oil",
		Anchor:   at(2026, time.March, 1),
		AnchorAt: map[string]float64{"odo": 79300},
		Rollover: FromCompletion, // the clock starts when the oil is new
		Due: []DueRule{
			{Meter: "odo", Every: iv(t, "5000"), Lead: iv(t, "300")},
			{Meter: MeterClock, Every: iv(t, "6mo"), Lead: iv(t, "14d")},
		},
	}
	odo := Meter{Name: "odo", Unit: "miles", StaleAfter: 45 * 24 * time.Hour}

	play(t, c, "odo", odo, []step{
		{on: at(2026, time.March, 1), reading: 79300, want: StatePending,
			note: "tracking starts. due at 84,300 miles or 1 September, whichever first"},
		{on: at(2026, time.May, 15), reading: 81500, want: StatePending,
			note: "a number typed into the Console. no capability involved"},
		{on: at(2026, time.August, 20), reading: 83900, want: StateDueSoon,
			note: "the calendar got there first this year: 400 miles short, but inside the fortnight"},
		{on: at(2026, time.September, 5), want: StateOverdue,
			note: "six months up"},
		{on: at(2026, time.September, 6), reading: 84050, want: StatePending,
			done: &Completion{At: at(2026, time.September, 6), By: "mika",
				Positions: map[string]float64{"odo": 84050}},
			note: "changed at 84,050. next: 89,050 miles or 6 March"},
		{on: at(2026, time.November, 1), want: StatePending, wantUnresolved: "odo",
			note: "no reading in eight weeks: it asks for a number rather than guessing"},
		{on: at(2026, time.November, 2), reading: 86000, want: StatePending,
			note: "answered. back to a real position"},
	})
}

// TestScenarioTheFilterWithNoDateOnIt is the simplest shape: no booking,
// no meter, nothing installed.
func TestScenarioTheFilterWithNoDateOnIt(t *testing.T) {
	c := Commitment{
		Name:     "furnace-filter",
		Anchor:   at(2026, time.June, 1),
		Rollover: FromCompletion,
		Due:      []DueRule{{Meter: MeterClock, Every: iv(t, "3mo"), Lead: iv(t, "14d")}},
	}
	play(t, c, "", Meter{}, []step{
		{on: at(2026, time.June, 1), want: StatePending},
		{on: at(2026, time.August, 18), want: StateDueSoon,
			note: "a fortnight's warning"},
		{on: at(2026, time.September, 4), want: StatePending,
			done: &Completion{At: at(2026, time.September, 4), By: "mika"},
			note: "done. next 4 December — measured from the new filter, not the old schedule"},
		{on: at(2026, time.November, 20), want: StateDueSoon},
		{on: at(2026, time.December, 10), want: StateOverdue},
	})
}

// TestScenarioTheTruckWithHomeAssistantIsTheSameTruck pins ADR-W035 A4:
// an installed capability changes when the household is asked, and
// changes nothing about the answer.
func TestScenarioTheTruckWithHomeAssistantIsTheSameTruck(t *testing.T) {
	c := Commitment{
		Name: "f150-oil", Anchor: at(2026, time.March, 1),
		AnchorAt: map[string]float64{"odo": 79300}, Rollover: FromCompletion,
		Due: []DueRule{{Meter: "odo", Every: iv(t, "5000"), Lead: iv(t, "300")}},
	}
	now := at(2026, time.August, 20)

	// Door one: a person typed it into the Console.
	typed := Meter{Name: "odo", Readings: readings(now, 84100.0)}
	// Door four: Home Assistant saw the truck arrive home and an
	// Automation asked; the answer came back through the event intake.
	// Same number, same moment, different provenance.
	published := Meter{Name: "odo", Readings: []Reading{{At: now, Value: 84100, By: "publisher-home-automation"}}}

	a := eval(t, Input{Commitment: c, Meters: map[string]Meter{"odo": typed}, Now: now})
	b := eval(t, Input{Commitment: c, Meters: map[string]Meter{"odo": published}, Now: now})

	if a.State != b.State || a.Trigger != b.Trigger || a.Reason != b.Reason {
		t.Errorf("the door a reading came through changed the answer:\n typed:     %+v\n published: %+v", a, b)
	}
	if a.State != StateDueSoon {
		t.Errorf("state = %q, want due_soon", a.State)
	}
	t.Logf("both doors: %s", a.Reason)
}
