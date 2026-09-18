package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// declareMeter puts a Meter into desired state directly. The plan path
// is exercised elsewhere; what these tests are about is the reading
// intake, which is deliberately NOT a plan.
func declareMeter(t *testing.T, h *harness, name string, spec resource.MeterSpec) {
	t.Helper()
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
		Metadata: resource.Metadata{Name: name, Owner: "household/mika"},
		Meter:    &spec,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := h.s.State.Commit("test", "", "declare meter "+name); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func odometer(t *testing.T, h *harness) {
	t.Helper()
	declareMeter(t, h, "f150-odometer", resource.MeterSpec{
		Description: "Odometer on the F-150", Unit: "miles", StaleAfter: "1080h"})
}

func TestRecordingAReadingIsNotAPlan(t *testing.T) {
	h := newHarness(t)
	odometer(t, h)

	var receipt struct {
		Seq       int64 `json:"seq"`
		Duplicate bool  `json:"duplicate"`
	}
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/household/meters/f150-odometer/readings",
		map[string]any{"value": 79300}, &receipt)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if receipt.Seq == 0 || receipt.Duplicate {
		t.Errorf("receipt = %+v, want a fresh seq", receipt)
	}

	// And nothing is waiting for anybody to approve.
	var plans []map[string]any
	h.do(runtimedb.RoleAdmin, "GET", "/api/v1/plans", nil, &plans)
	if len(plans) != 0 {
		t.Errorf("%d plans after writing down a mileage, want none", len(plans))
	}
}

func TestAReadingForAnUndeclaredMeterIsRefused(t *testing.T) {
	h := newHarness(t)
	odometer(t, h)
	// A name with a typo in it. Accepting this would accumulate readings
	// nothing can see, under a meter that then looks unread.
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/household/meters/f150-odomiter/readings",
		map[string]any{"value": 79300}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestTheCalendarTakesNoReadings(t *testing.T) {
	h := newHarness(t)
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/household/meters/clock/readings",
		map[string]any{"value": 1}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404: the calendar reads itself", resp.StatusCode)
	}
}

func TestAReadingFromTheFutureIsRefused(t *testing.T) {
	h := newHarness(t)
	odometer(t, h)
	future := time.Now().Add(2 * time.Hour)
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/household/meters/f150-odometer/readings",
		map[string]any{"value": 79300, "at": future}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: the due-ness function cannot see past now anyway", resp.StatusCode)
	}
}

