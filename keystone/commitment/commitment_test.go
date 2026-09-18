package commitment

import (
	"slices"
	"strings"
	"testing"
	"time"
)

var chicago = mustLoc("America/Chicago")

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// at is a local wall-clock moment in the household's timezone.
func at(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 9, 0, 0, 0, chicago)
}

func iv(t *testing.T, s string) Interval {
	t.Helper()
	got, err := ParseInterval(s)
	if err != nil {
		t.Fatalf("ParseInterval(%q): %v", s, err)
	}
	return got
}

func readings(vals ...any) []Reading {
	var out []Reading
	for i := 0; i < len(vals); i += 2 {
		out = append(out, Reading{At: vals[i].(time.Time), Value: vals[i+1].(float64)})
	}
	return out
}

func eval(t *testing.T, in Input) Occurrence {
	t.Helper()
	in.Loc = chicago
	occ, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return occ
}

func ruleFor(occ Occurrence, meter string) RuleStatus {
	for _, r := range occ.Rules {
		if r.Meter == meter {
			return r
		}
	}
	return RuleStatus{}
}

// ---------------------------------------------------------------------
// The clock is a meter like any other (A2).
// ---------------------------------------------------------------------

func TestClockCommitmentWalksPendingToOverdue(t *testing.T) {
	// A filter changed on 1 June, due every three months, warning a
	// fortnight out. Due 1 September; due_soon from 18 August.
	c := Commitment{
		Name:     "furnace-filter",
		Anchor:   at(2026, time.June, 1),
		Rollover: FromCompletion,
		Due:      []DueRule{{Meter: MeterClock, Every: iv(t, "3mo"), Lead: iv(t, "14d")}},
	}

	for _, tc := range []struct {
		now  time.Time
		want State
	}{
		{at(2026, time.June, 2), StatePending},
		{at(2026, time.August, 17), StatePending},
		{at(2026, time.August, 18), StateDueSoon},
		{at(2026, time.August, 31), StateDueSoon},
		{at(2026, time.September, 1), StateOverdue},
		{at(2026, time.September, 20), StateOverdue},
	} {
		occ := eval(t, Input{Commitment: c, Now: tc.now})
		if occ.State != tc.want {
			t.Errorf("on %s: state = %q, want %q (%s)",
				tc.now.Format("2 Jan"), occ.State, tc.want, occ.Reason)
		}
		if occ.Cycle != 1 {
			t.Errorf("on %s: cycle = %d, want 1", tc.now.Format("2 Jan"), occ.Cycle)
		}
	}

	// The calendar rendering is there for a person to read.
	occ := eval(t, Input{Commitment: c, Now: at(2026, time.August, 20)})
	if got := ruleFor(occ, MeterClock).DueTime; !got.Equal(at(2026, time.September, 1)) {
		t.Errorf("dueTime = %s, want 1 Sep 2026", got)
	}
}

// ---------------------------------------------------------------------
// A counter meter, and the arithmetic behind the verdict.
// ---------------------------------------------------------------------

func TestCounterCommitmentWalksPendingToOverdue(t *testing.T) {
	c := Commitment{
		Name:     "f150-oil",
		Anchor:   at(2026, time.March, 1),
		AnchorAt: map[string]float64{"odo": 79300},
		Rollover: FromCompletion,
		Due:      []DueRule{{Meter: "odo", Every: iv(t, "5000"), Lead: iv(t, "300")}},
	}

	for _, tc := range []struct {
		miles float64
		want  State
	}{
		{79300, StatePending},
		{83999, StatePending},
		{84000, StateDueSoon}, // 84300 - 300
		{84299, StateDueSoon},
		{84300, StateOverdue},
		{85000, StateOverdue},
	} {
		in := Input{
			Commitment: c,
			Meters:     map[string]Meter{"odo": {Name: "odo", Unit: "miles", Readings: readings(at(2026, time.June, 1), tc.miles)}},
			Now:        at(2026, time.June, 2),
		}
		occ := eval(t, in)
		if occ.State != tc.want {
			t.Errorf("at %g miles: state = %q, want %q (%s)", tc.miles, occ.State, tc.want, occ.Reason)
		}
		st := ruleFor(occ, "odo")
		if st.DueAt != 84300 {
			t.Errorf("at %g miles: dueAt = %g, want 84300", tc.miles, st.DueAt)
		}
		if want := 84300 - tc.miles; st.Remaining != want {
			t.Errorf("at %g miles: remaining = %g, want %g", tc.miles, st.Remaining, want)
		}
	}
}

