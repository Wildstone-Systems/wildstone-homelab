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

	if !h.s.who(nil, "mika").guards("maya") {
		t.Error("the recorded guardian has no standing over the child")
	}
	if h.s.who(nil, "sandy").guards("maya") {
		t.Error("an unrelated adult has standing over somebody else's child")
	}
	// Asymmetric: the child never reaches the guardian.
	if h.s.who(nil, "maya").guards("mika") {
		t.Error("guardianship runs child to guardian; it must only run the other way")
	}
	// And it is not "the adults" — an adult is not a guardian of an
	// adult, whatever the column says.
	if h.s.who(nil, "mika").guards("sandy") {
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
		`"anchor":"` + time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02") + `",` +
		`"partOfDay":"morning","rollover":"fromSchedule","scope":"personal"}`
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

	// Anchored yesterday so the first threshold is today, which is what
	// the page's own anchor does for a daily routine.
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	for _, r := range []struct{ what, part string }{
		{"Pyjamas on", "evening"},
		{"Make your bed", "morning"},
		{"Homework", "afternoon"},
	} {
		body := `{"description":"` + r.what + `","about":"maya","every":"1d",` +
			`"anchor":"` + yesterday + `",` +
			`"partOfDay":"` + r.part + `","rollover":"fromSchedule","scope":"personal"}`
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

// "Bins out every Thursday night" lands on Thursday, and on no other
// day. A weekly routine with no weekday drifts to whatever day it was
// created on, which is the bug this exists to prevent.
func TestAWeeklyRoutineLandsOnItsOwnWeekday(t *testing.T) {
	h := homeHarness(t)
	h.s.Upkeep = &upkeep.Engine{DB: h.s.DB, State: h.s.State, Loc: time.UTC}
	h.person("sandy")

	// The next Thursday from today, which is what the page's anchor
	// computes and what fromSchedule then repeats from.
	now := time.Now().UTC()
	ahead := (int(time.Thursday) - int(now.Weekday()) + 7) % 7
	thursday := now.AddDate(0, 0, ahead)
	// One interval before the first due date: the anchor opens the
	// cycle and the threshold falls an interval later.
	anchor := thursday.AddDate(0, 0, -7).Format("2006-01-02")

	body := `{"description":"Bins out","about":"sandy","every":"1w","anchor":"` + anchor + `",` +
		`"partOfDay":"evening","rollover":"fromSchedule","scope":"personal"}`
	res := h.member(http.MethodPost, "/api/v1/home/commitments", "sandy", "homepage_users",
		stringsNewReader(body), "application/json")
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("adding a weekly routine: status %d", res.StatusCode)
	}

	got := h.agenda(t, "sandy", "sandy")
	var on []string
	for _, d := range got.Days {
		for _, it := range d.Items {
			if it.Title == "Bins out" {
				on = append(on, d.Date)
			}
		}
	}
	if len(on) != 1 {
		t.Fatalf("Bins out appears on %v, want exactly the one Thursday", on)
	}
	day, err := time.Parse("2006-01-02", on[0])
	if err != nil {
		t.Fatal(err)
	}
	if day.Weekday() != time.Thursday {
		t.Errorf("Bins out landed on a %s, want Thursday", day.Weekday())
	}
	if day.Format("2006-01-02") != thursday.Format("2006-01-02") {
		t.Errorf("landed on %s, want the Thursday %s", on[0], thursday.Format("2006-01-02"))
	}
}

// A meter-driven commitment has no date of its own and still has to be
// somewhere, or the agenda is quietly incomplete.
func TestAnUndatedCommitmentStillLandsOnToday(t *testing.T) {
	h := homeHarness(t)
	h.s.Upkeep = &upkeep.Engine{DB: h.s.DB, State: h.s.State, Loc: time.UTC}
	h.person("sandy")

	body := `{"description":"Read the odometer","about":"sandy","every":"1d",` +
		`"anchor":"` + time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02") + `",` +
		`"rollover":"fromSchedule","scope":"personal"}`
	res := h.member(http.MethodPost, "/api/v1/home/commitments", "sandy", "homepage_users",
		stringsNewReader(body), "application/json")
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("adding: %d", res.StatusCode)
	}
	got := h.agenda(t, "sandy", "sandy")
	if len(got.Days) == 0 || len(got.Days[0].Items) == 0 {
		t.Fatal("today is empty; a daily routine anchored today should be on it")
	}
}

