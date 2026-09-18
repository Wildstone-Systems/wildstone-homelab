package resource

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MeterSpec is the spec for kind Meter (ADR-W035 A1): a number the
// household keeps that only counts up — miles on a vehicle, hours on a
// blower, gallons through a filter — against which commitments come due.
//
// A meter declares what the number IS and how it should be read. It does
// not declare what is owed against it: one odometer feeds the oil change,
// the tyres and the brakes, each of which is its own Commitment with its
// own anchor and interval. That separation is why staleAfter and estimate
// live here rather than there — how often somebody looks at the truck,
// and how fast it is driven, are facts about the truck.
//
// Nothing here is executed. The resource is Keystone-owned: it lives in
// the Statefile, is applied at no node, and is present exactly when it is
// desired, the same arrangement Automation has. Readings are NOT part of
// the resource — a reading is evidence recorded as it happens, not
// desired state, and requiring a plan to write down a mileage would mean
// nobody ever writes down a mileage (ADR-W035 A16).
type MeterSpec struct {
	// Description is the human sentence for the plan review and the
	// Console: what this number is.
	Description string `yaml:"description" json:"description"`
	// Unit is what the number counts, in the words a household uses:
	// miles, km, hours, gallons. Display only — nothing computes with
	// it, and two meters in different units are never compared, because
	// a commitment's threshold is always in its own meter's coordinates.
	Unit string `yaml:"unit" json:"unit"`
	// StaleAfter is how old the newest reading may be before this meter
	// can no longer support the conclusion "not due yet" (ADR-W035 A11).
	// A Go duration; empty means it never goes stale, which is right for
	// a meter something else keeps current and wrong for one a person
	// reads off a dashboard.
	StaleAfter string `yaml:"staleAfter,omitempty" json:"staleAfter,omitempty"`
	// Estimate is how fast this meter usually moves, as "<number>/<period>"
	// — "12000/yr", "30/d". It is used ONLY to project a date for
	// display, and a projection never moves a state. Empty means derive
	// a rate from the readings, or show nothing.
	Estimate string `yaml:"estimate,omitempty" json:"estimate,omitempty"`
}

// MeterClock is the name of the built-in meter every household has
// without declaring anything: the calendar (ADR-W035 A2).
//
// It is RESERVED rather than shipped as a Meter resource, and a
// declaration using this name is refused. A household that could declare
// its own clock would have two of them — one in the Statefile and one on
// the wall — and the interesting question would become which is right.
// Keystone supplies this one; `wild meter list` shows it as built in.
//
// keystone/commitment carries the same constant, because it is a leaf
// package on the standard library and importing this one would end that.
// meter_test.go pins the two equal.
const MeterClock = "clock"

const (
	// MaxMeterStaleAfter bounds the staleness window. A meter allowed to
	// go unread for longer than a year is not being tracked.
	MaxMeterStaleAfter = 365 * 24 * time.Hour
	// MaxMeterUnitBytes bounds the unit. "miles" and "engine hours" fit;
	// a sentence does not.
	MaxMeterUnitBytes = 32
)

// meterUnitRegexp is the shape of a unit: lowercase words, optionally
// several. Deliberately narrow — a unit is rendered next to a number on
// a family's dashboard, and markup, newlines or a stray "/" there are a
// presentation bug waiting to be found in a browser rather than here.
var meterUnitRegexp = regexp.MustCompile(`^[a-z]+(?:[ -][a-z]+)*$`)

// meterEstimateRegexp is "<number>/<period>", e.g. 12000/yr.
var meterEstimateRegexp = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)/(d|w|mo|yr)$`)

// meterEstimatePeriodDays is how many days each period is taken to be.
// The month and year are averages, which is right for a projection and
// would be wrong for a threshold — thresholds use the real calendar, in
// keystone/commitment.
var meterEstimatePeriodDays = map[string]float64{
	"d":  1,
	"w":  7,
	"mo": 365.25 / 12,
	"yr": 365.25,
}

// MeterEstimatePerDay parses a spec's Estimate into units per day, which
// is the form the due-ness function takes. Returns 0 when unset.
func MeterEstimatePerDay(estimate string) (float64, error) {
	if strings.TrimSpace(estimate) == "" {
		return 0, nil
	}
	m := meterEstimateRegexp.FindStringSubmatch(estimate)
	if m == nil {
		return 0, fmt.Errorf("%q must be <number>/<period>, where period is d, w, mo or yr — for example 12000/yr", estimate)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%q must carry a positive number", estimate)
	}
	return v / meterEstimatePeriodDays[m[2]], nil
}

// MeterStaleAfter parses a spec's StaleAfter. Zero means never stale.
func MeterStaleAfter(staleAfter string) (time.Duration, error) {
	if strings.TrimSpace(staleAfter) == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(staleAfter)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (use forms like 72h, 1080h)", staleAfter)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q must be positive; omit it for a meter that never goes stale", staleAfter)
	}
	if d > MaxMeterStaleAfter {
		return 0, fmt.Errorf("%s exceeds the %s bound: a meter unread for longer than that is not being tracked", staleAfter, MaxMeterStaleAfter)
	}
	return d, nil
}

// HouseholdOwnerAllowed reports whether an owner is one a household
// resource may carry: the household as a whole ("household"), one member
// ("household/<name>"), or a capability that ships it
// ("capability/<name>"). Nothing is orphaned (ADR-W004 A1); nothing is
// "system" either — it always belongs to someone who can be asked.
func HouseholdOwnerAllowed(owner string) bool {
	if owner == "household" {
		return true
	}
	for _, prefix := range []string{"household/", "capability/"} {
		if rest, ok := strings.CutPrefix(owner, prefix); ok {
			return nameRegexp.MatchString(rest)
		}
	}
	return false
}

func validateMeter(r *Resource) error {
	fail := func(field, msg string) error {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: field, Msg: msg}
	}
	if r.Meter == nil {
		return fail("spec", "meter spec missing")
	}
	if r.Metadata.Name == MeterClock {
		return fail("metadata.name", fmt.Sprintf(
			"%q is the built-in calendar meter and cannot be declared: a household with two clocks has to decide which one is right (ADR-W035 A2)", MeterClock))
	}
	if !HouseholdOwnerAllowed(r.Metadata.Owner) {
		return fail("metadata.owner", "must be household, household/<member>, or capability/<name>")
	}
	m := r.Meter
	if strings.TrimSpace(m.Description) == "" {
		return fail("spec.description", "must not be empty: it is what a reviewer reads before approving the meter")
	}
	if err := checkAutomationText(m.Description, 500); err != "" {
		return fail("spec.description", err)
	}
	if strings.TrimSpace(m.Unit) == "" {
		return fail("spec.unit", "must say what the number counts: miles, hours, gallons")
	}
	if len(m.Unit) > MaxMeterUnitBytes {
		return fail("spec.unit", fmt.Sprintf("exceeds %d bytes", MaxMeterUnitBytes))
	}
	if !meterUnitRegexp.MatchString(m.Unit) {
		return fail("spec.unit", fmt.Sprintf("%q must be lowercase words, e.g. miles or engine hours", m.Unit))
	}
	if _, err := MeterStaleAfter(m.StaleAfter); err != nil {
		return fail("spec.staleAfter", err.Error())
	}
	if _, err := MeterEstimatePerDay(m.Estimate); err != nil {
		return fail("spec.estimate", err.Error())
	}
	return nil
}