// ---------------------------------------------------------------------
// Several meters, earliest wins (A10).
// ---------------------------------------------------------------------

func TestEarliestOfSeveralMetersDecides(t *testing.T) {
	// Every 5,000 miles or 6 months, whichever comes first.
	c := Commitment{
		Name:     "f150-oil",
		Anchor:   at(2026, time.March, 1),
		AnchorAt: map[string]float64{"odo": 79300},
		Rollover: FromCompletion,
		Due: []DueRule{
			{Meter: "odo", Every: iv(t, "5000"), Lead: iv(t, "300")},
			{Meter: MeterClock, Every: iv(t, "6mo"), Lead: iv(t, "14d")},
		},
	}
	meters := func(day time.Time, miles float64) map[string]Meter {
		return map[string]Meter{"odo": {Name: "odo", Readings: readings(day, miles)}}
	}

	// A light-mileage year: the calendar gets there first.
	occ := eval(t, Input{
		Commitment: c,
		Meters:     meters(at(2026, time.August, 20), 81900),
		Now:        at(2026, time.August, 20),
	})
	if occ.State != StateDueSoon || occ.Trigger != MeterClock {
		t.Errorf("light mileage: state %q trigger %q, want due_soon/clock (%s)", occ.State, occ.Trigger, occ.Reason)
	}

	// A heavy-mileage year: the odometer gets there first, well before
	// the six months are up.
	occ = eval(t, Input{
		Commitment: c,
		Meters:     meters(at(2026, time.June, 10), 84100),
		Now:        at(2026, time.June, 10),
	})
	if occ.State != StateDueSoon || occ.Trigger != "odo" {
		t.Errorf("heavy mileage: state %q trigger %q, want due_soon/odo (%s)", occ.State, occ.Trigger, occ.Reason)
	}

	// Crossed beats merely close, whichever meter it is.
	occ = eval(t, Input{
		Commitment: c,
		Meters:     meters(at(2026, time.June, 10), 84500),
		Now:        at(2026, time.June, 10),
	})
	if occ.State != StateOverdue || occ.Trigger != "odo" {
		t.Errorf("crossed: state %q trigger %q, want overdue/odo (%s)", occ.State, occ.Trigger, occ.Reason)
	}
}

func TestWhenBothMetersQualifyTheFurtherAlongOneSpeaks(t *testing.T) {
	// The tie-break, isolated: two thresholds both inside their lead
	// window at once. The one further along its OWN interval decides, so
	// that a month and a mile can be compared at all — and so that the
	// answer does not depend on the order the rules happen to be written
	// in. The odometer rule is declared first in both cases below.
	c := Commitment{
		Name:     "f150-oil",
		Anchor:   at(2026, time.March, 1),
		AnchorAt: map[string]float64{"odo": 79300},
		Rollover: FromCompletion,
		Due: []DueRule{
			{Meter: "odo", Every: iv(t, "5000"), Lead: iv(t, "300")},     // due 84300, soon from 84000
			{Meter: MeterClock, Every: iv(t, "6mo"), Lead: iv(t, "30d")}, // due 1 Sep, soon from 2 Aug
		},
	}
	day := at(2026, time.August, 25) // clock is 177/184 = 96.2% through

	// Both soon; the clock is further along, and it is declared second.
	occ := eval(t, Input{
		Commitment: c,
		Meters:     map[string]Meter{"odo": {Name: "odo", Readings: readings(day, 84100.0)}}, // 96.0%
		Now:        day,
	})
	if !ruleFor(occ, "odo").Soon || !ruleFor(occ, MeterClock).Soon {
		t.Fatalf("this test needs both rules inside their lead window: odo=%+v clock=%+v",
			ruleFor(occ, "odo").Soon, ruleFor(occ, MeterClock).Soon)
	}
	if occ.Trigger != MeterClock {
		t.Errorf("trigger = %q, want clock: it is further along (clock %.3f vs odo %.3f)",
			occ.Trigger, ruleFor(occ, MeterClock).Progress, ruleFor(occ, "odo").Progress)
	}

	// Same day, more miles: now the odometer is the further along.
	occ = eval(t, Input{
		Commitment: c,
		Meters:     map[string]Meter{"odo": {Name: "odo", Readings: readings(day, 84280.0)}}, // 99.6%
		Now:        day,
	})
	if occ.Trigger != "odo" {
		t.Errorf("trigger = %q, want odo (odo %.3f vs clock %.3f)",
			occ.Trigger, ruleFor(occ, "odo").Progress, ruleFor(occ, MeterClock).Progress)
	}

	// And with both thresholds crossed, the same rule applies: the one
	// that went furthest past speaks. On 10 September the clock is 9 days
	// into a 184-day interval past its threshold (104.9%), so it takes
	// more than a token overshoot for the odometer to outrank it.
	occ = eval(t, Input{
		Commitment: c,
		Meters:     map[string]Meter{"odo": {Name: "odo", Readings: readings(at(2026, time.September, 10), 84800.0)}}, // 110%
		Now:        at(2026, time.September, 10),
	})
	if !ruleFor(occ, "odo").Crossed || !ruleFor(occ, MeterClock).Crossed {
		t.Fatal("this test needs both thresholds crossed")
	}
	if occ.State != StateOverdue || occ.Trigger != "odo" {
		t.Errorf("state %q trigger %q, want overdue/odo (odo %.3f vs clock %.3f)",
			occ.State, occ.Trigger, ruleFor(occ, "odo").Progress, ruleFor(occ, MeterClock).Progress)
	}
}

