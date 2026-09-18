package commitment

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Interval is one step of a due rule: how far apart two occurrences of a
// commitment sit, in the coordinate space of the meter the rule names.
//
// Three forms, because two genuinely different things are being counted
// and pretending otherwise produces wrong dates:
//
//   - Amount — a plain number on a counter meter. 5000 miles.
//   - Span — an exact elapsed time on the clock. 90d, 24h. A day here is
//     always 24 hours.
//   - Months — a calendar step on the clock. 6mo, 1yr. It lands on the
//     same day of the month, which is what a person means by "every six
//     months" and is not the same as 182 days.
//
// Exactly one is set. The clock takes Span or Months; a counter meter
// takes Amount. ADR-W034 A2 makes the clock a meter like any other, and
// this type is where that claim is paid for: everything above here works
// in positions, and only advance() knows that one meter's positions are
// seconds and that adding six months to them is a calendar operation.
type Interval struct {
	Amount float64       `json:"amount,omitempty"`
	Span   time.Duration `json:"span,omitempty"`
	Months int           `json:"months,omitempty"`
}

// Zero reports whether nothing is set — a lead of zero, which means the
// commitment never enters due_soon and goes straight from pending to
// overdue at its threshold.
func (iv Interval) Zero() bool { return iv.Amount == 0 && iv.Span == 0 && iv.Months == 0 }

// Calendar reports whether this interval is measured in time.
func (iv Interval) Calendar() bool { return iv.Span != 0 || iv.Months != 0 }

// Validate refuses an interval that is negative, or that sets more than
// one form. A zero interval is valid: it is what a rule with no lead
// window carries.
func (iv Interval) Validate() error {
	set := 0
	for _, on := range []bool{iv.Amount != 0, iv.Span != 0, iv.Months != 0} {
		if on {
			set++
		}
	}
	if set > 1 {
		return fmt.Errorf("commitment: an interval sets one of amount, span or months, not %d of them", set)
	}
	if iv.Amount < 0 || iv.Span < 0 || iv.Months < 0 {
		return fmt.Errorf("commitment: an interval counts forward: %+v", iv)
	}
	return nil
}

func (iv Interval) String() string {
	switch {
	case iv.Months != 0 && iv.Months%12 == 0:
		return strconv.Itoa(iv.Months/12) + "yr"
	case iv.Months != 0:
		return strconv.Itoa(iv.Months) + "mo"
	case iv.Span != 0 && iv.Span%(24*time.Hour) == 0:
		return strconv.FormatInt(int64(iv.Span/(24*time.Hour)), 10) + "d"
	case iv.Span != 0:
		return iv.Span.String()
	case iv.Amount != 0:
		return strconv.FormatFloat(iv.Amount, 'f', -1, 64)
	}
	return "0"
}

// ParseInterval reads the forms a household writes in a Commitment:
//
//	5000     5000.5          a count, for a counter meter
//	90d      2w      36h     an exact span, for the clock
//	6mo      1yr             a calendar step, for the clock
//
// "m" is minutes, as everywhere else in Go; a month is "mo". That
// ambiguity is the single most common way to write an interval that
// parses and is wrong by a factor of forty-three thousand, so the long
// form is required rather than guessed at.
func ParseInterval(s string) (Interval, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Interval{}, fmt.Errorf("commitment: empty interval")
	}
	// No whitespace INSIDE. "6 mo" parsed as six months only because the
	// quantity was trimmed on its way to ParseFloat, which made it legal
	// here and illegal to the Statefile validator that offers the same
	// grammar — so one tool accepted what the other refused. Found by the
	// test in structural_test.go that pins the two against one table,
	// which is the entire reason that test exists.
	if strings.ContainsAny(s, " \t\n\r") {
		return Interval{}, fmt.Errorf(
			"commitment: %q has a space in it: an interval is one word — a number (5000), a span (90d, 2w, 36h) or a calendar step (6mo, 1yr)", s)
	}
	fail := func() (Interval, error) {
		return Interval{}, fmt.Errorf("commitment: %q is not an interval: use a number (5000), a span (90d, 2w, 36h) or a calendar step (6mo, 1yr)", s)
	}
	num := func(body string) (float64, bool) {
		v, err := strconv.ParseFloat(strings.TrimSpace(body), 64)
		if err != nil || v <= 0 {
			return 0, false
		}
		return v, true
	}
	whole := func(body string) (int, bool) {
		v, ok := num(body)
		if !ok || v != float64(int(v)) {
			return 0, false
		}
		return int(v), true
	}

	switch {
	case strings.HasSuffix(s, "yr"), strings.HasSuffix(s, "y"):
		n, ok := whole(strings.TrimSuffix(strings.TrimSuffix(s, "yr"), "y"))
		if !ok {
			return fail()
		}
		return Interval{Months: n * 12}, nil
	case strings.HasSuffix(s, "mo"):
		n, ok := whole(strings.TrimSuffix(s, "mo"))
		if !ok {
			return fail()
		}
		return Interval{Months: n}, nil
	case strings.HasSuffix(s, "w"):
		v, ok := num(strings.TrimSuffix(s, "w"))
		if !ok {
			return fail()
		}
		return Interval{Span: time.Duration(v * float64(7*24*time.Hour))}, nil
	case strings.HasSuffix(s, "d"):
		v, ok := num(strings.TrimSuffix(s, "d"))
		if !ok {
			return fail()
		}
		return Interval{Span: time.Duration(v * float64(24*time.Hour))}, nil
	}
	if v, ok := num(s); ok {
		return Interval{Amount: v}, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return fail()
	}
	return Interval{Span: d}, nil
}
