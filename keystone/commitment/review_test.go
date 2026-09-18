package commitment

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// Five defects found by review on PR #242, each pinned here before it was
// fixed. They share a shape worth naming: every one is a case where the
// function quietly produced a number instead of saying it could not.

// 1. A fromSchedule counter's thresholds are walked from the ORIGINAL
// anchor, so translating them into today's coordinates needs every
// rebase since that anchor — not only those since the last completion.
func TestFromScheduleCarriesRebasesSinceTheOriginalAnchor(t *testing.T) {
	c := Commitment{
		Name: "f150-tyres", Anchor: at(2026, time.January, 1),
		AnchorAt: map[string]float64{"odo": 0}, Rollover: FromSchedule,
		Due: []DueRule{{Meter: "odo", Every: iv(t, "5000")}},
	}
	occ := eval(t, Input{
		Commitment: c,
		Meters: map[string]Meter{"odo": {Name: "odo", Readings: []Reading{
			{At: at(2026, time.March, 1), Value: 4000},
			// The cluster is replaced and restarts at zero: 4,000 travelled
			// so far, in a coordinate space shifted by -4,000.
			{At: at(2026, time.March, 2), Value: 0, Rebase: true, Prior: 4000},
			{At: at(2026, time.June, 1), Value: 1200},
		}}},
		// Done at 1,000 on the new cluster — 5,000 travelled in all.
		History: []Completion{{At: at(2026, time.May, 1), Positions: map[string]float64{"odo": 1000}}},
		Now:     at(2026, time.June, 1),
	})
	st := ruleFor(occ, "odo")
	// Slot two of the schedule is 10,000 travelled, which on a cluster
	// reading 4,000 less than the road is 6,000.
	if st.DueAt != 6000 {
		t.Errorf("dueAt = %g, want 6000: the rebase predates the completion but still moved the coordinates", st.DueAt)
	}
	if st.Anchor != 1000 {
		t.Errorf("anchor = %g, want 1000", st.Anchor)
	}
}

// 2. A counter that goes backwards without a declared rebase is a fault —
// a typo, or a meter swapped without saying so. Accepting it lets an
// overdue commitment quietly return to pending.
func TestAnUndeclaredCounterRegressionIsAFaultNotAPosition(t *testing.T) {
	occ := eval(t, Input{
		Commitment: staleTruck(t), // due at 84300, lead 300
		Meters: map[string]Meter{"odo": {Name: "odo", Readings: []Reading{
			{At: at(2026, time.June, 1), Value: 84400}, // overdue
			{At: at(2026, time.June, 2), Value: 83000}, // fat-fingered
		}}},
		Now: at(2026, time.June, 3),
	})
	// The occurrence lands on pending, as it does for a meter never read
	// or read too long ago — that is A11's shape and this is one more
	// case of it. What must NOT happen is that it reads as all-clear, so
	// what is pinned is the difference between the two: nothing decided
	// it, the meter is listed as wanting attention, and the sentence
	// names the fault rather than reporting a clean bill.
	st := ruleFor(occ, "odo")
	if st.Unresolved == "" {
		t.Fatalf("rule reported no problem; a counter that counts down is a fault, not a value")
	}
	if !strings.Contains(st.Unresolved, "backwards") {
		t.Errorf("explanation = %q, want it to name the regression", st.Unresolved)
	}
	if st.Known {
		t.Errorf("the rule claims a known position from a reading it cannot trust")
	}
	if occ.Trigger != "" {
		t.Errorf("trigger = %q, want empty: no rule was fit to decide", occ.Trigger)
	}
	if !slices.Contains(occ.Unresolved, "odo") {
		t.Errorf("unresolved = %v, want it to name odo", occ.Unresolved)
	}
	if strings.Contains(occ.Reason, "not due yet") {
		t.Errorf("reason = %q reads as an all-clear", occ.Reason)
	}
	if !strings.Contains(occ.Reason, "backwards") {
		t.Errorf("reason = %q, want the household told what is actually wrong", occ.Reason)
	}
}