// ---------------------------------------------------------------------
// Rollover: the two modes, and why both are needed (A9).
// ---------------------------------------------------------------------

func TestRolloverFromCompletionMeasuresFromTheWork(t *testing.T) {
	c := Commitment{
		Name:     "f150-oil",
		Anchor:   at(2026, time.March, 1),
		AnchorAt: map[string]float64{"odo": 79300},
		Rollover: FromCompletion,
		Due:      []DueRule{{Meter: "odo", Every: iv(t, "5000"), Lead: iv(t, "300")}},
	}
	// Done late, at 84,800 rather than the 84,300 it was due at. The next
	// one is measured from the oil that is actually in the engine.
	occ := eval(t, Input{
		Commitment: c,
		Meters:     map[string]Meter{"odo": {Name: "odo", Readings: readings(at(2026, time.July, 4), 84800.0, at(2026, time.July, 20), 85200.0)}},
		History:    []Completion{{At: at(2026, time.July, 4), Positions: map[string]float64{"odo": 84800}}},
		Now:        at(2026, time.July, 20),
	})
	if occ.Cycle != 2 {
		t.Fatalf("cycle = %d, want 2", occ.Cycle)
	}
	if got := ruleFor(occ, "odo").DueAt; got != 89800 {
		t.Errorf("dueAt = %g, want 89800 (84800 + 5000)", got)
	}
	if occ.State != StatePending {
		t.Errorf("state = %q, want pending (%s)", occ.State, occ.Reason)
	}
}

func TestRolloverFromScheduleIgnoresWhenItWasDone(t *testing.T) {
	// The checkup: anchored to September, and it stays in September
	// however late the visit happens.
	c := Commitment{
		Name:     "kai-annual-checkup",
		Anchor:   at(2025, time.September, 10),
		Rollover: FromSchedule,
		Due:      []DueRule{{Meter: MeterClock, Every: iv(t, "1yr"), Lead: iv(t, "60d")}},
	}

	// Done more or less on time.
	occ := eval(t, Input{
		Commitment: c,
		History:    []Completion{{At: at(2026, time.September, 15)}},
		Now:        at(2026, time.October, 1),
	})
	if got := ruleFor(occ, MeterClock).DueTime; !got.Equal(at(2027, time.September, 10)) {
		t.Errorf("after an on-time visit: next due %s, want 10 Sep 2027", got.Format("2 Jan 2006"))
	}

	// Done two months late. fromCompletion would push next year's visit
	// to November; fromSchedule must not.
	occ = eval(t, Input{
		Commitment: c,
		History:    []Completion{{At: at(2026, time.November, 20)}},
		Now:        at(2026, time.December, 1),
	})
	if got := ruleFor(occ, MeterClock).DueTime; !got.Equal(at(2027, time.September, 10)) {
		t.Errorf("after a late visit: next due %s, want 10 Sep 2027 (not November)", got.Format("2 Jan 2006"))
	}

	// Done so late that a whole slot was skipped: the next one is the
	// next slot in the FUTURE, not one already behind us.
	occ = eval(t, Input{
		Commitment: c,
		History:    []Completion{{At: at(2027, time.November, 20)}},
		Now:        at(2027, time.December, 1),
	})
	if got := ruleFor(occ, MeterClock).DueTime; !got.Equal(at(2028, time.September, 10)) {
		t.Errorf("after a very late visit: next due %s, want 10 Sep 2028", got.Format("2 Jan 2006"))
	}
}

