// Package commitment is the due-ness function: given what the household
// has committed to, what its meters have read, and what has been done
// about it, it says whether the thing is due — and nothing else.
//
// It is work package K3 of ADR-W034 (draft), built first and deliberately
// alone, because it is the only part of that design that is hard to get
// right and the only part that does not move when the storage, the API
// or the resource schema around it change.
//
// What this package is:
//
//   - A pure function. Evaluate takes everything it needs as arguments
//     and returns a decision. No clock of its own, no database, no
//     network, no model. The same inputs always produce the same
//     answer, which is what makes a commitment replayable against
//     history before anybody approves it (ADR-W024 A7, ADR-W034 A5).
//   - Honest about what it does not know. A meter nobody has read
//     recently cannot support the conclusion "not due yet", and this
//     package says so rather than doing arithmetic on a stale number
//     (ADR-W034 A11).
//   - Silent about notification. It returns a state; who hears about it
//     is an Automation the household writes (ADR-W034 A12).
//
// What this package is not: storage (K4), a resource schema (K1/K2), a
// scheduler, or anything that can act. It imports nothing but the
// standard library, and should stay that way.
//
// The central idea is that a due date is a threshold on a number that
// counts up, and the calendar is one such number. So there is one code
// path, working in positions — miles on an odometer, seconds on the
// clock — and only advance() knows which is which.
package commitment

import (
	"fmt"
	"math"
	"slices"
	"time"
)

// MeterClock is the built-in meter every household has without
// installing, configuring or publishing anything (ADR-W034 A2). Its
// positions are seconds since the Unix epoch, and its readings are not
// recorded: the current position is whatever Now says.
const MeterClock = "clock"

// State is where one occurrence of a commitment stands.
//
// Evaluate returns only the four open states. Satisfied and Waived are
// how a cycle CLOSES — they are recorded by the ledger when a completion
// is written, and the next occurrence opens immediately (ADR-W034 A6),
// so the function that answers "where does this stand" never returns
// them.
type State string

const (
	StatePending   State = "pending"
	StateDueSoon   State = "due_soon"
	StateScheduled State = "scheduled"
	StateOverdue   State = "overdue"
	StateSatisfied State = "satisfied"
	StateWaived    State = "waived"
)

// Rollover decides where the next occurrence is measured from
// (ADR-W034 A9). Both are needed and neither is a sensible default, so
// there is no zero value: an empty Rollover is a validation error.
type Rollover string

const (
	// Once is a commitment that happens one time. A dentist appointment
	// on the third, a recall to get seen to, a brake inspection at
	// 90,000 miles. It has a due point, a lead, and optionally a
	// booking, exactly as a recurring one does; what it does not have is
	// a next occurrence, so satisfying it closes the commitment rather
	// than opening another cycle.
	//
	// Spelled out rather than expressed by leaving Rollover empty. A
	// missing field that silently means "once" is how a household writes
	// a recurring commitment, forgets the mode, and is told about the
	// furnace filter exactly one time — which is the failure A9 refuses
	// a default in order to prevent.
	Once Rollover = "once"
	// FromCompletion measures the next occurrence from where the meter
	// stood when the work was done. The oil change: the clock starts
	// when the oil is new, and drift is correct.
	FromCompletion Rollover = "fromCompletion"
	// FromSchedule measures from the original anchor, ignoring when the
	// work was actually done. The annual checkup: a visit in November
	// does not move next year's appointment to November.
	FromSchedule Rollover = "fromSchedule"
)

// Meter is a number that counts up, and what is known about it.
type Meter struct {
	Name string `json:"name"`
	Unit string `json:"unit,omitempty"`
	// StaleAfter is how old the newest reading may be before this meter
	// can no longer support the conclusion "not due yet". Zero means it
	// never goes stale, which is right for a meter something else keeps
	// current and wrong for one a person reads off a dashboard.
	StaleAfter time.Duration `json:"staleAfter,omitempty"`
	// Estimate is units per day, used ONLY to project a date for display.
	// A projection never moves a state (ADR-W034 A11). Zero means derive
	// a rate from the readings, or show nothing.
	Estimate float64 `json:"estimate,omitempty"`
	// Readings in any order; Evaluate sorts a copy.
	Readings []Reading `json:"readings,omitempty"`
}

