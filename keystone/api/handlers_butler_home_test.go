package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// The activity seam applies the page's own audience test. A
// member-scoped event about somebody else is not this member's to hear
// in chat any more than it is theirs to read on the page — and the two
// must agree, because a widget is a window onto a page rather than a
// second opinion about it.
func TestActivitySeamWithholdsAnotherMembersFacts(t *testing.T) {
	h := homeHarness(t)
	h.s.MemberScoped = func(eventType string) bool { return eventType == "presence.arrived" }
	now := time.Now()
	for _, e := range []runtimedb.HouseholdEvent{
		{EventID: "e1", Source: "test", Type: "presence.arrived", Subject: "robin", OccurredAt: now.Add(-3 * time.Minute)},
		{EventID: "e2", Source: "test", Type: "presence.arrived", Subject: "sam", OccurredAt: now.Add(-2 * time.Minute)},
		{EventID: "e3", Source: "test", Type: "bins.collected", OccurredAt: now.Add(-time.Minute)},
	} {
		if _, _, err := h.s.DB.AppendHouseholdEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	seam := h.s.HouseholdActivitySeam()

	lines, err := seam(context.Background(), butler.ActivityLookup{Member: "sam"})
	if err != nil {
		t.Fatalf("activity seam: %v", err)
	}
	joined := renderLines(lines)
	if strings.Contains(joined, "robin") {
		t.Errorf("sam was told about robin's movements: %s", joined)
	}
	if !strings.Contains(joined, "Sam") && !strings.Contains(joined, "sam") {
		t.Errorf("sam was not told about their own: %s", joined)
	}

	// In a GROUP the narrowing is stronger and does not consult
	// MemberScoped at all: the member who is not in the room is not
	// discussed, whatever the deployment classifies their events as.
	group, err := seam(context.Background(), butler.ActivityLookup{Member: "sam", GroupSafe: true})
	if err != nil {
		t.Fatalf("activity seam: %v", err)
	}
	if strings.Contains(renderLines(group), "robin") {
		t.Errorf("a room was told about somebody who is not in it: %s", renderLines(group))
	}
}

// An unmapped verb is DROPPED, never printed raw. The page's reasoning
// applies with more force to a model, which will read
// "automation.action_failed" aloud as though it were English.
func TestActivitySeamDropsUnmappedVerbs(t *testing.T) {
	h := homeHarness(t)
	if _, _, err := h.s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID: "x1", Source: "test", Type: "nobody.wrote.a.sentence.for.this", OccurredAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	lines, err := h.s.HouseholdActivitySeam()(context.Background(), butler.ActivityLookup{Member: "sam"})
	if err != nil {
		t.Fatalf("activity seam: %v", err)
	}
	if strings.Contains(renderLines(lines), "nobody.wrote") {
		t.Fatalf("a machine verb reached a model verbatim: %s", renderLines(lines))
	}
}

// The counts come from their own queries. A digest that derived them
// from the window it happened to read goes quietly wrong in exactly the
// household that has a backlog, which is the one asking.
func TestNotificationSeamCountsPastTheWindow(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	settings := runtimedb.DefaultButlerSettings()
	if _, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
		Kind: "lock-up", Recipient: "sandy", DedupeKey: "the-question",
		Body: "Lock up for the night?", Asks: true, TTL: 72 * time.Hour,
	}, settings, time.Now()); err != nil {
		t.Fatal(err)
	}
	for i := range 30 {
		if _, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
			Kind: "washing-done", Recipient: "sandy", DedupeKey: fmt.Sprintf("noise-%d", i),
		}, settings, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	digest, err := h.s.HouseholdNotificationsSeam()(context.Background(),
		butler.NotificationLookup{Member: "sandy", Limit: 3})
	if err != nil {
		t.Fatalf("notification seam: %v", err)
	}
	if len(digest.Lines) > 3 {
		t.Fatalf("the window was not bounded: %d lines", len(digest.Lines))
	}
	if digest.Unread <= len(digest.Lines) {
		t.Errorf("unread = %d with %d lines read: the count is derived from the window",
			digest.Unread, len(digest.Lines))
	}
	if digest.Waiting != 1 {
		t.Errorf("waiting = %d, want the one question still unanswered", digest.Waiting)
	}
}

