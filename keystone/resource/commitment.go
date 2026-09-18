package resource

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CommitmentSpec is the spec for kind Commitment (ADR-W035 A1): a thing
// the household owes itself, due when a meter reaches a number.
//
// Keystone-owned like Meter and Automation: it lives in the Statefile, is
// applied at no node, and acts on nothing by itself. What it declares is
// WHEN something comes due and WHO the answer belongs to. Where each
// cycle stands — pending, due soon, scheduled, overdue — is runtime
// state, because it changes without anybody declaring anything.
type CommitmentSpec struct {
	// Description is the human sentence for the plan review, the Console
	// and the household's own front page.
	Description string `yaml:"description" json:"description"`
	// Disabled pauses a commitment without deleting it or losing its
	// history. The zero value is enabled.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	// Due is one threshold per meter. Several are allowed and the
	// earliest to trip decides — "every 6 months or 5,000 miles,
	// whichever comes first" (A10).
	Due []CommitmentDue `yaml:"due" json:"due"`
	// RequiresBooking splits knowing from happening. When true, being
	// due is not enough: the commitment goes on asking until an
	// appointment exists, which is the state a checkup needs and a
	// furnace filter does not (A7).
	RequiresBooking bool `yaml:"requiresBooking,omitempty" json:"requiresBooking,omitempty"`
	// Rollover is once, fromCompletion or fromSchedule (A9). Required:
	// none of the three is a safe default, and a household that wrote a
	// recurring commitment and got a one-off would be told about the
	// furnace filter exactly once.
	Rollover string `yaml:"rollover" json:"rollover"`
	// Anchor is when tracking starts, as a date (2026-09-10) or an
	// instant. A date is resolved against the household's own timezone
	// when the commitment is evaluated, not here: this package does not
	// know what time it is anywhere.
	Anchor string `yaml:"anchor" json:"anchor"`
	// AnchorAt is where each counter meter stood at Anchor. Optional —
	// without it the reading nearest the anchor is used — and worth
	// recording when the household knows the number, because "nearest"
	// is only as good as the record.
	AnchorAt map[string]float64 `yaml:"anchorAt,omitempty" json:"anchorAt,omitempty"`
	// SatisfiedBy names the household event that closes a cycle. Absent
	// means it is closed by somebody saying so, which is the ordinary
	// case: most things are done by a person who then records it.
	SatisfiedBy *CommitmentSatisfiedBy `yaml:"satisfiedBy,omitempty" json:"satisfiedBy,omitempty"`
	// Owner, MayComplete and Notify are three questions, and answering
	// them with one field is what makes a child the person nagged about
	// his own checkup (A21).
	//
	// metadata.owner says who it is ABOUT. MayComplete says who may
	// satisfy, book or waive it — what the household homepage renders as
	// `decidable`. Notify says who hears about a state change. Empty
	// MayComplete means the household's adults; empty Notify means
	// whoever may complete it.
	MayComplete []string `yaml:"mayComplete,omitempty" json:"mayComplete,omitempty"`
	Notify      []string `yaml:"notify,omitempty" json:"notify,omitempty"`
	// Scope is household or personal, mirroring the memory scopes rather
	// than inventing a second vocabulary for the same question. The
	// furnace filter is household; a medication refill is personal.
	// Empty means household.
	Scope string `yaml:"scope,omitempty" json:"scope,omitempty"`
}

// CommitmentDue is one threshold: this meter, this far apart, warning
// this early.
type CommitmentDue struct {
	// Meter is the name of a Meter resource, or "clock" for the calendar.
	Meter string `yaml:"meter" json:"meter"`
	// Every is the gap between occurrences: a number for a counter
	// ("5000"), a span or calendar step for the clock ("90d", "6mo",
	// "1yr").
	Every string `yaml:"every,omitempty" json:"every,omitempty"`
	// At and AtValue are an absolute due point, and only a one-off may
	// carry one — a fixed point cannot repeat. At is a date for the
	// clock; AtValue is a number for a counter.
	At      string  `yaml:"at,omitempty" json:"at,omitempty"`
	AtValue float64 `yaml:"atValue,omitempty" json:"atValue,omitempty"`
	// Lead is how far ahead of the threshold it starts asking. Empty
	// means no warning at all: pending until the moment it is overdue.
	Lead string `yaml:"lead,omitempty" json:"lead,omitempty"`
}

