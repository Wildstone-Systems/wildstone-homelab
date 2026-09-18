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

// memberJSONAs is memberJSON for a member in extra directory groups.
func (h *harness) memberJSONAs(t *testing.T, method, path, user, groups string, body any) int {
	t.Helper()
	var r io.Reader = strings.NewReader("")
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		r = bytes.NewReader(encoded)
	}
	resp := h.member(method, path, user, groups, r, "application/json")
	defer resp.Body.Close()
	return resp.StatusCode
}

// summaryAs reads the page for a member the proxy put in extra groups.
func (h *harness) summaryAs(t *testing.T, user, groups string) homeSummaryResponse {
	t.Helper()
	res := h.member(http.MethodGet, "/api/v1/home/summary", user, groups, nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("summary for %s: status %d", user, res.StatusCode)
	}
	var got homeSummaryResponse
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decoding summary: %v", err)
	}
	return got
}

// `mayComplete: [group/parents]` is a rule, not a comment (Codex, #244).
//
// This used to pass for everybody: a group reference was treated as
// permissive on the reasoning that the write path would refuse anyone
// who was not in it, and the write path calls the same function. So a
// commitment narrowed to the adults could be ticked off by anybody who
// could reach the page, and the narrowing was decorative.
func TestAGroupInMayCompleteIsAnsweredRatherThanAssumed(t *testing.T) {
	h := commitmentHarness(t)
	overdueCheckup(t, h, []string{"group/parents"}, "")

	inGroup, ok := commitmentItem(h.summaryAs(t, "mika", "homepage_users,parents").NeedsYou.Items, "kai-checkup")
	if !ok {
		t.Fatal("the checkup is missing from a parent's card")
	}
	if !inGroup.Decidable {
		t.Errorf("somebody in group/parents was not offered the affordance: %+v", inGroup)
	}
	outOfGroup, ok := commitmentItem(h.needsYou(t, "kai"), "kai-checkup")
	if !ok {
		t.Fatal("the checkup should still be visible to the child it is about")
	}
	if outOfGroup.Decidable {
		t.Errorf("somebody outside group/parents was offered the affordance: %+v", outOfGroup)
	}

	// Read both cards before writing: answering it closes the cycle, and
	// a closed commitment is asking nobody for anything.
	if got := h.memberJSONAs(t, http.MethodPost,
		"/api/v1/home/commitments/kai-checkup/done", "mika", "homepage_users,parents", nil); got != http.StatusCreated {
		t.Errorf("a parent was refused: status %d, want 201", got)
	}
}

// And the write path refuses them, which is what the comment claimed
// all along.
func TestSomebodyOutsideTheGroupIsRefusedAtTheWritePath(t *testing.T) {
	h := commitmentHarness(t)
	overdueCheckup(t, h, []string{"group/parents"}, "")
	if got := h.memberJSONAs(t, http.MethodPost,
		"/api/v1/home/commitments/kai-checkup/done", "kai", "homepage_users", nil); got != http.StatusForbidden {
		t.Errorf("status = %d, want 403", got)
	}
	// A group the household never created narrows the commitment to
	// nobody. That is the safe direction and it is deliberate: finding
	// out because nobody can tick it off beats finding out because
	// everybody could.
	h.declareCommitment(t, "nobody", resource.CommitmentSpec{
		Description: "narrowed to a group that does not exist",
		Rollover:    resource.RolloverFromCompletion,
		Anchor:      time.Now().AddDate(-1, 0, 0).Format("2006-01-02"),
		Due:         []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "3mo", Lead: "14d"}},
		MayComplete: []string{"group/nonexistent"},
	}, "household/mika")
	if got := h.memberJSONAs(t, http.MethodPost,
		"/api/v1/home/commitments/nobody/done", "mika", "homepage_users", nil); got != http.StatusForbidden {
		t.Errorf("status = %d, want 403", got)
	}
}

