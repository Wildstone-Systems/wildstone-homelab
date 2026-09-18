package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/upkeep"
)

// commitmentHarness is the household page with the commitment engine
// wired to it, the way main.go does.
func commitmentHarness(t *testing.T) *harness {
	t.Helper()
	h := homeHarness(t)
	h.s.Upkeep = &upkeep.Engine{DB: h.s.DB, State: h.s.State, Loc: time.UTC}
	return h
}

func (h *harness) declareCommitment(t *testing.T, name string, spec resource.CommitmentSpec, owner string) {
	t.Helper()
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindCommitment,
		Metadata:   resource.Metadata{Name: name, Owner: owner},
		Commitment: &spec,
	}); err != nil {
		t.Fatalf("Put(%s): %v", name, err)
	}
}

// overdueCheckup is Kai's, and the household's adults answer it.
func overdueCheckup(t *testing.T, h *harness, mayComplete []string, scope string) {
	t.Helper()
	h.declareCommitment(t, "kai-checkup", resource.CommitmentSpec{
		Description:     "Kai's annual well-child visit",
		Rollover:        resource.RolloverFromSchedule,
		Anchor:          time.Now().AddDate(-2, 0, 0).Format("2006-01-02"),
		Due:             []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "1yr", Lead: "60d"}},
		RequiresBooking: true,
		MayComplete:     mayComplete,
		Scope:           scope,
	}, "household/kai")
}

func (h *harness) needsYou(t *testing.T, user string) []needsYouItem {
	t.Helper()
	return h.summary(t, user).NeedsYou.Items
}

func commitmentItem(items []needsYouItem, ref string) (needsYouItem, bool) {
	for _, it := range items {
		if it.Kind == needsYouCommitment && it.Ref == ref {
			return it, true
		}
	}
	return needsYouItem{}, false
}

// The case the whole design exists for: the item is ABOUT a child and
// ANSWERED by a parent, and the page must render it that way to both.
func TestTheCheckupIsAboutTheChildAndAnsweredByAParent(t *testing.T) {
	h := commitmentHarness(t)
	overdueCheckup(t, h, []string{"household/mika"}, "")

	parent, ok := commitmentItem(h.needsYou(t, "mika"), "kai-checkup")
	if !ok {
		t.Fatal("the checkup is missing from the parent's card")
	}
	if !parent.Decidable {
		t.Errorf("the parent cannot act on it: %+v", parent)
	}
	if parent.WaitingOn != "you" {
		t.Errorf("waitingOn = %q, want you", parent.WaitingOn)
	}

	child, ok := commitmentItem(h.needsYou(t, "kai"), "kai-checkup")
	if !ok {
		t.Fatal("the checkup is missing from the child's card")
	}
	if child.Decidable {
		t.Errorf("the child was offered an affordance they do not have: %+v", child)
	}
	if child.WaitingOn != "mika" && child.WaitingOn != "kai" {
		t.Errorf("waitingOn = %q, want the person who must act", child.WaitingOn)
	}
}

// A personal commitment is the owner's business. Same boundary the
// memory scopes draw, not a second vocabulary for the same question.
func TestAPersonalCommitmentIsNotOnSomebodyElsesPage(t *testing.T) {
	h := commitmentHarness(t)
	h.declareCommitment(t, "mika-refill", resource.CommitmentSpec{
		Description: "Repeat prescription", Rollover: resource.RolloverFromCompletion,
		Anchor: time.Now().AddDate(0, -6, 0).Format("2006-01-02"),
		Due:    []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "3mo", Lead: "14d"}},
		Scope:  resource.CommitmentScopePersonal,
	}, "household/mika")

	if _, ok := commitmentItem(h.needsYou(t, "mika"), "mika-refill"); !ok {
		t.Error("the owner cannot see their own personal commitment")
	}
	if _, ok := commitmentItem(h.needsYou(t, "sarah"), "mika-refill"); ok {
		t.Error("a personal commitment appeared on somebody else's page")
	}
}

// A commitment quietly counting down is not waiting on anybody.
func TestOnlyCommitmentsThatWantSomethingAppear(t *testing.T) {
	h := commitmentHarness(t)
	h.declareCommitment(t, "furnace-filter", resource.CommitmentSpec{
		Description: "Furnace filter", Rollover: resource.RolloverFromCompletion,
		Anchor: time.Now().Format("2006-01-02"),
		Due:    []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "3mo", Lead: "14d"}},
	}, "household")
	if _, ok := commitmentItem(h.needsYou(t, "mika"), "furnace-filter"); ok {
		t.Error("a commitment three months from due is filling the card")
	}
}

