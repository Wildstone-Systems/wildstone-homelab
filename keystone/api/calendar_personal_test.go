package api

// A member's own calendars on the Calendar page (ADR-W034 C2), and the
// Person-keyed shared grants beside them (#372).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/caldav"
)

// fakeAccount is one person's principal: the calendars under it, each a
// fakeBook of its own.
type fakeAccount struct {
	fakeBook
	calendars []caldav.Collection
	books     map[string]*fakeBook
	// missing is a principal nobody has signed in to yet.
	missing bool
	moved   []string
}

func (a *fakeAccount) Calendars(_ context.Context, principal string) ([]caldav.Collection, error) {
	if a.missing {
		return nil, nil
	}
	return a.calendars, nil
}
func (a *fakeAccount) MakeCalendar(_ context.Context, collection, name, color string) error {
	if a.missing {
		return caldav.ErrNoPrincipal
	}
	u, _ := url.Parse(collection)
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	a.calendars = append(a.calendars, caldav.Collection{Segment: parts[len(parts)-1], URL: collection, Name: name, Color: color})
	return nil
}
func (a *fakeAccount) MakePrincipal(context.Context, string) error {
	a.missing = false
	return nil
}
func (a *fakeAccount) MoveEvent(_ context.Context, href, destination string) (string, error) {
	a.moved = append(a.moved, destination)
	return destination + "moved.ics", nil
}

// isPrincipalURL recognises a person's principal on the test origin.
func isPrincipalURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	path := strings.Trim(p.Path, "/")
	return path != "" && !strings.Contains(path, "/") && path != "GROUPS"
}

// personalHarness gives sam and alex an account each, and routes every
// address the server asks for to the right fake. Anything else fails the
// test: that is the property that one member's page never reaches another
// member's calendars.
func personalHarness(t *testing.T) (*harness, map[string]*fakeAccount) {
	h, house, school, reg := sharedCalendarHarness(t)
	schoolURL, _ := reg.Calendars[1].CollectionURL(testCollection)
	accounts := map[string]*fakeAccount{
		"sam":  {calendars: []caldav.Collection{{Segment: "personal", URL: "http://radicale:5232/sam/personal/", Name: "My calendar", Color: ""}}, books: map[string]*fakeBook{"personal": {}}},
		"alex": {calendars: []caldav.Collection{{Segment: "secret", URL: "http://radicale:5232/alex/secret/", Name: "Alex only", Color: "#123456"}}, books: map[string]*fakeBook{"secret": {}}},
	}
	h.s.CalendarBookFor = func(u string) CalendarBook {
		switch u {
		case testCollection:
			return house
		case schoolURL:
			return school
		}
		p, _ := url.Parse(u)
		parts := strings.Split(strings.Trim(p.Path, "/"), "/")
		account, ok := accounts[parts[0]]
		if !ok {
			t.Fatalf("a calendar outside the fixture was reached: %s", u)
		}
		if len(parts) == 1 {
			return account
		}
		if b, ok := account.books[parts[1]]; ok {
			return &struct {
				*fakeBook
				*fakeAccount
			}{b, account}
		}
		return account
	}
	return h, accounts
}

func (h *harness) calendarGet(member, query string) map[string]json.RawMessage {
	h.t.Helper()
	h.unthrottled()
	r := h.member("GET", "/api/v1/calendar/events?from=2026-09-01&to=2026-09-30"+query, member, "calendar_users", nil, "")
	defer r.Body.Close()
	var b map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		h.t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK {
		h.t.Fatalf("GET %s: %d %s", query, r.StatusCode, b["error"])
	}
	return b
}

func (h *harness) calendarSend(member, path, body string) (int, map[string]any) {
	h.t.Helper()
	h.unthrottled()
	r := h.member("POST", path, member, "calendar_users", strings.NewReader(body), "application/json")
	defer r.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(r.Body).Decode(&out)
	return r.StatusCode, out
}

