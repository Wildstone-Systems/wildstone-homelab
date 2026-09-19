package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

const testCollection = "http://radicale:5232/GROUPS/Y2FsZW5kYXJfdXNlcnM=/"

// fakeBook is the shared calendar, recording what the page asked of it.
type fakeBook struct {
	events  []caldav.Event
	writes  []caldav.EventWrite
	deletes []string
}

func (f *fakeBook) Events(context.Context, time.Time, time.Time) ([]caldav.Event, error) {
	return f.events, nil
}
func (f *fakeBook) CreateEvent(_ context.Context, w caldav.EventWrite) (adapter.Answer, error) {
	f.writes = append(f.writes, w)
	return adapter.Answer{Data: map[string]any{"uid": "new"}}, nil
}
func (f *fakeBook) UpdateEvent(_ context.Context, w caldav.EventWrite) (adapter.Answer, error) {
	f.writes = append(f.writes, w)
	return adapter.Answer{Data: map[string]any{"href": w.Href}}, nil
}
func (f *fakeBook) DeleteEvent(_ context.Context, w caldav.EventWrite) (adapter.Answer, error) {
	f.deletes = append(f.deletes, w.Href)
	return adapter.Answer{Data: map[string]any{"href": w.Href}}, nil
}

// calendarHarness is a household with the calendar installed — its
// LdapGroup is what the surface is gated on — and Keystone given the
// shared collection.
func calendarHarness(t *testing.T) (*harness, *fakeBook) {
	t.Helper()
	h := newHarness(t)
	h.s.TrustedIngress = []string{"127.0.0.1", "::1"}
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: "wildstone.dev/v1", Kind: resource.KindLdapGroup,
		Metadata:  resource.Metadata{Name: "calendar-users", Owner: "capability/calendar", Package: "calendar-radicale", Node: "n1"},
		LdapGroup: &resource.LdapGroupSpec{Name: "calendar_users"},
	}); err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	book := &fakeBook{}
	h.s.HouseholdLoc = loc
	h.s.CalendarCollectionURL = testCollection
	h.s.CalendarBook = book
	return h, book
}

func (h *harness) calendarPost(path string, body any) (*http.Response, map[string]any) {
	h.t.Helper()
	b, _ := json.Marshal(body)
	res := h.member(http.MethodPost, path, "mika", "calendar_users", strings.NewReader(string(b)), "application/json")
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

// The page is gated on the calendar's own group, and is absent — not
// forbidden — where no calendar is installed.
func TestTheCalendarPageIsForTheCalendarsGroup(t *testing.T) {
	h, _ := calendarHarness(t)
	for _, c := range []struct {
		groups string
		want   int
	}{
		{"calendar_users", http.StatusOK},
		{"homepage_users", http.StatusForbidden},
	} {
		h.unthrottled()
		res := h.member(http.MethodGet, "/calendar/", "mika", c.groups, nil, "")
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Errorf("GET /calendar/ in [%s] = %d, want %d", c.groups, res.StatusCode, c.want)
		}
	}

	bare := newHarness(t)
	bare.s.TrustedIngress = []string{"127.0.0.1", "::1"}
	res := bare.member(http.MethodGet, "/calendar/", "mika", "calendar_users", nil, "")
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /calendar/ with no calendar installed = %d, want 404", res.StatusCode)
	}
}