func TestTheTwoRolloverModesDivergeOnTheSameHistory(t *testing.T) {
	// The point of having both, on one set of facts: an annual thing
	// done six weeks late.
	base := Commitment{
		Name:   "annual",
		Anchor: at(2025, time.September, 10),
		Due:    []DueRule{{Meter: MeterClock, Every: iv(t, "1yr"), Lead: iv(t, "60d")}},
	}
	history := []Completion{{At: at(2026, time.October, 25)}}

	sched, comp := base, base
	sched.Rollover, comp.Rollover = FromSchedule, FromCompletion

	a := eval(t, Input{Commitment: sched, History: history, Now: at(2026, time.November, 1)})
	b := eval(t, Input{Commitment: comp, History: history, Now: at(2026, time.November, 1)})

	if got := ruleFor(a, MeterClock).DueTime; !got.Equal(at(2027, time.September, 10)) {
		t.Errorf("fromSchedule: %s, want 10 Sep 2027", got.Format("2 Jan 2006"))
	}
	if got := ruleFor(b, MeterClock).DueTime; !got.Equal(at(2027, time.October, 25)) {
		t.Errorf("fromCompletion: %s, want 25 Oct 2027", got.Format("2 Jan 2006"))
	}
}

func TestWaivingClosesTheCycleWithoutClaimingItWasDone(t *testing.T) {
	c := Commitment{
		Name:     "furnace-filter",
		Anchor:   at(2026, time.June, 1),
		Rollover: FromCompletion,
		Due:      []DueRule{{Meter: MeterClock, Every: iv(t, "3mo"), Lead: iv(t, "14d")}},
	}
	occ := eval(t, Input{
		Commitment: c,
		History:    []Completion{{At: at(2026, time.September, 3), Waived: true, Reason: "away, doing it next month"}},
		Now:        at(2026, time.September, 4),
	})
	if occ.Cycle != 2 {
		t.Errorf("cycle = %d, want 2: a waive closes the cycle like a completion", occ.Cycle)
	}
	if occ.State != StatePending {
		t.Errorf("state = %q, want pending", occ.State)
	}
	if got := ruleFor(occ, MeterClock).DueTime; !got.Equal(at(2026, time.December, 3)) {
		t.Errorf("next due %s, want 3 Dec 2026", got.Format("2 Jan 2006"))
	}
}

// ---------------------------------------------------------------------
// Booking: knowing is not the same as having an appointment (A7).
// ---------------------------------------------------------------------

func checkup(t *testing.T) Commitment {
	t.Helper()
	return Commitment{
		Name:            "kai-annual-checkup",
		Anchor:          at(2025, time.September, 10),
		Rollover:        FromSchedule,
		RequiresBooking: true,
		Due:             []DueRule{{Meter: MeterClock, Every: iv(t, "1yr"), Lead: iv(t, "60d")}},
	}
}

func TestDueSoonWithNoBookingKeepsAsking(t *testing.T) {
	occ := eval(t, Input{Commitment: checkup(t), Now: at(2026, time.August, 1)})
	if occ.State != StateDueSoon {
		t.Fatalf("state = %q, want due_soon", occ.State)
	}
	if !strings.Contains(occ.Reason, "no appointment") {
		t.Errorf("reason = %q, want it to say there is no appointment", occ.Reason)
	}
	// Two months later, still nothing booked: it does not quieten down,
	// it escalates. This is the failure the whole design exists for.
	occ = eval(t, Input{Commitment: checkup(t), Now: at(2026, time.October, 1)})
	if occ.State != StateOverdue {
		t.Errorf("state = %q, want overdue", occ.State)
	}
}