// Booking is an answer, and answering makes the item go away.
func TestBookingAnswersTheItem(t *testing.T) {
	h := commitmentHarness(t)
	overdueCheckup(t, h, []string{"household/mika"}, "")
	if _, ok := commitmentItem(h.needsYou(t, "mika"), "kai-checkup"); !ok {
		t.Fatal("the checkup is not on the card to begin with")
	}

	when := time.Now().AddDate(0, 0, 14)
	resp := h.memberJSON(t, http.MethodPost, "/api/v1/home/commitments/kai-checkup/booked", "mika",
		map[string]any{"at": when})
	if resp != http.StatusCreated {
		t.Fatalf("booking: status %d", resp)
	}
	if _, ok := commitmentItem(h.needsYou(t, "mika"), "kai-checkup"); ok {
		t.Error("the item is still asking after somebody booked it")
	}
}

func TestDoneAndWaivedBothCloseTheCycle(t *testing.T) {
	for _, what := range []string{"done", "waived"} {
		t.Run(what, func(t *testing.T) {
			h := commitmentHarness(t)
			overdueCheckup(t, h, []string{"household/mika"}, "")
			if got := h.memberJSON(t, http.MethodPost,
				"/api/v1/home/commitments/kai-checkup/"+what, "mika",
				map[string]any{"reason": "seen to"}); got != http.StatusCreated {
				t.Fatalf("status %d", got)
			}
			done, err := h.s.DB.ListCommitmentCompletions("kai-checkup")
			if err != nil || len(done) != 1 {
				t.Fatalf("completions = %+v, %v", done, err)
			}
			if done[0].Waived != (what == "waived") {
				t.Errorf("waived = %v for %q", done[0].Waived, what)
			}
			if done[0].RecordedBy != "mika" {
				t.Errorf("recordedBy = %q, want the authenticated member", done[0].RecordedBy)
			}
		})
	}
}

// The affordance is absent for somebody who may not answer; going round
// the page is still refused.
func TestSomebodyWhoMayNotAnswerIsRefusedAtTheWritePath(t *testing.T) {
	h := commitmentHarness(t)
	overdueCheckup(t, h, []string{"household/mika"}, "")
	if got := h.memberJSON(t, http.MethodPost, "/api/v1/home/commitments/kai-checkup/done", "kai", nil); got != http.StatusForbidden {
		t.Errorf("status = %d, want 403", got)
	}
}

// A member can answer the other kind of question too: what does it say.
func TestAMemberCanRecordAMeterReadingFromTheirOwnPage(t *testing.T) {
	h := commitmentHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
		Metadata: resource.Metadata{Name: "f150-odometer", Owner: "household"},
		Meter:    &resource.MeterSpec{Description: "Odometer", Unit: "miles"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := h.memberJSON(t, http.MethodPost, "/api/v1/home/meters/f150-odometer/readings", "mika",
		map[string]any{"value": 84210}); got != http.StatusCreated {
		t.Fatalf("status %d", got)
	}
	latest, err := h.s.DB.LatestMeterReading("f150-odometer")
	if err != nil {
		t.Fatalf("LatestMeterReading: %v", err)
	}
	if latest.Value != 84210 || latest.RecordedBy != "mika" || latest.Source != runtimedb.ReadingByPerson {
		t.Errorf("reading = %+v", latest)
	}
	if got := h.memberJSON(t, http.MethodPost, "/api/v1/home/meters/nope/readings", "mika",
		map[string]any{"value": 1}); got != http.StatusNotFound {
		t.Errorf("an undeclared meter gave %d, want 404", got)
	}
}

// An overdue commitment is red and a due-soon one is amber, because
// colour is never the only signal but it is still a signal.
func TestToneSeparatesOverdueFromComingDue(t *testing.T) {
	h := commitmentHarness(t)
	overdueCheckup(t, h, nil, "")
	item, ok := commitmentItem(h.needsYou(t, "mika"), "kai-checkup")
	if !ok {
		t.Fatal("missing")
	}
	if item.Tone != portal.ToneFailed {
		t.Errorf("tone = %q, want the overdue one", item.Tone)
	}
	if !strings.Contains(item.Consequence, "past its date") {
		t.Errorf("consequence = %q", item.Consequence)
	}
}

// memberJSON posts a JSON body as the identity proxy would for a member
// and returns the status.
func (h *harness) memberJSON(t *testing.T, method, path, user string, body any) int {
	t.Helper()
	var r io.Reader = strings.NewReader("")
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		r = bytes.NewReader(encoded)
	}
	resp := h.member(method, path, user, "homepage_users", r, "application/json")
	defer resp.Body.Close()
	return resp.StatusCode
}
