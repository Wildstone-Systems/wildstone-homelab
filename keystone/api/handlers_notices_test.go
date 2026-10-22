package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// A person sets their own preferences and nobody else's; an
// administrator may set anyone's.
func TestNoticePreferencesAreYourOwn(t *testing.T) {
	h := newHarness(t)

	if res := h.do(runtimedb.RoleOperator, http.MethodPut, "/api/v1/notices/preferences",
		map[string]any{"quietStartMin": 1320, "quietEndMin": 420, "urgentOverride": true},
		nil); res.StatusCode != http.StatusOK {
		t.Fatalf("setting your own preferences = %d", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleOperator, http.MethodPut, "/api/v1/notices/preferences",
		map[string]any{"member": "sam", "quietStartMin": 0, "quietEndMin": 0},
		nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("setting somebody else's preferences = %d, want 403", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/notices/preferences",
		map[string]any{"member": "sam", "quietStartMin": 60, "quietEndMin": 420},
		nil); res.StatusCode != http.StatusOK {
		t.Fatalf("an administrator setting a member's preferences = %d", res.StatusCode)
	}
	// And a window that is not a time of day is refused.
	if res := h.do(runtimedb.RoleOperator, http.MethodPut, "/api/v1/notices/preferences",
		map[string]any{"quietStartMin": 5000}, nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("an impossible quiet window = %d, want 400", res.StatusCode)
	}
}

// Only an administrator declares an urgent kind, and it has to say why.
func TestOnlyAdminsDeclareUrgentKinds(t *testing.T) {
	h := newHarness(t)
	if res := h.do(runtimedb.RoleOperator, http.MethodPut, "/api/v1/notices/kinds",
		map[string]any{"kind": "anything", "urgent": true, "description": "because"},
		nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("an operator declared an urgent kind: %d", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/notices/kinds",
		map[string]any{"kind": "smoke-alarm", "urgent": true}, nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("an urgent kind with no stated reason = %d, want 400", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/notices/kinds",
		map[string]any{
			"kind": "smoke-alarm", "urgent": true, "relevance": 100,
			"description": "a life-safety device reported an alarm",
		}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("declaring an urgent kind = %d", res.StatusCode)
	}
}

// A reply that is not clearly yes or no does nothing, and says so.
func TestAmbiguousAnswerIsRefusedWithAnExplanation(t *testing.T) {
	h := newHarness(t)
	h.person("op")
	raised, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
		Kind: "approval", Recipient: "op", DedupeKey: "n1", Asks: true,
	}, runtimedb.DefaultButlerSettings(), timeSoon())
	if err != nil {
		t.Fatal(err)
	}
	var refusal struct {
		Error string `json:"error"`
	}
	res := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/notices/answer",
		map[string]any{"seq": raised.Seq, "answer": "maybe later"}, &refusal)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("an ambiguous reply = %d, want 400", res.StatusCode)
	}
	if !strings.Contains(refusal.Error, "not consent") {
		t.Fatalf("the refusal does not explain itself: %q", refusal.Error)
	}
	var ok struct {
		Approved bool `json:"approved"`
	}
	if res := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/notices/answer",
		map[string]any{"seq": raised.Seq, "answer": "yes"}, &ok); res.StatusCode != http.StatusOK {
		t.Fatalf("a clear yes = %d", res.StatusCode)
	}
	if !ok.Approved {
		t.Fatal("a clear yes did not approve")
	}
}

// Missed notices are visible, and counted, because otherwise nobody
// finds out they were never told.
func TestMissedNoticesAreVisible(t *testing.T) {
	h := newHarness(t)
	h.person("op")
	// Raised an hour ago with a one-millisecond life: nobody was ever
	// going to see it.
	if _, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
		Kind: "task-done", Recipient: "op", DedupeKey: "n1", TTL: time.Millisecond,
	}, runtimedb.DefaultButlerSettings(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	var swept struct {
		Expired int `json:"expired"`
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/notices/expire", map[string]any{}, &swept); res.StatusCode != http.StatusOK {
		t.Fatalf("expire = %d", res.StatusCode)
	}
	var listed struct {
		Notices []runtimedb.Notice `json:"notices"`
		Missed  int                `json:"missed"`
		Note    string             `json:"note"`
	}
	h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/notices", nil, &listed)
	if listed.Missed != 1 {
		t.Fatalf("missed = %d, want the one nobody was told about: %+v", listed.Missed, listed.Notices)
	}
	if !strings.Contains(listed.Note, "nobody was told") {
		t.Fatalf("the listing does not explain what missed means: %q", listed.Note)
	}
}