// 3. fromCompletion already refuses to guess when a completion recorded no
// reading. fromSchedule silently assumed exactly one slot had passed,
// which is the same missing evidence answered two different ways.
func TestFromScheduleAlsoNeedsTheCompletionsReading(t *testing.T) {
	c := Commitment{
		Name: "f150-tyres", Anchor: at(2026, time.January, 1),
		AnchorAt: map[string]float64{"odo": 0}, Rollover: FromSchedule,
		Due: []DueRule{{Meter: "odo", Every: iv(t, "5000")}},
	}
	occ := eval(t, Input{
		Commitment: c,
		Meters:     map[string]Meter{"odo": {Name: "odo", Readings: readings(at(2026, time.June, 1), 23000.0)}},
		History:    []Completion{{At: at(2026, time.May, 1)}}, // no Positions
		Now:        at(2026, time.June, 1),
	})
	st := ruleFor(occ, "odo")
	if st.Unresolved == "" {
		t.Errorf("no reading was recorded at completion, so no future slot can be placed; got a verdict instead")
	}
	if occ.State == StateOverdue {
		t.Errorf("state = overdue, reached by assuming one slot had passed")
	}
}

// 4. Evaluate is handed the moment it is asked about so that a commitment
// can be replayed against history. A reading taken after that moment is a
// fact the replay is not entitled to see.
func TestAReadingFromAfterNowIsNotVisible(t *testing.T) {
	c := Commitment{
		Name: "x", Anchor: at(2026, time.January, 1),
		AnchorAt: map[string]float64{"odo": 0}, Rollover: FromCompletion,
		Due: []DueRule{{Meter: "odo", Every: iv(t, "100")}},
	}
	occ := eval(t, Input{
		Commitment: c,
		Meters: map[string]Meter{"odo": {Name: "odo", Readings: []Reading{
			{At: at(2026, time.January, 2), Value: 10},
			{At: at(2026, time.January, 20), Value: 110}, // after Now
		}}},
		Now: at(2026, time.January, 10),
	})
	st := ruleFor(occ, "odo")
	if st.Position != 10 {
		t.Errorf("position = %g, want 10: the January 20 reading had not happened yet", st.Position)
	}
	if occ.State != StatePending {
		t.Errorf("state = %q, want pending: a replay saw the future", occ.State)
	}
}

func TestARebaseFromAfterNowIsNotVisible(t *testing.T) {
	occ := eval(t, Input{
		Commitment: staleTruck(t), // anchor 79300, due 84300
		Meters: map[string]Meter{"odo": {Name: "odo", Readings: []Reading{
			{At: at(2026, time.June, 1), Value: 84000},
			{At: at(2026, time.July, 1), Value: 0, Rebase: true, Prior: 84500},
		}}},
		Now: at(2026, time.June, 10),
	})
	if got := ruleFor(occ, "odo").DueAt; got != 84300 {
		t.Errorf("dueAt = %g, want 84300: a rebase a month in the future moved today's threshold", got)
	}
}

// 5. fromSchedule promises a schedule fixed to the original anchor.
// clamping compounds if each step is taken from the previous clamped
// date, so 31 January drifts to the 28th forever after one February.
func TestAMonthlyFixedScheduleKeepsTheOriginalDay(t *testing.T) {
	c := Commitment{
		Name: "rent", Anchor: at(2026, time.January, 31),
		Rollover: FromSchedule,
		Due:      []DueRule{{Meter: MeterClock, Every: iv(t, "1mo")}},
	}
	// Cycle one: 28 February, clamped because February is short.
	occ := eval(t, Input{Commitment: c, Now: at(2026, time.February, 1)})
	if got := ruleFor(occ, MeterClock).DueTime; !got.Equal(at(2026, time.February, 28)) {
		t.Fatalf("cycle 1 due %s, want 28 Feb 2026", got.Format("2 Jan 2006"))
	}
	// Cycle two must return to the 31st, not stay on the 28th.
	occ = eval(t, Input{
		Commitment: c,
		History:    []Completion{{At: at(2026, time.February, 28)}},
		Now:        at(2026, time.March, 1),
	})
	if got := ruleFor(occ, MeterClock).DueTime; !got.Equal(at(2026, time.March, 31)) {
		t.Errorf("cycle 2 due %s, want 31 Mar 2026: the clamp compounded", got.Format("2 Jan 2006"))
	}
}

