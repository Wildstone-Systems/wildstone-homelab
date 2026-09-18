package commitment

import "time"

// A position is a point in one meter's coordinate space, as a float64:
// miles on an odometer, hours on a blower, seconds since the Unix epoch
// on the clock. One type for both is what lets the rest of this package
// have a single code path (ADR-W035 A2), and this file is the only place
// that knows the clock is different.
//
// Seconds as float64 hold the epoch to well under a microsecond — about
// 1.8e9 needs 31 bits of a 53-bit mantissa — which is far finer than
// anything a household commitment cares about.

func clockPos(t time.Time) float64 {
	return float64(t.Unix()) + float64(t.Nanosecond())/1e9
}

func clockTime(p float64, loc *time.Location) time.Time {
	sec := int64(p)
	nsec := int64((p - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).In(loc)
}

// advance returns the position one interval past p.
//
// For a counter this is addition. For the clock it is addition too, when
// the interval is an exact span — but a calendar step is not addition at
// all: "every six months" means the same day of the month, through
// months of different lengths and through daylight saving, so it goes
// out to a time.Time and back.
func advance(p float64, iv Interval, clock bool, loc *time.Location) float64 {
	if !clock {
		return p + iv.Amount
	}
	if iv.Months != 0 {
		return clockPos(addMonths(clockTime(p, loc), iv.Months, loc))
	}
	return p + iv.Span.Seconds()
}

// retreat is advance backwards, used to place a lead window before a
// threshold. A zero interval retreats nowhere, which is how a rule with
// no lead goes straight from pending to overdue.
func retreat(p float64, iv Interval, clock bool, loc *time.Location) float64 {
	if iv.Zero() {
		return p
	}
	if !clock {
		return p - iv.Amount
	}
	if iv.Months != 0 {
		return clockPos(addMonths(clockTime(p, loc), -iv.Months, loc))
	}
	return p - iv.Span.Seconds()
}

// addMonths steps n calendar months, clamping to the end of the target
// month.
//
// time.Time.AddDate does not clamp: 31 January plus one month is 3
// March, because it normalises 31 February forward. For a household
// that is simply wrong — a filter changed on the 31st is due at the end
// of February, not in March — so the day is clamped to the last one the
// target month has.
func addMonths(t time.Time, n int, loc *time.Location) time.Time {
	t = t.In(loc)
	y, m, d := t.Date()
	hh, mm, ss := t.Clock()
	// Day 1 of the target month, with the month arithmetic normalised by
	// time.Date (month 13 becomes January of the next year).
	target := time.Date(y, m+time.Month(n), 1, hh, mm, ss, t.Nanosecond(), loc)
	// Day 0 of the month after the target is the target's last day.
	lastDay := time.Date(target.Year(), target.Month()+1, 0, 0, 0, 0, 0, loc).Day()
	if d > lastDay {
		d = lastDay
	}
	return time.Date(target.Year(), target.Month(), d, hh, mm, ss, t.Nanosecond(), loc)
}