func TestBookingMovesItToScheduled(t *testing.T) {
	occ := eval(t, Input{
		Commitment: checkup(t),
		Booking:    &Booking{At: at(2026, time.September, 15), By: "mika"},
		Now:        at(2026, time.August, 1),
	})
	if occ.State != StateScheduled {
		t.Errorf("state = %q, want scheduled", occ.State)
	}
	if occ.PastDue {
		t.Errorf("pastDue = true, want false: booked before the threshold")
	}
}

func TestBookedButAlreadyLateIsScheduledAndSaysSo(t *testing.T) {
	occ := eval(t, Input{
		Commitment: checkup(t),
		Booking:    &Booking{At: at(2026, time.September, 15)},
		Now:        at(2026, time.September, 12), // past the 10 Sep threshold
	})
	if occ.State != StateScheduled {
		t.Errorf("state = %q, want scheduled: the appointment exists", occ.State)
	}
	if !occ.PastDue {
		t.Errorf("pastDue = false, want true: the threshold is behind us")
	}
}

func TestAMissedAppointmentGoesBackToOverdue(t *testing.T) {
	// The appointment came and went and nothing was recorded. That is
	// not "scheduled", and it is the second way this gets missed.
	occ := eval(t, Input{
		Commitment: checkup(t),
		Booking:    &Booking{At: at(2026, time.September, 15)},
		Now:        at(2026, time.September, 16),
	})
	if occ.State != StateOverdue {
		t.Errorf("state = %q, want overdue", occ.State)
	}
	if !occ.PastDue {
		t.Errorf("pastDue = false, want true")
	}
}

func TestBookingEarlyWhileStillPending(t *testing.T) {
	occ := eval(t, Input{
		Commitment: checkup(t),
		Booking:    &Booking{At: at(2026, time.September, 15)},
		Now:        at(2026, time.May, 1), // long before the lead window
	})
	if occ.State != StateScheduled {
		t.Errorf("state = %q, want scheduled: booking early is allowed", occ.State)
	}
}

func TestACommitmentThatNeedsNoBookingIsNeverScheduled(t *testing.T) {
	c := Commitment{
		Name: "furnace-filter", Anchor: at(2026, time.June, 1), Rollover: FromCompletion,
		Due: []DueRule{{Meter: MeterClock, Every: iv(t, "3mo"), Lead: iv(t, "14d")}},
	}
	occ := eval(t, Input{Commitment: c, Now: at(2026, time.August, 20)})
	if occ.State != StateDueSoon {
		t.Errorf("state = %q, want due_soon", occ.State)
	}
	if occ.PastDue {
		t.Errorf("pastDue = true on a commitment that books nothing")
	}
}

// ---------------------------------------------------------------------
// Not knowing is a state (A11).
// ---------------------------------------------------------------------

func staleTruck(t *testing.T) Commitment {
	t.Helper()
	return Commitment{
		Name: "f150-oil", Anchor: at(2026, time.March, 1),
		AnchorAt: map[string]float64{"odo": 79300}, Rollover: FromCompletion,
		Due: []DueRule{{Meter: "odo", Every: iv(t, "5000"), Lead: iv(t, "300")}},
	}
}

func TestAStaleReadingCannotSupportNotDueYet(t *testing.T) {
	in := Input{
		Commitment: staleTruck(t),
		Meters: map[string]Meter{"odo": {
			Name: "odo", StaleAfter: 45 * 24 * time.Hour,
			Readings: readings(at(2026, time.June, 1), 82000.0),
		}},
		Now: at(2026, time.September, 1), // 92 days later
	}
	occ := eval(t, in)
	if occ.State != StatePending {
		t.Errorf("state = %q, want pending: nothing is known well enough to escalate", occ.State)
	}
	if !slices.Contains(occ.Unresolved, "odo") {
		t.Errorf("unresolved = %v, want it to name odo", occ.Unresolved)
	}
	if occ.Trigger != "" {
		t.Errorf("trigger = %q, want empty: no rule was fit to decide", occ.Trigger)
	}
	if !strings.Contains(occ.Reason, "needs a reading") {
		t.Errorf("reason = %q, want it to ask for a reading", occ.Reason)
	}
}