// 1b. The other half of the same defect. When a fromSchedule counter
// completion is late enough to have passed a slot, deciding WHICH slots
// are behind it compares the completion's recorded position against the
// schedule — and those two live in different coordinate spaces if the
// meter was replaced in between. Comparing them raw skips the wrong
// number of slots, which is a different wrong answer from the one above
// and needs its own case to provoke.
func TestALateCompletionIsPlacedOnTheScheduleInTheRightCoordinates(t *testing.T) {
	c := Commitment{
		Name: "f150-tyres", Anchor: at(2026, time.January, 1),
		AnchorAt: map[string]float64{"odo": 0}, Rollover: FromSchedule,
		Due: []DueRule{{Meter: "odo", Every: iv(t, "5000")}},
	}
	occ := eval(t, Input{
		Commitment: c,
		Meters: map[string]Meter{"odo": {Name: "odo", Readings: []Reading{
			{At: at(2026, time.March, 1), Value: 4000},
			{At: at(2026, time.March, 2), Value: 0, Rebase: true, Prior: 4000},
			{At: at(2026, time.June, 1), Value: 9000},
		}}},
		// 8,000 on the new cluster is 12,000 travelled: slots at 5,000 and
		// 10,000 are both behind it, so the open cycle runs 10,000→15,000.
		History: []Completion{{At: at(2026, time.May, 1), Positions: map[string]float64{"odo": 8000}}},
		Now:     at(2026, time.June, 1),
	})
	st := ruleFor(occ, "odo")
	if st.DueAt != 11000 {
		t.Errorf("dueAt = %g, want 11000 (slot 15000, on a cluster reading 4000 low)", st.DueAt)
	}
	if st.Anchor != 6000 {
		t.Errorf("anchor = %g, want 6000 (slot 10000, same shift)", st.Anchor)
	}
}

// ---------------------------------------------------------------------
// Second review round on PR #242. The first round bounded meter readings
// at Now; these are the places the same guarantee was still leaking.
// ---------------------------------------------------------------------

// 6. Completions are facts too, and a replay is not entitled to ones
// that have not happened yet.
func TestACompletionFromAfterNowIsNotVisible(t *testing.T) {
	c := Commitment{
		Name: "furnace-filter", Anchor: at(2026, time.June, 1),
		Rollover: FromCompletion,
		Due:      []DueRule{{Meter: MeterClock, Every: iv(t, "3mo"), Lead: iv(t, "14d")}},
	}
	occ := eval(t, Input{
		Commitment: c,
		History:    []Completion{{At: at(2026, time.September, 4), By: "mika"}},
		Now:        at(2026, time.September, 2), // before that completion
	})
	if occ.Cycle != 1 {
		t.Errorf("cycle = %d, want 1: the 4 September completion has not happened yet", occ.Cycle)
	}
	if occ.State != StateOverdue {
		t.Errorf("state = %q, want overdue: cycle one was due 1 September", occ.State)
	}
}

func TestAOneOffIsNotSatisfiedByAFutureCompletion(t *testing.T) {
	// The sharper version: a one-off would report itself DONE before the
	// work happened.
	occ := eval(t, Input{
		Commitment: dentist(t), // due 3 Oct
		History:    []Completion{{At: at(2026, time.October, 3), By: "mika"}},
		Now:        at(2026, time.September, 28),
	})
	if occ.State == StateSatisfied {
		t.Fatalf("a one-off reported itself satisfied five days before the appointment")
	}
	if occ.State != StateDueSoon {
		t.Errorf("state = %q, want due_soon", occ.State)
	}
}