// A holiday week moves the bins and leaves the schedule alone.
//
// This is the whole reason a temporary change is a BOOKING and not an
// edit: upkeep drops a booking once the cycle it was made for closes,
// so the shift is spent on one week and Thursday comes back by itself.
func TestMovingAChoreShiftsOneCycleAndNotTheSchedule(t *testing.T) {
	h := homeHarness(t)
	h.s.Upkeep = &upkeep.Engine{DB: h.s.DB, State: h.s.State, Loc: time.UTC}
	h.person("sandy")

	now := time.Now().UTC()
	ahead := (int(time.Thursday) - int(now.Weekday()) + 7) % 7
	thursday := now.AddDate(0, 0, ahead)
	anchor := thursday.AddDate(0, 0, -7).Format("2006-01-02")

	body := `{"description":"Bins out","about":"sandy","every":"1w","anchor":"` + anchor + `",` +
		`"partOfDay":"evening","rollover":"fromSchedule","scope":"personal"}`
	res := h.member(http.MethodPost, "/api/v1/home/commitments", "sandy", "homepage_users",
		stringsNewReader(body), "application/json")
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("adding: %d", res.StatusCode)
	}

	dayOf := func(title string) (string, bool) {
		got := h.agenda(t, "sandy", "sandy")
		for _, d := range got.Days {
			for _, it := range d.Items {
				if it.Title == title {
					return d.Date, it.Moved
				}
			}
		}
		return "", false
	}

	before, moved := dayOf("Bins out")
	if before != thursday.Format("2006-01-02") {
		t.Fatalf("starts on %s, want the Thursday %s", before, thursday.Format("2006-01-02"))
	}
	if moved {
		t.Error("nothing has been moved yet and the row says it has")
	}

	// The holiday: this week it goes out on the Friday.
	friday := thursday.AddDate(0, 0, 1)
	move := `{"at":"` + friday.Format("2006-01-02") + `T12:00:00Z"}`
	res = h.member(http.MethodPost, "/api/v1/home/commitments/bins-out/booked", "sandy", "homepage_users",
		stringsNewReader(move), "application/json")
	res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		t.Fatalf("moving: %d", res.StatusCode)
	}

	after, movedNow := dayOf("Bins out")
	if after != friday.Format("2006-01-02") {
		t.Errorf("after moving it sits on %s, want the Friday %s", after, friday.Format("2006-01-02"))
	}
	if !movedNow {
		t.Error("a moved cycle does not say it was moved, so it looks like a broken schedule")
	}

	// And the state is untouched: a chore does not become "booked"
	// because somebody shifted it by a day.
	standings, err := h.s.Upkeep.Evaluate()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range standings {
		if st.Name == "bins-out" && st.RequiresBooking {
			t.Error("shifting a chore turned it into something that requires an appointment")
		}
	}
}

// A routine can be corrected and removed from the same surface it was
// entered on, by the Member who entered it.
func TestAMemberEditsAndRemovesARoutine(t *testing.T) {
	h := homeHarness(t)
	h.s.Upkeep = &upkeep.Engine{DB: h.s.DB, State: h.s.State, Loc: time.UTC}
	h.person("sandy")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	add := `{"description":"Make bed","about":"sandy","every":"1d","anchor":"` + yesterday + `",` +
		`"partOfDay":"morning","rollover":"fromSchedule","scope":"personal"}`
	res := h.member(http.MethodPost, "/api/v1/home/commitments", "sandy", "homepage_users",
		stringsNewReader(add), "application/json")
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("adding: %d", res.StatusCode)
	}

	find := func(title string) (agendaItem, bool) {
		got := h.agenda(t, "sandy", "sandy")
		for _, d := range got.Days {
			for _, it := range d.Items {
				if it.Title == title {
					return it, true
				}
			}
		}
		return agendaItem{}, false
	}

	it, ok := find("Make bed")
	if !ok {
		t.Fatal("the routine is not on the day")
	}
	if it.Every != "1d" {
		t.Errorf("every = %q, want 1d — an edit form cannot open on what it does not know", it.Every)
	}

	// Correct the wording and move it to the evening. The part of day
	// has to actually change: PUT never applied it, so an edit form
	// would have offered a control that silently did nothing.
	edit := `{"description":"Make your bed","about":"sandy","every":"1d",` +
		`"partOfDay":"evening","rollover":"fromSchedule","scope":"personal"}`
	res = h.member(http.MethodPut, "/api/v1/home/commitments/make-bed", "sandy", "homepage_users",
		stringsNewReader(edit), "application/json")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("editing: %d", res.StatusCode)
	}
	edited, ok := find("Make your bed")
	if !ok {
		t.Fatal("the edited routine is gone from the day")
	}
	if edited.PartOfDay != "evening" {
		t.Errorf("partOfDay = %q after editing, want evening", edited.PartOfDay)
	}

	// And it can be taken away.
	res = h.member(http.MethodDelete, "/api/v1/home/commitments/make-bed", "sandy", "homepage_users", nil, "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNoContent {
		t.Fatalf("removing: %d", res.StatusCode)
	}
	if _, still := find("Make your bed"); still {
		t.Error("the routine is still on the day after being removed")
	}
}

