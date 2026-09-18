package domestic

// Tests for the six calendar actions.
//
// Most of these are claims about the SUMMARY, which reads as an odd
// thing to assert until you see where it goes: butler.do_household_action
// hands the model Result.Summary and drops Result.Detail, so the summary
// is not a log line — it is the answer the household hears. A read that
// found three things and said "ok" is a working executor and a useless
// one.
//
// The rest are claims about what reaches the calendar, and the sharpest
// of those are the refusals: a change that matched nothing, or matched
// four things, must not write.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav"
)

// chicago is a real household timezone with a real offset, so a test
// that reads a wall-clock time as UTC fails here rather than passing by
// coincidence on a machine set to UTC.
var chicago = mustLoad("America/Chicago")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// now is a Friday morning.
var calNow = time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)

// fakeCalendar serves a CalDAV REPORT from a fixed set of events and
// remembers every write.
type fakeCalendar struct {
	*httptest.Server
	events   []string // VEVENT bodies
	requests []*http.Request
	bodies   []string
}

func newFakeCalendar(t *testing.T, events ...string) *fakeCalendar {
	t.Helper()
	f := &fakeCalendar{events: events}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body []byte
		if req.Body != nil {
			body, _ = io.ReadAll(req.Body)
		}
		f.requests = append(f.requests, req)
		f.bodies = append(f.bodies, string(body))

		switch req.Method {
		case "REPORT":
			var b strings.Builder
			b.WriteString(`<?xml version="1.0"?><multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`)
			for i, ev := range f.events {
				fmt.Fprintf(&b, `<response><href>/cal/e%d.ics</href><propstat><prop>`+
					`<getetag>"e%d"</getetag><C:calendar-data>BEGIN:VCALENDAR%s%s%sEND:VCALENDAR`+
					`</C:calendar-data></prop></propstat></response>`,
					i, i, "\r\n", ev, "\r\n")
			}
			b.WriteString(`</multistatus>`)
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(b.String()))
		case http.MethodGet:
			w.Header().Set("ETag", `"e0"`)
			_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\n" + f.events[0] + "\r\nEND:VCALENDAR\r\n"))
		case http.MethodPut:
			w.Header().Set("ETag", `"v2"`)
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeCalendar) sent(method string) bool {
	for _, r := range f.requests {
		if r.Method == method {
			return true
		}
	}
	return false
}

func (f *fakeCalendar) lastBody(t *testing.T, method string) string {
	t.Helper()
	for i := len(f.requests) - 1; i >= 0; i-- {
		if f.requests[i].Method == method {
			return f.bodies[i]
		}
	}
	t.Fatalf("no %s was sent", method)
	return ""
}

// vevent builds one event body for the fake.
func vevent(uid, summary, dtstart, dtend string) string {
	lines := []string{"BEGIN:VEVENT", "UID:" + uid, "SUMMARY:" + summary, "DTSTART:" + dtstart}
	if dtend != "" {
		lines = append(lines, "DTEND:"+dtend)
	}
	lines = append(lines, "END:VEVENT")
	return strings.Join(lines, "\r\n")
}

func calendarUnder(f *fakeCalendar) *Calendar {
	return &Calendar{
		Upstream: &caldav.Upstream{
			CollectionURL: f.URL + "/cal/",
			Username:      "wildstone-calendar",
			Password:      "ref-only",
			Writable:      true,
			Location:      chicago,
			Now:           func() time.Time { return calNow },
			NewUID:        func() string { return "fixed-uid" },
		},
		Location: chicago,
		Now:      func() time.Time { return calNow },
	}
}

func run(t *testing.T, c *Calendar, tool string, params map[string]string) (Result, error) {
	t.Helper()
	for _, e := range c.Executors() {
		if e.Name() == CalendarCapability+"."+tool {
			return e.Execute(context.Background(), Call{Tool: e.Name(), Tier: CalendarTools[tool], Params: params})
		}
	}
	t.Fatalf("no executor named calendar.%s", tool)
	return Result{}, nil
}

