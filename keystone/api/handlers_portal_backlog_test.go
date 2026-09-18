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
	h.person("sandy")
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
	h.person("sandy")
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

// A fourth waiting decision is counted even though only three are shown.
//
// The card shows three and says "+N more". The count and the rows have
// to come from the same read, and a read the size of the card can only
// ever say "+0".
func TestAFourthWaitingDecisionIsCounted(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	for i := range 5 {
		if _, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
			Kind: "lock-up", Recipient: "sandy", DedupeKey: fmt.Sprintf("q-%d", i),
			Asks: true, TTL: 72 * time.Hour,
		}, runtimedb.DefaultButlerSettings(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	got := h.summary(t, "sandy")
	if len(got.NeedsYou.Items) != maxNeedsYou {
		t.Fatalf("the card shows %d items, want %d", len(got.NeedsYou.Items), maxNeedsYou)
	}
	if got.NeedsYou.More != 2 {
		t.Errorf("the card says +%d more, want +2 — two decisions are invisible", got.NeedsYou.More)
	}
}

// A plan still waiting on the owner is found behind a week of resolved
// ones.
func TestAProposedPlanSurvivesABacklogOfResolvedPlans(t *testing.T) {
	h := homeHarness(t)
	if _, err := h.s.DB.InsertPlan("admin", "mika", "install immich", "low", "{}"); err != nil {
		t.Fatal(err)
	}
	// Sixty resolved plans on top of it — more than the fifty the card
	// used to read.
	for i := range 60 {
		id, err := h.s.DB.InsertPlan("admin", "mika", fmt.Sprintf("noise %d", i), "low", "{}")
		if err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.TransitionPlan(id, runtimedb.PlanApproved, "mika"); err != nil {
			t.Fatal(err)
		}
	}

	got := h.summary(t, "sandy")
	found := false
	for _, i := range got.NeedsYou.Items {
		if i.WaitingOn == "the owner" {
			found = true
		}
	}
	if !found {
		t.Error("the card says nothing is waiting for the owner while a plan is proposed")
	}
}

// "Today at home" shows this evening, not this morning.
func TestTodayAtHomeShowsTheNewestEventsOfTheDay(t *testing.T) {
	h := homeHarness(t)
	// Anchored to the household's own midnight rather than to a number
	// of hours ago: "six hours before now" is before midnight for the
	// first six hours of any day, and those events would be outside the
	// card's day entirely — a test that passes because its fixture is
	// missing.
	loc := h.s.HouseholdLoc
	if loc == nil {
		loc = time.Local
	}
	local := time.Now().In(loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)

	// EVERY STAMP IS RELATIVE TO MIDNIGHT, not to now, so the fixture
	// does not depend on what time the suite happens to run. A second
	// past midnight rather than on it, because a stamp inside the
	// boundary second is not reliably ordered against the boundary
	// itself — which is the hole the Since bound had to be written
	// around, and a test that sat in it would be testing the hole.
	early := midnight.Add(time.Second)
	// More events early in the day than the card reads at all, so
	// anything taking a window from the OLD end fills it with these.
	for i := range maxActivityScan + 20 {
		if _, _, err := h.s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
			EventID: fmt.Sprintf("early-%d", i), OccurredAt: early.Add(time.Duration(i) * time.Millisecond),
			Source: "portal", Type: "filedrop.uploaded", Entity: "a.jpg",
		}); err != nil {
			t.Fatal(err)
		}
	}
	newest := early.Add(time.Hour)
	if _, _, err := h.s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID: "later-today", OccurredAt: newest,
		Source: "portal", Type: "filedrop.uploaded", Entity: "the-newest.jpg",
	}); err != nil {
		t.Fatal(err)
	}

	got := h.summary(t, "sandy")
	if len(got.Activity.Rows) == 0 {
		t.Fatal("today's card is empty")
	}
	if !got.Activity.Rows[0].At.Equal(newest) {
		t.Errorf("today's card leads with an event from %s, want the newest (%s) — it is showing the start of the day",
			got.Activity.Rows[0].At.Format(time.TimeOnly), newest.Format(time.TimeOnly))
	}
}

// An unread notice behind more than the card's whole window is still
// counted and still shown.
//
// The earlier fix sorted before truncating, which was necessary and not
// sufficient: the sort only ever saw the newest rows the query returned.
func TestAnUnreadNotificationBehindAWholeWindowIsStillFound(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	unread, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
		Kind: "washing-done", Recipient: "sandy", DedupeKey: "the-unread-one",
	}, runtimedb.DefaultButlerSettings(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// More read notices on top of it than the card reads at all.
	for i := range maxNotificationsScan + 10 {
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
		t.Errorf("the header says %d unread, want 1", got.Notifications.Unread)
	}
	shown := false
	for _, n := range got.Notifications.Items {
		if n.Seq == unread.Seq {
			shown = true
		}
	}
	if !shown {
		t.Error("the one unread notification is outside the window the card draws from")
	}
}