// A time typed on the page is the household's wall clock, whatever the
// browser's clock says: 18:00 on a September Sunday in Chicago is 18:00
// CDT, and a half-hour end is kept.
func TestAnEventIsWrittenAtTheHouseholdsTime(t *testing.T) {
	h, book := calendarHarness(t)
	res, out := h.calendarPost("/api/v1/calendar/events", map[string]any{
		"title": "Dinner at Nonna's", "date": "2026-09-20", "startTime": "18:00", "endTime": "19:30",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("create = %d: %v", res.StatusCode, out)
	}
	if len(book.writes) != 1 {
		t.Fatalf("want one write, got %d", len(book.writes))
	}
	w := book.writes[0]
	if w.Start != "2026-09-20T18:00:00-05:00" || w.End != "2026-09-20T19:30:00-05:00" || w.AllDay {
		t.Errorf("written window = %s..%s allDay=%v, want 18:00-19:30 CDT", w.Start, w.End, w.AllDay)
	}
	if w.Summary == nil || *w.Summary != "Dinner at Nonna's" {
		t.Errorf("summary = %v", w.Summary)
	}
}

// iCalendar's all-day end is exclusive: a two-day trip on the 20th and
// 21st ends on the 22nd, and a one-day event ends the day after it.
func TestAnAllDayEventEndsTheDayAfterItsLastDay(t *testing.T) {
	h, book := calendarHarness(t)
	for _, c := range []struct {
		body       map[string]any
		start, end string
	}{
		{map[string]any{"title": "Birthday", "date": "2026-09-20", "allDay": true}, "2026-09-20", "2026-09-21"},
		{map[string]any{"title": "Trip", "date": "2026-09-20", "endDate": "2026-09-21", "allDay": true}, "2026-09-20", "2026-09-22"},
	} {
		book.writes = nil
		if res, out := h.calendarPost("/api/v1/calendar/events", c.body); res.StatusCode != http.StatusOK {
			t.Fatalf("create %v = %d: %v", c.body, res.StatusCode, out)
		}
		if w := book.writes[0]; w.Start != c.start || w.End != c.end || !w.AllDay {
			t.Errorf("%v wrote %s..%s allDay=%v, want %s..%s", c.body["title"], w.Start, w.End, w.AllDay, c.start, c.end)
		}
	}

	if res, _ := h.calendarPost("/api/v1/calendar/events", map[string]any{
		"title": "Backwards", "date": "2026-09-20", "startTime": "19:00", "endTime": "18:00",
	}); res.StatusCode != http.StatusBadRequest {
		t.Errorf("an event ending before it starts = %d, want 400", res.StatusCode)
	}
}

// The adapter follows any absolute href with the calendar's credential
// attached. A browser can send anything, so only a path naming one
// event directly inside the household collection reaches it.
func TestAnEventIsOnlyAddressedInsideTheHouseholdCollection(t *testing.T) {
	h, book := calendarHarness(t)
	for _, href := range []string{
		"https://attacker.example/steal.ics",
		"//attacker.example/steal.ics",
		"/GROUPS/b3RoZXI=/x.ics",
		"/GROUPS/Y2FsZW5kYXJfdXNlcnM=/../b3RoZXI=/x.ics",
		"/GROUPS/Y2FsZW5kYXJfdXNlcnM=/sub/x.ics",
		"/GROUPS/Y2FsZW5kYXJfdXNlcnM=/",
		"relative.ics",
		"",
	} {
		h.unthrottled()
		res, _ := h.calendarPost("/api/v1/calendar/events/delete", map[string]any{"href": href})
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("delete href %q = %d, want 400", href, res.StatusCode)
		}
	}
	if len(book.deletes) != 0 {
		t.Fatalf("a refused href still reached the calendar: %v", book.deletes)
	}

	h.unthrottled()
	res, out := h.calendarPost("/api/v1/calendar/events/delete",
		map[string]any{"href": "/GROUPS/Y2FsZW5kYXJfdXNlcnM=/abc-123.ics", "title": "Dinner"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("delete of an event in the collection = %d: %v", res.StatusCode, out)
	}
	if want := "http://radicale:5232/GROUPS/Y2FsZW5kYXJfdXNlcnM=/abc-123.ics"; len(book.deletes) != 1 || book.deletes[0] != want {
		t.Errorf("deleted %v, want [%s]", book.deletes, want)
	}
}

// Events come back filed under the household's days: an all-day DATE
// is read as the day it names, a multi-day one reports its last day
// (DTEND is exclusive), and a timed event late in the evening stays on
// that evening rather than sliding into tomorrow's UTC date.
func TestEventsAreFiledUnderTheHouseholdsDays(t *testing.T) {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	trip := pageEvent(caldav.Event{
		Summary: "Trip", AllDay: true,
		Start: time.Date(2026, 9, 20, 0, 0, 0, 0, loc), End: time.Date(2026, 9, 22, 0, 0, 0, 0, loc),
	}, loc)
	if trip.Day != "2026-09-20" || trip.LastDay != "2026-09-21" || trip.StartTime != "" {
		t.Errorf("all-day trip = %+v, want 20th to 21st with no times", trip)
	}
	late := pageEvent(caldav.Event{
		Summary: "Late film",
		Start:   time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC), // 22:00 CDT on the 20th
		End:     time.Date(2026, 9, 21, 5, 0, 0, 0, time.UTC), // midnight CDT
	}, loc)
	if late.Day != "2026-09-20" || late.StartTime != "22:00" || late.LastDay != "2026-09-20" {
		t.Errorf("late event = %+v, want the 20th, 22:00, ending that night", late)
	}
}

// Without the calendar's sign-in the page says so in words, rather
// than drawing an empty month that reads as a free one.
func TestThePageSaysWhenItHasNoSignIn(t *testing.T) {
	h, _ := calendarHarness(t)
	h.s.CalendarBook = nil
	res := h.member(http.MethodGet, "/api/v1/calendar/events?from=2026-09-01&to=2026-09-30", "mika", "calendar_users", nil, "")
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(out["error"].(string), "sign-in") {
		t.Errorf("no book = %d %v, want 503 naming the missing sign-in", res.StatusCode, out)
	}
}

