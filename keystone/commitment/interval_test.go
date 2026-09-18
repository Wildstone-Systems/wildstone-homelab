package commitment

import (
	"strings"
	"testing"
	"time"
)

func TestParseInterval(t *testing.T) {
	day := 24 * time.Hour
	for _, tc := range []struct {
		in   string
		want Interval
	}{
		{"5000", Interval{Amount: 5000}},
		{"5000.5", Interval{Amount: 5000.5}},
		{" 300 ", Interval{Amount: 300}},
		{"90d", Interval{Span: 90 * day}},
		{"2w", Interval{Span: 14 * day}},
		{"36h", Interval{Span: 36 * time.Hour}},
		{"30m", Interval{Span: 30 * time.Minute}}, // minutes, as everywhere in Go
		{"6mo", Interval{Months: 6}},
		{"18mo", Interval{Months: 18}},
		{"1yr", Interval{Months: 12}},
		{"2y", Interval{Months: 24}},
	} {
		got, err := ParseInterval(tc.in)
		if err != nil {
			t.Errorf("ParseInterval(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseInterval(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseIntervalRejectsNonsense(t *testing.T) {
	// "1m" is thirty minutes' worth of trouble if it silently means a
	// month, so the long form is required and everything else refuses.
	for _, in := range []string{"", "   ", "abc", "0", "-5", "-5000", "1.5mo", "1day", "2wk", "mo", "5000x"} {
		if got, err := ParseInterval(in); err == nil {
			t.Errorf("ParseInterval(%q) = %+v, want an error", in, got)
		}
	}
}

func TestParseIntervalErrorsSayWhatIsAccepted(t *testing.T) {
	_, err := ParseInterval("1 month")
	if err == nil {
		t.Fatal("accepted \"1 month\"")
	}
	for _, want := range []string{"5000", "90d", "6mo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not show the %q form", err, want)
		}
	}
}

func TestIntervalString(t *testing.T) {
	for _, tc := range []struct {
		in   Interval
		want string
	}{
		{Interval{Amount: 5000}, "5000"},
		{Interval{Amount: 0.5}, "0.5"},
		{Interval{Span: 90 * 24 * time.Hour}, "90d"},
		{Interval{Span: 36 * time.Hour}, "36h0m0s"},
		{Interval{Months: 6}, "6mo"},
		{Interval{Months: 24}, "2yr"},
		{Interval{}, "0"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIntervalValidate(t *testing.T) {
	for _, iv := range []Interval{{}, {Amount: 5000}, {Span: time.Hour}, {Months: 6}} {
		if err := iv.Validate(); err != nil {
			t.Errorf("%+v: %v", iv, err)
		}
	}
	for _, iv := range []Interval{
		{Amount: 5000, Months: 6},
		{Span: time.Hour, Months: 6},
		{Amount: -1},
		{Months: -6},
	} {
		if err := iv.Validate(); err == nil {
			t.Errorf("%+v was accepted", iv)
		}
	}
}

func TestIntervalRoundTripsThroughItsOwnString(t *testing.T) {
	for _, s := range []string{"5000", "90d", "6mo", "2yr"} {
		first, err := ParseInterval(s)
		if err != nil {
			t.Fatalf("ParseInterval(%q): %v", s, err)
		}
		second, err := ParseInterval(first.String())
		if err != nil {
			t.Fatalf("ParseInterval(%q): %v", first.String(), err)
		}
		if first != second {
			t.Errorf("%q round-tripped to %+v, want %+v", s, second, first)
		}
	}
}