func TestABookingMadeAfterNowIsNotVisible(t *testing.T) {
	// The third instance of the same class, which the review did not
	// name: a replay must not see an appointment that had not been made.
	occ := eval(t, Input{
		Commitment: checkup(t),
		Booking: &Booking{
			At:       at(2026, time.September, 15),
			BookedAt: at(2026, time.September, 12), // booked later than Now
		},
		Now: at(2026, time.August, 1),
	})
	if occ.State == StateScheduled {
		t.Errorf("state = scheduled from a booking that had not been made yet")
	}
	if occ.State != StateDueSoon {
		t.Errorf("state = %q, want due_soon", occ.State)
	}
}

// 7. The clock rule refuses a due point that was already past before
// tracking began. A counter's should too.
func TestACounterDuePointBeforeItsAnchorIsRefused(t *testing.T) {
	in := Input{
		Commitment: Commitment{
			Name: "f150-brakes", Anchor: at(2026, time.March, 1),
			AnchorAt: map[string]float64{"odo": 100}, Rollover: Once,
			Due: []DueRule{{Meter: "odo", AtValue: 50}},
		},
		Meters: map[string]Meter{"odo": {Name: "odo", Readings: readings(at(2026, time.June, 1), 120.0)}},
		Now:    at(2026, time.June, 2),
		Loc:    chicago,
	}
	_, err := Evaluate(in)
	if err == nil {
		t.Fatal("accepted a due point of 50 on a meter that already read 100 when tracking began")
	}
	if !strings.Contains(err.Error(), "already past before tracking began") {
		t.Errorf("error = %q, want the same wording the clock rule uses", err)
	}
}

// 8. A meter outlives the commitments that read it. The odometer feeds
// oil, tyres and brakes, each starting on its own day, so falling back
// to the meter's oldest-ever reading anchors a June commitment in
// January.
func TestTheAnchorFallbackUsesTheReadingAtTrackingStart(t *testing.T) {
	c := Commitment{
		Name: "f150-tyres", Anchor: at(2026, time.June, 1),
		Rollover: FromCompletion, // no AnchorAt: the fallback is what is under test
		Due:      []DueRule{{Meter: "odo", Every: iv(t, "5000")}},
	}
	occ := eval(t, Input{
		Commitment: c,
		Meters: map[string]Meter{"odo": {Name: "odo", Readings: []Reading{
			{At: at(2026, time.January, 1), Value: 1000}, // long before this commitment
			{At: at(2026, time.June, 1), Value: 2000},    // where tracking actually starts
			{At: at(2026, time.July, 1), Value: 2400},
		}}},
		Now: at(2026, time.July, 1),
	})
	if got := ruleFor(occ, "odo").DueAt; got != 7000 {
		t.Errorf("dueAt = %g, want 7000 (2000 at tracking start + 5000), not an anchor from January", got)
	}
}

func TestTheAnchorFallbackIgnoresACoordinateResetThatPredatesTracking(t *testing.T) {
	// Worse than merely early: the pre-anchor reading is in a coordinate
	// space the rebase window deliberately excludes, so the anchor and
	// the translation disagree.
	c := Commitment{
		Name: "f150-tyres", Anchor: at(2026, time.June, 1),
		Rollover: FromCompletion,
		Due:      []DueRule{{Meter: "odo", Every: iv(t, "5000")}},
	}
	occ := eval(t, Input{
		Commitment: c,
		Meters: map[string]Meter{"odo": {Name: "odo", Readings: []Reading{
			{At: at(2026, time.January, 1), Value: 90000},
			{At: at(2026, time.February, 1), Value: 0, Rebase: true, Prior: 90000},
			{At: at(2026, time.June, 1), Value: 3000}, // tracking starts here
			{At: at(2026, time.July, 1), Value: 3500},
		}}},
		Now: at(2026, time.July, 1),
	})
	if got := ruleFor(occ, "odo").DueAt; got != 8000 {
		t.Errorf("dueAt = %g, want 8000 (3000 + 5000) in the current coordinates", got)
	}
}