// The memory panel shows the newest, which is what its heading says and
// what a member wants to correct.
func TestTheMemoryPanelShowsTheNewestNotTheOldest(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	for i := range maxMemoryShown + 10 {
		remember(t, h, "sandy", fmt.Sprintf("fact number %d", i), runtimedb.ButlerAudiencePrivate)
	}
	newest := remember(t, h, "sandy", "Sandy started running on Tuesdays", runtimedb.ButlerAudiencePrivate)

	h.unthrottled()
	res := h.member(http.MethodGet, "/api/v1/assistant/memory", "sandy", "assistant_users", nil, "")
	defer res.Body.Close()
	var got struct {
		Items []memoryItem `json:"items"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) == 0 {
		t.Fatal("the panel is empty")
	}
	if got.Items[0].Seq != newest {
		t.Errorf("the panel leads with record %d, want the newest (%d) — it is showing the oldest",
			got.Items[0].Seq, newest)
	}
}

// A member's own words survive the round trip to their request list.
func TestACatalogNoteWithAQuoteIsReadBackWhole(t *testing.T) {
	h := homeHarness(t)
	const note = `Use the "family" edition, not the one with \ in it`
	payload, err := json.Marshal(map[string]string{"note": note, "package": "immich"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID: "req-quoted", OccurredAt: time.Now(), Subject: "sandy",
		Source: "portal", Type: catalogEventType, Entity: "immich", Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}

	h.unthrottled()
	res := h.member(http.MethodGet, "/api/v1/catalog/requests", "sandy", "homepage_users", nil, "")
	defer res.Body.Close()
	var got struct {
		Requests []struct {
			Note string `json:"note"`
		} `json:"requests"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(got.Requests))
	}
	if got.Requests[0].Note != note {
		t.Errorf("the note reads %q, want %q", got.Requests[0].Note, note)
	}
}