func TestAStaleReadingThatAlreadyCrossedIsStillOverdue(t *testing.T) {
	// 84,900 against a threshold of 84,300 is past due whether the number
	// was taken this morning or two months ago. Staleness undermines the
	// negative conclusion only.
	occ := eval(t, Input{
		Commitment: staleTruck(t),
		Meters: map[string]Meter{"odo": {
			Name: "odo", StaleAfter: 45 * 24 * time.Hour,
			Readings: readings(at(2026, time.June, 1), 84900.0),
		}},
		Now: at(2026, time.September, 1),
	})
	if occ.State != StateOverdue {
		t.Errorf("state = %q, want overdue", occ.State)
	}
	if len(occ.Unresolved) != 0 {
		t.Errorf("unresolved = %v, want none: the reading settled it", occ.Unresolved)
	}
	if !ruleFor(occ, "odo").Stale {
		t.Errorf("the rule should still be marked stale, for the record")
	}
}

func TestAMeterNeverReadIsUnresolvedNotZero(t *testing.T) {
	occ := eval(t, Input{
		Commitment: staleTruck(t),
		Meters:     map[string]Meter{"odo": {Name: "odo"}},
		Now:        at(2026, time.September, 1),
	})
	if occ.State != StatePending || !slices.Contains(occ.Unresolved, "odo") {
		t.Errorf("state %q unresolved %v, want pending and unresolved", occ.State, occ.Unresolved)
	}
	if ruleFor(occ, "odo").Known {
		t.Errorf("a meter with no readings must not report a known position")
	}
}

func TestOneUnresolvedMeterDoesNotSilenceTheOther(t *testing.T) {
	// The odometer is stale, but six months is six months.
	c := staleTruck(t)
	c.Due = append(c.Due, DueRule{Meter: MeterClock, Every: iv(t, "6mo"), Lead: iv(t, "14d")})
	occ := eval(t, Input{
		Commitment: c,
		Meters: map[string]Meter{"odo": {
			Name: "odo", StaleAfter: 45 * 24 * time.Hour,
			Readings: readings(at(2026, time.June, 1), 82000.0),
		}},
		Now: at(2026, time.September, 20),
	})
	if occ.State != StateOverdue || occ.Trigger != MeterClock {
		t.Errorf("state %q trigger %q, want overdue/clock (%s)", occ.State, occ.Trigger, occ.Reason)
	}
	if !slices.Contains(occ.Unresolved, "odo") {
		t.Errorf("unresolved = %v, want odo still listed: a reading is still wanted", occ.Unresolved)
	}
}

func TestACompletionWithNoEvidenceCannotAnchorTheNextCycle(t *testing.T) {
	// ADR-W034 A8: an oil change recorded with no mileage is a note, not
	// a maintenance record, and the function says which.
	occ := eval(t, Input{
		Commitment: staleTruck(t),
		Meters:     map[string]Meter{"odo": {Name: "odo", Readings: readings(at(2026, time.July, 10), 85000.0)}},
		History:    []Completion{{At: at(2026, time.July, 4)}}, // no Positions
		Now:        at(2026, time.July, 10),
	})
	if !slices.Contains(occ.Unresolved, "odo") {
		t.Fatalf("unresolved = %v, want odo", occ.Unresolved)
	}
	if got := ruleFor(occ, "odo").Unresolved; !strings.Contains(got, "recorded no reading") {
		t.Errorf("explanation = %q, want it to name the missing evidence", got)
	}
}

// ---------------------------------------------------------------------
// A replaced meter keeps the obligation (Reading.Rebase).
// ---------------------------------------------------------------------

func TestARebaseCarriesTheThresholdIntoTheNewCoordinates(t *testing.T) {
	// Due at 84,300 on the old odometer. It was replaced at 84,210 and
	// the new unit started from zero, so the remaining 90 miles are still
	// 90 miles.
	occ := eval(t, Input{
		Commitment: staleTruck(t),
		Meters: map[string]Meter{"odo": {Name: "odo", Readings: []Reading{
			{At: at(2026, time.June, 1), Value: 84210},
			{At: at(2026, time.June, 2), Value: 0, Rebase: true, Reason: "cluster replaced"},
			{At: at(2026, time.June, 10), Value: 40},
		}}},
		Now: at(2026, time.June, 10),
	})
	st := ruleFor(occ, "odo")
	if st.DueAt != 90 {
		t.Errorf("dueAt = %g, want 90 (84300 - 84210)", st.DueAt)
	}
	if st.Remaining != 50 {
		t.Errorf("remaining = %g, want 50", st.Remaining)
	}
	if occ.State != StateDueSoon {
		t.Errorf("state = %q, want due_soon: 50 miles is inside the 300-mile lead", occ.State)
	}
}

