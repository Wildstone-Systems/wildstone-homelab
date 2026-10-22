package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/caldav"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/sharedcalendar"
)

func sharedCalendarHarness(t *testing.T) (*harness, *fakeBook, *fakeBook, sharedcalendar.Registry) {
	h, house := calendarHarness(t)
	reg := sharedcalendar.Legacy()
	reg.Calendars = append(reg.Calendars, sharedcalendar.Calendar{ID: "school", Name: "School", Color: "#f59e0b", ReadGroups: []string{"calendar_users"}, WriteGroups: []string{"adults"}}, sharedcalendar.Calendar{ID: "restricted", Name: "Restricted", Color: "#000000", ReadMembers: []string{"alex"}, WriteMembers: []string{"alex"}})
	err := h.s.State.Put(resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindConfigFile, Metadata: resource.Metadata{Name: sharedcalendar.ConfigName, Node: "n1", Owner: "capability/calendar"}, ConfigFile: &resource.ConfigFileSpec{Filename: "calendars.json", Content: reg.JSON()}})
	if err != nil {
		t.Fatal(err)
	}
	// Every member is a Person on record, as in any provisioned household:
	// display choices and Person-keyed grants are stored against one.
	for _, m := range []string{"sam", "mika", "alex"} {
		if err := h.s.DB.RecordButlerMember(m, "member", "", ""); err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.AdmitButlerMember(m, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	school := &fakeBook{}
	schoolURL, _ := reg.Calendars[1].CollectionURL(testCollection)
	h.s.CalendarBookFor = func(u string) CalendarBook {
		if u == testCollection {
			return house
		}
		if u == schoolURL {
			return school
		}
		if isPrincipalURL(u) {
			return &fakeAccount{}
		}
		t.Fatalf("unauthorized calendar reached upstream: %s", u)
		return nil
	}
	return h, house, school, reg
}
func TestSharedCalendarReadsAndVisibilityArePerMember(t *testing.T) {
	h, house, school, _ := sharedCalendarHarness(t)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	house.events = []caldav.Event{{Summary: "Family", Start: at.Add(time.Hour), Href: "/GROUPS/Y2FsZW5kYXJfdXNlcnM=/family.ics"}}
	school.events = []caldav.Event{{Summary: "School", Start: at, Href: "/school.ics"}}
	list := func(member string) map[string]json.RawMessage {
		t.Helper()
		h.unthrottled()
		r := h.member("GET", "/api/v1/calendar/events?from=2026-09-01&to=2026-09-30", member, "calendar_users", nil, "")
		defer r.Body.Close()
		var b map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Fatal(err)
		}
		if r.StatusCode != 200 {
			t.Fatal(r.StatusCode, string(b["error"]))
		}
		return b
	}
	b := list("sam")
	var choices []calendarChoice
	json.Unmarshal(b["calendars"], &choices)
	if len(choices) != 2 || choices[1].Writable {
		t.Fatal(choices)
	}
	var events []calendarPageEvent
	json.Unmarshal(b["events"], &events)
	if len(events) != 2 || events[0].Calendar != "school" || events[0].Color != "#f59e0b" {
		t.Fatal(events)
	}
	r := h.member("POST", "/api/v1/calendar/visibility", "sam", "calendar_users", strings.NewReader(`{"calendar":"school","visible":false}`), "application/json")
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	b = list("sam")
	json.Unmarshal(b["events"], &events)
	if len(events) != 1 || events[0].Calendar != "household" {
		t.Fatal(events)
	}
	b = list("mika")
	json.Unmarshal(b["events"], &events)
	if len(events) != 2 {
		t.Fatal("visibility leaked to another member")
	}
}
func TestSharedCalendarWritesRefuseUnauthorizedTargets(t *testing.T) {
	h, house, school, reg := sharedCalendarHarness(t)
	schoolPath := reg.Calendars[1].Path()
	for _, op := range []string{"", "/update", "/delete"} {
		h.unthrottled()
		body := `{"calendar":"school","href":"` + schoolPath + `event.ics","title":"School","date":"2026-09-20","allDay":true}`
		r := h.member("POST", "/api/v1/calendar/events"+op, "sam", "calendar_users", strings.NewReader(body), "application/json")
		defer r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			data, _ := io.ReadAll(r.Body)
			t.Fatal(op, r.StatusCode, string(data))
		}
	}
	h.unthrottled()
	r := h.member("POST", "/api/v1/calendar/events/update", "sam", "calendar_users", strings.NewReader(`{"calendar":"household","targetCalendar":"school","href":"/GROUPS/Y2FsZW5kYXJfdXNlcnM=/event.ics","title":"Move","date":"2026-09-20","allDay":true}`), "application/json")
	r.Body.Close()
	if r.StatusCode != 403 {
		t.Fatal(r.StatusCode)
	}
	if len(house.writes)+len(school.writes)+len(school.deletes) != 0 {
		t.Fatal("unauthorized mutation reached upstream")
	}
	h.unthrottled()
	r = h.member("POST", "/api/v1/calendar/events", "sam", "calendar_users,adults", strings.NewReader(`{"calendar":"school","title":"Allowed","date":"2026-09-20","allDay":true}`), "application/json")
	r.Body.Close()
	if r.StatusCode != 200 || len(school.writes) != 1 {
		t.Fatal(r.StatusCode, school.writes)
	}
	h.unthrottled()
	r = h.member("GET", "/api/v1/calendar/events?calendar=restricted&from=2026-09-01&to=2026-09-30", "sam", "calendar_users", nil, "")
	r.Body.Close()
	if r.StatusCode == 200 {
		t.Fatal("addressed hidden calendar")
	}
}

