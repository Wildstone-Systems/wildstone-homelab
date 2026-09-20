package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// child records a Person who is scheduled and cannot sign in: the state
// ADR-W033 A8 describes. Deliberately NOT admitted — AdmitButlerMember
// refuses a child by every path, and the agenda must work anyway.
func (h *harness) child(handle, guardian string) {
	h.t.Helper()
	if err := h.s.DB.RecordButlerMember(handle, runtimedb.RoleMember, runtimedb.ProfileChild, guardian); err != nil {
		h.t.Fatalf("recording child %q: %v", handle, err)
	}
}

func (h *harness) agenda(t *testing.T, reader, who string) agendaPage {
	t.Helper()
	path := "/api/v1/home/agenda"
	if who != "" {
		path += "?who=" + who
	}
	res := h.member(http.MethodGet, path, reader, "homepage_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("agenda for %q: status %d", who, res.StatusCode)
	}
	var got agendaPage
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decoding agenda: %v", err)
	}
	return got
}

// A child who cannot sign in still has a day. This is the whole point of
// A8: being scheduled and being a principal are different, and the
// agenda is built on the first.
func TestAChildWhoCannotSignInStillHasADay(t *testing.T) {
	h := homeHarness(t)
	h.person("mika")
	h.child("maya", "mika")

	got := h.agenda(t, "mika", "maya")
	if !got.Available {
		t.Fatalf("no agenda for a child: %q", got.Problem)
	}
	if got.Who != "maya" {
		t.Errorf("assembled a day for %q, want maya", got.Who)
	}

	var listed, marked bool
	for _, p := range got.People {
		if p.Handle == "maya" {
			listed, marked = true, p.Child
		}
	}
	if !listed {
		t.Error("a child who cannot sign in is not offered as a subject")
	}
	if !marked {
		t.Error("the child profile is not reported, so a surface cannot say whose day it is")
	}

	// And they are still not admitted. The prototype must not have
	// quietly opened the door ADR-W030 Part C holds shut.
	if err := h.s.DB.AdmitButlerMember("maya", "test"); err == nil {
		t.Error("a child was admitted; the Child surface gate has moved")
	}
}

// A guardian sees their child's personal records (A9). Nobody else does.
func TestAGuardianSeesTheirChildsDayAndAStrangerDoesNot(t *testing.T) {
	h := homeHarness(t)
	h.person("mika", "sandy")
	h.child("maya", "mika")

	if !h.s.isGuardianOf("mika", "maya") {
		t.Error("the recorded guardian has no standing over the child")
	}
	if h.s.isGuardianOf("sandy", "maya") {
		t.Error("an unrelated adult has standing over somebody else's child")
	}
	// Asymmetric: the child never reaches the guardian.
	if h.s.isGuardianOf("maya", "mika") {
		t.Error("guardianship runs child to guardian; it must only run the other way")
	}
	// And it is not "the adults" — an adult is not a guardian of an
	// adult, whatever the column says.
	if h.s.isGuardianOf("mika", "sandy") {
		t.Error("an adult acquired standing over another adult")
	}
}

// An event names who it is for, and only their day carries it.
func TestAnEventReachesTheDayOfThePersonItNames(t *testing.T) {
	h := homeHarness(t)
	h.person("mika")
	h.child("maya", "mika")

	// The harness sets no household zone, which is a real posture (the
	// handler falls back); pin one here so the test is not about that.
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	h.s.HouseholdLoc = loc
	at := time.Now().In(loc).Add(3 * time.Hour)
	h.s.CalendarBook = &fakeBook{events: []caldav.Event{
		{UID: "swim", Summary: "Swimming", Start: at, Attendees: []string{"Maya"}},
		{UID: "dentist", Summary: "Dentist", Start: at, Attendees: []string{"mika"}},
		{UID: "bins", Summary: "Bins out", Start: at},
	}}

	maya := h.agenda(t, "mika", "maya")
	if got := agendaTitles(maya); len(got) != 1 || got[0] != "Swimming" {
		t.Errorf("maya's day = %v, want only Swimming", got)
	}

	mika := h.agenda(t, "mika", "mika")
	titles := agendaTitles(mika)
	if !contains(titles, "Dentist") {
		t.Errorf("mika's day = %v, want the Dentist he is named on", titles)
	}
	if contains(titles, "Swimming") {
		t.Error("an event naming the child landed on the adult's day")
	}
	// An event naming nobody belongs to the household, not to a person.
	if contains(titles, "Bins out") {
		t.Error("an unattributed household event was filed under a person")
	}
}

// The day opens on the reader when nobody is named, and an unknown
// subject falls back rather than erroring: a wall tablet must not be
// able to reach a 404 by way of a stale query string.
func TestTheAgendaOpensOnYouAndFallsBack(t *testing.T) {
	h := homeHarness(t)
	h.person("mika")

	if got := h.agenda(t, "mika", "").Who; got != "mika" {
		t.Errorf("with no subject the agenda opened on %q, want the reader", got)
	}
	if got := h.agenda(t, "mika", "nobody-by-that-name").Who; got != "mika" {
		t.Errorf("an unknown subject resolved to %q, want the reader", got)
	}
}

// Today and tomorrow are always drawn, empty or not: "nothing on" is an
// answer a household wants, and a page that silently omitted today
// would read as broken.
func TestAnEmptyTodayIsStillADay(t *testing.T) {
	h := homeHarness(t)
	h.person("mika")

	got := h.agenda(t, "mika", "mika")
	if len(got.Days) < 2 {
		t.Fatalf("drew %d days, want today and tomorrow at least", len(got.Days))
	}
	if got.Days[0].Label != "Today" || got.Days[1].Label != "Tomorrow" {
		t.Errorf("days open %q, %q; want Today, Tomorrow", got.Days[0].Label, got.Days[1].Label)
	}
}

func agendaTitles(p agendaPage) []string {
	var out []string
	for _, d := range p.Days {
		for _, it := range d.Items {
			out = append(out, it.Title)
		}
	}
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