func TestARebaseWithAnExplicitPriorNeedsNoEarlierReading(t *testing.T) {
	occ := eval(t, Input{
		Commitment: staleTruck(t),
		Meters: map[string]Meter{"odo": {Name: "odo", Readings: []Reading{
			{At: at(2026, time.June, 2), Value: 0, Rebase: true, Prior: 84210},
			{At: at(2026, time.June, 10), Value: 40},
		}}},
		Now: at(2026, time.June, 10),
	})
	if got := ruleFor(occ, "odo").DueAt; got != 90 {
		t.Errorf("dueAt = %g, want 90", got)
	}
}

func TestARebaseWithNothingToTranslateFromIsUnresolved(t *testing.T) {
	// Nobody wrote the number down before the swap, so the old threshold
	// cannot be carried across. Say so; do not invent it.
	occ := eval(t, Input{
		Commitment: staleTruck(t),
		Meters: map[string]Meter{"odo": {Name: "odo", Readings: []Reading{
			{At: at(2026, time.June, 2), Value: 0, Rebase: true},
			{At: at(2026, time.June, 10), Value: 40},
		}}},
		Now: at(2026, time.June, 10),
	})
	if !slices.Contains(occ.Unresolved, "odo") {
		t.Errorf("unresolved = %v, want odo", occ.Unresolved)
	}
	if got := ruleFor(occ, "odo").Unresolved; !strings.Contains(got, "replaced") {
		t.Errorf("explanation = %q, want it to name the replacement", got)
	}
}

// ---------------------------------------------------------------------
// Projections are decoration (A11).
// ---------------------------------------------------------------------

func TestAProjectionIsOfferedAndChangesNothing(t *testing.T) {
	c := staleTruck(t)
	in := Input{
		Commitment: c,
		Meters: map[string]Meter{"odo": {Name: "odo", Readings: readings(
			at(2026, time.March, 1), 79300.0,
			at(2026, time.May, 30), 82300.0, // 3,000 miles in 90 days
		)}},
		Now: at(2026, time.May, 30),
	}
	occ := eval(t, in)
	if occ.State != StatePending {
		t.Fatalf("state = %q, want pending", occ.State)
	}
	if occ.Projection == nil {
		t.Fatal("no projection offered")
	}
	if occ.Projection.Source != "readings" {
		t.Errorf("source = %q, want readings", occ.Projection.Source)
	}
	// 2,000 miles to go at 33.3/day is about 60 days out.
	want := at(2026, time.July, 29)
	if d := occ.Projection.At.Sub(want); d > 36*time.Hour || d < -36*time.Hour {
		t.Errorf("projected %s, want about %s", occ.Projection.At.Format("2 Jan 2006"), want.Format("2 Jan 2006"))
	}

	// The declared estimate wins when there is one, and the state is the
	// same either way.
	m := in.Meters["odo"]
	m.Estimate = 100
	in.Meters = map[string]Meter{"odo": m}
	occ2 := eval(t, in)
	if occ2.Projection.Source != "estimate" || occ2.Projection.RatePerDay != 100 {
		t.Errorf("projection = %+v, want the declared estimate", occ2.Projection)
	}
	if occ2.State != occ.State {
		t.Errorf("a projection changed the state: %q then %q", occ.State, occ2.State)
	}
}

func TestNoProjectionWithoutSomethingToProjectFrom(t *testing.T) {
	occ := eval(t, Input{
		Commitment: staleTruck(t),
		Meters:     map[string]Meter{"odo": {Name: "odo", Readings: readings(at(2026, time.May, 1), 80000.0)}},
		Now:        at(2026, time.May, 2),
	})
	if occ.Projection != nil {
		t.Errorf("projection = %+v, want none from a single reading", occ.Projection)
	}
}

// ---------------------------------------------------------------------
// Purity, which is the property the rest of the design leans on (A5).
// ---------------------------------------------------------------------

