package commitment

import (
	"testing"
	"time"
)

func TestClockPositionRoundTrips(t *testing.T) {
	for _, want := range []time.Time{
		at(2026, time.September, 17),
		time.Date(2026, 2, 28, 23, 59, 59, 0, chicago),
		time.Date(1999, 12, 31, 0, 0, 0, 0, time.UTC),
	} {
		got := clockTime(clockPos(want), chicago)
		if !got.Equal(want) {
			t.Errorf("round trip of %s gave %s", want, got)
		}
	}
}

func TestAddMonthsClampsToTheEndOfTheMonth(t *testing.T) {
	// time.Time.AddDate would say 3 March here, having normalised 31
	// February forward. A filter changed on the 31st is due at the end of
	// February, not in March.
	for _, tc := range []struct {
		from time.Time
		n    int
		want time.Time
	}{
		{at(2026, time.January, 31), 1, at(2026, time.February, 28)},
		{at(2028, time.January, 31), 1, at(2028, time.February, 29)}, // leap year
		{at(2026, time.March, 31), -1, at(2026, time.February, 28)},  // and backwards
		{at(2026, time.August, 31), 1, at(2026, time.September, 30)},
		{at(2026, time.December, 15), 1, at(2027, time.January, 15)}, // year rolls
		{at(2026, time.January, 15), -1, at(2025, time.December, 15)},
		{at(2026, time.March, 10), 12, at(2027, time.March, 10)},
		{at(2026, time.June, 1), 3, at(2026, time.September, 1)},
	} {
		got := addMonths(tc.from, tc.n, chicago)
		if !got.Equal(tc.want) {
			t.Errorf("addMonths(%s, %d) = %s, want %s",
				tc.from.Format("2 Jan 2006"), tc.n, got.Format("2 Jan 2006"), tc.want.Format("2 Jan 2006"))
		}
	}

	// Sanity: this is the behaviour AddDate does NOT have, which is the
	// whole reason addMonths exists.
	if naive := at(2026, time.January, 31).AddDate(0, 1, 0); naive.Month() != time.March {
		t.Fatalf("AddDate no longer overflows; addMonths may be redundant (got %s)", naive.Format("2 Jan 2006"))
	}
}

func TestCalendarStepsKeepTheLocalTimeAcrossDaylightSaving(t *testing.T) {
	// 15 January 09:00 CST, six months on, is 15 July 09:00 CDT — the
	// same time of day to the household, which is what "every six
	// months" means to a person.
	start := time.Date(2026, 1, 15, 9, 0, 0, 0, chicago)
	got := clockTime(advance(clockPos(start), Interval{Months: 6}, true, chicago), chicago)
	if got.Hour() != 9 || got.Day() != 15 || got.Month() != time.July {
		t.Errorf("six months from %s = %s, want 15 July at 09:00 local", start, got)
	}

	// An exact span does not: 90 days is 2,160 hours, and crossing the
	// spring transition moves the wall clock by an hour. That difference
	// is real and is why Interval keeps the two apart.
	span := clockTime(advance(clockPos(time.Date(2026, 2, 1, 9, 0, 0, 0, chicago)), Interval{Span: 90 * 24 * time.Hour}, true, chicago), chicago)
	if span.Hour() != 10 {
		t.Errorf("90d from 1 Feb 09:00 = %s, want 10:00 local (an hour of DST)", span)
	}
}

func TestAdvanceAndRetreatAreInverses(t *testing.T) {
	start := clockPos(at(2026, time.June, 15))
	for _, iv := range []Interval{{Months: 6}, {Months: 12}, {Span: 90 * 24 * time.Hour}, {Span: 36 * time.Hour}} {
		if got := retreat(advance(start, iv, true, chicago), iv, true, chicago); got != start {
			t.Errorf("advance then retreat by %s gave %s, want %s",
				iv, clockTime(got, chicago).Format(time.RFC3339), clockTime(start, chicago).Format(time.RFC3339))
		}
	}
	// A counter is plain arithmetic.
	if got := retreat(advance(79300, Interval{Amount: 5000}, false, chicago), Interval{Amount: 300}, false, chicago); got != 84000 {
		t.Errorf("counter arithmetic = %g, want 84000", got)
	}
	// A zero lead retreats nowhere: the rule has no warning window.
	if got := retreat(start, Interval{}, true, chicago); got != start {
		t.Errorf("a zero interval moved the position")
	}
}
