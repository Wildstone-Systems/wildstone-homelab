package api

// Memory: what a member may read, and what they may forget.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func TestDashboardPromotionRequiresTheOwningPerson(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy", "maya")
	seq := remember(t, h, "sandy", "Sandy likes jasmine", runtimedb.ButlerAudiencePrivate)
	path := "/api/v1/assistant/memory/promote?seq=" + strconv.FormatInt(seq, 10)
	denied := h.member(http.MethodPost, path, "maya", "assistant_users", nil, "")
	denied.Body.Close()
	if denied.StatusCode != http.StatusNotFound {
		t.Fatalf("another Person promoted memory: %d", denied.StatusCode)
	}
	approved := h.member(http.MethodPost, path, "sandy", "assistant_users", nil, "")
	defer approved.Body.Close()
	if approved.StatusCode != http.StatusOK {
		t.Fatalf("owning Person promotion: %d", approved.StatusCode)
	}
}

// remember records one memory and returns its sequence number.
func remember(t *testing.T, h *harness, owner, content string, audience runtimedb.ButlerTaskAudience) int64 {
	t.Helper()
	seq, err := h.s.DB.RecordMemory(runtimedb.MemoryRecord{
		Kind: "fact", Content: content, Owner: owner, Audience: audience,
		PermissionLabel: "household", SourceKind: "butler_session", SourceID: "bs-1",
		Confidence: "stated", Author: owner,
	})
	if err != nil {
		t.Fatalf("RecordMemory(%q): %v", content, err)
	}
	return seq
}