func TestAMembersOwnCalendarsAreTheirsAlone(t *testing.T) {
	h, accounts := personalHarness(t)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	accounts["sam"].books["personal"].events = []caldav.Event{{Summary: "Dentist", Start: at, Href: "/sam/personal/dentist.ics"}}
	accounts["alex"].books["secret"].events = []caldav.Event{{Summary: "Surprise party", Start: at, Href: "/alex/secret/party.ics"}}

	b := h.calendarGet("sam", "")
	var choices []calendarChoice
	json.Unmarshal(b["calendars"], &choices)
	var own *calendarChoice
	for i, c := range choices {
		if c.Personal {
			own = &choices[i]
		}
		if c.ID == "personal:alex/secret" {
			t.Fatal("sam was offered alex's calendar")
		}
	}
	if own == nil || own.ID != "personal:sam/personal" || !own.Writable || own.Color != defaultPersonalColor {
		t.Fatalf("sam's own calendar = %+v in %+v", own, choices)
	}
	var events []calendarPageEvent
	json.Unmarshal(b["events"], &events)
	found := false
	for _, e := range events {
		if e.Title == "Surprise party" {
			t.Fatal("sam saw alex's event")
		}
		if e.Title == "Dentist" && e.Personal && e.Writable {
			found = true
		}
	}
	if !found {
		t.Fatalf("sam's own event is missing: %+v", events)
	}

	// Naming somebody else's calendar reaches nothing.
	for _, id := range []string{"personal:alex/secret", "personal:sam/../alex/secret", "personal:"} {
		h.unthrottled()
		r := h.member("GET", "/api/v1/calendar/events?from=2026-09-01&to=2026-09-30&calendar="+url.QueryEscape(id), "sam", "calendar_users", nil, "")
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Errorf("reading %q answered %d", id, r.StatusCode)
		}
		if status, _ := h.calendarSend("sam", "/api/v1/calendar/events", `{"calendar":"`+id+`","title":"x","date":"2026-09-21","allDay":true}`); status != http.StatusForbidden {
			t.Errorf("writing %q answered %d", id, status)
		}
	}
	if len(accounts["alex"].books["secret"].writes) != 0 {
		t.Fatal("alex's calendar was written")
	}

	// Their own is theirs to write, and the household activity does not
	// repeat what is on it.
	if status, out := h.calendarSend("sam", "/api/v1/calendar/events", `{"calendar":"personal:sam/personal","title":"Therapy","date":"2026-09-21","allDay":true}`); status != http.StatusOK {
		t.Fatal(status, out)
	}
	if len(accounts["sam"].books["personal"].writes) != 1 {
		t.Fatal("the event did not reach sam's calendar")
	}
	evs, err := h.s.DB.EventsAfter(0, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if strings.Contains(e.Message, "Therapy") {
			t.Fatalf("household activity names a personal event: %q", e.Message)
		}
	}

	// Hiding it is remembered like any other calendar.
	if status, out := h.calendarSend("sam", "/api/v1/calendar/visibility", `{"calendar":"personal:sam/personal","visible":false}`); status != http.StatusOK {
		t.Fatal(status, out)
	}
	json.Unmarshal(h.calendarGet("sam", "")["events"], &events)
	for _, e := range events {
		if e.Personal {
			t.Fatal("a hidden personal calendar was still shown")
		}
	}
	if status, _ := h.calendarSend("sam", "/api/v1/calendar/visibility", `{"calendar":"personal:alex/secret","visible":false}`); status != http.StatusForbidden {
		t.Fatal("stored a preference for somebody else's calendar")
	}
}

func TestAnEventDoesNotCrossBetweenSharedAndPersonal(t *testing.T) {
	h, accounts := personalHarness(t)
	status, out := h.calendarSend("sam", "/api/v1/calendar/events/update",
		`{"calendar":"household","targetCalendar":"personal:sam/personal","href":"/GROUPS/Y2FsZW5kYXJfdXNlcnM=/x.ics","title":"x","date":"2026-09-21","allDay":true}`)
	if status != http.StatusBadRequest {
		t.Fatal(status, out)
	}
	status, out = h.calendarSend("sam", "/api/v1/calendar/events/update",
		`{"calendar":"personal:sam/personal","targetCalendar":"household","href":"/sam/personal/x.ics","title":"x","date":"2026-09-21","allDay":true}`)
	if status != http.StatusBadRequest {
		t.Fatal(status, out)
	}
	if len(accounts["sam"].moved) != 0 {
		t.Fatal("an event crossed")
	}
}

