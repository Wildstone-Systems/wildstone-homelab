package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func (h *harness) upkeep(t *testing.T, user string) upkeepPage {
	t.Helper()
	res := h.member(http.MethodGet, "/api/v1/home/upkeep", user, "homepage_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("upkeep for %s: status %d", user, res.StatusCode)
	}
	var got upkeepPage
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return got
}

func (h *harness) history(t *testing.T, user, name string) commitmentHistoryPage {
	t.Helper()
	res := h.member(http.MethodGet, "/api/v1/home/commitments/"+name, user, "homepage_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("history for %s: status %d", name, res.StatusCode)
	}
	var got commitmentHistoryPage
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return got
}

func (h *harness) truck(t *testing.T) {
	t.Helper()
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
		Metadata: resource.Metadata{Name: "f150-odometer", Owner: "household"},
		Meter:    &resource.MeterSpec{Description: "F-150 odometer", Unit: "miles"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, c := range []struct{ name, desc, every string }{
		{"f150-oil", "Oil and filter", "5000"},
		{"f150-tyres", "Tyre rotation", "7500"},
	} {
		h.declareCommitment(t, c.name, resource.CommitmentSpec{
			Description: c.desc, Rollover: resource.RolloverFromCompletion,
			Anchor: time.Now().AddDate(-1, 0, 0).Format("2006-01-02"),
			Due:    []resource.CommitmentDue{{Meter: "f150-odometer", Every: c.every, Lead: "300"}},
		}, "household/mika")
	}
}

// The page shows what the Needs-you card deliberately hides.
//
// `commitmentWants` filters the card to things asking for something, so
// a commitment quietly counting down was visible to nobody but an
// Owner in the Console. A truck at 3,000 of 5,000 miles is the normal
// case and it had no home.
func TestTheTrackerPageShowsWhatIsNotAsking(t *testing.T) {
	h := commitmentHarness(t)
	h.truck(t)
	if _, _, err := h.s.DB.AppendMeterReading(runtimedb.MeterReading{
		Meter: "f150-odometer", Value: 100, At: time.Now(),
		RecordedBy: "mika", Source: runtimedb.ReadingByPerson}); err != nil {
		t.Fatalf("AppendMeterReading: %v", err)
	}
	// Nothing is due, so the card is empty.
	if items := h.needsYou(t, "mika"); len(items) != 0 {
		t.Fatalf("the card is showing %d items for a truck nothing is due on", len(items))
	}
	// The page still knows about both.
	page := h.upkeep(t, "mika")
	if len(page.Tracked) != 1 {
		t.Fatalf("tracked = %+v, want the one truck", page.Tracked)
	}
	if got := page.Tracked[0].Meter.Name; got != "f150-odometer" {
		t.Errorf("meter = %q", got)
	}
	if len(page.Tracked[0].Commitments) != 2 {
		t.Errorf("commitments under the truck = %d, want oil and tyres",
			len(page.Tracked[0].Commitments))
	}
	// And the number is entered against the METER, once, not per
	// commitment.
	for _, row := range page.Tracked[0].Commitments {
		if len(row.Needs) != 0 {
			t.Errorf("%s carries its own number box: %+v — the meter's box collects it",
				row.Name, row.Needs)
		}
	}
}

// Grouped by meter, because that is the model: one odometer feeds
// everything measured against it.
func TestCommitmentsAreGroupedUnderTheMeterTheyMeasureAgainst(t *testing.T) {
	h := commitmentHarness(t)
	h.truck(t)
	h.declareCommitment(t, "kai-checkup", resource.CommitmentSpec{
		Description: "Kai's checkup", Rollover: resource.RolloverFromSchedule,
		Anchor:          time.Now().AddDate(-2, 0, 0).Format("2006-01-02"),
		Due:             []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "1yr", Lead: "60d"}},
		RequiresBooking: true,
	}, "household/kai")

	page := h.upkeep(t, "mika")
	if len(page.Tracked) != 1 || len(page.Tracked[0].Commitments) != 2 {
		t.Fatalf("tracked = %+v, want both truck jobs under the odometer", page.Tracked)
	}
	// A clock-only commitment has no counter to group under.
	if len(page.Calendar) != 1 || page.Calendar[0].Name != "kai-checkup" {
		t.Fatalf("calendar = %+v, want the checkup", page.Calendar)
	}
}

