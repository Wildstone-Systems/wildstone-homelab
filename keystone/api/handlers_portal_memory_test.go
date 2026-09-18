package api

// Memory: what a member may read, and what they may forget.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

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