// The home card opens Wildstone's page when there is one, and falls
// back to the server's own address only when there is not.
func TestTheCardOpensTheCalendarPage(t *testing.T) {
	desired := []resource.Resource{ourRoute}
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	s := &Server{CalendarCollectionURL: testCollection}
	s.CalendarEvents = func(time.Time, time.Time) ([]CalendarCardEvent, error) { return nil, nil }
	if card := s.homeCalendar("mika", desired, now); card.Page != "" {
		t.Errorf("with no book the card offered page %q", card.Page)
	}
	s.CalendarBook = &fakeBook{}
	if card := s.homeCalendar("mika", desired, now); card.Page != "/calendar/" {
		t.Errorf("card.Page = %q, want /calendar/", card.Page)
	}
}

// Through the real response path, redactor included. A CalDAV server
// names an event's file after its UID — token-shaped — and the API's
// redactor replaced it with [REDACTED], so the page asked to remove a
// file that did not exist and Radicale answered 404. The address must
// come back exactly as the server reported it, and a delete made with
// it must reach that file.
func TestAnEventsAddressSurvivesTheResponseAndCanBeRemoved(t *testing.T) {
	h, book := calendarHarness(t)
	const reported = "/GROUPS/Y2FsZW5kYXJfdXNlcnM%3D/wildstone-8f14e45fceea167a5a36dedd4bea2543.ics"
	loc := h.s.HouseholdLoc
	book.events = []caldav.Event{{
		Href: reported, Summary: "Work Day", AllDay: true,
		Start: time.Date(2026, 9, 19, 0, 0, 0, 0, loc), End: time.Date(2026, 9, 20, 0, 0, 0, 0, loc),
	}}

	res := h.member(http.MethodGet, "/api/v1/calendar/events?from=2026-09-01&to=2026-09-30", "mika", "calendar_users", nil, "")
	var out struct {
		Events []map[string]any `json:"events"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	if len(out.Events) != 1 || out.Events[0]["calendarEvent"] != reported {
		t.Fatalf("the event's address came back as %v, want %q", out.Events, reported)
	}

	h.unthrottled()
	if res, body := h.calendarPost("/api/v1/calendar/events/delete",
		map[string]any{"href": out.Events[0]["calendarEvent"], "title": "Work Day"}); res.StatusCode != http.StatusOK {
		t.Fatalf("delete with the address the page was given = %d: %v", res.StatusCode, body)
	}
	if want := "http://radicale:5232/GROUPS/Y2FsZW5kYXJfdXNlcnM=/wildstone-8f14e45fceea167a5a36dedd4bea2543.ics"; len(book.deletes) != 1 || book.deletes[0] != want {
		t.Errorf("deleted %v, want [%s]", book.deletes, want)
	}
}

// The page shows an event's place and note, sends a new one's only when
// filled in, and on a change always sends both — so emptying the place
// on the page clears it instead of leaving it behind.
func TestThePagesPlaceAndNotesRoundTrip(t *testing.T) {
	h, book := calendarHarness(t)
	loc := h.s.HouseholdLoc
	book.events = []caldav.Event{{
		Href: "/GROUPS/Y2FsZW5kYXJfdXNlcnM%3D/w.ics", Summary: "Work Day",
		Start: time.Date(2026, 9, 19, 6, 0, 0, 0, loc), End: time.Date(2026, 9, 19, 19, 0, 0, 0, loc),
		Location: "At work", Description: "Take out the trash",
	}}
	res := h.member(http.MethodGet, "/api/v1/calendar/events?from=2026-09-19&to=2026-09-19", "mika", "calendar_users", nil, "")
	var out struct {
		Events []map[string]any `json:"events"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	if len(out.Events) != 1 || out.Events[0]["location"] != "At work" || out.Events[0]["notes"] != "Take out the trash" {
		t.Fatalf("the page was not given the place and note: %v", out.Events)
	}

	h.unthrottled()
	if res, body := h.calendarPost("/api/v1/calendar/events", map[string]any{
		"title": "Dentist", "date": "2026-09-22", "startTime": "09:00", "endTime": "09:30",
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("create = %d: %v", res.StatusCode, body)
	}
	if w := book.writes[0]; w.Location != nil || w.Description != nil {
		t.Errorf("an add with no place or note still sent them: %v %v", w.Location, w.Description)
	}

	h.unthrottled()
	if res, body := h.calendarPost("/api/v1/calendar/events/update", map[string]any{
		"href": "/GROUPS/Y2FsZW5kYXJfdXNlcnM%3D/w.ics", "title": "Work Day", "date": "2026-09-19",
		"startTime": "06:00", "endTime": "19:00", "location": "", "notes": "Take out the trash",
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("update = %d: %v", res.StatusCode, body)
	}
	w := book.writes[1]
	if w.Location == nil || *w.Location != "" {
		t.Errorf("an emptied place was not sent as a clear: %v", w.Location)
	}
	if w.Description == nil || *w.Description != "Take out the trash" {
		t.Errorf("the note was not sent: %v", w.Description)
	}
	if w.Start != "2026-09-19T06:00:00-05:00" || w.End != "2026-09-19T19:00:00-05:00" {
		t.Errorf("the times moved: %s..%s", w.Start, w.End)
	}
}