// A counter-based commitment answered with no number wedges itself
// forever (Codex, #244).
//
// The completion closes the cycle and records `{}` for positions. Both
// rollover modes need the position the meter stood at to measure the
// next cycle from, so the new cycle opens permanently unresolved — and
// there is no endpoint that deletes or corrects a completion, so the
// household cannot get it back. The schema has said "evidence is not
// optional" since it was written; this is the code agreeing.
func TestACounterCommitmentCannotBeClosedWithoutItsNumber(t *testing.T) {
	h := commitmentHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
		Metadata: resource.Metadata{Name: "f150-odometer", Owner: "household"},
		Meter:    &resource.MeterSpec{Description: "Odometer", Unit: "miles"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h.declareCommitment(t, "f150-oil", resource.CommitmentSpec{
		Description: "F-150 oil and filter", Rollover: resource.RolloverFromCompletion,
		Anchor: time.Now().AddDate(-1, 0, 0).Format("2006-01-02"),
		Due:    []resource.CommitmentDue{{Meter: "f150-odometer", Every: "5000", Lead: "300"}},
	}, "household/mika")

	for _, what := range []string{"done", "waived"} {
		t.Run(what+" with no number", func(t *testing.T) {
			got := h.memberJSON(t, http.MethodPost, "/api/v1/home/commitments/f150-oil/"+what, "mika", nil)
			if got != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: closing a counter cycle with no reading "+
					"leaves the next one permanently unresolved and nothing can undo it", got)
			}
		})
	}

	// With the number, it closes — and the number is what the next cycle
	// is measured from.
	if got := h.memberJSON(t, http.MethodPost, "/api/v1/home/commitments/f150-oil/done", "mika",
		map[string]any{"meters": map[string]any{"f150-odometer": 84210}}); got != http.StatusCreated {
		t.Fatalf("status = %d, want 201", got)
	}
	done, err := h.s.DB.ListCommitmentCompletions("f150-oil")
	if err != nil {
		t.Fatalf("ListCommitmentCompletions: %v", err)
	}
	if len(done) != 1 || done[0].Positions["f150-odometer"] != 84210 {
		t.Errorf("completion = %+v, want the odometer recorded", done)
	}
}