func TestANegativeReadingIsRefused(t *testing.T) {
	h := newHarness(t)
	odometer(t, h)
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/household/meters/f150-odometer/readings",
		map[string]any{"value": -5}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// The body cannot name its own author, so a reading is always attributed
// to the credential that carried it.
func TestAReadingIsAttributedToItsCaller(t *testing.T) {
	h := newHarness(t)
	odometer(t, h)
	h.do(runtimedb.RoleAdmin, "POST", "/api/v1/household/meters/f150-odometer/readings",
		map[string]any{"value": 79300, "recordedBy": "somebody-else", "source": "publisher"}, nil)

	got, err := h.s.DB.LatestMeterReading("f150-odometer")
	if err != nil {
		t.Fatalf("LatestMeterReading: %v", err)
	}
	if got.RecordedBy != "root-admin" {
		t.Errorf("recordedBy = %q, want the authenticated principal", got.RecordedBy)
	}
	if got.Source != runtimedb.ReadingByPerson {
		t.Errorf("source = %q, want person: a body cannot promote itself to a publisher", got.Source)
	}
}

func TestAReadingTakenEarlierIsRecordedAsEarlier(t *testing.T) {
	h := newHarness(t)
	odometer(t, h)
	morning := time.Now().Add(-8 * time.Hour).UTC().Truncate(time.Second)
	h.do(runtimedb.RoleAdmin, "POST", "/api/v1/household/meters/f150-odometer/readings",
		map[string]any{"value": 79300, "at": morning}, nil)

	got, err := h.s.DB.LatestMeterReading("f150-odometer")
	if err != nil {
		t.Fatalf("LatestMeterReading: %v", err)
	}
	if !got.At.Equal(morning) {
		t.Errorf("At = %s, want %s", got.At, morning)
	}
	if !got.RecordedAt.After(got.At) {
		t.Errorf("RecordedAt %s should be after At %s", got.RecordedAt, got.At)
	}
}

func TestListingMetersIncludesTheCalendarAndWhereEachStands(t *testing.T) {
	h := newHarness(t)
	odometer(t, h)
	declareMeter(t, h, "furnace-hours", resource.MeterSpec{
		Description: "Blower hours", Unit: "hours"})
	h.do(runtimedb.RoleAdmin, "POST", "/api/v1/household/meters/f150-odometer/readings",
		map[string]any{"value": 79300}, nil)

	var meters []meterView
	resp := h.do(runtimedb.RoleAdmin, "GET", "/api/v1/household/meters", nil, &meters)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	by := map[string]meterView{}
	for _, m := range meters {
		by[m.Name] = m
	}
	if !by["clock"].BuiltIn {
		t.Errorf("the calendar is missing from the list, or not marked built in")
	}
	if by["f150-odometer"].Latest == nil || by["f150-odometer"].Latest.Value != 79300 {
		t.Errorf("odometer = %+v, want its latest reading", by["f150-odometer"])
	}
	// Never read is absent, not zero — the distinction A11 turns on.
	if by["furnace-hours"].Latest != nil {
		t.Errorf("a meter nobody has read reported a reading: %+v", by["furnace-hours"].Latest)
	}
}

func TestAMeterReadTooLongAgoIsMarkedStale(t *testing.T) {
	h := newHarness(t)
	odometer(t, h) // staleAfter 1080h — forty-five days
	old := time.Now().Add(-60 * 24 * time.Hour).UTC()
	h.do(runtimedb.RoleAdmin, "POST", "/api/v1/household/meters/f150-odometer/readings",
		map[string]any{"value": 79300, "at": old}, nil)

	var meters []meterView
	h.do(runtimedb.RoleAdmin, "GET", "/api/v1/household/meters", nil, &meters)
	for _, m := range meters {
		if m.Name != "f150-odometer" {
			continue
		}
		if !m.Stale {
			t.Errorf("a reading sixty days old was not marked stale")
		}
		return
	}
	t.Fatal("the odometer was not listed")
}

func TestReadingHistoryComesBackOldestFirst(t *testing.T) {
	h := newHarness(t)
	odometer(t, h)
	base := time.Now().Add(-90 * 24 * time.Hour).UTC()
	for i, v := range []float64{79300, 80500, 82000} {
		h.do(runtimedb.RoleAdmin, "POST", "/api/v1/household/meters/f150-odometer/readings",
			map[string]any{"value": v, "at": base.AddDate(0, 0, i*20)}, nil)
	}
	var readings []runtimedb.MeterReading
	resp := h.do(runtimedb.RoleAdmin, "GET", "/api/v1/household/meters/f150-odometer/readings", nil, &readings)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(readings) != 3 || readings[0].Value != 79300 || readings[2].Value != 82000 {
		t.Errorf("readings = %+v, want oldest first", readings)
	}
}

func TestAReadingNeedsMoreThanReadAccess(t *testing.T) {
	h := newHarness(t)
	odometer(t, h)
	// The butler may read and propose, and may not record a number it
	// was not given (ADR-W034 A13).
	resp := h.do(runtimedb.RoleButler, "POST", "/api/v1/household/meters/f150-odometer/readings",
		map[string]any{"value": 79300}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	// Reading the history is fine.
	var readings []runtimedb.MeterReading
	if resp := h.do(runtimedb.RoleButler, "GET", "/api/v1/household/meters/f150-odometer/readings", nil, &readings); resp.StatusCode != http.StatusOK {
		t.Errorf("butler could not read the history: %d", resp.StatusCode)
	}
}

func TestARetriedPublisherReadingIsANoOpOverTheAPI(t *testing.T) {
	h := newHarness(t)
	odometer(t, h)
	body := map[string]any{"value": 79300, "externalId": "ha-0001"}

	var first, second struct {
		Seq       int64 `json:"seq"`
		Duplicate bool  `json:"duplicate"`
	}
	if resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/household/meters/f150-odometer/readings", body, &first); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first: status %d", resp.StatusCode)
	}
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/household/meters/f150-odometer/readings", body, &second)
	if resp.StatusCode != http.StatusOK || !second.Duplicate || second.Seq != first.Seq {
		t.Errorf("retry gave %d %+v, want 200 and the first seq", resp.StatusCode, second)
	}
}

func TestReadingsRefuseAnOversizedBody(t *testing.T) {
	h := newHarness(t)
	odometer(t, h)
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/household/meters/f150-odometer/readings",
		map[string]any{"value": 1, "reason": strings.Repeat("x", maxReadingBytes+1)}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