func TestSharedCalendarDashboardAndBookingKeepTheirCollection(t *testing.T) {
	h, house, school, reg := sharedCalendarHarness(t)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	house.events = []caldav.Event{{Summary: "Later family", Start: at.Add(time.Hour)}}
	school.events = []caldav.Event{{Summary: "Earlier school", Start: at}}
	h.s.CalendarEvents = func(time.Time, time.Time) ([]CalendarCardEvent, error) {
		t.Fatal("registry dashboard used global calendar feed")
		return nil, nil
	}
	desired, err := h.s.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	card := h.s.homeCalendar("sam", append(desired, ourRoute), at, "calendar_users")
	if len(card.Events) != 2 || card.Events[0].Calendar != "school" || card.Events[0].Title != "Earlier school" {
		t.Fatal(card)
	}
	schoolURL, _ := reg.Calendars[1].CollectionURL(testCollection)
	href := schoolURL + "appointment.ics"
	school.stored = map[string]caldav.Event{href: {Href: href, Summary: "Appointment", Start: at.Add(2 * time.Hour)}}
	if _, err := h.s.DB.RecordCommitmentBooking(runtimedb.CommitmentBooking{Commitment: "fixture", At: at, BookedBy: "sam", CalendarID: "school", CalendarEvent: href}); err != nil {
		t.Fatal(err)
	}
	n, err := h.s.SyncCalendarBookings(context.Background())
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	booking, err := h.s.DB.LatestCommitmentBooking("fixture")
	if err != nil || booking.CalendarID != "school" || booking.CalendarEvent != href || !booking.At.Equal(at.Add(2*time.Hour)) {
		t.Fatal(booking, err)
	}
}

type movingCalendarBook struct {
	*fakeBook
	moved bool
}

func (b *movingCalendarBook) MoveEvent(_ context.Context, href, destination string) (string, error) {
	b.moved = true
	return destination + "event.ics", nil
}
func TestSharedCalendarMoveUpdatesLinkedBooking(t *testing.T) {
	h, house, _, reg := sharedCalendarHarness(t)
	moving := &movingCalendarBook{fakeBook: house}
	factory := h.s.CalendarBookFor
	h.s.CalendarBookFor = func(u string) CalendarBook {
		if u == testCollection {
			return moving
		}
		return factory(u)
	}
	href := testCollection + "event.ics"
	if _, err := h.s.DB.RecordCommitmentBooking(runtimedb.CommitmentBooking{Commitment: "moving", At: time.Now(), BookedBy: "sam", CalendarEvent: href}); err != nil {
		t.Fatal(err)
	}
	r := h.member("POST", "/api/v1/calendar/events/update", "sam", "calendar_users,adults", strings.NewReader(`{"calendar":"household","targetCalendar":"school","href":"/GROUPS/Y2FsZW5kYXJfdXNlcnM=/event.ics","title":"Lesson","date":"2026-09-20","allDay":true}`), "application/json")
	defer r.Body.Close()
	if r.StatusCode != 200 || !moving.moved {
		data, _ := io.ReadAll(r.Body)
		t.Fatal(r.StatusCode, string(data))
	}
	b, err := h.s.DB.LatestCommitmentBooking("moving")
	target, _ := reg.Calendars[1].CollectionURL(testCollection)
	if err != nil || b.CalendarID != "school" || b.CalendarEvent != target+"event.ics" {
		t.Fatal(b, err)
	}
}