// A member who has just read the meter should not have to type the
// number twice. A reading recorded at or before the completion is the
// evidence, and the intake snapshots it.
func TestARecentReadingIsEvidenceEnoughToCloseACounterCycle(t *testing.T) {
	h := commitmentHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
		Metadata: resource.Metadata{Name: "f150-odometer", Owner: "household"},
		Meter:    &resource.MeterSpec{Description: "Odometer", Unit: "miles", StaleAfter: "720h"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h.declareCommitment(t, "f150-oil", resource.CommitmentSpec{
		Description: "F-150 oil and filter", Rollover: resource.RolloverFromCompletion,
		Anchor: time.Now().AddDate(-1, 0, 0).Format("2006-01-02"),
		Due:    []resource.CommitmentDue{{Meter: "f150-odometer", Every: "5000", Lead: "300"}},
	}, "household/mika")

	if got := h.memberJSON(t, http.MethodPost, "/api/v1/home/meters/f150-odometer/readings", "mika",
		map[string]any{"value": 84210}); got != http.StatusCreated {
		t.Fatalf("recording the reading: status %d", got)
	}
	if got := h.memberJSON(t, http.MethodPost, "/api/v1/home/commitments/f150-oil/done", "mika", nil); got != http.StatusCreated {
		t.Fatalf("status = %d, want 201: a reading the household just took is the evidence", got)
	}
	done, err := h.s.DB.ListCommitmentCompletions("f150-oil")
	if err != nil {
		t.Fatalf("ListCommitmentCompletions: %v", err)
	}
	if len(done) != 1 || done[0].Positions["f150-odometer"] != 84210 {
		t.Errorf("completion = %+v, want the reading snapshotted", done)
	}
}

// A reading too old to rely on still closes the cycle — and says so.
//
// The stricter first version refused it, on the reasoning that measuring
// the next cycle from a June odometer produces a number nobody checked.
// That was stricter than the household needs: the point is that the oil
// gets changed at about five thousand miles, and a commitment that
// cannot be closed sits overdue forever with the work already done,
// which is the failure the ADR says a schema cannot fix.
//
// The error runs the safe way. A stale reading is BEHIND where the meter
// actually stood, so the next cycle comes due EARLY. Nagging sooner is a
// nuisance; nagging never is a missed oil change.
//
// What is not negotiable is that the record says which numbers were
// guessed.
func TestAStaleReadingClosesTheCycleAndIsRecordedAsApproximate(t *testing.T) {
	h := commitmentHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
		Metadata: resource.Metadata{Name: "f150-odometer", Owner: "household"},
		Meter:    &resource.MeterSpec{Description: "Odometer", Unit: "miles", StaleAfter: "168h"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h.declareCommitment(t, "f150-oil", resource.CommitmentSpec{
		Description: "F-150 oil and filter", Rollover: resource.RolloverFromCompletion,
		Anchor: time.Now().AddDate(-1, 0, 0).Format("2006-01-02"),
		Due:    []resource.CommitmentDue{{Meter: "f150-odometer", Every: "5000", Lead: "300"}},
	}, "household/mika")
	if _, _, err := h.s.DB.AppendMeterReading(runtimedb.MeterReading{
		Meter: "f150-odometer", Value: 84210, At: time.Now().AddDate(0, -3, 0),
		RecordedBy: "mika", Source: runtimedb.ReadingByPerson}); err != nil {
		t.Fatalf("AppendMeterReading: %v", err)
	}
	if got := h.memberJSON(t, http.MethodPost, "/api/v1/home/commitments/f150-oil/done", "mika", nil); got != http.StatusCreated {
		t.Fatalf("status = %d, want 201: a rough number is not no number", got)
	}
	done := h.completions(t, "f150-oil")
	if len(done) != 1 {
		t.Fatalf("completions = %d, want 1", len(done))
	}
	if done[0].Positions["f150-odometer"] != 84210 {
		t.Errorf("position = %v, want the reading it had", done[0].Positions["f150-odometer"])
	}
	if len(done[0].Approximate) != 1 || done[0].Approximate[0] != "f150-odometer" {
		t.Errorf("approximate = %v, want the odometer named: a maintenance history that hides "+
			"which mileages were guessed is the quiet kind of lie", done[0].Approximate)
	}
}

// A number nobody has EVER recorded is still refused, and is still the
// only thing that is. There is nothing to be approximately right with.
func TestAMeterNobodyHasEverReadStillRefusesToClose(t *testing.T) {
	h := commitmentHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
		Metadata: resource.Metadata{Name: "f150-odometer", Owner: "household"},
		Meter:    &resource.MeterSpec{Description: "Odometer", Unit: "miles"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h.declareCommitment(t, "f150-oil", resource.CommitmentSpec{
		Description: "F-150 oil and filter", Rollover: resource.RolloverFromCompletion,
		Anchor: time.Now().AddDate(-1, 0, 0).Format("2006-01-02"),
		Due:    []resource.CommitmentDue{{Meter: "f150-odometer", Every: "5000", Lead: "300"}},
	}, "household/mika")
	if got := h.memberJSON(t, http.MethodPost, "/api/v1/home/commitments/f150-oil/done", "mika", nil); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
	// Typed, it closes — and is not marked approximate, because somebody
	// was standing at the truck.
	if got := h.memberJSON(t, http.MethodPost, "/api/v1/home/commitments/f150-oil/done", "mika",
		map[string]any{"meters": map[string]any{"f150-odometer": 90000}}); got != http.StatusCreated {
		t.Fatalf("status = %d, want 201", got)
	}
	done := h.completions(t, "f150-oil")
	if len(done) != 1 || len(done[0].Approximate) != 0 {
		t.Errorf("completion = %+v, want one with nothing guessed", done)
	}
}

func (h *harness) completions(t *testing.T, name string) []runtimedb.CommitmentCompletion {
	t.Helper()
	done, err := h.s.DB.ListCommitmentCompletions(name)
	if err != nil {
		t.Fatalf("ListCommitmentCompletions: %v", err)
	}
	return done
}

// A one-off measures nothing from its completion, because nothing
// follows it. Requiring evidence there would refuse an answer for the
// sake of a cycle that never opens.
func TestAOneOffCounterCommitmentNeedsNoNumberToClose(t *testing.T) {
	h := commitmentHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
		Metadata: resource.Metadata{Name: "f150-odometer", Owner: "household"},
		Meter:    &resource.MeterSpec{Description: "Odometer", Unit: "miles"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h.declareCommitment(t, "f150-warranty", resource.CommitmentSpec{
		Description: "Warranty inspection, once", Rollover: resource.RolloverOnce,
		Anchor: time.Now().AddDate(-1, 0, 0).Format("2006-01-02"),
		Due:    []resource.CommitmentDue{{Meter: "f150-odometer", AtValue: 36000}},
	}, "household/mika")
	if got := h.memberJSON(t, http.MethodPost, "/api/v1/home/commitments/f150-warranty/done", "mika", nil); got != http.StatusCreated {
		t.Errorf("status = %d, want 201: a one-off opens no next cycle to measure", got)
	}
}

// The server lists what a commitment can be answered with, so the page
// never works it out for itself (Codex, #244).
func TestTheCardIsToldWhichAnswersACommitmentTakes(t *testing.T) {
	h := commitmentHarness(t)
	overdueCheckup(t, h, []string{"household/mika"}, "")

	parent, ok := commitmentItem(h.needsYou(t, "mika"), "kai-checkup")
	if !ok {
		t.Fatal("the checkup is missing")
	}
	verbs := map[string]bool{}
	for _, a := range parent.Asks {
		verbs[a.Verb] = true
		if a.Label == "" {
			t.Errorf("ask %q has no label: the page has no sentence of its own to fall back on", a.Verb)
		}
	}
	for _, want := range []string{"done", "waived", "booked"} {
		if !verbs[want] {
			t.Errorf("no %q ask: the checkup requires booking and is answerable", want)
		}
	}

	// Somebody who may not answer is offered nothing. The affordance is
	// absent, never present-and-refused.
	child, ok := commitmentItem(h.needsYou(t, "kai"), "kai-checkup")
	if !ok {
		t.Fatal("the checkup is missing from the child's card")
	}
	if len(child.Asks) != 0 {
		t.Errorf("asks = %+v for somebody who may not answer", child.Asks)
	}
}

// A commitment nobody can conclude asks for the NUMBER and nothing
// else. "Done" on a card that cannot say whether the thing is due is an
// answer to a question nobody asked.
func TestAnUnresolvedCommitmentAsksForTheNumberFirst(t *testing.T) {
	h := commitmentHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
		Metadata: resource.Metadata{Name: "f150-odometer", Owner: "household"},
		Meter:    &resource.MeterSpec{Description: "Odometer", Unit: "miles"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h.declareCommitment(t, "f150-oil", resource.CommitmentSpec{
		Description: "F-150 oil and filter", Rollover: resource.RolloverFromCompletion,
		Anchor: time.Now().AddDate(-1, 0, 0).Format("2006-01-02"),
		Due:    []resource.CommitmentDue{{Meter: "f150-odometer", Every: "5000", Lead: "300"}},
	}, "household/mika")

	item, ok := commitmentItem(h.needsYou(t, "mika"), "f150-oil")
	if !ok {
		t.Fatal("a commitment nobody can conclude is not asking for anything")
	}
	// The reading comes first, and a way to close the cycle is still
	// offered: a card that asks and cannot be answered is the fault
	// this pair of tests exists for.
	if len(item.Asks) < 2 {
		t.Fatalf("asks = %+v, want the reading and a way to close it", item.Asks)
	}
	if item.Asks[0].Verb != "reading" {
		t.Errorf("ask = %+v, want the reading offered first", item.Asks[0])
	}
	if len(item.Needs) != 1 || item.Needs[0].Meter != "f150-odometer" {
		t.Fatalf("needs = %+v, want the odometer", item.Needs)
	}
	if item.Needs[0].Unit != "miles" {
		t.Errorf("unit = %q, want the declared unit so the box says what to type", item.Needs[0].Unit)
	}
}

// The card must not offer an answer the write path will refuse.
//
// Two ways it did. Both are the same fault as the group check: two
// places deciding one question and disagreeing.
//
// (1) A counter rule that is stale AND already crossed is `overdue`
// with a trigger, not `unresolved` — so the card took the ordinary
// branch and offered Done, while `Evidence` refused it for being
// stale. Every tap returned 400.
func TestAStaleButOverdueCommitmentAsksForTheNumberItWillDemand(t *testing.T) {
	h := commitmentHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
		Metadata: resource.Metadata{Name: "f150-odometer", Owner: "household"},
		Meter:    &resource.MeterSpec{Description: "Odometer", Unit: "miles", StaleAfter: "168h"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h.declareCommitment(t, "f150-oil", resource.CommitmentSpec{
		Description: "F-150 oil and filter", Rollover: resource.RolloverFromCompletion,
		Anchor: time.Now().AddDate(-1, 0, 0).Format("2006-01-02"),
		Due:    []resource.CommitmentDue{{Meter: "f150-odometer", Every: "5000", Lead: "300"}},
	}, "household/mika")
	// Read long ago, and already well past the threshold: stale, but
	// crossed, so the engine can still say it is overdue.
	if _, _, err := h.s.DB.AppendMeterReading(runtimedb.MeterReading{
		Meter: "f150-odometer", Value: 90000, At: time.Now().AddDate(0, -3, 0),
		RecordedBy: "mika", Source: runtimedb.ReadingByPerson}); err != nil {
		t.Fatalf("AppendMeterReading: %v", err)
	}

	item, ok := commitmentItem(h.needsYou(t, "mika"), "f150-oil")
	if !ok {
		t.Fatal("an overdue oil change is not on the card")
	}
	assertAsksAreAnswerable(t, h, "f150-oil", item)
}

// (2) When the reason a rule cannot be concluded is that the LAST
// COMPLETION recorded no reading, a fresh meter reading does not fix
// it — only a new completion carrying the number does. The card
// offered the reading box alone, so the household had no way out at
// all.
func TestACommitmentWedgedByAnOldCompletionCanStillBeAnswered(t *testing.T) {
	h := commitmentHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
		Metadata: resource.Metadata{Name: "f150-odometer", Owner: "household"},
		Meter:    &resource.MeterSpec{Description: "Odometer", Unit: "miles"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h.declareCommitment(t, "f150-oil", resource.CommitmentSpec{
		Description: "F-150 oil and filter", Rollover: resource.RolloverFromCompletion,
		Anchor: time.Now().AddDate(-1, 0, 0).Format("2006-01-02"),
		Due:    []resource.CommitmentDue{{Meter: "f150-odometer", Every: "5000", Lead: "300"}},
	}, "household/mika")
	// The shape every pre-0042 completion has: closed, with no number.
	if _, _, err := h.s.DB.RecordCommitmentCompletion(runtimedb.CommitmentCompletion{
		Commitment: "f150-oil", At: time.Now().AddDate(0, -6, 0), RecordedBy: "mika",
	}); err != nil {
		t.Fatalf("RecordCommitmentCompletion: %v", err)
	}
	if _, _, err := h.s.DB.AppendMeterReading(runtimedb.MeterReading{
		Meter: "f150-odometer", Value: 84210, At: time.Now(),
		RecordedBy: "mika", Source: runtimedb.ReadingByPerson}); err != nil {
		t.Fatalf("AppendMeterReading: %v", err)
	}

	item, ok := commitmentItem(h.needsYou(t, "mika"), "f150-oil")
	if !ok {
		t.Fatal("a wedged commitment is not asking for anything")
	}
	assertAsksAreAnswerable(t, h, "f150-oil", item)
}

// assertAsksAreAnswerable is the property both cases are about: every
// answer the card offers must be one the write path accepts, and the
// card must offer at least one way forward.
func assertAsksAreAnswerable(t *testing.T, h *harness, name string, item needsYouItem) {
	t.Helper()
	if len(item.Asks) == 0 {
		t.Fatal("the card asks for something and offers no way to answer it")
	}
	// Whatever the card asks the household to type, typed once.
	typed := map[string]any{}
	for _, n := range item.Needs {
		typed[n.Meter] = 99999.0
	}
	var closable bool
	for _, a := range item.Asks {
		switch a.Verb {
		case "reading":
			for _, n := range item.Needs {
				if got := h.memberJSON(t, http.MethodPost,
					"/api/v1/home/meters/"+n.Meter+"/readings", "mika",
					map[string]any{"value": 99999}); got != http.StatusCreated {
					t.Errorf("the card offered %q for %s and the write path gave %d", a.Label, n.Meter, got)
				}
			}
		case "done", "waived":
			closable = true
			body := map[string]any{}
			if len(typed) > 0 {
				body["meters"] = typed
			}
			if got := h.memberJSON(t, http.MethodPost,
				"/api/v1/home/commitments/"+name+"/"+a.Verb, "mika", body); got != http.StatusCreated {
				t.Errorf("the card offered %q and the write path gave %d — "+
					"an affordance the engine refuses is worse than none", a.Label, got)
			}
		}
	}
	if !closable {
		t.Error("the card offers no way to close the cycle, so the household cannot get unstuck")
	}
}