// A member reads their own and the household's, and nobody else's.
func TestMemoryShowsYoursAndTheHouseholdsOnly(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy", "maya")
	mine := remember(t, h, "sandy", "Sandy does not eat fish", runtimedb.ButlerAudiencePrivate)
	theirs := remember(t, h, "maya", "Maya is allergic to bees", runtimedb.ButlerAudiencePrivate)
	shared := remember(t, h, "maya", "The bins go out on Tuesday", runtimedb.ButlerAudienceShared)

	res := h.member(http.MethodGet, "/api/v1/assistant/memory", "sandy", "assistant_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	var got struct {
		Items []memoryItem `json:"items"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}

	seen := map[int64]memoryItem{}
	for _, i := range got.Items {
		seen[i.Seq] = i
	}
	if _, ok := seen[mine]; !ok {
		t.Error("sandy cannot see what the assistant learned about her")
	}
	if _, ok := seen[theirs]; ok {
		t.Error("sandy can read what the assistant learned about maya")
	}
	if _, ok := seen[shared]; !ok {
		t.Error("the household's shared memory is hidden from a member of it")
	}
	// Only her own is hers to forget; the shared row is labelled, not
	// actionable.
	if !seen[mine].Mine {
		t.Error("her own record is not marked as hers")
	}
	if seen[shared].Mine {
		t.Error("household memory is offered to a member as theirs to forget")
	}
}

// A member forgets their own, and cannot reach anybody else's by
// naming its sequence number.
func TestForgettingReachesOnlyYourOwn(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy", "maya")
	mine := remember(t, h, "sandy", "Sandy does not eat fish", runtimedb.ButlerAudiencePrivate)
	theirs := remember(t, h, "maya", "Maya is allergic to bees", runtimedb.ButlerAudiencePrivate)
	shared := remember(t, h, "maya", "The bins go out on Tuesday", runtimedb.ButlerAudienceShared)

	forget := func(seq int64, as string) int {
		res := h.member(http.MethodPost,
			"/api/v1/assistant/memory/forget?seq="+strconv.FormatInt(seq, 10),
			as, "assistant_users", nil, "")
		defer res.Body.Close()
		return res.StatusCode
	}

	// Somebody else's, and the household's, both read as not found —
	// "that is not yours" would confirm the record exists and whose.
	if code := forget(theirs, "sandy"); code != http.StatusNotFound {
		t.Errorf("forgetting maya's record: status %d, want 404", code)
	}
	if code := forget(shared, "sandy"); code != http.StatusNotFound {
		t.Errorf("forgetting household memory: status %d, want 404", code)
	}
	// And neither was touched.
	for _, seq := range []int64{theirs, shared} {
		m, err := h.s.DB.GetMemory(seq)
		if err != nil {
			t.Fatal(err)
		}
		if !m.ForgottenAt.IsZero() {
			t.Errorf("record %d was forgotten by a refused request", seq)
		}
	}

	// Her own goes.
	if code := forget(mine, "sandy"); code != http.StatusOK {
		t.Fatalf("forgetting her own: status %d", code)
	}
	m, err := h.s.DB.GetMemory(mine)
	if err != nil {
		t.Fatal(err)
	}
	if m.ForgottenAt.IsZero() {
		t.Error("her own record was not forgotten")
	}
}

// A machine source name never reaches the page.
func TestMemorySourcesAreHouseholdLanguage(t *testing.T) {
	for _, kind := range []string{"butler_session", "household_event", "routine", "correction"} {
		got := memorySource(runtimedb.MemoryRecord{SourceKind: kind})
		if got == "" {
			t.Errorf("%s has no household sentence", kind)
		}
		if got == kind {
			t.Errorf("%s renders as itself", kind)
		}
	}
	// An unmapped kind yields no attribution rather than machine text —
	// the same rule the activity stream follows.
	if got := memorySource(runtimedb.MemoryRecord{SourceKind: "reconcile_tick"}); got != "" {
		t.Errorf("an unmapped source rendered as %q", got)
	}
}

// What the member has already forgotten is not what the assistant
// remembers.
//
// The panel reads through ExportMemoryFor, which is an EXPORT and
// deliberately hands a person their own forgotten records back (A3).
// That is right for an export and wrong for this heading: a member who
// presses forget, sees the item still listed on the next refresh, and
// is offered the button again has been told their forget did nothing.
func TestForgottenAndCorrectedMemoryLeavesThePanel(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	kept := remember(t, h, "sandy", "Sandy walks the dog at six", runtimedb.ButlerAudiencePrivate)
	forgotten := remember(t, h, "sandy", "Sandy does not eat fish", runtimedb.ButlerAudiencePrivate)
	corrected := remember(t, h, "sandy", "Sandy drives a blue car", runtimedb.ButlerAudiencePrivate)

	panel := func() map[int64]memoryItem {
		t.Helper()
		h.unthrottled()
		res := h.member(http.MethodGet, "/api/v1/assistant/memory", "sandy", "assistant_users", nil, "")
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("memory: status %d", res.StatusCode)
		}
		var got struct {
			Items []memoryItem `json:"items"`
		}
		if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		seen := map[int64]memoryItem{}
		for _, i := range got.Items {
			seen[i.Seq] = i
		}
		return seen
	}

	before := panel()
	for _, seq := range []int64{kept, forgotten, corrected} {
		if _, ok := before[seq]; !ok {
			t.Fatalf("record %d is missing before anything was forgotten", seq)
		}
	}

	h.unthrottled()
	res := h.member(http.MethodPost,
		"/api/v1/assistant/memory/forget?seq="+strconv.FormatInt(forgotten, 10),
		"sandy", "assistant_users", nil, "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("forget: status %d", res.StatusCode)
	}

	replacement, err := h.s.DB.CorrectMemory(corrected, runtimedb.MemoryRecord{
		Kind: "fact", Content: "Sandy drives a green car", Owner: "sandy",
		Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "household",
		SourceKind: "correction", SourceID: "bs-2", Confidence: "stated", Author: "sandy",
	}, "sandy")
	if err != nil {
		t.Fatalf("CorrectMemory: %v", err)
	}

	after := panel()
	if _, ok := after[forgotten]; ok {
		t.Error("a record the member forgot is still on the page that offers to forget it")
	}
	if _, ok := after[corrected]; ok {
		t.Error("a superseded record is still shown beside the correction that replaced it")
	}
	if _, ok := after[kept]; !ok {
		t.Error("filtering removed a record that is still remembered")
	}
	if _, ok := after[replacement]; !ok {
		t.Error("the correction that replaced a record is not shown")
	}
}

func TestDashboardPromotionAppearsOnceAfterRefresh(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy", "maya")
	source := remember(t, h, "sandy", "Sandy likes jasmine", runtimedb.ButlerAudiencePrivate)
	promoted, err := h.s.DB.PromotePersonalMemory(source, "sandy")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		person string
		seq    int64
		mine   bool
	}{{"sandy", source, true}, {"maya", promoted, false}} {
		res := h.member(http.MethodGet, "/api/v1/assistant/memory", tc.person, "assistant_users", nil, "")
		var got struct {
			Items []memoryItem `json:"items"`
		}
		err := json.NewDecoder(res.Body).Decode(&got)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Items) != 1 || got.Items[0].Seq != tc.seq || got.Items[0].Mine != tc.mine {
			t.Errorf("%s refreshed panel: %+v", tc.person, got.Items)
		}
	}
}