// Selecting a subject grants no standing the reader did not hold
// (ADR-W033 A10). The rule is about standing, not about hardware —
// nothing tells the server whether a screen is being handed round, so
// it has to hold identically in every session.
func TestTheSubjectSelectorGrantsNoStanding(t *testing.T) {
	h := homeHarness(t)
	h.s.Upkeep = &upkeep.Engine{DB: h.s.DB, State: h.s.State, Loc: time.UTC}
	h.person("sandy", "mika")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	// Mika's own personal routine.
	body := `{"description":"Take the tablets","about":"mika","every":"1d","anchor":"` + yesterday + `",` +
		`"partOfDay":"morning","rollover":"fromSchedule","scope":"personal"}`
	res := h.member(http.MethodPost, "/api/v1/home/commitments", "mika", "homepage_users",
		stringsNewReader(body), "application/json")
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("adding: %d", res.StatusCode)
	}

	// Mika sees it on his own day.
	if !contains(agendaTitles(h.agenda(t, "mika", "mika")), "Take the tablets") {
		t.Error("a member cannot see their own personal routine")
	}
	// Sandy, selecting Mika, does not — she is not his guardian and
	// the picker is not a way to become one.
	if contains(agendaTitles(h.agenda(t, "sandy", "mika")), "Take the tablets") {
		t.Error("selecting another adult disclosed their personal routine")
	}
}

// Ticking off a weekly chore must not make it disappear.
//
// A weekly routine completed today reopens exactly a week out, and the
// day window rendered today..today+6 — so Done dropped the bins off
// the end of the agenda entirely. Not just invisible: the row is where
// Edit and Remove live, so the household could not reach it at all.
func TestCompletingAWeeklyRoutineDoesNotHideIt(t *testing.T) {
	h := homeHarness(t)
	h.s.Upkeep = &upkeep.Engine{DB: h.s.DB, State: h.s.State, Loc: time.UTC}
	h.person("sandy")

	// Anchored a week back, so it is due today and completing it opens
	// the next cycle exactly agendaDays out — the boundary case.
	anchor := time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02")
	body := `{"description":"Bins out","about":"sandy","every":"1w","anchor":"` + anchor + `",` +
		`"partOfDay":"evening","rollover":"fromSchedule","scope":"personal"}`
	res := h.member(http.MethodPost, "/api/v1/home/commitments", "sandy", "homepage_users",
		stringsNewReader(body), "application/json")
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("adding: %d", res.StatusCode)
	}
	if !contains(agendaTitles(h.agenda(t, "sandy", "sandy")), "Bins out") {
		t.Fatal("not on the agenda before completing it")
	}

	res = h.member(http.MethodPost, "/api/v1/home/commitments/bins-out/done", "sandy", "homepage_users",
		stringsNewReader(`{}`), "application/json")
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("completing: %d", res.StatusCode)
	}

	after := h.agenda(t, "sandy", "sandy")
	if !contains(agendaTitles(after), "Bins out") {
		t.Fatal("the routine vanished after being ticked off; there is now no way to edit or remove it")
	}
	// And it comes back a week out rather than sitting on today.
	for _, d := range after.Days {
		for _, it := range d.Items {
			if it.Title != "Bins out" {
				continue
			}
			day, err := time.Parse("2006-01-02", d.Date)
			if err != nil {
				t.Fatal(err)
			}
			if !day.After(time.Now().UTC().AddDate(0, 0, 5)) {
				t.Errorf("the next cycle landed on %s, want about a week out", d.Date)
			}
		}
	}
}

