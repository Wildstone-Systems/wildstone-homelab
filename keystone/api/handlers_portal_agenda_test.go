package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/upkeep"
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

// The person who runs the kids' week is a Member, not an Owner, and she
// enters routines from the dashboard. This is the whole usability claim:
// no Console, no Owner, no Plan for somebody else to approve.
func TestAMemberAddsARoutineFromTheDashboard(t *testing.T) {
	h := homeHarness(t)
	h.s.Upkeep = &upkeep.Engine{DB: h.s.DB, State: h.s.State, Loc: time.UTC}
	h.person("sandy")
	h.child("maya", "sandy")

	body := `{"description":"Make your bed","about":"maya","every":"1d",` +
		`"partOfDay":"morning","rollover":"fromCompletion","scope":"personal"}`
	res := h.member(http.MethodPost, "/api/v1/home/commitments", "sandy", "homepage_users",
		stringsNewReader(body), "application/json")
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("a member adding a routine: status %d, want 201", res.StatusCode)
	}

	// And it lands on the child's day, marked as a morning thing.
	got := h.agenda(t, "sandy", "maya")
	var found *agendaItem
	for i := range got.Days {
		for j := range got.Days[i].Items {
			if got.Days[i].Items[j].Title == "Make your bed" {
				found = &got.Days[i].Items[j]
			}
		}
	}
	if found == nil {
		t.Fatalf("the routine is not on maya's day: %v", agendaTitles(got))
	}
	if found.PartOfDay != "morning" {
		t.Errorf("partOfDay = %q, want morning", found.PartOfDay)
	}
	// The guardian may tick it off; that is A9 reaching the tick and not
	// just the read.
	if !found.Done {
		t.Error("the guardian is not offered the tick on her own child's routine")
	}
	if found.Name == "" {
		t.Error("no name on the row, so the page has nothing to tick off with")
	}
}

// A day sorts the way a day happens, not by when a record was made.
func TestTheDaySortsMorningAfternoonEvening(t *testing.T) {
	h := homeHarness(t)
	h.s.Upkeep = &upkeep.Engine{DB: h.s.DB, State: h.s.State, Loc: time.UTC}
	h.person("sandy")
	h.child("maya", "sandy")

	for _, r := range []struct{ what, part string }{
		{"Pyjamas on", "evening"},
		{"Make your bed", "morning"},
		{"Homework", "afternoon"},
	} {
		body := `{"description":"` + r.what + `","about":"maya","every":"1d",` +
			`"partOfDay":"` + r.part + `","rollover":"fromCompletion","scope":"personal"}`
		res := h.member(http.MethodPost, "/api/v1/home/commitments", "sandy", "homepage_users",
			stringsNewReader(body), "application/json")
		res.Body.Close()
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("adding %q: %d", r.what, res.StatusCode)
		}
	}

	got := h.agenda(t, "sandy", "maya")
	if len(got.Days) == 0 {
		t.Fatal("no days")
	}
	var order []string
	for _, it := range got.Days[0].Items {
		if it.PartOfDay != "" {
			order = append(order, it.PartOfDay)
		}
	}
	want := []string{"morning", "afternoon", "evening"}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Errorf("today ran %v, want %v", order, want)
	}
}

// A child can be named on a commitment. The upkeep page's suggestion
// list said "children included" and filtered on admitted, which a child
// can never be.
func TestAChildIsOfferedAsSomebodyToNameOnACommitment(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	h.child("maya", "sandy")

	people := h.s.householdPeople()
	var found bool
	for _, p := range people {
		if p == "maya" {
			found = true
		}
	}
	if !found {
		t.Errorf("household people = %v, want maya among them", people)
	}
}

func stringsNewReader(s string) *strings.Reader { return strings.NewReader(s) }