// Every declared tool is registered, under the capability the package
// declares. The catalog test pins this against the package; this pins
// it against the map, so a tool added to CalendarTools and forgotten in
// Executors fails here rather than at somebody's dispatch.
func TestEveryCalendarToolIsRegistered(t *testing.T) {
	c := calendarUnder(newFakeCalendar(t))
	got := map[string]bool{}
	for _, e := range c.Executors() {
		got[e.Name()] = true
	}
	for name := range CalendarTools {
		if !got[CalendarCapability+"."+name] {
			t.Errorf("calendar.%s is in CalendarTools but no executor answers to it", name)
		}
	}
	if len(got) != len(CalendarTools) {
		t.Errorf("registered %d executors for %d declared tools", len(got), len(CalendarTools))
	}
}

// The agenda is the answer, not a status line.
func TestAgendaReadsAsAnAnswer(t *testing.T) {
	f := newFakeCalendar(t,
		vevent("a", "School run", "20260918T130000Z", "20260918T133000Z"),      // 08:00 Chicago
		vevent("b", "Dinner with Sam", "20260918T233000Z", "20260919T010000Z"), // 18:30 Chicago
	)
	got, err := run(t, calendarUnder(f), CalToolAgenda, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Today", "2 things", "08:00 School run", "18:30 Dinner with Sam"} {
		if !strings.Contains(got.Summary, want) {
			t.Errorf("the agenda summary does not contain %q — a model reads this back to the household:\n%s",
				want, got.Summary)
		}
	}
	// 13:00 UTC is 08:00 in Chicago. An executor that rendered UTC would
	// tell the household the school run is at one in the afternoon.
	if strings.Contains(got.Summary, "13:00") {
		t.Errorf("the agenda was rendered in UTC rather than the household's timezone:\n%s", got.Summary)
	}
}

// An empty day is a real answer and must not read as a failure.
func TestAnEmptyDayIsAnAnswerNotAnError(t *testing.T) {
	got, err := run(t, calendarUnder(newFakeCalendar(t)), CalToolAgenda, map[string]string{"date": "tomorrow"})
	if err != nil {
		t.Fatalf("an empty day came back as an error: %v", err)
	}
	if !strings.Contains(got.Summary, "Tomorrow") || !strings.Contains(got.Summary, "nothing") {
		t.Errorf("an empty day should say so plainly, got: %q", got.Summary)
	}
}

func TestAgendaRefusesAnUnreasonableWindow(t *testing.T) {
	c := calendarUnder(newFakeCalendar(t))
	for _, days := range []string{"0", "365", "lots"} {
		if _, err := run(t, c, CalToolAgenda, map[string]string{"days": days}); err == nil {
			t.Errorf("days=%q was accepted", days)
		}
	}
}

