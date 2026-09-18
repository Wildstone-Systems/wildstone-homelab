package api

import (
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/commitment"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// K1 exists to serve K3, and this is the only place all three pieces —
// the declared Meter, the stored readings, and the due-ness function —
// are in the same room. Without it, whether a `runtimedb.MeterReading`
// can become a `commitment.Reading` at all is a question K4 would
// discover the answer to, months from now, with the store already built.
//
// This is not the bridge; K4 builds that. It is the proof that one can
// exist, written while the schema is still cheap to change.
func TestWhatK1StoresIsWhatTheDuenessFunctionTakes(t *testing.T) {
	h := newHarness(t)
	declareMeter(t, h, "f150-odometer", resource.MeterSpec{
		Description: "Odometer on the F-150",
		Unit:        "miles",
		StaleAfter:  "1080h",
		Estimate:    "12000/yr",
	})

	anchor := time.Now().Add(-120 * 24 * time.Hour).UTC()
	for i, v := range []float64{79300, 81500, 84100} {
		h.do(runtimedb.RoleAdmin, "POST", "/api/v1/household/meters/f150-odometer/readings",
			map[string]any{"value": v, "at": anchor.AddDate(0, 0, i*50)}, nil)
	}

	// --- the conversion K4 will own ---
	current, err := h.s.State.Load()
	if err != nil {
		t.Fatalf("State.Load: %v", err)
	}
	var spec *resource.MeterSpec
	for _, res := range current {
		if res.Kind == resource.KindMeter && res.Metadata.Name == "f150-odometer" {
			spec = res.Meter
		}
	}
	if spec == nil {
		t.Fatal("the declared meter is not in desired state")
	}
	staleAfter, err := resource.MeterStaleAfter(spec.StaleAfter)
	if err != nil {
		t.Fatalf("MeterStaleAfter: %v", err)
	}
	perDay, err := resource.MeterEstimatePerDay(spec.Estimate)
	if err != nil {
		t.Fatalf("MeterEstimatePerDay: %v", err)
	}
	stored, err := h.s.DB.ListMeterReadings("f150-odometer", 0)
	if err != nil {
		t.Fatalf("ListMeterReadings: %v", err)
	}
	meter := commitment.Meter{
		Name: "f150-odometer", Unit: spec.Unit,
		StaleAfter: staleAfter, Estimate: perDay,
	}
	for _, r := range stored {
		meter.Readings = append(meter.Readings, commitment.Reading{
			At: r.At, Value: r.Value, By: r.RecordedBy,
			Rebase: r.Rebase, Prior: r.Prior, Reason: r.Reason,
		})
	}

	// --- and the function takes it ---
	occ, err := commitment.Evaluate(commitment.Input{
		Commitment: commitment.Commitment{
			Name:     "f150-oil",
			Anchor:   anchor,
			AnchorAt: map[string]float64{"f150-odometer": 79300},
			Rollover: commitment.FromCompletion,
			Due: []commitment.DueRule{{
				Meter: "f150-odometer",
				Every: commitment.Interval{Amount: 5000},
				Lead:  commitment.Interval{Amount: 300},
			}},
		},
		Meters: map[string]commitment.Meter{"f150-odometer": meter},
		Now:    time.Now(),
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// Due at 84,300, the truck is at 84,100, the lead is 300.
	if occ.State != commitment.StateDueSoon {
		t.Errorf("state = %q, want due_soon (%s)", occ.State, occ.Reason)
	}
	if occ.Projection == nil {
		t.Errorf("no projection: the meter's declared estimate did not reach the function")
	} else if occ.Projection.Source != "estimate" {
		t.Errorf("projection source = %q, want the declared estimate", occ.Projection.Source)
	}

	// The reading the household typed in is the one the engine saw, and
	// nothing about which door it came through travelled with it.
	rule := occ.Rules[0]
	if rule.Position != 84100 {
		t.Errorf("position = %g, want 84100", rule.Position)
	}
	if rule.DueAt != 84300 {
		t.Errorf("dueAt = %g, want 84300", rule.DueAt)
	}
}