// Reading is one observation of a meter, however it arrived: typed into
// the Console, sent from the CLI, said in household chat, or published
// by an installed capability. All four are equal here (ADR-W034 A3) —
// this package cannot tell them apart and has no reason to.
type Reading struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
	By    string    `json:"by,omitempty"`
	// Rebase marks the point where the meter's coordinate space restarts
	// but the obligation does not: a replaced odometer, a swapped-out
	// hour counter. The commitment's thresholds are translated into the
	// new coordinates so that what was 400 miles away stays 400 miles
	// away.
	//
	// A genuinely new obligation — a new furnace, so the filter clock
	// restarts — is NOT a rebase. That is a completion or a waive
	// followed by a new anchor, and conflating the two silently forgives
	// an overdue item.
	Rebase bool `json:"rebase,omitempty"`
	// Prior is what the meter read just before it was replaced, in the
	// OLD coordinate space. Only meaningful with Rebase. Zero means "use
	// the reading before this one", which is right when somebody wrote
	// the number down before the swap and unusable when nobody did — in
	// which case the rule reports itself unresolved rather than guessing.
	Prior  float64 `json:"prior,omitempty"`
	Reason string  `json:"reason,omitempty"`
}

// DueRule is one threshold: this meter, this far apart, warning this
// early. A commitment carries one or more, and the earliest to trip
// decides (ADR-W034 A10).
type DueRule struct {
	Meter string `json:"meter"`
	// Every is the gap between occurrences, measured from the cycle's
	// anchor. Required for a recurring commitment; for a one-off it is
	// one of the two ways to say when, and At is usually the other.
	Every Interval `json:"every,omitzero"`
	// At and AtValue are an ABSOLUTE due point, and only a one-off may
	// carry one: a fixed point cannot repeat. At is for the clock,
	// AtValue for a counter — two fields because a date and a mileage
	// are not the same kind of thing, though a schema above this can
	// present them as one and pick by the meter.
	//
	// This exists because "the dentist on the third" is the commonest
	// one-off there is, and expressing it as an interval from an anchor
	// would mean a person doing calendar arithmetic to say a date they
	// already know.
	At      time.Time `json:"at,omitzero"`
	AtValue float64   `json:"atValue,omitempty"`
	// Lead is how far ahead of the threshold the commitment starts
	// asking. Zero means no warning at all: pending until the moment it
	// is overdue.
	Lead Interval `json:"lead,omitzero"`
}

// Commitment is a thing the household owes itself.
type Commitment struct {
	Name string    `json:"name"`
	Due  []DueRule `json:"due"`
	// RequiresBooking splits the two failures apart. When true, knowing
	// the thing is due is not enough: due_soon persists, and keeps
	// asking, until an appointment actually exists (ADR-W034 A7).
	RequiresBooking bool     `json:"requiresBooking,omitempty"`
	Rollover        Rollover `json:"rollover"`
	// Anchor is when tracking starts — the first cycle is measured from
	// here.
	Anchor time.Time `json:"anchor"`
	// AnchorAt is where each counter meter stood at Anchor. Optional:
	// without it the earliest recorded reading is used, which is right
	// when tracking began with a reading and wrong when it began
	// mid-life, so record it when the household knows it.
	AnchorAt map[string]float64 `json:"anchorAt,omitempty"`
}

