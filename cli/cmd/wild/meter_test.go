package main

import (
	"strings"
	"testing"
	"time"
)

func standing(latest *struct {
	Value      float64   `json:"value"`
	At         time.Time `json:"at"`
	RecordedBy string    `json:"recordedBy"`
	Source     string    `json:"source"`
	Rebase     bool      `json:"rebase"`
	Reason     string    `json:"reason"`
}, unit string, stale, builtIn bool) string {
	return meterStanding(meterView{Unit: unit, Stale: stale, BuiltIn: builtIn, Latest: latest})
}

// The three cases a person reads, and the one distinction that matters:
// never read, read too long ago, and fine must not look alike.
func TestMeterStandingKeepsSilenceAndAllClearApart(t *testing.T) {
	read := &struct {
		Value      float64   `json:"value"`
		At         time.Time `json:"at"`
		RecordedBy string    `json:"recordedBy"`
		Source     string    `json:"source"`
		Rebase     bool      `json:"rebase"`
		Reason     string    `json:"reason"`
	}{Value: 84210, At: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)}

	never := standing(nil, "miles", false, false)
	stale := standing(read, "miles", true, false)
	fine := standing(read, "miles", false, false)
	builtIn := standing(nil, "", false, true)

	if !strings.Contains(never, "never read") {
		t.Errorf("never-read = %q", never)
	}
	if !strings.Contains(stale, "too old") {
		t.Errorf("stale = %q, want it to say the number cannot be relied on", stale)
	}
	if strings.Contains(fine, "too old") {
		t.Errorf("a current reading was described as too old: %q", fine)
	}
	if never == fine || stale == fine || never == stale {
		t.Errorf("two different standings render the same:\n never: %q\n stale: %q\n fine:  %q", never, stale, fine)
	}
	if !strings.Contains(builtIn, "built in") {
		t.Errorf("built-in = %q", builtIn)
	}
	// A never-read meter must not report a number at all, least of all a
	// zero — a meter nobody has read does not read zero (ADR-W034 A11).
	if strings.Contains(never, "0") {
		t.Errorf("never-read standing %q contains a number", never)
	}
}