// Another member's notifications are never read here, and a lookup with
// no member fails closed rather than defaulting to everybody's.
func TestNotificationSeamIsPerPersonAndFailsClosed(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy", "robin")
	settings := runtimedb.DefaultButlerSettings()
	for _, who := range []string{"sandy", "robin"} {
		if _, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
			Kind: "washing-done", Recipient: who, DedupeKey: "wash-" + who,
		}, settings, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	seam := h.s.HouseholdNotificationsSeam()

	sandy, err := seam(context.Background(), butler.NotificationLookup{Member: "sandy"})
	if err != nil {
		t.Fatalf("notification seam: %v", err)
	}
	if sandy.Unread != 1 {
		t.Errorf("sandy's unread count is %d, want 1 — it is counting somebody else's", sandy.Unread)
	}

	if _, err := seam(context.Background(), butler.NotificationLookup{}); err == nil {
		t.Fatal("a lookup with no member was answered")
	}
}

// A machine that cannot be read is reported as unreadable, and no
// hostname reaches the family surface — §7's rule for the card, which a
// spoken answer is not the place to start breaking.
func TestMachineSeamNamesNoHostsAndReportsWhatItCannotRead(t *testing.T) {
	h := homeHarness(t)
	if err := h.s.DB.RecordNodeJoin("node-alpha"); err != nil {
		t.Fatal(err)
	}
	// No Agents wired, so the probe cannot answer — which is the case
	// the card exists to be honest about.
	machines, err := h.s.HouseholdMachinesSeam()(context.Background())
	if err != nil {
		t.Fatalf("machine seam: %v", err)
	}
	if len(machines) != 1 {
		t.Fatalf("machines = %d, want 1", len(machines))
	}
	if machines[0].Connected {
		t.Errorf("an unreachable machine reported as connected: %+v", machines[0])
	}
	if machines[0].Problem == "" {
		t.Errorf("an unreadable machine gave no reason: %+v", machines[0])
	}
	if strings.Contains(machines[0].Name, "node-alpha") {
		t.Errorf("a hostname reached the family surface: %q", machines[0].Name)
	}
	if machines[0].CPUKnown || machines[0].MemKnown || machines[0].DiskKnown {
		t.Errorf("an unreachable machine claimed readings: %+v", machines[0])
	}
}

func renderLines(lines []butler.ActivityLine) string {
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		parts = append(parts, l.Sentence)
	}
	return strings.Join(parts, " | ")
}

// A publisher that reports late must not hide the rows behind it.
//
// The stream comes back newest first by SEQUENCE — the order things were
// recorded — so an event delivered now with an OccurredAt from last week
// sits among today's rows. Ending the walk at the first row outside the
// window let one late delivery truncate the answer.
func TestActivitySeamDoesNotStopAtALateDelivery(t *testing.T) {
	h := homeHarness(t)
	now := time.Now()
	// Recorded in this order; the middle one happened a week ago.
	for _, e := range []runtimedb.HouseholdEvent{
		{EventID: "a", Source: "test", Type: "bins.collected", OccurredAt: now.Add(-10 * time.Minute)},
		{EventID: "b", Source: "test", Type: "bins.collected", OccurredAt: now.Add(-7 * 24 * time.Hour)},
		{EventID: "c", Source: "test", Type: "bins.collected", OccurredAt: now.Add(-5 * time.Minute)},
	} {
		if _, _, err := h.s.DB.AppendHouseholdEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	lines, err := h.s.HouseholdActivitySeam()(context.Background(), butler.ActivityLookup{
		Member: "sam", Since: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("activity seam: %v", err)
	}
	// Both in-window rows, and not the week-old one.
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want the two inside the window: %+v", len(lines), lines)
	}
	for _, l := range lines {
		if l.At.Before(now.Add(-time.Hour)) {
			t.Errorf("a row outside the window was returned: %+v", l)
		}
	}
}
