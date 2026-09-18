package api

// The three cards, in a household with a backlog.
//
// Each of these used to read a fixed window of the newest rows and look
// for what it wanted among them, which works in a test with four rows
// and fails in the household the card was written for. What they share
// is that the failure is SILENT: an empty card and a quiet household
// look exactly alike, and the member is the one who finds out.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// A decision waiting behind a week of ordinary notices is still on the
// card.
func TestAPendingQuestionSurvivesABacklogOfNotices(t *testing.T) {
	h := homeHarness(t)
	asked, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
		Kind: "lock-up", Recipient: "sandy", DedupeKey: "the-question", Asks: true,
		TTL: 72 * time.Hour,
	}, runtimedb.DefaultButlerSettings(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Sixty ordinary notices on top of it — more than the fifty the
	// card used to read.
	for i := range 60 {
		if _, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
			Kind: "washing-done", Recipient: "sandy", DedupeKey: fmt.Sprintf("noise-%d", i),
		}, runtimedb.DefaultButlerSettings(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	got := h.summary(t, "sandy")
	found := false
	for _, i := range got.NeedsYou.Items {
		if i.Seq == asked.Seq {
			found = true
		}
	}
	if !found {
		t.Fatalf("the question waiting on sandy is not on her card (%d items)", len(got.NeedsYou.Items))
	}
}

// An unread notification behind a page of read ones is still shown.
//
// The count in the header and the rows beneath it have to come from the
// same set: a card that says "1 unread" and shows no unread row is
// telling the member to go looking for something it is hiding.
func TestAnUnreadNotificationIsNotCutBeforeTheSort(t *testing.T) {
	h := homeHarness(t)
	unread, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
		Kind: "washing-done", Recipient: "sandy", DedupeKey: "the-unread-one",
	}, runtimedb.DefaultButlerSettings(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Then a card's worth of newer ones, every one of them read.
	for i := range maxNotifications + 2 {
		n, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
			Kind: "washing-done", Recipient: "sandy", DedupeKey: fmt.Sprintf("read-%d", i),
		}, runtimedb.DefaultButlerSettings(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.AcknowledgeNotice(n.Seq); err != nil {
			t.Fatal(err)
		}
	}

	got := h.summary(t, "sandy")
	if got.Notifications.Unread != 1 {
		t.Fatalf("the header says %d unread, want 1", got.Notifications.Unread)
	}
	shown := false
	for _, n := range got.Notifications.Items {
		if n.Seq == unread.Seq {
			shown = true
		}
	}
	if !shown {
		t.Errorf("the card advertises an unread notification and shows none of it (%d rows)",
			len(got.Notifications.Items))
	}
}

// A member's catalog request survives a busy week of household events.
func TestACatalogRequestSurvivesABusyHousehold(t *testing.T) {
	h := homeHarness(t)
	if _, _, err := h.s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID: "req-1", OccurredAt: time.Now().Add(-48 * time.Hour),
		Subject: "sandy", Source: "portal", Type: catalogEventType, Entity: "immich",
	}); err != nil {
		t.Fatal(err)
	}
	// Then more newer events than the page used to read.
	for i := range 450 {
		if _, _, err := h.s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
			EventID: fmt.Sprintf("noise-%d", i), OccurredAt: time.Now(),
			Subject: "maya", Source: "portal", Type: "filedrop.uploaded", Entity: "a.jpg",
		}); err != nil {
			t.Fatal(err)
		}
	}

	h.unthrottled()
	res := h.member(http.MethodGet, "/api/v1/catalog/requests", "sandy", "homepage_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("catalog requests: status %d", res.StatusCode)
	}
	var got struct {
		Requests []struct {
			Package string `json:"package"`
			State   string `json:"state"`
		} `json:"requests"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Requests) != 1 || got.Requests[0].Package != "immich" {
		t.Fatalf("sandy's request history is %v, want the one immich request", got.Requests)
	}
}