// CommitmentSatisfiedBy names the household event that closes a cycle.
type CommitmentSatisfiedBy struct {
	EventType string `yaml:"eventType" json:"eventType"`
	Subject   string `yaml:"subject,omitempty" json:"subject,omitempty"`
	Entity    string `yaml:"entity,omitempty" json:"entity,omitempty"`
}

// The rollover modes. Duplicated from keystone/commitment, which is a
// leaf on the standard library and cannot import this package; a test
// there pins the two sets equal, as it does for the clock meter's name.
const (
	RolloverOnce           = "once"
	RolloverFromCompletion = "fromCompletion"
	RolloverFromSchedule   = "fromSchedule"
)

// The scopes, mirroring ADR-W033 C1's memory scopes.
const (
	CommitmentScopeHousehold = "household"
	CommitmentScopePersonal  = "personal"
)

const (
	// MaxCommitmentDueRules bounds how many meters one commitment may
	// watch. Three is already unusual — miles, months, and one more.
	MaxCommitmentDueRules = 8
	// MaxCommitmentPeople bounds mayComplete and notify.
	MaxCommitmentPeople = 32
)

var commitmentRollovers = map[string]bool{
	RolloverOnce: true, RolloverFromCompletion: true, RolloverFromSchedule: true,
}

var commitmentScopes = map[string]bool{
	CommitmentScopeHousehold: true, CommitmentScopePersonal: true,
}

// CommitmentRollovers and CommitmentScopes are the closed sets, exported
// so anything OFFERING a choice — a Console form, an agent's authoring
// tool — offers what validation accepts. Offering a value that will be
// refused is offering a rejection.
func CommitmentRollovers() []string {
	return []string{RolloverOnce, RolloverFromCompletion, RolloverFromSchedule}
}
func CommitmentScopes() []string {
	return []string{CommitmentScopeHousehold, CommitmentScopePersonal}
}