// A conversation is named after what the member asked, and keeps that
// name however long the conversation goes on.
func TestAConversationIsNamedAfterTheMemberNotTheAssistant(t *testing.T) {
	h := homeHarness(t)
	id, err := h.s.DB.InsertButlerSession(runtimedb.ButlerSession{
		Principal: "portal", ActingFor: "sandy", Provider: "test",
		Surface: runtimedb.ButlerSurfaceFamily, Butler: "sandy",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AppendButlerTurns(id, []runtimedb.ButlerTurn{
		{Role: runtimedb.ButlerRoleUser, Text: "When do the bins go out?"},
		{Role: runtimedb.ButlerRoleAssistant, Text: "Tuesdays, before eight."},
	}); err != nil {
		t.Fatal(err)
	}

	title := func() string {
		t.Helper()
		h.unthrottled()
		res := h.member(http.MethodGet, "/api/v1/assistant/conversations", "sandy", "assistant_users", nil, "")
		defer res.Body.Close()
		var got struct {
			Conversations []struct {
				Title string `json:"title"`
			} `json:"conversations"`
		}
		if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if len(got.Conversations) != 1 {
			t.Fatalf("got %d conversations, want 1", len(got.Conversations))
		}
		return got.Conversations[0].Title
	}

	if got := title(); got != "When do the bins go out?" {
		t.Errorf("the conversation is called %q, want the member's own question", got)
	}

	// A follow-up must not rename it.
	if err := h.s.DB.AppendButlerTurns(id, []runtimedb.ButlerTurn{
		{Role: runtimedb.ButlerRoleUser, Text: "And the recycling?"},
		{Role: runtimedb.ButlerRoleAssistant, Text: "Every other Thursday."},
	}); err != nil {
		t.Fatal(err)
	}
	if got := title(); got != "When do the bins go out?" {
		t.Errorf("after a follow-up the conversation is called %q — the title moved", got)
	}
}

// A restore drill is found however diligently the household backs up.
//
// The two facts on this card are read from the same ledger the Console
// paginates, and a household with four daily backup plans puts four
// hundred rows between monthly drills. A card about whether the family
// could recover must not go quiet because they back up often.
func TestTheDrillIsFoundBehindAYearOfBackups(t *testing.T) {
	h := homeHarness(t)
	drill, err := h.s.DB.StartBackupRun("nightly", runtimedb.BackupRunDrill, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.FinishBackupRun(drill, runtimedb.BackupRunSucceeded, "restored"); err != nil {
		t.Fatal(err)
	}
	// And the last good backup, buried under a run of failures.
	good, err := h.s.DB.StartBackupRun("nightly", runtimedb.BackupRunBackup, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.FinishBackupRun(good, runtimedb.BackupRunSucceeded, "ok"); err != nil {
		t.Fatal(err)
	}
	for i := range 250 {
		seq, err := h.s.DB.StartBackupRun(fmt.Sprintf("plan-%d", i), runtimedb.BackupRunBackup, "p1")
		if err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.FinishBackupRun(seq, runtimedb.BackupRunFailed, "no"); err != nil {
			t.Fatal(err)
		}
	}

	got := h.summary(t, "sandy")
	if got.Machines.LastDrill.IsZero() {
		t.Error("the card lost the restore drill behind the household's own backups")
	}
	if !got.Machines.DrillOK {
		t.Error("the drill succeeded and the card does not say so")
	}
	if got.Machines.LastBackup.IsZero() {
		t.Error("the card lost the last successful backup behind a run of failures")
	}
}

// Clear means all of them, not a window's worth.
func TestClearingNotificationsClearsMoreThanAWindow(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	for i := range 260 {
		if _, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
			Kind: "washing-done", Recipient: "sandy", DedupeKey: fmt.Sprintf("n-%d", i),
		}, runtimedb.DefaultButlerSettings(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	// One unanswered question, which Clear must still leave alone.
	asked, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
		Kind: "lock-up", Recipient: "sandy", DedupeKey: "the-question", Asks: true,
		TTL: 72 * time.Hour,
	}, runtimedb.DefaultButlerSettings(), time.Now())
	if err != nil {
		t.Fatal(err)
	}

	h.unthrottled()
	res := h.member(http.MethodPost, "/api/v1/home/notices/clear", "sandy", "homepage_users", nil, "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("clear: status %d", res.StatusCode)
	}

	// One unread left, and it is the question: Clear does not touch a
	// decision that is still waiting, so it stays unacknowledged and
	// stays counted. Everything else has gone.
	got := h.summary(t, "sandy")
	if got.Notifications.Unread != 1 {
		t.Errorf("after Clear the card counts %d unread, want only the unanswered question — the button reported success and left the rest",
			got.Notifications.Unread)
	}
	// The question is untouched, and still on the card that asks it.
	after, err := h.s.DB.GetNotice(asked.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if !after.AcknowledgedAt.IsZero() {
		t.Error("Clear acknowledged an unanswered question, taking the decision off the page")
	}
}

// The household has one Sandy however she signed in.
func TestAMembersConversationsAreFoundWhateverTheCase(t *testing.T) {
	h := homeHarness(t)
	insertFamilySession(t, h, "sandy", "When do the bins go out?")

	h.unthrottled()
	res := h.member(http.MethodGet, "/api/v1/assistant/conversations", "Sandy", "assistant_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("conversations: status %d", res.StatusCode)
	}
	var got struct {
		Conversations []struct {
			ID string `json:"id"`
		} `json:"conversations"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Conversations) != 1 {
		t.Fatalf("Sandy sees %d conversations, want the 1 stored for sandy", len(got.Conversations))
	}

	// And the conversation itself opens, rather than reading as somebody
	// else's.
	h.unthrottled()
	one := h.member(http.MethodGet, "/api/v1/assistant/conversations/"+got.Conversations[0].ID,
		"Sandy", "assistant_users", nil, "")
	defer one.Body.Close()
	if one.StatusCode != http.StatusOK {
		t.Errorf("opening her own conversation as Sandy: status %d", one.StatusCode)
	}
}

// A second upload under a name somebody freed up is a second event.
func TestReusingAFreedNameIsStillAnEvent(t *testing.T) {
	h, _ := portalHarness(t)

	if res, body := h.upload("sandy", "shared", "notes.txt", "first"); res.StatusCode != http.StatusCreated {
		t.Fatalf("first upload = %d (%v)", res.StatusCode, body)
	}
	h.unthrottled()
	del := h.member(http.MethodPost, "/api/v1/filedrop/delete?namespace=shared&name=notes.txt",
		"sandy", "filedrop_users", nil, "")
	del.Body.Close()
	if del.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d", del.StatusCode)
	}
	h.unthrottled()
	if res, body := h.upload("sandy", "shared", "notes.txt", "second"); res.StatusCode != http.StatusCreated {
		t.Fatalf("second upload = %d (%v)", res.StatusCode, body)
	}

	events, err := h.s.DB.HouseholdEventsBefore(0, 50, runtimedb.HouseholdEventFilter{
		Type: "filedrop.uploaded",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Errorf("the household stream carries %d upload events, want 2 — the second was dropped as a duplicate of the first",
			len(events))
	}
}

// The whole household answers to the spelling the door forgave.
//
// The door admits one Sandy however she signed in, and then every
// member-scoped query underneath compares a stored name. A member
// authenticated under a capitalisation the records are not under is
// worse off than one who was refused: she is shown her own household,
// empty, which looks like an answer.
func TestACapitalisationTheDoorForgivesDoesNotEmptyTheHousehold(t *testing.T) {
	h := homeHarness(t)
	if err := h.s.DB.RecordButlerMember("sandy", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AdmitButlerMember("sandy", "mika"); err != nil {
		t.Fatal(err)
	}
	// Everything stored under the household's own spelling.
	remember(t, h, "sandy", "Sandy does not eat fish", runtimedb.ButlerAudiencePrivate)
	insertFamilySession(t, h, "sandy", "When do the bins go out?")
	if _, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
		Kind: "washing-done", Recipient: "sandy", DedupeKey: "n1",
	}, runtimedb.DefaultButlerSettings(), time.Now()); err != nil {
		t.Fatal(err)
	}

	// Read as the proxy might later assert her.
	h.unthrottled()
	res := h.member(http.MethodGet, "/api/v1/assistant/memory", "Sandy", "assistant_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("memory: status %d", res.StatusCode)
	}
	var mem struct {
		Items  []memoryItem `json:"items"`
		Member string       `json:"member"`
	}
	if err := json.NewDecoder(res.Body).Decode(&mem); err != nil {
		t.Fatal(err)
	}
	if len(mem.Items) == 0 {
		t.Error("Sandy's memory panel is empty; everything it holds is stored for sandy")
	}
	// The surface reports the household's spelling, not the asserted
	// one: it is the name her records are under.
	if mem.Member != "sandy" {
		t.Errorf("the surface calls her %q, want the spelling her records use", mem.Member)
	}

	h.unthrottled()
	notes := h.summary(t, "Sandy")
	if notes.Notifications.Unread != 1 {
		t.Errorf("Sandy's card counts %d unread, want the 1 raised for sandy", notes.Notifications.Unread)
	}
}

// "+N more" counts every waiting decision, not the ones that fit in a
// window somebody chose.
func TestTheHiddenDecisionCountIsNotAWindowSize(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	const waiting = 60
	for i := range waiting {
		if _, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
			Kind: "lock-up", Recipient: "sandy", DedupeKey: fmt.Sprintf("q-%d", i),
			Asks: true, TTL: 72 * time.Hour,
		}, runtimedb.DefaultButlerSettings(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	got := h.summary(t, "sandy")
	if len(got.NeedsYou.Items) != maxNeedsYou {
		t.Fatalf("the card shows %d items, want %d", len(got.NeedsYou.Items), maxNeedsYou)
	}
	if want := waiting - maxNeedsYou; got.NeedsYou.More != want {
		t.Errorf("the card says +%d more, want +%d — the count is the size of a window rather than the backlog",
			got.NeedsYou.More, want)
	}
}

// A card's worth of events this member cannot see does not make the
// household look quiet.
func TestTodayAtHomeWalksPastEventsThisMemberCannotSee(t *testing.T) {
	h := homeHarness(t)
	// Every member-scoped event about somebody else is withheld.
	h.s.MemberScoped = func(string) bool { return true }

	loc := h.s.HouseholdLoc
	if loc == nil {
		loc = time.Local
	}
	local := time.Now().In(loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	early := midnight.Add(time.Second)

	// The one row sandy may see, earliest today.
	if _, _, err := h.s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID: "hers", OccurredAt: early, Subject: "sandy",
		Source: "portal", Type: "filedrop.uploaded", Entity: "a.jpg",
	}); err != nil {
		t.Fatal(err)
	}
	// Then more of maya's than the card reads in one batch.
	for i := range maxActivityScan + 20 {
		if _, _, err := h.s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
			EventID: fmt.Sprintf("mayas-%d", i), OccurredAt: early.Add(time.Duration(i+1) * time.Millisecond),
			Subject: "maya", Source: "portal", Type: "filedrop.uploaded", Entity: "b.jpg",
		}); err != nil {
			t.Fatal(err)
		}
	}

	got := h.summary(t, "sandy")
	if !got.Activity.Available {
		t.Fatal("the activity card is unavailable")
	}
	if len(got.Activity.Rows) != 1 {
		t.Errorf("sandy's card shows %d rows, want the 1 event that is hers — a busy day rendered as a quiet one",
			len(got.Activity.Rows))
	}
}