// A finished one-off leaves the page it was coming up on. A repeating
// commitment never does — it opens its next cycle instead.
func TestAFinishedOneOffMovesToDoneAndARepeatingOneDoesNot(t *testing.T) {
	h := commitmentHarness(t)
	h.declareCommitment(t, "kai-dentist", resource.CommitmentSpec{
		Description: "The dentist", Rollover: resource.RolloverOnce,
		Anchor: time.Now().AddDate(0, -6, 0).Format("2006-01-02"),
		Due:    []resource.CommitmentDue{{Meter: resource.MeterClock, At: time.Now().AddDate(0, -3, 0).Format("2006-01-02")}},
	}, "household/kai")
	h.declareCommitment(t, "bins", resource.CommitmentSpec{
		Description: "Bins out", Rollover: resource.RolloverFromCompletion,
		Anchor: time.Now().AddDate(0, -1, 0).Format("2006-01-02"),
		Due:    []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "7d", Lead: "1d"}},
	}, "household/mika")

	for _, name := range []string{"kai-dentist", "bins"} {
		if got := h.memberJSON(t, http.MethodPost,
			"/api/v1/home/commitments/"+name+"/done", "mika", nil); got != http.StatusCreated {
			t.Fatalf("closing %s: %d", name, got)
		}
	}
	// A one-off SKIPPED on purpose is finished too — it will not come
	// round again either, and a household that decided not to do
	// something should not keep being shown it.
	h.declareCommitment(t, "kai-passport", resource.CommitmentSpec{
		Description: "Kai's passport", Rollover: resource.RolloverOnce,
		Anchor: time.Now().AddDate(0, -6, 0).Format("2006-01-02"),
		Due:    []resource.CommitmentDue{{Meter: resource.MeterClock, At: time.Now().AddDate(0, -3, 0).Format("2006-01-02")}},
	}, "household/kai")
	if got := h.memberJSON(t, http.MethodPost,
		"/api/v1/home/commitments/kai-passport/waived", "mika",
		map[string]any{"reason": "renewed it ourselves"}); got != http.StatusCreated {
		t.Fatalf("waiving: %d", got)
	}

	page := h.upkeep(t, "mika")
	done := map[string]bool{}
	for _, r := range page.Done {
		done[r.Name] = true
	}
	if !done["kai-dentist"] {
		t.Errorf("a finished one-off is not in Done: %+v", page.Done)
	}
	if !done["kai-passport"] {
		t.Errorf("a one-off skipped on purpose is not in Done: %+v", page.Done)
	}
	if len(page.Done) != 2 {
		t.Errorf("done = %+v, want only the two one-offs", page.Done)
	}
	for _, r := range page.Calendar {
		if r.Name == "bins" {
			return // the repeating one is still coming up, which is right
		}
	}
	t.Error("the repeating commitment left the page when its cycle closed")
}

// Personal stays personal, on the page as on the card — including from
// an Owner, which W033 states three separate times.
func TestThePageHonoursPersonalScope(t *testing.T) {
	h := commitmentHarness(t)
	h.declareCommitment(t, "mika-refill", resource.CommitmentSpec{
		Description: "Repeat prescription", Rollover: resource.RolloverFromCompletion,
		Anchor: time.Now().AddDate(0, -6, 0).Format("2006-01-02"),
		Due:    []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "3mo", Lead: "14d"}},
		Scope:  resource.CommitmentScopePersonal,
	}, "household/mika")

	mine := h.upkeep(t, "mika")
	if len(mine.Calendar) != 1 || !mine.Calendar[0].Personal {
		t.Fatalf("the owner cannot see their own personal commitment: %+v", mine.Calendar)
	}
	theirs := h.upkeep(t, "sarah")
	if len(theirs.Calendar) != 0 {
		t.Errorf("a personal commitment appeared on somebody else's page: %+v", theirs.Calendar)
	}
	// And its history is theirs too.
	res := h.member(http.MethodGet, "/api/v1/home/commitments/mika-refill", "sarah", "homepage_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("history status for a stranger = %d, want 404", res.StatusCode)
	}
}