func TestSearchNamesWhatItFound(t *testing.T) {
	f := newFakeCalendar(t,
		vevent("a", "Dentist for Sam", "20260921T150000Z", "20260921T153000Z"),
		vevent("b", "School run", "20260921T130000Z", ""),
	)
	got, err := run(t, calendarUnder(f), CalToolSearch, map[string]string{"query": "dentist"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Summary, "Dentist for Sam") {
		t.Errorf("search did not name what it found:\n%s", got.Summary)
	}
	if strings.Contains(got.Summary, "School run") {
		t.Errorf("search returned an event that does not match:\n%s", got.Summary)
	}
	if _, err := run(t, calendarUnder(f), CalToolSearch, map[string]string{}); err == nil {
		t.Error("a search with no query was accepted")
	}
}

// Free and busy are different answers, and the busy one says what is in
// the way — otherwise the household is told "no" with no way to act.
func TestAvailabilitySaysWhatIsInTheWay(t *testing.T) {
	busy := newFakeCalendar(t, vevent("a", "Dinner with Sam", "20260918T233000Z", "20260919T010000Z"))
	got, err := run(t, calendarUnder(busy), CalToolAvailability, map[string]string{
		"start": "2026-09-18T18:00", "end": "2026-09-18T19:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Summary, "Busy") || !strings.Contains(got.Summary, "Dinner with Sam") {
		t.Errorf("a clash did not name the thing in the way:\n%s", got.Summary)
	}

	free, err := run(t, calendarUnder(newFakeCalendar(t)), CalToolAvailability, map[string]string{
		"start": "2026-09-18T18:00", "end": "2026-09-18T19:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(free.Summary, "Free") {
		t.Errorf("an empty window did not read as free:\n%s", free.Summary)
	}
}

// A wall-clock time with no offset is the HOUSEHOLD's clock. This is the
// single most consequential line in the package: read as UTC, every
// event an agent creates lands hours off on every phone in the house.
func TestATimeWithNoOffsetIsTheHouseholdsClock(t *testing.T) {
	f := newFakeCalendar(t)
	_, err := run(t, calendarUnder(f), CalToolCreate, map[string]string{
		"title": "Dinner with Giulia", "start": "2026-09-24T18:30", "duration_minutes": "120",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := f.lastBody(t, http.MethodPut)
	// 18:30 in Chicago on 24 September (CDT, -05:00) is 23:30 UTC.
	if !strings.Contains(body, "DTSTART:20260924T233000Z") {
		t.Errorf("18:30 local was not written as 23:30Z — the household's dinner is on the wrong hour:\n%s", body)
	}
	if !strings.Contains(body, "DTEND:20260925T013000Z") {
		t.Errorf("duration_minutes=120 did not produce a two-hour event:\n%s", body)
	}
	if !strings.Contains(body, "SUMMARY:Dinner with Giulia") {
		t.Errorf("the title did not reach the calendar:\n%s", body)
	}
}

// An event with a start and no end is an hour, because a zero-length
// event renders as a point in time on most phones — which is not what
// anybody meant by "dinner at 6:30".
func TestAnEventWithNoEndLastsAnHour(t *testing.T) {
	f := newFakeCalendar(t)
	if _, err := run(t, calendarUnder(f), CalToolCreate, map[string]string{
		"title": "Call Nan", "start": "2026-09-24T18:30",
	}); err != nil {
		t.Fatal(err)
	}
	body := f.lastBody(t, http.MethodPut)
	if !strings.Contains(body, "DTEND:20260925T003000Z") {
		t.Errorf("an event with no end did not default to an hour:\n%s", body)
	}
}

func TestAnAllDayEventIsADate(t *testing.T) {
	f := newFakeCalendar(t)
	if _, err := run(t, calendarUnder(f), CalToolCreate, map[string]string{
		"title": "Nan's birthday", "start": "2026-11-02", "all_day": "true",
	}); err != nil {
		t.Fatal(err)
	}
	body := f.lastBody(t, http.MethodPut)
	if !strings.Contains(body, "DTSTART;VALUE=DATE:20261102") {
		t.Errorf("an all-day event was not written as a DATE:\n%s", body)
	}
}

// Nothing here parses English. A value that is not a value is a refusal,
// not a guess, and it must not reach the calendar.
func TestNonsenseIsRefusedAndNeverWritten(t *testing.T) {
	f := newFakeCalendar(t)
	c := calendarUnder(f)
	for name, params := range map[string]map[string]string{
		"no title":          {"start": "2026-09-24T18:30"},
		"no start":          {"title": "x"},
		"start is English":  {"title": "x", "start": "next thursday at half six"},
		"ends before start": {"title": "x", "start": "2026-09-24T18:30", "end": "2026-09-24T17:00"},
		"silly duration":    {"title": "x", "start": "2026-09-24T18:30", "duration_minutes": "-5"},
	} {
		if _, err := run(t, c, CalToolCreate, params); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if f.sent(http.MethodPut) {
		t.Error("a refused create still wrote to the calendar")
	}
}

// The refusals that matter most: a change that found nothing, and a
// change that found several. Picking one would report success for
// something the household did not ask for, and the only place that
// surfaces is somebody's phone.
func TestAnAmbiguousChangeIsRefusedByName(t *testing.T) {
	f := newFakeCalendar(t,
		vevent("a", "Dinner with Sam", "20260918T233000Z", "20260919T010000Z"),
		vevent("b", "Dinner with Giulia", "20260919T003000Z", "20260919T020000Z"),
	)
	c := calendarUnder(f)

	_, err := run(t, c, CalToolUpdate, map[string]string{
		"title": "dinner", "date": "2026-09-18", "start": "19:00",
	})
	if err == nil {
		t.Fatal("an ambiguous change was carried out")
	}
	for _, want := range []string{"2 events", "Dinner with Sam", "Dinner with Giulia"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q, so the household cannot pick: %v", want, err)
		}
	}
	if f.sent(http.MethodPut) {
		t.Error("an ambiguous change still wrote to the calendar")
	}
}

func TestAChangeThatMatchesNothingSaysWhatIsThere(t *testing.T) {
	f := newFakeCalendar(t, vevent("a", "School run", "20260918T130000Z", ""))
	_, err := run(t, calendarUnder(f), CalToolDelete, map[string]string{
		"title": "dentist", "date": "2026-09-18",
	})
	if err == nil {
		t.Fatal("deleting a thing that is not there succeeded")
	}
	if !strings.Contains(err.Error(), "School run") {
		t.Errorf("the refusal does not say what the day actually has: %v", err)
	}
	if f.sent(http.MethodDelete) {
		t.Error("a refused delete still reached the calendar")
	}
}

// An unambiguous change goes through, and it edits the event the search
// found rather than a new one.
func TestAnUnambiguousChangeIsCarriedOut(t *testing.T) {
	f := newFakeCalendar(t, vevent("a", "Dinner with Sam", "20260918T233000Z", "20260919T010000Z"))
	got, err := run(t, calendarUnder(f), CalToolUpdate, map[string]string{
		"title": "dinner with sam", "date": "2026-09-18", "start": "19:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Summary, "Dinner with Sam") {
		t.Errorf("the summary does not say what was changed: %q", got.Summary)
	}
	body := f.lastBody(t, http.MethodPut)
	// 19:00 Chicago on 18 September (CDT) is 00:00Z the next day.
	if !strings.Contains(body, "DTSTART:20260919T000000Z") {
		t.Errorf("the new time did not reach the calendar:\n%s", body)
	}
	if !strings.Contains(body, "UID:a") {
		t.Errorf("the change was written against the wrong event:\n%s", body)
	}
}

func TestAChangeWithNothingToChangeIsRefused(t *testing.T) {
	f := newFakeCalendar(t, vevent("a", "Dinner with Sam", "20260918T233000Z", ""))
	if _, err := run(t, calendarUnder(f), CalToolUpdate, map[string]string{
		"title": "dinner", "date": "2026-09-18",
	}); err == nil {
		t.Fatal("an empty change was accepted")
	}
}

func TestDeleteRemovesTheOneEventItFound(t *testing.T) {
	f := newFakeCalendar(t, vevent("a", "Dentist for Sam", "20260918T150000Z", ""))
	got, err := run(t, calendarUnder(f), CalToolDelete, map[string]string{
		"title": "dentist", "date": "2026-09-18",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.sent(http.MethodDelete) {
		t.Fatal("nothing was deleted")
	}
	if !strings.Contains(got.Summary, "Dentist for Sam") {
		t.Errorf("the summary does not name what was deleted: %q", got.Summary)
	}
	var deletes int
	for _, r := range f.requests {
		if r.Method == http.MethodDelete {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("one delete call removed %d events", deletes)
	}
}

// A control plane with no calendar configured refuses rather than
// panicking on a nil upstream.
func TestAnUnconfiguredCalendarRefuses(t *testing.T) {
	c := &Calendar{Location: chicago, Now: func() time.Time { return calNow }}
	for name := range CalendarTools {
		if _, err := run(t, c, name, map[string]string{}); err == nil {
			t.Errorf("calendar.%s answered with no calendar configured", name)
		}
	}
}

// The gateway's own checks still apply to these tools: a read tool
// carried at the wrong tier is refused before the executor runs.
func TestTheGatewayStillTierChecksCalendarTools(t *testing.T) {
	f := newFakeCalendar(t)
	gw := &Gateway{
		Declared: DeclaredFromPackages([]AgentToolSource{
			{Capability: CalendarCapability, Tools: CalendarTools},
		}),
	}
	for _, e := range calendarUnder(f).Executors() {
		gw.Register(e)
	}
	_, err := gw.Dispatch(context.Background(), Call{
		Tool: "calendar." + CalToolAgenda, Tier: "domestic",
	})
	if err == nil {
		t.Fatal("a read tool dispatched at the domestic tier was allowed")
	}
	if !strings.Contains(err.Error(), "read") {
		t.Errorf("the refusal does not name the declared tier: %v", err)
	}
	// And an infrastructure tier is refused for these as for everything
	// else a household rule can name.
	if _, err := gw.Dispatch(context.Background(), Call{
		Tool: "calendar." + CalToolDelete, Tier: "high",
	}); err == nil {
		t.Fatal("a calendar tool was dispatched at an infrastructure tier")
	}
}

// calendar.how_to_add is the answer to the question the whole capability
// is for: how do I get this on my phone.
func TestHowToAddAnswersForTheAskerAndNobodyElse(t *testing.T) {
	c := calendarUnder(newFakeCalendar(t))
	var askedFor string
	c.Handoff = func(member string) (CalendarHandoff, bool, error) {
		askedFor = member
		return CalendarHandoff{
			Server:            "https://cal.home.example.ts.net:51443",
			Username:          member,
			OwnCalendars:      "https://cal.home.example.ts.net:51443/mika/",
			HouseholdCalendar: "https://cal.home.example.ts.net:51443/household/",
			ProfileURL:        "https://home.example.ts.net/api/v1/home/calendar/profile.mobileconfig",
		}, true, nil
	}

	var tool Executor
	for _, e := range c.Executors() {
		if e.Name() == CalendarCapability+"."+CalToolSetup {
			tool = e
		}
	}
	if tool == nil {
		t.Fatal("calendar.how_to_add is not registered")
	}

	// The member comes from the SESSION, not from a param. A param would
	// be a way to ask for somebody else's set-up.
	got, err := tool.Execute(context.Background(), Call{
		Tool: tool.Name(), Tier: "read", Subject: "mika",
		Params: map[string]string{"member": "giulia", "user": "giulia"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if askedFor != "mika" {
		t.Errorf("the hand-off was built for %q; a param named a different person and was followed", askedFor)
	}
	for _, want := range []string{"profile.mobileconfig", "cal.home.example.ts.net:51443", "mika", "household"} {
		if !strings.Contains(got.Summary, want) {
			t.Errorf("the answer does not contain %q — this is what gets pasted into a chat message:\n%s",
				want, got.Summary)
		}
	}
	// The profile is the only thing that puts the SHARED calendar on an
	// iPhone, so it has to come first in an answer somebody skims.
	if strings.Index(got.Summary, "profile.mobileconfig") > strings.Index(got.Summary, "CalDAV account") {
		t.Errorf("the typed-address instructions come before the profile link:\n%s", got.Summary)
	}
	if strings.Contains(strings.ToLower(got.Summary), "giulia") {
		t.Errorf("another member's name reached the answer:\n%s", got.Summary)
	}
}

// A session with nobody bound to it cannot be set up, and a deployment
// that cannot say where the calendar is says so.
func TestHowToAddRefusesWhenItCannotAnswerTruthfully(t *testing.T) {
	c := calendarUnder(newFakeCalendar(t))
	var tool Executor
	for _, e := range c.Executors() {
		if e.Name() == CalendarCapability+"."+CalToolSetup {
			tool = e
		}
	}

	if _, err := tool.Execute(context.Background(), Call{Tier: "read"}); err == nil {
		t.Error("an unattributed session got a set-up answer")
	}
	if _, err := tool.Execute(context.Background(), Call{Tier: "read", Subject: "mika"}); err == nil {
		t.Error("a deployment with no hand-off seam invented an answer")
	}
	c.Handoff = func(string) (CalendarHandoff, bool, error) { return CalendarHandoff{}, false, nil }
	if _, err := tool.Execute(context.Background(), Call{Tier: "read", Subject: "mika"}); err == nil {
		t.Error("a household with no Wildstone calendar was told how to add one")
	}
}

// It answers on a deployment whose agent tools are not wired to a
// collection: "where is the calendar" is a different question from "what
// is on it", and it is the one a household asks first.
func TestHowToAddAnswersBeforeTheAgentToolsAreWired(t *testing.T) {
	c := &Calendar{
		Location: chicago, Now: func() time.Time { return calNow },
		Handoff: func(member string) (CalendarHandoff, bool, error) {
			return CalendarHandoff{Server: "https://cal.example", Username: member}, true, nil
		},
	}
	for _, e := range c.Executors() {
		if e.Name() != CalendarCapability+"."+CalToolSetup {
			continue
		}
		if _, err := e.Execute(context.Background(), Call{Tier: "read", Subject: "mika"}); err != nil {
			t.Fatalf("how_to_add refused on a deployment with no collection configured: %v", err)
		}
	}
}