func TestEvaluateIsPureAndDoesNotDisturbItsInputs(t *testing.T) {
	in := Input{
		Commitment: staleTruck(t),
		Meters: map[string]Meter{"odo": {Name: "odo", Readings: []Reading{
			// Deliberately out of order: the function sorts a copy.
			{At: at(2026, time.June, 10), Value: 84100},
			{At: at(2026, time.March, 1), Value: 79300},
		}}},
		History: []Completion{},
		Now:     at(2026, time.June, 10),
		Loc:     chicago,
	}
	before := slices.Clone(in.Meters["odo"].Readings)

	a, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	b, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if a.State != b.State || a.Trigger != b.Trigger || a.Reason != b.Reason {
		t.Errorf("two evaluations of one input disagreed:\n%+v\n%+v", a, b)
	}
	if !slices.EqualFunc(before, in.Meters["odo"].Readings, func(x, y Reading) bool {
		return x.At.Equal(y.At) && x.Value == y.Value
	}) {
		t.Errorf("Evaluate reordered the caller's readings")
	}
	if a.State != StateDueSoon {
		t.Errorf("state = %q, want due_soon at 84,100", a.State)
	}
}

func TestHistoryNeedNotArriveInOrder(t *testing.T) {
	c := checkup(t)
	shuffled := []Completion{
		{At: at(2026, time.September, 15)},
		{At: at(2024, time.September, 11)},
		{At: at(2025, time.September, 9)},
	}
	occ := eval(t, Input{Commitment: c, History: shuffled, Now: at(2026, time.October, 1)})
	if occ.Cycle != 4 {
		t.Errorf("cycle = %d, want 4", occ.Cycle)
	}
	if !occ.OpenedAt.Equal(at(2026, time.September, 15)) {
		t.Errorf("openedAt = %s, want the newest completion", occ.OpenedAt.Format("2 Jan 2006"))
	}
}

// ---------------------------------------------------------------------
// Refusals. A prototype that accepts nonsense teaches nothing.
// ---------------------------------------------------------------------

func TestEvaluateRefusesIncoherentInput(t *testing.T) {
	ok := Commitment{
		Name: "x", Anchor: at(2026, time.June, 1), Rollover: FromCompletion,
		Due: []DueRule{{Meter: MeterClock, Every: iv(t, "3mo")}},
	}
	meters := map[string]Meter{"odo": {Name: "odo", Readings: readings(at(2026, time.June, 1), 1.0)}}

	for _, tc := range []struct {
		name string
		in   Input
		want string
	}{
		{"no rollover", Input{Commitment: func() Commitment { c := ok; c.Rollover = ""; return c }(), Now: at(2026, time.June, 2)}, "fromCompletion or fromSchedule"},
		{"no due rule", Input{Commitment: func() Commitment { c := ok; c.Due = nil; return c }(), Now: at(2026, time.June, 2)}, "no due rule"},
		{"no anchor", Input{Commitment: func() Commitment { c := ok; c.Anchor = time.Time{}; return c }(), Now: at(2026, time.June, 2)}, "no anchor"},
		{"no now", Input{Commitment: ok}, "Now is required"},
		{"clock counted in miles", Input{
			Commitment: func() Commitment {
				c := ok
				c.Due = []DueRule{{Meter: MeterClock, Every: Interval{Amount: 5000}}}
				return c
			}(), Now: at(2026, time.June, 2)}, "measured in time"},
		{"counter counted in months", Input{
			Commitment: func() Commitment {
				c := ok
				c.Due = []DueRule{{Meter: "odo", Every: iv(t, "6mo")}}
				return c
			}(), Meters: meters, Now: at(2026, time.June, 2)}, "is a number"},
		{"meter named twice", Input{
			Commitment: func() Commitment {
				c := ok
				c.Due = []DueRule{{Meter: MeterClock, Every: iv(t, "3mo")}, {Meter: MeterClock, Every: iv(t, "1yr")}}
				return c
			}(), Now: at(2026, time.June, 2)}, "twice"},
		{"meter not supplied", Input{
			Commitment: func() Commitment {
				c := ok
				c.Due = []DueRule{{Meter: "odo", Every: Interval{Amount: 5000}}}
				return c
			}(), Now: at(2026, time.June, 2)}, "was not supplied"},
		{"zero interval", Input{
			Commitment: func() Commitment {
				c := ok
				c.Due = []DueRule{{Meter: MeterClock, Every: Interval{}}}
				return c
			}(), Now: at(2026, time.June, 2)}, "never comes due"},
		{"booking it never asked for", Input{
			Commitment: ok, Booking: &Booking{At: at(2026, time.July, 1)}, Now: at(2026, time.June, 2),
		}, "does not require one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.Loc = chicago
			_, err := Evaluate(tc.in)
			if err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