// The interval actually achieved is the number a schedule cannot tell
// you, and the only derived one in the history.
func TestHistoryShowsTheIntervalActuallyAchieved(t *testing.T) {
	h := commitmentHarness(t)
	h.truck(t)
	for _, at := range []float64{68980, 74320, 79100, 84210} {
		if _, _, err := h.s.DB.RecordCommitmentCompletion(runtimedb.CommitmentCompletion{
			Commitment: "f150-oil", At: time.Now().Add(-time.Duration(90000-at) * time.Hour),
			RecordedBy: "mika", Positions: map[string]float64{"f150-odometer": at},
			Note: "oil and filter",
		}); err != nil {
			t.Fatalf("RecordCommitmentCompletion: %v", err)
		}
	}
	page := h.history(t, "mika", "f150-oil")
	if len(page.Cycles) != 4 {
		t.Fatalf("cycles = %d, want 4", len(page.Cycles))
	}
	// Newest first.
	if page.Cycles[0].Position == nil || *page.Cycles[0].Position != 84210 {
		t.Fatalf("newest = %+v, want 84,210 first", page.Cycles[0])
	}
	if page.Cycles[0].Since != "5110 miles since last" {
		t.Errorf("since = %q, want the interval actually achieved (84210-79100)", page.Cycles[0].Since)
	}
	// The first cycle has nothing to measure from and says nothing.
	if page.Cycles[3].Since != "" {
		t.Errorf("the first cycle claims %q since a cycle that does not exist", page.Cycles[3].Since)
	}
	if page.Cycles[0].Note != "oil and filter" {
		t.Errorf("note = %q, want what was done", page.Cycles[0].Note)
	}
	if page.Row.Cycles != 4 {
		t.Errorf("row cycle count = %d, want 4", page.Row.Cycles)
	}
}

// A guessed number says so, every time it is shown. That is the whole
// reason the column exists.
func TestHistorySaysWhichNumbersWereGuessed(t *testing.T) {
	h := commitmentHarness(t)
	h.truck(t)
	if _, _, err := h.s.DB.RecordCommitmentCompletion(runtimedb.CommitmentCompletion{
		Commitment: "f150-oil", At: time.Now().Add(-24 * time.Hour), RecordedBy: "mika",
		Positions: map[string]float64{"f150-odometer": 84210}, Approximate: []string{"f150-odometer"},
	}); err != nil {
		t.Fatalf("RecordCommitmentCompletion: %v", err)
	}
	page := h.history(t, "mika", "f150-oil")
	if len(page.Cycles) != 1 || !page.Cycles[0].Approximate {
		t.Errorf("cycle = %+v, want the estimate marked", page.Cycles)
	}
}

// A commitment whose meter nobody declared cannot be grouped, and must
// not vanish: the household still owes the thing.
func TestACommitmentOnAnUndeclaredMeterStillAppears(t *testing.T) {
	h := commitmentHarness(t)
	h.declareCommitment(t, "orphan", resource.CommitmentSpec{
		Description: "Measured against nothing", Rollover: resource.RolloverFromCompletion,
		Anchor: time.Now().AddDate(0, -6, 0).Format("2006-01-02"),
		Due:    []resource.CommitmentDue{{Meter: "no-such-meter", Every: "100", Lead: "10"}},
	}, "household/mika")
	page := h.upkeep(t, "mika")
	for _, r := range page.Calendar {
		if r.Name == "orphan" {
			return
		}
	}
	t.Errorf("a commitment on an undeclared meter vanished: tracked=%+v calendar=%+v",
		page.Tracked, page.Calendar)
}

// Not being able to tell must never look like an all-clear.
func TestTheTrackerPageSaysWhenItCannotTell(t *testing.T) {
	h := commitmentHarness(t)
	h.s.Upkeep = nil
	page := h.upkeep(t, "mika")
	if page.Available {
		t.Fatal("the page claimed to be available with no engine behind it")
	}
	if page.Problem == "" {
		t.Error("an unavailable page says nothing")
	}
}
