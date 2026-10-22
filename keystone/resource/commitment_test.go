package resource

import (
	"strings"
	"testing"
	"time"
)

func commitmentResource(spec CommitmentSpec) Resource {
	return Resource{
		ApiVersion: ApiVersion, Kind: KindCommitment,
		Metadata:   Metadata{Name: "f150-oil", Owner: "household/mika"},
		Commitment: &spec,
	}
}

func TestTheThreeShapesADRW035Describes(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec CommitmentSpec
	}{
		{"the checkup nobody booked", CommitmentSpec{
			Description: "Kai's annual well-child visit", Rollover: RolloverFromSchedule,
			Anchor: "2025-09-10", RequiresBooking: true,
			Due:         []CommitmentDue{{Meter: MeterClock, Every: "1yr", Lead: "60d"}},
			SatisfiedBy: &CommitmentSatisfiedBy{EventType: "health.visit_completed", Subject: "kai"},
			MayComplete: []string{"group/adults"},
		}},
		{"the truck, on two meters", CommitmentSpec{
			Description: "F-150 oil and filter", Rollover: RolloverFromCompletion,
			Anchor: "2026-03-01", AnchorAt: map[string]float64{"f150-odometer": 79300},
			Due: []CommitmentDue{
				{Meter: "f150-odometer", Every: "5000", Lead: "300"},
				{Meter: MeterClock, Every: "6mo", Lead: "14d"},
			},
			SatisfiedBy: &CommitmentSatisfiedBy{EventType: "vehicle.serviced", Entity: "f150"},
		}},
		{"the dentist on the third", CommitmentSpec{
			Description: "Kai's dentist appointment", Rollover: RolloverOnce,
			Anchor: "2026-09-18", RequiresBooking: true,
			Due:         []CommitmentDue{{Meter: MeterClock, At: "2026-10-03", Lead: "7d"}},
			MayComplete: []string{"household/mika", "household/sarah"},
			Scope:       CommitmentScopeHousehold,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := commitmentResource(tc.spec)
			if err := r.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestCommitmentValidationRefusals(t *testing.T) {
	ok := CommitmentSpec{
		Description: "F-150 oil", Rollover: RolloverFromCompletion, Anchor: "2026-03-01",
		Due: []CommitmentDue{{Meter: "f150-odometer", Every: "5000"}},
	}
	with := func(f func(*CommitmentSpec)) CommitmentSpec {
		c := ok
		c.Due = append([]CommitmentDue{}, ok.Due...)
		f(&c)
		return c
	}
	for _, tc := range []struct {
		name  string
		spec  CommitmentSpec
		field string
	}{
		{"no rollover", with(func(c *CommitmentSpec) { c.Rollover = "" }), "spec.rollover"},
		{"invented rollover", with(func(c *CommitmentSpec) { c.Rollover = "whenever" }), "spec.rollover"},
		{"no anchor", with(func(c *CommitmentSpec) { c.Anchor = "" }), "spec.anchor"},
		{"anchor is prose", with(func(c *CommitmentSpec) { c.Anchor = "last March" }), "spec.anchor"},
		{"no description", with(func(c *CommitmentSpec) { c.Description = "" }), "spec.description"},
		{"no thresholds", with(func(c *CommitmentSpec) { c.Due = nil }), "spec.due"},
		{"invented scope", with(func(c *CommitmentSpec) { c.Scope = "secret" }), "spec.scope"},
		{"one meter twice", with(func(c *CommitmentSpec) {
			c.Due = append(c.Due, CommitmentDue{Meter: "f150-odometer", Every: "9000"})
		}), "spec.due[1].meter"},
		{"clock counted in miles", with(func(c *CommitmentSpec) {
			c.Due = []CommitmentDue{{Meter: MeterClock, Every: "5000"}}
		}), "spec.due[0].every"},
		{"counter counted in months", with(func(c *CommitmentSpec) {
			c.Due = []CommitmentDue{{Meter: "odo", Every: "6mo"}}
		}), "spec.due[0].every"},
		{"lead in the wrong units", with(func(c *CommitmentSpec) {
			c.Due = []CommitmentDue{{Meter: "odo", Every: "5000", Lead: "14d"}}
		}), "spec.due[0].lead"},
		{"no interval at all", with(func(c *CommitmentSpec) {
			c.Due = []CommitmentDue{{Meter: "odo"}}
		}), "spec.due[0]"},
		{"a month written as m", with(func(c *CommitmentSpec) {
			c.Due = []CommitmentDue{{Meter: MeterClock, Every: "6m"}}
		}), ""}, // accepted: six minutes, which is the point of requiring mo
		{"absolute point on a recurring commitment", with(func(c *CommitmentSpec) {
			c.Due = []CommitmentDue{{Meter: MeterClock, At: "2026-10-03"}}
		}), "spec.due[0]"},
		{"anchorAt for a meter nothing measures", with(func(c *CommitmentSpec) {
			c.AnchorAt = map[string]float64{"furnace-hours": 10}
		}), "spec.anchorAt"},
		{"anchorAt for the calendar", with(func(c *CommitmentSpec) {
			c.Due = []CommitmentDue{{Meter: MeterClock, Every: "6mo"}}
			c.AnchorAt = map[string]float64{MeterClock: 10}
		}), "spec.anchorAt"},
		{"satisfiedBy is not an event type", with(func(c *CommitmentSpec) {
			c.SatisfiedBy = &CommitmentSatisfiedBy{EventType: "Vehicle Serviced"}
		}), "spec.satisfiedBy.eventType"},
		{"mayComplete names a stranger", with(func(c *CommitmentSpec) {
			c.MayComplete = []string{"whoever"}
		}), "spec.mayComplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := commitmentResource(tc.spec)
			err := r.Validate()
			if tc.field == "" {
				if err != nil {
					t.Fatalf("refused something valid: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
			var ve *ValidationError
			if !asValidationError(err, &ve) || ve.Field != tc.field {
				t.Fatalf("want a %s error, got %v", tc.field, err)
			}
		})
	}
}

// A one-off's absolute due point must be ahead of where tracking began,
// on the clock and on a counter alike — the asymmetry review found twice
// in the engine, refused here at the door.
func TestADuePointBehindTrackingIsRefusedOnBothKindsOfMeter(t *testing.T) {
	clock := commitmentResource(CommitmentSpec{
		Description: "dentist", Rollover: RolloverOnce, Anchor: "2026-09-18",
		Due: []CommitmentDue{{Meter: MeterClock, At: "2026-08-01"}},
	})
	if err := clock.Validate(); err == nil || !strings.Contains(err.Error(), "already past") {
		t.Errorf("clock: %v", err)
	}
	counter := commitmentResource(CommitmentSpec{
		Description: "brakes", Rollover: RolloverOnce, Anchor: "2026-03-01",
		AnchorAt: map[string]float64{"odo": 100},
		Due:      []CommitmentDue{{Meter: "odo", AtValue: 50}},
	})
	if err := counter.Validate(); err == nil || !strings.Contains(err.Error(), "already past") {
		t.Errorf("counter: %v", err)
	}
}

func TestCommitmentRoundTripsThroughYAML(t *testing.T) {
	r := commitmentResource(CommitmentSpec{
		Description: "F-150 oil and filter", Rollover: RolloverFromCompletion,
		Anchor: "2026-03-01", AnchorAt: map[string]float64{"f150-odometer": 79300},
		Due: []CommitmentDue{
			{Meter: "f150-odometer", Every: "5000", Lead: "300"},
			{Meter: MeterClock, Every: "6mo", Lead: "14d"},
		},
		SatisfiedBy: &CommitmentSatisfiedBy{EventType: "vehicle.serviced", Entity: "f150"},
		MayComplete: []string{"group/adults"},
	})
	out, err := Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := Unmarshal(out)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Commitment == nil || len(back.Commitment.Due) != 2 {
		t.Fatalf("round trip lost the thresholds: %+v", back.Commitment)
	}
	if back.Commitment.AnchorAt["f150-odometer"] != 79300 ||
		back.Commitment.SatisfiedBy.Entity != "f150" ||
		back.Commitment.Due[1].Lead != "14d" {
		t.Errorf("round trip changed the spec: %+v", back.Commitment)
	}
}

// An RFC3339 anchor names a moment, and resolving it in the household's
// timezone must not move it. Codex found this on #244: the caller
// rebuilt every anchor's wall-clock fields in loc, so noon UTC became
// noon Pacific and every threshold derived from it shifted seven hours.
func TestAnRFC3339AnchorKeepsItsInstantAndABareDateDoesNot(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	instant, err := CommitmentAnchorAt("2026-09-10T12:00:00Z", la)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC); !instant.Equal(want) {
		t.Errorf("RFC3339 anchor moved: got %s, want %s", instant, want)
	}
	// A bare date, by contrast, IS reinterpreted: it means that day where
	// the household lives, and midnight there is not midnight in UTC.
	bare, err := CommitmentAnchorAt("2026-09-10", la)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 10, 0, 0, 0, 0, la); !bare.Equal(want) {
		t.Errorf("bare date not resolved in the household's timezone: got %s, want %s", bare, want)
	}
	if bare.Equal(time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)) {
		t.Error("bare date was left in UTC; the household's own midnight is the point")
	}
}