// commitmentIntervalRegexp is the shape of an interval. It must accept
// exactly what keystone/commitment's ParseInterval accepts, and a test
// there pins the two against a shared table — a validator that accepted
// what the engine then rejected would let a household approve a plan
// that could never run.
//
// "m" is minutes, as everywhere in Go; a month is "mo".
var commitmentIntervalRegexp = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)?|[0-9]+(?:\.[0-9]+)?(?:d|w)|[0-9]+(?:mo|yr|y)|[0-9]+(?:\.[0-9]+)?(?:h|m|s|ms|us|ns))$`)

// commitmentDateRegexp is a plain calendar date. Resolved against the
// household's timezone at evaluation, never here.
var commitmentDateRegexp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// ParseCommitmentAnchor reads an anchor: a date, or an RFC3339 instant.
// A bare date is returned midnight-in-UTC and is meant to be re-resolved
// against the household's timezone by the caller that knows it.
func ParseCommitmentAnchor(anchor string) (time.Time, error) {
	anchor = strings.TrimSpace(anchor)
	if anchor == "" {
		return time.Time{}, fmt.Errorf("required: the first cycle has to start somewhere")
	}
	if commitmentDateRegexp.MatchString(anchor) {
		t, err := time.Parse("2006-01-02", anchor)
		if err != nil {
			return time.Time{}, fmt.Errorf("%q is not a date", anchor)
		}
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, anchor)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q must be a date (2026-09-10) or an RFC3339 instant", anchor)
	}
	return t, nil
}

// CommitmentIntervalValid reports whether an interval is one the engine
// will accept.
func CommitmentIntervalValid(s string) bool {
	s = strings.TrimSpace(s)
	if !commitmentIntervalRegexp.MatchString(s) {
		return false
	}
	// A leading zero quantity parses and means nothing.
	digits := strings.TrimRight(s, "abcdefghijklmnopqrstuvwxyz")
	v, err := strconv.ParseFloat(digits, 64)
	return err == nil && v > 0
}

func validateCommitment(r *Resource) error {
	fail := func(field, msg string) error {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: field, Msg: msg}
	}
	if r.Commitment == nil {
		return fail("spec", "commitment spec missing")
	}
	if !HouseholdOwnerAllowed(r.Metadata.Owner) {
		return fail("metadata.owner", "must be household, household/<member>, or capability/<name>")
	}
	c := r.Commitment
	if strings.TrimSpace(c.Description) == "" {
		return fail("spec.description", "must not be empty: it is what a reviewer reads, and what the household is later told")
	}
	if err := checkAutomationText(c.Description, 500); err != "" {
		return fail("spec.description", err)
	}
	if !commitmentRollovers[c.Rollover] {
		return fail("spec.rollover", fmt.Sprintf(
			"must be one of %s — there is no safe default, and a recurring commitment that silently became a one-off would be mentioned exactly once (ADR-W035 A9)",
			strings.Join(CommitmentRollovers(), ", ")))
	}
	anchor, err := ParseCommitmentAnchor(c.Anchor)
	if err != nil {
		return fail("spec.anchor", err.Error())
	}
	if c.Scope != "" && !commitmentScopes[c.Scope] {
		return fail("spec.scope", fmt.Sprintf("must be one of %s", strings.Join(CommitmentScopes(), ", ")))
	}
	if len(c.Due) == 0 {
		return fail("spec.due", "needs at least one threshold: nothing can come due")
	}
	if len(c.Due) > MaxCommitmentDueRules {
		return fail("spec.due", fmt.Sprintf("at most %d thresholds", MaxCommitmentDueRules))
	}

	seen := map[string]bool{}
	for i, d := range c.Due {
		field := fmt.Sprintf("spec.due[%d]", i)
		if d.Meter == "" {
			return fail(field+".meter", "names no meter")
		}
		if seen[d.Meter] {
			return fail(field+".meter", fmt.Sprintf("meter %q appears twice; one threshold per meter", d.Meter))
		}
		seen[d.Meter] = true
		if d.Meter != MeterClock && !nameRegexp.MatchString(d.Meter) {
			return fail(field+".meter", fmt.Sprintf("%q is not a resource name", d.Meter))
		}

		absolute := d.At != "" || d.AtValue != 0
		if absolute && c.Rollover != RolloverOnce {
			return fail(field, fmt.Sprintf(
				"an absolute due point belongs to a one-off — a fixed point cannot repeat, so use every with rollover %s", c.Rollover))
		}
		if absolute && d.Every != "" {
			return fail(field, "says both when it is due and how far apart it recurs; pick one")
		}
		if !absolute && d.Every == "" {
			return fail(field, "says neither every nor an absolute due point: nothing can come due")
		}
		if d.At != "" && d.Meter != MeterClock {
			return fail(field+".at", fmt.Sprintf("meter %q counts, so its absolute due point is a number (atValue), not a date", d.Meter))
		}
		if d.AtValue != 0 && d.Meter == MeterClock {
			return fail(field+".atValue", "the clock's absolute due point is a date (at), not a number")
		}
		if d.AtValue < 0 {
			return fail(field+".atValue", "a meter counts up from zero")
		}
		if d.At != "" {
			due, derr := ParseCommitmentAnchor(d.At)
			if derr != nil {
				return fail(field+".at", derr.Error())
			}
			if !due.After(anchor) {
				return fail(field+".at", fmt.Sprintf(
					"due %s is not after the anchor %s: it was already past before tracking began", d.At, c.Anchor))
			}
		}
		if start, ok := c.AnchorAt[d.Meter]; ok && d.AtValue != 0 && d.AtValue <= start {
			return fail(field+".atValue", fmt.Sprintf(
				"due at %g, but meter %q already read %g when tracking began: it was already past before tracking began", d.AtValue, d.Meter, start))
		}

		for name, v := range map[string]string{"every": d.Every, "lead": d.Lead} {
			if v == "" {
				continue
			}
			if !CommitmentIntervalValid(v) {
				return fail(field+"."+name, fmt.Sprintf(
					"%q is not an interval: use a number (5000), a span (90d, 2w, 36h) or a calendar step (6mo, 1yr) — m is minutes, a month is mo", v))
			}
			calendar := intervalIsCalendar(v)
			if d.Meter == MeterClock && !calendar {
				return fail(field+"."+name, fmt.Sprintf("the clock is measured in time (90d, 6mo, 1yr), not in %s", v))
			}
			if d.Meter != MeterClock && calendar {
				return fail(field+"."+name, fmt.Sprintf("meter %q counts, so its %s is a number, not %s", d.Meter, name, v))
			}
		}
	}

	for meter := range c.AnchorAt {
		if !seen[meter] {
			return fail("spec.anchorAt", fmt.Sprintf(
				"names meter %q, which no threshold uses: a starting position for a meter nothing measures against is a typo", meter))
		}
		if meter == MeterClock {
			return fail("spec.anchorAt", "the calendar's starting position is the anchor itself")
		}
	}

	if c.SatisfiedBy != nil {
		if !automationEventTypeRegexp.MatchString(c.SatisfiedBy.EventType) {
			return fail("spec.satisfiedBy.eventType", fmt.Sprintf(
				"%q must be namespace.name in lowercase [a-z0-9_], e.g. vehicle.serviced", c.SatisfiedBy.EventType))
		}
		for name, v := range map[string]string{"subject": c.SatisfiedBy.Subject, "entity": c.SatisfiedBy.Entity} {
			if err := checkAutomationIdentifier(v); err != "" {
				return fail("spec.satisfiedBy."+name, err)
			}
		}
	}

	for field, people := range map[string][]string{"spec.mayComplete": c.MayComplete, "spec.notify": c.Notify} {
		if len(people) > MaxCommitmentPeople {
			return fail(field, fmt.Sprintf("at most %d entries", MaxCommitmentPeople))
		}
		for _, p := range people {
			if !HouseholdOwnerAllowed(p) && !commitmentGroupAllowed(p) {
				return fail(field, fmt.Sprintf(
					"%q must be household, household/<member> or group/<name>", p))
			}
		}
	}
	return nil
}

// commitmentGroupAllowed accepts ADR-W033's group namespace, so that
// "the adults" can be named as the group it already is rather than as a
// word this package would have to define. W033 A1 makes `child` a Member
// profile rather than a role, so there is no adults-versus-children
// vocabulary here to get wrong.
func commitmentGroupAllowed(s string) bool {
	rest, ok := strings.CutPrefix(s, "group/")
	return ok && nameRegexp.MatchString(rest)
}

// intervalIsCalendar reports whether an interval is measured in time
// rather than in counts.
func intervalIsCalendar(s string) bool {
	return strings.TrimRight(strings.TrimSpace(s), "0123456789.") != ""
}

// CommitmentAnchorAt resolves an anchor against the household's own
// timezone, which is the only place that knows what time it is.
//
// A BARE DATE AND AN INSTANT ARE NOT THE SAME KIND OF THING, and
// treating them alike is a bug this had. `2026-09-10` is a day where the
// household lives, so it is built in loc and midnight there is where the
// first cycle starts. `2026-09-10T12:00:00Z` already names a moment; a
// caller that rebuilt its wall-clock fields in loc would move it seven
// hours in an America/Los_Angeles household, and every threshold derived
// from it would move with it — silently, and by an amount that still
// looks like a plausible date.
func CommitmentAnchorAt(anchor string, loc *time.Location) (time.Time, error) {
	t, err := ParseCommitmentAnchor(anchor)
	if err != nil {
		return time.Time{}, err
	}
	if loc == nil {
		loc = time.UTC
	}
	if !commitmentDateRegexp.MatchString(strings.TrimSpace(anchor)) {
		// An instant. It is already the moment it says it is.
		return t, nil
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc), nil
}
