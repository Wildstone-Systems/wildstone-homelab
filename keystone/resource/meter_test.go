package resource

import (
	"strings"
	"testing"
	"time"
)

func meterResource(name string, spec MeterSpec) Resource {
	return Resource{
		ApiVersion: ApiVersion, Kind: KindMeter,
		Metadata: Metadata{Name: name, Owner: "household/mika"},
		Meter:    &spec,
	}
}

func TestAValidMeter(t *testing.T) {
	r := meterResource("f150-odometer", MeterSpec{
		Description: "Odometer on the F-150",
		Unit:        "miles",
		StaleAfter:  "1080h",
		Estimate:    "12000/yr",
	})
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// The clock is the meter every household has without declaring one, so
// declaring one is refused rather than quietly shadowing it.
func TestTheClockMeterCannotBeDeclared(t *testing.T) {
	r := meterResource(MeterClock, MeterSpec{Description: "my own clock", Unit: "seconds"})
	err := r.Validate()
	if err == nil {
		t.Fatal("a Meter named clock was accepted")
	}
	var ve *ValidationError
	if !asValidationError(err, &ve) || ve.Field != "metadata.name" {
		t.Fatalf("want a metadata.name error, got %v", err)
	}
	if !strings.Contains(ve.Msg, "built-in") {
		t.Errorf("message %q does not explain why", ve.Msg)
	}
}

func TestMeterValidationRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spec  MeterSpec
		owner string
		field string
	}{
		{"no description", MeterSpec{Unit: "miles"}, "household", "spec.description"},
		{"no unit", MeterSpec{Description: "d"}, "household", "spec.unit"},
		{"unit is a sentence", MeterSpec{Description: "d", Unit: "Miles, since the last service"}, "household", "spec.unit"},
		{"unit shouts", MeterSpec{Description: "d", Unit: "MILES"}, "household", "spec.unit"},
		{"staleAfter is not a duration", MeterSpec{Description: "d", Unit: "miles", StaleAfter: "45 days"}, "household", "spec.staleAfter"},
		{"staleAfter is negative", MeterSpec{Description: "d", Unit: "miles", StaleAfter: "-72h"}, "household", "spec.staleAfter"},
		{"staleAfter beyond the bound", MeterSpec{Description: "d", Unit: "miles", StaleAfter: "9000h"}, "household", "spec.staleAfter"},
		{"estimate has no period", MeterSpec{Description: "d", Unit: "miles", Estimate: "12000"}, "household", "spec.estimate"},
		{"estimate period is invented", MeterSpec{Description: "d", Unit: "miles", Estimate: "12000/fortnight"}, "household", "spec.estimate"},
		{"estimate is zero", MeterSpec{Description: "d", Unit: "miles", Estimate: "0/yr"}, "household", "spec.estimate"},
		{"owner is system", MeterSpec{Description: "d", Unit: "miles"}, "system", "metadata.owner"},
		{"owner is empty", MeterSpec{Description: "d", Unit: "miles"}, "", "metadata.owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := meterResource("odo", tc.spec)
			r.Metadata.Owner = tc.owner
			err := r.Validate()
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

func TestMeterEstimatePerDay(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
	}{
		{"", 0},
		{"30/d", 30},
		{"70/w", 10},
		{"12000/yr", 12000 / 365.25},
		{"1000/mo", 1000 / (365.25 / 12)},
		{"0.5/d", 0.5},
	} {
		got, err := MeterEstimatePerDay(tc.in)
		if err != nil {
			t.Errorf("MeterEstimatePerDay(%q): %v", tc.in, err)
			continue
		}
		if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("MeterEstimatePerDay(%q) = %g, want %g", tc.in, got, tc.want)
		}
	}
	for _, in := range []string{"12000", "12000/", "/yr", "yr", "-5/d", "12000/y", "12,000/yr"} {
		if got, err := MeterEstimatePerDay(in); err == nil {
			t.Errorf("MeterEstimatePerDay(%q) = %g, want an error", in, got)
		}
	}
}

func TestMeterStaleAfter(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"72h", 72 * time.Hour},
		{"1080h", 1080 * time.Hour},
	} {
		got, err := MeterStaleAfter(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("MeterStaleAfter(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"45 days", "-1h", "0h", "9000h"} {
		if _, err := MeterStaleAfter(in); err == nil {
			t.Errorf("MeterStaleAfter(%q) was accepted", in)
		}
	}
}

func TestHouseholdOwnerAllowed(t *testing.T) {
	for _, ok := range []string{"household", "household/mika", "capability/home-automation"} {
		if !HouseholdOwnerAllowed(ok) {
			t.Errorf("%q was refused", ok)
		}
	}
	for _, bad := range []string{"", "system", "household/", "capability/", "mika", "household/Mika!"} {
		if HouseholdOwnerAllowed(bad) {
			t.Errorf("%q was allowed", bad)
		}
	}
	// The two household kinds must agree about who may own one; a meter
	// owned by somebody a rule could not belong to is a gap in the same
	// rule stated twice.
	for _, owner := range []string{"household", "household/mika", "capability/x", "system", ""} {
		if HouseholdOwnerAllowed(owner) != AutomationOwnerAllowed(owner) {
			t.Errorf("%q: meter and automation disagree about ownership", owner)
		}
	}
}

func TestMeterRoundTripsThroughYAML(t *testing.T) {
	r := meterResource("f150-odometer", MeterSpec{
		Description: "Odometer on the F-150", Unit: "miles",
		StaleAfter: "1080h", Estimate: "12000/yr",
	})
	out, err := Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := Unmarshal(out)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Kind != KindMeter || back.Meter == nil {
		t.Fatalf("round trip lost the spec: %+v", back)
	}
	if *back.Meter != *r.Meter {
		t.Errorf("round trip changed the spec:\n %+v\n %+v", *back.Meter, *r.Meter)
	}
}