// Completion closes a cycle. Evidence is not optional: the positions the
// meters stood at are what the next cycle is measured from, so an oil
// change recorded without a mileage is a note rather than a maintenance
// record (ADR-W034 A8).
type Completion struct {
	At        time.Time          `json:"at"`
	By        string             `json:"by,omitempty"`
	Positions map[string]float64 `json:"positions,omitempty"`
	// Waived records a cycle deliberately skipped, with a reason. It
	// closes the cycle exactly as a completion does — the next one still
	// opens — and is kept distinct so that "we decided not to" never
	// reads as "it was done".
	Waived bool   `json:"waived,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Booking is an appointment for the open cycle: the fact that separates
// "I know the checkup is due" from "the checkup is going to happen".
type Booking struct {
	At       time.Time `json:"at"`
	BookedAt time.Time `json:"bookedAt,omitzero"`
	By       string    `json:"by,omitempty"`
}

// Input is everything the decision depends on. Passing the clock in
// rather than reading it is what makes this replayable.
type Input struct {
	Commitment Commitment       `json:"commitment"`
	Meters     map[string]Meter `json:"meters,omitempty"`
	// History is every closed cycle, in any order; Evaluate sorts a copy.
	History []Completion `json:"history,omitempty"`
	// Booking for the OPEN cycle, if one has been made.
	Booking *Booking  `json:"booking,omitempty"`
	Now     time.Time `json:"now"`
	// Loc is the household's clock (ADR-W029: a household has one
	// timezone). Calendar steps land on the same local day of the month,
	// across daylight saving. nil means UTC.
	Loc *time.Location `json:"-"`
}

// RuleStatus is one threshold's arithmetic, kept in the result because a
// person asking "why does it think that" deserves the numbers rather
// than a verdict.
type RuleStatus struct {
	Meter    string `json:"meter"`
	Calendar bool   `json:"calendar"`
	// Anchor, DueAt and LeadAt are positions in this meter's coordinate
	// space, after any rebase translation.
	Anchor float64 `json:"anchor"`
	DueAt  float64 `json:"dueAt"`
	LeadAt float64 `json:"leadAt"`
	// Position is where the meter stands now: the newest reading, or the
	// clock. Meaningful only when Known.
	Position float64 `json:"position"`
	Known    bool    `json:"known"`
	// Stale means the newest reading is too old to support "not due
	// yet". It does NOT invalidate a threshold already crossed: a
	// reading of 84,900 against a threshold of 84,500 is past due
	// whether it was taken this morning or last month.
	Stale      bool      `json:"stale"`
	Remaining  float64   `json:"remaining"`
	Progress   float64   `json:"progress"`
	Crossed    bool      `json:"crossed"`
	Soon       bool      `json:"soon"`
	ObservedAt time.Time `json:"observedAt,omitzero"`
	// AnchorTime, DueTime and LeadTime are the calendar rendering of a
	// clock rule's positions. Zero for a counter rule — that is what a
	// projection is for.
	AnchorTime time.Time `json:"anchorTime,omitzero"`
	DueTime    time.Time `json:"dueTime,omitzero"`
	LeadTime   time.Time `json:"leadTime,omitzero"`
	// Unresolved says, in a sentence, why this rule could not be
	// concluded from. Empty when it could.
	Unresolved string `json:"unresolved,omitempty"`
}

// Projection is a guess about when a counter rule will come due, for
// display only. It exists so a household can plan; it is labelled
// everywhere it appears, and nothing in this package ever reads it back
// (ADR-W034 A11).
type Projection struct {
	Meter      string    `json:"meter"`
	At         time.Time `json:"at"`
	RatePerDay float64   `json:"ratePerDay"`
	// Source is "estimate" (the meter's declared rate) or "readings"
	// (derived from how fast it has actually moved).
	Source string `json:"source"`
}

// Occurrence is one cycle of a commitment: the answer.
type Occurrence struct {
	Cycle    int       `json:"cycle"`
	State    State     `json:"state"`
	OpenedAt time.Time `json:"openedAt"`
	// Trigger names the meter whose rule decided the state.
	Trigger string       `json:"trigger,omitempty"`
	Rules   []RuleStatus `json:"rules"`
	// PastDue marks a scheduled occurrence whose threshold has already
	// been crossed: the appointment exists, and it is late.
	PastDue bool `json:"pastDue,omitempty"`
	// Unresolved lists the meters that could not be concluded from —
	// never read, or read too long ago. This is what "go and look at the
	// odometer" is made of.
	Unresolved []string    `json:"unresolved,omitempty"`
	Projection *Projection `json:"projection,omitempty"`
	// Reason is one sentence for the timeline record and the
	// notification, in plain words.
	Reason string `json:"reason"`
}

// maxScheduleSteps bounds the walk that finds the current slot of a
// fromSchedule commitment. A daily commitment anchored twenty years ago
// is about 7,300 steps; past this the input is wrong in a way that
// should be reported rather than looped over.
const maxScheduleSteps = 100_000

// Evaluate answers where a commitment stands, as a pure function of its
// definition, its meters, what has been done, and the moment being asked
// about.
func Evaluate(in Input) (Occurrence, error) {
	loc := in.Loc
	if loc == nil {
		loc = time.UTC
	}
	c := in.Commitment
	if err := validate(in); err != nil {
		return Occurrence{}, err
	}

	history := slices.Clone(in.History)
	slices.SortStableFunc(history, func(a, b Completion) int { return a.At.Compare(b.At) })
	closed := len(history)

	// A one-off that has been answered is finished, and the honest answer
	// to "where does this stand" is the closing state itself.
	//
	// A6 says Evaluate never returns satisfied or waived, and for a
	// recurring commitment that holds: the moment one cycle closes the
	// next has already opened, so the closing state is never where things
	// stand. A one-off has no next cycle, so the exception is exactly as
	// wide as the case that needs it.
	if c.Rollover == Once && closed > 0 {
		// The FIRST completion is the one that closed it. A second is a
		// ledger that allowed an answered one-off to be answered again,
		// which K4 prevents; this function reports the close that
		// happened rather than editorialising about the rest.
		return closedOnce(c, history[0]), nil
	}

	occ := Occurrence{Cycle: closed + 1, OpenedAt: c.Anchor}
	var last *Completion
	if closed > 0 {
		last = &history[closed-1]
		occ.OpenedAt = last.At
	}

	for _, rule := range c.Due {
		st, err := evalRule(in, rule, last, closed, loc)
		if err != nil {
			return Occurrence{}, err
		}
		occ.Rules = append(occ.Rules, st)
		if st.Unresolved != "" {
			occ.Unresolved = append(occ.Unresolved, st.Meter)
		}
	}

	decide(&occ)
	applyBooking(&occ, c, in.Booking, in.Now)
	occ.Projection = project(in, occ)
	occ.Reason = explain(occ, c, loc)
	return occ, nil
}

// closedOnce is a finished one-off: no rules to evaluate, nothing owed,
// and a sentence saying how it ended.
func closedOnce(c Commitment, by Completion) Occurrence {
	occ := Occurrence{Cycle: 1, OpenedAt: c.Anchor, Rules: []RuleStatus{}}
	when := by.At.Format("2 Jan 2006")
	if by.Waived {
		occ.State = StateWaived
		occ.Reason = c.Name + " was not done, by a decision on " + when
		if by.Reason != "" {
			occ.Reason += ": " + by.Reason
		}
		return occ
	}
	occ.State = StateSatisfied
	occ.Reason = c.Name + " was done on " + when
	return occ
}

func validate(in Input) error {
	c := in.Commitment
	if c.Name == "" {
		return fmt.Errorf("commitment: a commitment needs a name")
	}
	if len(c.Due) == 0 {
		return fmt.Errorf("commitment: %q has no due rule: nothing can come due", c.Name)
	}
	switch c.Rollover {
	case Once, FromCompletion, FromSchedule:
	default:
		return fmt.Errorf("commitment: %q must say rollover: once, fromCompletion or fromSchedule — there is no safe default (ADR-W034 A9)", c.Name)
	}
	if c.Anchor.IsZero() {
		return fmt.Errorf("commitment: %q has no anchor: the first cycle has to start somewhere", c.Name)
	}
	if in.Now.IsZero() {
		return fmt.Errorf("commitment: Now is required; this function does not read a clock")
	}
	seen := map[string]bool{}
	for i, rule := range c.Due {
		if rule.Meter == "" {
			return fmt.Errorf("commitment: %q due[%d] names no meter", c.Name, i)
		}
		if seen[rule.Meter] {
			return fmt.Errorf("commitment: %q names meter %q twice; one threshold per meter", c.Name, rule.Meter)
		}
		seen[rule.Meter] = true
		if err := rule.Every.Validate(); err != nil {
			return fmt.Errorf("commitment: %q due[%d].every: %w", c.Name, i, err)
		}
		if err := rule.Lead.Validate(); err != nil {
			return fmt.Errorf("commitment: %q due[%d].lead: %w", c.Name, i, err)
		}
		absolute := !rule.At.IsZero() || rule.AtValue != 0
		if absolute && c.Rollover != Once {
			return fmt.Errorf("commitment: %q due[%d]: an absolute due point belongs to a one-off — a fixed point cannot repeat, so use every with rollover %s", c.Name, i, c.Rollover)
		}
		if absolute && !rule.Every.Zero() {
			return fmt.Errorf("commitment: %q due[%d]: says both when it is due and how far apart it recurs; pick one", c.Name, i)
		}
		if !absolute && rule.Every.Zero() {
			return fmt.Errorf("commitment: %q due[%d] says neither every nor an absolute due point: nothing can come due", c.Name, i)
		}
		if !rule.At.IsZero() && rule.Meter != MeterClock {
			return fmt.Errorf("commitment: %q due[%d]: meter %q counts, so its absolute due point is a number (atValue), not a date", c.Name, i, rule.Meter)
		}
		if rule.AtValue != 0 && rule.Meter == MeterClock {
			return fmt.Errorf("commitment: %q due[%d]: the clock's absolute due point is a date (at), not a number", c.Name, i)
		}
		if !rule.At.IsZero() && !rule.At.After(c.Anchor) {
			return fmt.Errorf("commitment: %q due[%d]: due %s is not after the anchor %s: it was already past before tracking began",
				c.Name, i, rule.At.Format("2 Jan 2006"), c.Anchor.Format("2 Jan 2006"))
		}
		if rule.Meter == MeterClock {
			if !rule.Every.Zero() && !rule.Every.Calendar() {
				return fmt.Errorf("commitment: %q due[%d]: the clock is measured in time (90d, 6mo, 1yr), not in %s", c.Name, i, rule.Every)
			}
			if !rule.Lead.Zero() && !rule.Lead.Calendar() {
				return fmt.Errorf("commitment: %q due[%d].lead: the clock is measured in time, not in %s", c.Name, i, rule.Lead)
			}
			continue
		}
		if rule.Every.Calendar() {
			return fmt.Errorf("commitment: %q due[%d]: meter %q counts, so its interval is a number, not %s", c.Name, i, rule.Meter, rule.Every)
		}
		if !rule.Lead.Zero() && rule.Lead.Calendar() {
			return fmt.Errorf("commitment: %q due[%d].lead: meter %q counts, so its lead is a number, not %s", c.Name, i, rule.Meter, rule.Lead)
		}
		if _, ok := in.Meters[rule.Meter]; !ok {
			return fmt.Errorf("commitment: %q names meter %q, which was not supplied", c.Name, rule.Meter)
		}
	}
	if in.Booking != nil && !c.RequiresBooking {
		return fmt.Errorf("commitment: %q has a booking but does not require one; either it books or it does not", c.Name)
	}
	if in.Booking != nil && in.Booking.At.IsZero() {
		return fmt.Errorf("commitment: %q has a booking with no date", c.Name)
	}
	return nil
}

// evalRule does one threshold's arithmetic.
func evalRule(in Input, rule DueRule, last *Completion, closed int, loc *time.Location) (RuleStatus, error) {
	c := in.Commitment
	st := RuleStatus{Meter: rule.Meter, Calendar: rule.Meter == MeterClock}

	base, ok := baseAnchor(in, rule)
	if !ok {
		st.Unresolved = fmt.Sprintf("meter %q has never been read, and the commitment does not say where it started", rule.Meter)
		return st, nil
	}

	// Where does the open cycle start, and where does it come due?
	//
	// An absolute due point is the simple case and comes first: the
	// household said when, so there is no interval to walk and no
	// rollover to apply.
	if !rule.At.IsZero() || rule.AtValue != 0 {
		anchor := base
		due := rule.AtValue
		if st.Calendar {
			due = clockPos(rule.At)
		}
		return finishRule(in, rule, st, anchor, due, c.Anchor, loc)
	}

	anchor, due := base, advance(base, rule.Every, st.Calendar, loc)
	switch c.Rollover {
	case FromCompletion:
		if last != nil {
			p, ok := completionPosition(*last, rule.Meter)
			if !ok {
				st.Unresolved = fmt.Sprintf(
					"the last completion recorded no reading for meter %q, so the next one cannot be measured from it (ADR-W034 A8)", rule.Meter)
				return st, nil
			}
			anchor = p
			due = advance(anchor, rule.Every, st.Calendar, loc)
		}
	case FromSchedule:
		// The schedule is fixed to the original anchor, so every slot is
		// measured from base and the open cycle is found by counting
		// slots rather than by walking positions.
		n := closed
		if last != nil {
			p, ok := completionPosition(*last, rule.Meter)
			if !ok {
				// The same missing evidence fromCompletion refuses, and
				// for the same reason: without the reading at completion
				// there is no way to know which slot the work landed in,
				// and assuming exactly one passed can open a cycle that
				// is already behind the meter (ADR-W034 A8).
				st.Unresolved = fmt.Sprintf(
					"the last completion recorded no reading for meter %q, so which scheduled slot it met cannot be known (ADR-W034 A8)", rule.Meter)
				return st, nil
			}
			// p was recorded in the coordinate space of its own day. The
			// schedule lives in the original one, so compare like with
			// like before deciding which slots are behind us.
			if !st.Calendar {
				d, ok := rebaseDelta(in.Meters[rule.Meter], c.Anchor, last.At)
				if !ok {
					st.Unresolved = fmt.Sprintf(
						"meter %q was replaced with no reading recorded beforehand, so the completion cannot be placed on the schedule", rule.Meter)
					return st, nil
				}
				p -= d
			}
			// A cycle closed LATE can leave the next slot already behind
			// it — a checkup anchored to September and finally done the
			// following November. Skip to the first slot after it, which
			// is what "stays in September" has to mean in a year it
			// slipped.
			for scheduleSlot(base, rule.Every, n+1, st.Calendar, loc) <= p {
				n++
				if n > maxScheduleSteps {
					return st, fmt.Errorf("commitment: %q meter %q: schedule did not reach %v after %d steps", c.Name, rule.Meter, p, maxScheduleSteps)
				}
			}
		}
		anchor = scheduleSlot(base, rule.Every, n, st.Calendar, loc)
		due = scheduleSlot(base, rule.Every, n+1, st.Calendar, loc)
	}

	// Where the rebase window opens follows where the cycle was
	// anchored: at the last completion for fromCompletion, and at the
	// commitment's own anchor for fromSchedule, whose slots were walked
	// from there.
	anchorTime := c.Anchor
	if last != nil && c.Rollover == FromCompletion {
		anchorTime = last.At
	}
	return finishRule(in, rule, st, anchor, due, anchorTime, loc)
}

// finishRule turns a rule's anchor and due point into its status: the
// rebase translation, where the meter stands now, and what that means.
//
// Shared by both paths into it — an interval walked from a cycle's
// anchor, and a one-off's absolute due point — because a threshold
// should be crossed on exactly the same terms however it was arrived at.
func finishRule(in Input, rule DueRule, st RuleStatus, anchor, due float64, anchorTime time.Time, loc *time.Location) (RuleStatus, error) {
	// A replaced meter moves the coordinate space under the thresholds.
	if !st.Calendar {
		delta, ok := rebaseDelta(in.Meters[rule.Meter], anchorTime, in.Now)
		if !ok {
			st.Unresolved = fmt.Sprintf(
				"meter %q was replaced with no reading recorded beforehand, so the old thresholds cannot be carried across", rule.Meter)
			return st, nil
		}
		anchor += delta
		due += delta
	}

	lead := retreat(due, rule.Lead, st.Calendar, loc)
	st.Anchor, st.DueAt, st.LeadAt = anchor, due, lead
	if st.Calendar {
		st.AnchorTime, st.DueTime, st.LeadTime = clockTime(anchor, loc), clockTime(due, loc), clockTime(lead, loc)
	}

	// Where does the meter stand?
	if st.Calendar {
		st.Position, st.Known, st.ObservedAt = clockPos(in.Now), true, in.Now
	} else {
		m := in.Meters[rule.Meter]
		if m.Name == "" {
			m.Name = rule.Meter
		}
		r, problem := counterPosition(m, in.Now)
		if problem != "" {
			st.Unresolved = problem
			return st, nil
		}
		st.Position, st.Known, st.ObservedAt = r.Value, true, r.At
		if m.StaleAfter > 0 && in.Now.Sub(r.At) > m.StaleAfter {
			st.Stale = true
		}
	}

	st.Remaining = st.DueAt - st.Position
	st.Crossed = st.Position >= st.DueAt
	st.Soon = !st.Crossed && st.Position >= st.LeadAt
	if span := st.DueAt - st.Anchor; span > 0 {
		st.Progress = (st.Position - st.Anchor) / span
	}
	// Staleness only undermines the NEGATIVE conclusion. A threshold the
	// last real reading already passed is passed, however old that
	// reading is.
	if st.Stale && !st.Crossed {
		st.Unresolved = fmt.Sprintf(
			"meter %q was last read %s ago, too long to conclude it is not due: it needs a fresh reading",
			rule.Meter, humanAge(in.Now.Sub(st.ObservedAt)))
	}
	return st, nil
}

// scheduleSlot is the nth due point of a fixed schedule, measured from
// base every time rather than by stepping from the slot before it.
//
// That distinction is the whole function. Stepping compounds the
// month-end clamp: a schedule anchored on 31 January is due 28 February,
// and a step from THAT lands on 28 March, so one short month moves every
// occurrence for the rest of the commitment's life. A fixed schedule
// promises the original anchor's day, so each slot is computed from the
// anchor and only the month it lands in may shorten it.
func scheduleSlot(base float64, iv Interval, n int, calendar bool, loc *time.Location) float64 {
	if n <= 0 {
		return base
	}
	if !calendar {
		return base + float64(n)*iv.Amount
	}
	if iv.Months != 0 {
		return clockPos(addMonths(clockTime(base, loc), n*iv.Months, loc))
	}
	return base + float64(n)*iv.Span.Seconds()
}

// humanAge renders how long ago something was, for a sentence a
// household reads. Go's own duration format is honest and unreadable
// past a day or two: "2208h0m0s" is ninety-two days, and nobody should
// have to work that out from a notification.
func humanAge(d time.Duration) string {
	switch days := int(d.Hours() / 24); {
	case days >= 14:
		return fmt.Sprintf("%d weeks", days/7)
	case days >= 2:
		return fmt.Sprintf("%d days", days)
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return "less than two hours"
	}
}

// baseAnchor is where a rule's very first cycle is measured from.
func baseAnchor(in Input, rule DueRule) (float64, bool) {
	if rule.Meter == MeterClock {
		return clockPos(in.Commitment.Anchor), true
	}
	if v, ok := in.Commitment.AnchorAt[rule.Meter]; ok {
		return v, true
	}
	m := in.Meters[rule.Meter]
	if r, ok := oldestReading(m, in.Now); ok {
		return r.Value, true
	}
	return 0, false
}

func completionPosition(c Completion, meter string) (float64, bool) {
	if meter == MeterClock {
		return clockPos(c.At), true
	}
	v, ok := c.Positions[meter]
	return v, ok
}

// rebaseDelta is how far a meter's coordinate space shifted over the
// window (after, upto], summed across every replacement in it.
//
// A window rather than a single "since" because the two rollover modes
// measure from different places. A fromCompletion cycle is anchored at
// the position the last completion recorded, so only replacements after
// that completion have moved anything. A fromSchedule cycle is walked
// from the commitment's ORIGINAL anchor, so it carries every replacement
// since then — including ones that happened before the last completion,
// which a "since the completion" window would silently drop.
func rebaseDelta(m Meter, after, upto time.Time) (float64, bool) {
	rs := sortedReadings(m, upto)
	delta, ok := 0.0, true
	for i, r := range rs {
		if !r.Rebase || !r.At.After(after) {
			continue
		}
		prior, have := r.Prior, r.Prior != 0
		if !have && i > 0 {
			prior, have = rs[i-1].Value, true
		}
		if !have {
			return 0, false
		}
		delta += r.Value - prior
	}
	return delta, ok
}

// sortedReadings returns a meter's readings in order, up to and
// including upto, and never past it.
//
// The bound is the point of the function. Evaluate is handed the moment
// it is asked about precisely so that a commitment can be replayed
// against history, and a replay that can see a reading taken after that
// moment is not a replay — it is a peek. The same bound protects the
// live path from a publisher whose clock runs fast.
func sortedReadings(m Meter, upto time.Time) []Reading {
	rs := make([]Reading, 0, len(m.Readings))
	for _, r := range m.Readings {
		if !r.At.After(upto) {
			rs = append(rs, r)
		}
	}
	slices.SortStableFunc(rs, func(a, b Reading) int { return a.At.Compare(b.At) })
	return rs
}

// counterPosition is where a counter stands as of now, or the reason it
// cannot be said.
//
// A counter that goes DOWN without a declared rebase is a fault — a
// mistyped digit, or a meter replaced without anybody saying so — and
// taking the lower number as a position is how an overdue commitment
// walks quietly back to pending. The meter only counts up; when the
// record says otherwise, the record is wrong and this says so.
func counterPosition(m Meter, now time.Time) (Reading, string) {
	rs := sortedReadings(m, now)
	if len(rs) == 0 {
		return Reading{}, fmt.Sprintf("meter %q has never been read", m.Name)
	}
	for i := 1; i < len(rs); i++ {
		if rs[i].Rebase || rs[i].Value >= rs[i-1].Value {
			continue
		}
		return Reading{}, fmt.Sprintf(
			"meter %q went backwards, from %g to %g on %s, with no replacement recorded: a counter only counts up, so this is a mistyped reading or an undeclared new meter",
			m.Name, rs[i-1].Value, rs[i].Value, rs[i].At.Format("2 Jan 2006"))
	}
	return rs[len(rs)-1], ""
}

func oldestReading(m Meter, upto time.Time) (Reading, bool) {
	rs := sortedReadings(m, upto)
	if len(rs) == 0 {
		return Reading{}, false
	}
	return rs[0], true
}

// decide folds the rules into one state. Earliest wins: whichever
// threshold is furthest along its own interval is the one that speaks,
// which is how "every 6 months or 5,000 miles" compares a month against
// a mile (ADR-W034 A10).
//
// A rule that could not be concluded from never decides anything. It
// leaves its meter in Unresolved, where it becomes a request for a
// reading rather than a verdict (ADR-W034 A11).
func decide(occ *Occurrence) {
	furthest := func(where func(RuleStatus) bool) (RuleStatus, bool) {
		var best RuleStatus
		found := false
		for _, st := range occ.Rules {
			if !st.Known || st.Unresolved != "" || !where(st) {
				continue
			}
			if !found || st.Progress > best.Progress {
				best, found = st, true
			}
		}
		return best, found
	}

	if st, ok := furthest(func(s RuleStatus) bool { return s.Crossed }); ok {
		occ.State, occ.Trigger = StateOverdue, st.Meter
		return
	}
	if st, ok := furthest(func(s RuleStatus) bool { return s.Soon }); ok {
		occ.State, occ.Trigger = StateDueSoon, st.Meter
		return
	}
	occ.State = StatePending
	if st, ok := furthest(func(RuleStatus) bool { return true }); ok {
		occ.Trigger = st.Meter
	}
}

// applyBooking overlays an appointment on a computed state.
func applyBooking(occ *Occurrence, c Commitment, b *Booking, now time.Time) {
	if !c.RequiresBooking {
		return
	}
	if b == nil {
		// Nothing was booked. due_soon therefore stays due_soon and goes
		// on asking: acknowledging a nudge is not an appointment, and
		// the gap between those two is how a checkup gets missed.
		occ.PastDue = false
		return
	}
	if b.At.After(now) {
		occ.PastDue = occ.State == StateOverdue
		occ.State = StateScheduled
		return
	}
	// The appointment came and went with nothing recorded. That is not
	// "scheduled" and it is not "pending": it is the thing that was
	// supposed to close this cycle, missed.
	occ.State = StateOverdue
	occ.PastDue = true
}

// project guesses a date for the nearest counter threshold. Display
// only, and labelled as such wherever it surfaces.
func project(in Input, occ Occurrence) *Projection {
	var best *RuleStatus
	for i := range occ.Rules {
		st := &occ.Rules[i]
		if st.Calendar || !st.Known || st.Crossed {
			continue
		}
		if best == nil || st.Progress > best.Progress {
			best = st
		}
	}
	if best == nil {
		return nil
	}
	m := in.Meters[best.Meter]
	rate, source := m.Estimate, "estimate"
	if rate <= 0 {
		r, ok := observedRate(m, in.Now)
		if !ok {
			return nil
		}
		rate, source = r, "readings"
	}
	days := best.Remaining / rate
	if math.IsInf(days, 0) || math.IsNaN(days) || days < 0 {
		return nil
	}
	return &Projection{
		Meter:      best.Meter,
		At:         in.Now.Add(time.Duration(days * float64(24*time.Hour))),
		RatePerDay: rate,
		Source:     source,
	}
}

// observedRate is how fast a meter has actually moved, since the last
// time its coordinate space restarted.
func observedRate(m Meter, now time.Time) (float64, bool) {
	rs := sortedReadings(m, now)
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Rebase {
			rs = rs[i:]
			break
		}
	}
	if len(rs) < 2 {
		return 0, false
	}
	first, lastR := rs[0], rs[len(rs)-1]
	days := lastR.At.Sub(first.At).Hours() / 24
	if days <= 0 {
		return 0, false
	}
	rate := (lastR.Value - first.Value) / days
	if rate <= 0 {
		return 0, false
	}
	return rate, true
}

func explain(occ Occurrence, c Commitment, loc *time.Location) string {
	var trig *RuleStatus
	for i := range occ.Rules {
		if occ.Rules[i].Meter == occ.Trigger {
			trig = &occ.Rules[i]
		}
	}
	where := func() string {
		if trig == nil {
			return ""
		}
		if trig.Calendar {
			return " (due " + clockTime(trig.DueAt, loc).Format("2 Jan 2006") + ")"
		}
		return fmt.Sprintf(" (%s at %g, due at %g)", trig.Meter, trig.Position, trig.DueAt)
	}
	switch occ.State {
	case StateOverdue:
		if occ.PastDue && len(occ.Unresolved) == 0 && occ.Trigger == "" {
			return c.Name + ": the appointment passed with nothing recorded"
		}
		return c.Name + " is overdue" + where()
	case StateScheduled:
		if occ.PastDue {
			return c.Name + " is booked, and already past due" + where()
		}
		return c.Name + " is booked"
	case StateDueSoon:
		if c.RequiresBooking {
			return c.Name + " is coming due and has no appointment" + where()
		}
		return c.Name + " is coming due" + where()
	default:
		if len(occ.Unresolved) > 0 && occ.Trigger == "" {
			// Say which of the several reasons it is. "Stale", "never
			// read" and "counts downwards" are three different requests
			// of the household, and one generic sentence for all three
			// sends somebody to look at a dashboard when the actual
			// problem is a digit typed wrongly last Tuesday.
			for _, st := range occ.Rules {
				if st.Unresolved != "" {
					return c.Name + ": nothing to say yet — " + st.Unresolved
				}
			}
		}
		return c.Name + " is not due yet" + where()
	}
}