func TestCreatingAPersonalCalendar(t *testing.T) {
	h, accounts := personalHarness(t)
	accounts["sam"].missing = true
	accounts["sam"].calendars = nil
	status, out := h.calendarSend("sam", "/api/v1/calendar/personal", `{"name":"Work shifts","color":"#AA3300"}`)
	if status != http.StatusOK {
		t.Fatal(status, out)
	}
	if out["personal"] != true || out["name"] != "Work shifts" || out["color"] != "#aa3300" || !strings.HasPrefix(out["id"].(string), personalCalendarPrefix) {
		t.Fatal(out)
	}
	cals := accounts["sam"].calendars
	if len(cals) != 1 || !strings.HasPrefix(cals[0].URL, "http://radicale:5232/sam/") || cals[0].Name != "Work shifts" {
		t.Fatalf("created %+v", cals)
	}
	for _, bad := range []string{`{"name":""}`, `{"name":"x","color":"red"}`, `{"name":"` + strings.Repeat("x", 121) + `"}`, `{"name":"a\u0000b"}`} {
		if status, _ := h.calendarSend("sam", "/api/v1/calendar/personal", bad); status != http.StatusBadRequest {
			t.Errorf("%s answered %d", bad, status)
		}
	}
}

// A rename does not lose the calendars made under the old login: Radicale
// keeps them there, and the page still lists them, while a new calendar
// is made under the login the member now signs in with.
func TestPersonalCalendarsSurviveARename(t *testing.T) {
	h, accounts := personalHarness(t)
	accounts["samuel"] = &fakeAccount{missing: true, books: map[string]*fakeBook{}}
	if err := h.s.DB.RenameButlerMemberHandle("sam", "samuel"); err != nil {
		t.Fatal(err)
	}
	var choices []calendarChoice
	json.Unmarshal(h.calendarGet("samuel", "")["calendars"], &choices)
	found := false
	for _, c := range choices {
		found = found || c.ID == "personal:sam/personal"
	}
	if !found {
		t.Fatalf("the calendar made before the rename is gone: %+v", choices)
	}
	if status, out := h.calendarSend("samuel", "/api/v1/calendar/personal", `{"name":"New"}`); status != http.StatusOK ||
		!strings.HasPrefix(out["id"].(string), "personal:samuel/") {
		t.Fatal(status, out)
	}
	if len(accounts["samuel"].calendars) != 1 || len(accounts["sam"].calendars) != 1 {
		t.Fatal("the new calendar was not made under the current login")
	}
}

// Somebody who signs in under a name a renamed Person used to carry is
// not shown, and cannot write, that Person's calendars (#426 review).
func TestAnOldLoginDoesNotReachItsPersonsCalendars(t *testing.T) {
	h, accounts := personalHarness(t)
	accounts["samuel"] = &fakeAccount{books: map[string]*fakeBook{}}
	if err := h.s.DB.RenameButlerMemberHandle("sam", "samuel"); err != nil {
		t.Fatal(err)
	}
	// "sam" is now a directory account with no Person, which the portal
	// door admits.
	b := h.calendarGet("sam", "")
	var choices []calendarChoice
	json.Unmarshal(b["calendars"], &choices)
	for _, c := range choices {
		if c.Personal {
			t.Fatalf("the old login was shown %+v", c)
		}
	}
	if len(b["personalNote"]) == 0 {
		t.Fatal("the page did not say why")
	}
	if status, _ := h.calendarSend("sam", "/api/v1/calendar/events", `{"calendar":"personal:sam/personal","title":"x","date":"2026-09-21","allDay":true}`); status != http.StatusForbidden {
		t.Fatalf("the old login wrote the Person's calendar: %d", status)
	}
	if status, _ := h.calendarSend("sam", "/api/v1/calendar/personal", `{"name":"x"}`); status != http.StatusForbidden {
		t.Fatalf("the old login made a calendar in the Person's account: %d", status)
	}
	if len(accounts["sam"].books["personal"].writes) != 0 || len(accounts["sam"].calendars) != 1 {
		t.Fatal("the Person's account was changed")
	}
}

// A shared grant written for somebody follows them through a rename.
func TestASharedGrantFollowsItsPersonThroughARename(t *testing.T) {
	h, _, _, reg := sharedCalendarHarness(t)
	if err := h.s.DB.RenameButlerMemberHandle("alex", "alexandra"); err != nil {
		t.Fatal(err)
	}
	restricted := reg.Calendars[2]
	if !h.s.calendarAllows(restricted, "alexandra", nil, true) {
		t.Fatal("the renamed member lost the restricted calendar")
	}
	if h.s.calendarAllows(restricted, "sam", nil, false) {
		t.Fatal("somebody else gained it")
	}
}