// A guardian can actually tick off their child's routine.
//
// The agenda offered the button and the answer endpoint refused it:
// Done was computed with guardian standing and /done authorized on
// mayComplete alone. And because a child cannot sign in, an
// unnarrowed personal routine of theirs had NOBODY able to complete
// it — the guarantee A9 makes, unimplemented on the write path.
func TestAGuardianCanCompleteTheirChildsRoutine(t *testing.T) {
	h := homeHarness(t)
	h.s.Upkeep = &upkeep.Engine{DB: h.s.DB, State: h.s.State, Loc: time.UTC}
	h.person("sandy")
	h.child("maya", "sandy")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	body := `{"description":"Make your bed","about":"maya","every":"1d","anchor":"` + yesterday + `",` +
		`"partOfDay":"morning","rollover":"fromSchedule","scope":"personal"}`
	res := h.member(http.MethodPost, "/api/v1/home/commitments", "sandy", "homepage_users",
		stringsNewReader(body), "application/json")
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("adding: %d", res.StatusCode)
	}

	// Offered...
	var offered bool
	for _, d := range h.agenda(t, "sandy", "maya").Days {
		for _, it := range d.Items {
			if it.Title == "Make your bed" {
				offered = it.Done
			}
		}
	}
	if !offered {
		t.Fatal("the guardian is not offered the tick")
	}
	// ...and accepted. This is the pair that was broken.
	res = h.member(http.MethodPost, "/api/v1/home/commitments/make-your-bed/done",
		"sandy", "homepage_users", stringsNewReader(`{}`), "application/json")
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("the guardian's tick was refused: %d — the agenda offers a button the API rejects", res.StatusCode)
	}

	// An unrelated adult still cannot.
	h.person("bob")
	res = h.member(http.MethodPost, "/api/v1/home/commitments/make-your-bed/done",
		"bob", "homepage_users", stringsNewReader(`{}`), "application/json")
	res.Body.Close()
	if res.StatusCode == http.StatusCreated {
		t.Error("an unrelated adult completed somebody else's child's routine")
	}
}

// Edit is offered only where the routine form can put the record back.
//
// A commitment that requires a booking holds more than the form sends,
// and homeEditCommitment rewrites those fields from its input — so a
// dentist appointment offered the same Edit button would become a
// daily routine the moment somebody corrected its title.
func TestEditIsNotOfferedWhereItWouldLoseFields(t *testing.T) {
	h := homeHarness(t)
	h.s.Upkeep = &upkeep.Engine{DB: h.s.DB, State: h.s.State, Loc: time.UTC}
	h.person("sandy")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	routine := `{"description":"Make bed","about":"sandy","every":"1d","anchor":"` + yesterday + `",` +
		`"rollover":"fromSchedule","scope":"personal"}`
	res := h.member(http.MethodPost, "/api/v1/home/commitments", "sandy", "homepage_users",
		stringsNewReader(routine), "application/json")
	res.Body.Close()

	appointment := `{"description":"Dentist","about":"sandy","every":"1d","anchor":"` + yesterday + `",` +
		`"rollover":"fromSchedule","scope":"personal","requiresBooking":true}`
	res = h.member(http.MethodPost, "/api/v1/home/commitments", "sandy", "homepage_users",
		stringsNewReader(appointment), "application/json")
	res.Body.Close()

	for _, d := range h.agenda(t, "sandy", "sandy").Days {
		for _, it := range d.Items {
			switch it.Title {
			case "Make bed":
				if !it.Editable {
					t.Error("a plain routine is not editable, so the form is offered nowhere")
				}
			case "Dentist":
				if it.Editable {
					t.Error("a booking commitment is editable by the routine form, which would strip its booking")
				}
			}
		}
	}
}

// Day labels survive a clock change. The local midnights either side of
// a spring-forward are 23 hours apart, and dividing by 24 made tomorrow
// "Today".
func TestDayLabelsSurviveADaylightSavingChange(t *testing.T) {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	// 2026-03-08 is the US spring-forward.
	today := time.Date(2026, 3, 7, 0, 0, 0, 0, loc)
	if got := agendaLabel(today, today); got != "Today" {
		t.Errorf("today = %q", got)
	}
	if got := agendaLabel(today.AddDate(0, 0, 1), today); got != "Tomorrow" {
		t.Errorf("the day after a 23-hour night = %q, want Tomorrow", got)
	}
	if got := agendaLabel(today.AddDate(0, 0, 2), today); got == "Today" || got == "Tomorrow" {
		t.Errorf("two days out = %q", got)
	}
}
