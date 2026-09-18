package caldav

// Tests for the write path.
//
// Every one of these is a claim about the BYTES that reach the calendar,
// because that is where this code can be wrong in a way nothing reports.
// A malformed VCALENDAR is accepted by the server, replicated to every
// phone in the house, and shows up as an event that is missing, on the
// wrong day, or silently stripped of its alarm.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
)

var testNow = time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

// recorder is a calendar that remembers what it was sent.
type recorder struct {
	*httptest.Server
	requests []*http.Request
	bodies   []string
	stored   string
	etag     string
	status   int // overrides the success status when non-zero
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{etag: `"v1"`}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body []byte
		if req.Body != nil {
			body, _ = io.ReadAll(req.Body)
		}
		r.requests = append(r.requests, req)
		r.bodies = append(r.bodies, string(body))

		switch req.Method {
		case http.MethodGet:
			w.Header().Set("ETag", r.etag)
			w.Header().Set("Content-Type", "text/calendar")
			_, _ = w.Write([]byte(r.stored))
		case http.MethodPut:
			if r.status != 0 {
				w.WriteHeader(r.status)
				return
			}
			r.stored = string(body)
			w.Header().Set("ETag", `"v2"`)
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			if r.status != 0 {
				w.WriteHeader(r.status)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *recorder) lastRequest(t *testing.T, method string) (*http.Request, string) {
	t.Helper()
	for i := len(r.requests) - 1; i >= 0; i-- {
		if r.requests[i].Method == method {
			return r.requests[i], r.bodies[i]
		}
	}
	t.Fatalf("no %s was sent (saw %d requests)", method, len(r.requests))
	return nil, ""
}

func writable(r *recorder) *Upstream {
	return &Upstream{
		CollectionURL: r.URL + "/calendars/sam/default/",
		Username:      "sam", Password: "ref-only",
		Writable: true,
		Now:      func() time.Time { return testNow },
		NewUID:   func() string { return "fixed-uid" },
	}
}

func act(t *testing.T, u *Upstream, capability string, body string) (adapter.Answer, error) {
	t.Helper()
	return u.Act(context.Background(), capability, json.RawMessage(body))
}

// A read-only adapter refuses every action, and says so the same way an
// adapter without a write path would.
func TestAReadOnlyAdapterRefusesEveryWrite(t *testing.T) {
	r := newRecorder(t)
	u := writable(r)
	u.Writable = false

	for _, capability := range ActionCapabilities {
		_, err := act(t, u, capability, `{"summary":"x","start":"2026-09-18T18:30:00Z","end":"2026-09-18T20:00:00Z"}`)
		if !errors.Is(err, adapter.ErrNoSuchCapability) {
			t.Errorf("%s on a read-only adapter = %v, want ErrNoSuchCapability", capability, err)
		}
	}
	if len(r.requests) != 0 {
		t.Errorf("a read-only adapter sent %d requests to the calendar; it must send none", len(r.requests))
	}
}

// Capabilities reports the write path with its reason rather than
// hiding it, so that whoever decides whether to widen a connection can
// see what widening it would turn on.
func TestCapabilitiesReportWritesEvenWhenTheyAreOff(t *testing.T) {
	r := newRecorder(t)
	// The reachability probe is a REPORT, which the recorder answers 405
	// to — so use a server that answers it, to isolate the flag.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><multistatus xmlns="DAV:"></multistatus>`))
	}))
	defer ok.Close()

	u := writable(r)
	u.CollectionURL = ok.URL
	u.Writable = false

	caps, err := u.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]adapter.Capability{}
	for _, c := range caps {
		found[c.Name] = c
	}
	for _, name := range ActionCapabilities {
		c, present := found[name]
		if !present {
			t.Fatalf("%s is not reported at all; nobody can decide to turn on a capability they cannot see", name)
		}
		if c.Kind != "action" {
			t.Errorf("%s is reported as kind %q, want action", name, c.Kind)
		}
		if c.Available {
			t.Errorf("%s reported available on a read-only adapter", name)
		}
		if c.Detail == "" {
			t.Errorf("%s is unavailable with no reason a person could act on", name)
		}
	}

	u.Writable = true
	caps, err = u.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caps {
		if c.Kind == "action" && !c.Available {
			t.Errorf("%s is still unavailable after writes were turned on: %s", c.Name, c.Detail)
		}
	}
}

// A create writes a complete VCALENDAR, and does it as a create.
func TestCreateWritesAWholeCalendarObjectAndRefusesToOverwrite(t *testing.T) {
	r := newRecorder(t)
	u := writable(r)

	answer, err := act(t, u, CapCreateEvent,
		`{"summary":"Dinner with Sam","location":"Columbia, MO","start":"2026-09-25T18:30:00-05:00","end":"2026-09-25T20:00:00-05:00"}`)
	if err != nil {
		t.Fatal(err)
	}

	req, body := r.lastRequest(t, http.MethodPut)
	if got := req.Header.Get("If-None-Match"); got != "*" {
		t.Errorf("If-None-Match = %q, want \"*\": without it a colliding UID silently replaces an existing event", got)
	}
	if !strings.HasSuffix(req.URL.Path, "/fixed-uid.ics") {
		t.Errorf("the event was written to %s, not to <uid>.ics", req.URL.Path)
	}
	if user, pass, ok := req.BasicAuth(); !ok || user != "sam" || pass != "ref-only" {
		t.Error("the write went out without the calendar credential")
	}

	for _, want := range []string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "BEGIN:VEVENT",
		"UID:fixed-uid",
		"DTSTAMP:20260918T090000Z",
		// -05:00 at half six in the evening is 23:30 UTC. An adapter
		// that wrote the local wall clock without its offset would put
		// this dinner five hours early on every phone.
		"DTSTART:20260925T233000Z",
		"DTEND:20260926T010000Z",
		"SUMMARY:Dinner with Sam",
		"END:VEVENT", "END:VCALENDAR",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the written object does not contain %q:\n%s", want, body)
		}
	}
	// A comma in a LOCATION is a property separator unless it is
	// escaped, and "Columbia, MO" is the ordinary case rather than a
	// contrived one.
	if !strings.Contains(body, `LOCATION:Columbia\, MO`) {
		t.Errorf("the location's comma was not escaped:\n%s", body)
	}
	if !strings.Contains(body, "\r\n") {
		t.Error("the object is not CRLF-terminated, which RFC 5545 requires")
	}

	data, _ := answer.Data.(map[string]any)
	if data["uid"] != "fixed-uid" {
		t.Errorf("the answer does not name the event it created: %v", answer.Data)
	}
	if data["etag"] != `"v2"` {
		t.Errorf("the answer does not carry the etag the server returned: %v", data["etag"])
	}
}

// An all-day event is a DATE, not a midnight DATETIME.
func TestAnAllDayEventIsWrittenAsADate(t *testing.T) {
	r := newRecorder(t)
	if _, err := act(t, writable(r), CapCreateEvent,
		`{"summary":"Nan's birthday","start":"2026-11-02","end":"2026-11-03","allDay":true}`); err != nil {
		t.Fatal(err)
	}
	_, body := r.lastRequest(t, http.MethodPut)
	if !strings.Contains(body, "DTSTART;VALUE=DATE:20261102") {
		t.Errorf("an all-day event was not written as a DATE; a DATETIME shows on the wrong day in another "+
			"timezone:\n%s", body)
	}
	if strings.Contains(body, "DTSTART:20261102T") {
		t.Errorf("an all-day event was written as a DATETIME:\n%s", body)
	}
}

// The regression this whole design exists for.
func TestAnUpdateKeepsEverythingItDoesNotUnderstand(t *testing.T) {
	r := newRecorder(t)
	r.stored = strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"BEGIN:VTIMEZONE",
		"TZID:America/Chicago",
		"BEGIN:STANDARD",
		// The trap: a VTIMEZONE has a DTSTART of its own, and a
		// rewrite by property name alone moves it.
		"DTSTART:19701101T020000",
		"TZOFFSETFROM:-0500",
		"TZOFFSETTO:-0600",
		"END:STANDARD",
		"END:VTIMEZONE",
		"BEGIN:VEVENT",
		"UID:phone-made-this",
		"DTSTAMP:20260901T120000Z",
		"DTSTART:20260925T233000Z",
		"DTEND:20260926T010000Z",
		"SUMMARY:Dinner",
		"RRULE:FREQ=WEEKLY;BYDAY=FR",
		"ATTENDEE;CN=Sam:mailto:sam@example.com",
		"ORGANIZER:mailto:mika@example.com",
		"BEGIN:VALARM",
		"TRIGGER:-PT30M",
		"ACTION:DISPLAY",
		"DESCRIPTION:Reminder",
		"END:VALARM",
		"END:VEVENT",
		"END:VCALENDAR",
	}, "\r\n")

	if _, err := act(t, writable(r), CapUpdateEvent,
		`{"href":"/calendars/sam/default/phone-made-this.ics","summary":"Dinner with Sam"}`); err != nil {
		t.Fatal(err)
	}
	_, body := r.lastRequest(t, http.MethodPut)

	for _, want := range []string{
		"RRULE:FREQ=WEEKLY;BYDAY=FR",
		"ATTENDEE;CN=Sam:mailto:sam@example.com",
		"ORGANIZER:mailto:mika@example.com",
		"BEGIN:VALARM", "TRIGGER:-PT30M", "END:VALARM",
		"BEGIN:VTIMEZONE", "TZID:America/Chicago",
		// The VTIMEZONE's own DTSTART, untouched.
		"DTSTART:19701101T020000",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("updating the summary dropped or moved %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "SUMMARY:Dinner with Sam") {
		t.Errorf("the summary was not changed:\n%s", body)
	}
	// The VALARM's DESCRIPTION is not the event's, so an edit that did
	// not name one must leave it exactly as it was.
	if !strings.Contains(body, "DESCRIPTION:Reminder") {
		t.Errorf("the alarm's own description was rewritten:\n%s", body)
	}
	if !strings.Contains(body, "DTSTAMP:20260918T090000Z") {
		t.Errorf("DTSTAMP was not refreshed; a client syncing on it would not notice the edit:\n%s", body)
	}
}

// A property the event never had is added inside the VEVENT.
func TestAnUpdateCanAddAPropertyTheEventNeverHad(t *testing.T) {
	r := newRecorder(t)
	r.stored = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:x\r\nSUMMARY:Dinner\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

	if _, err := act(t, writable(r), CapUpdateEvent, `{"uid":"x","location":"The kitchen"}`); err != nil {
		t.Fatal(err)
	}
	_, body := r.lastRequest(t, http.MethodPut)
	if !strings.Contains(body, "LOCATION:The kitchen") {
		t.Errorf("the location was not added:\n%s", body)
	}
	if strings.Index(body, "LOCATION:") > strings.Index(body, "END:VEVENT") {
		t.Errorf("the location landed outside the event it belongs to:\n%s", body)
	}
}

// A conditional write is how two people editing one event is not a lost
// update, and 412 has to read as that rather than as an outage.
func TestAConditionalUpdateIsSentAndItsRefusalIsExplained(t *testing.T) {
	r := newRecorder(t)
	r.stored = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:x\r\nSUMMARY:Dinner\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	r.status = http.StatusPreconditionFailed

	_, err := act(t, writable(r), CapUpdateEvent, `{"uid":"x","etag":"\"v1\"","summary":"Supper"}`)
	if err == nil {
		t.Fatal("a 412 was reported as a successful write")
	}
	if errors.Is(err, adapter.ErrUnavailable) {
		t.Fatalf("a 412 was reported as the calendar being unavailable, which would make a caller wait "+
			"instead of re-reading: %v", err)
	}
	if !strings.Contains(err.Error(), "changed since it was read") {
		t.Errorf("a 412 was not explained as a concurrent change: %v", err)
	}

	req, _ := r.lastRequest(t, http.MethodPut)
	if got := req.Header.Get("If-Match"); got != `"v1"` {
		t.Errorf("If-Match = %q, want the caller's etag: without it the write lands on whatever is there now", got)
	}
}

// With no etag from the caller, the one the read returned is used — so
// an update is still conditional on the version it actually edited.
func TestAnUpdateWithNoEtagUsesTheOneItJustRead(t *testing.T) {
	r := newRecorder(t)
	r.etag = `"server-side"`
	r.stored = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:x\r\nSUMMARY:Dinner\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

	if _, err := act(t, writable(r), CapUpdateEvent, `{"uid":"x","summary":"Supper"}`); err != nil {
		t.Fatal(err)
	}
	req, _ := r.lastRequest(t, http.MethodPut)
	if got := req.Header.Get("If-Match"); got != `"server-side"` {
		t.Errorf("If-Match = %q, want the etag the GET returned", got)
	}
}

func TestDeleteRemovesTheEventConditionally(t *testing.T) {
	r := newRecorder(t)
	if _, err := act(t, writable(r), CapDeleteEvent, `{"uid":"fixed-uid","etag":"\"v1\""}`); err != nil {
		t.Fatal(err)
	}
	req, _ := r.lastRequest(t, http.MethodDelete)
	if !strings.HasSuffix(req.URL.Path, "/fixed-uid.ics") {
		t.Errorf("delete addressed %s", req.URL.Path)
	}
	if got := req.Header.Get("If-Match"); got != `"v1"` {
		t.Errorf("If-Match = %q on a delete: an unconditional delete removes whatever is there now", got)
	}
}

// An href the server reported is resolved against the collection, so a
// read hands straight to a write.
func TestAnHrefFromAReadResolvesToTheSameEvent(t *testing.T) {
	events, err := parseEvents(`<?xml version="1.0"?>
<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
 <response>
  <href>/calendars/sam/default/phone-made-this.ics</href>
  <propstat><prop><getetag>"abc"</getetag><C:calendar-data>BEGIN:VEVENT
UID:phone-made-this
SUMMARY:Dinner
DTSTART:20260925T233000Z
END:VEVENT</C:calendar-data></prop></propstat>
 </response>
</multistatus>`)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("parsed %d events, want 1", len(events))
	}
	if events[0].Href != "/calendars/sam/default/phone-made-this.ics" {
		t.Fatalf("the read did not carry the event's address: %q", events[0].Href)
	}

	r := newRecorder(t)
	r.stored = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:phone-made-this\r\nSUMMARY:Dinner\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if _, err := act(t, writable(r), CapUpdateEvent,
		fmt.Sprintf(`{"href":%q,"summary":"Supper"}`, events[0].Href)); err != nil {
		t.Fatal(err)
	}
	req, _ := r.lastRequest(t, http.MethodPut)
	if req.URL.Path != "/calendars/sam/default/phone-made-this.ics" {
		t.Errorf("the update went to %s, not to the event the read named", req.URL.Path)
	}
}

// A caller-supplied UID becomes part of a URL, so it is a positive list
// rather than an escape.
func TestAUidThatWouldLeaveTheCollectionIsRefused(t *testing.T) {
	r := newRecorder(t)
	// "" is deliberately absent: an omitted uid means "mint one", which
	// is the ordinary case rather than a refusal.
	for _, uid := range []string{"../../etc/passwd", "a/b", "..", strings.Repeat("x", 129), "a b"} {
		body, _ := json.Marshal(map[string]any{
			"uid": uid, "summary": "x",
			"start": "2026-09-18T18:30:00Z", "end": "2026-09-18T20:00:00Z",
		})
		if _, err := act(t, writable(r), CapCreateEvent, string(body)); err == nil {
			t.Errorf("uid %q was accepted", uid)
		}
	}
	if len(r.requests) != 0 {
		t.Errorf("a refused uid still reached the calendar (%d requests)", len(r.requests))
	}
}

// The requests that are wrong on their face never reach the calendar.
func TestNonsenseWritesAreRefusedBeforeTheyReachTheCalendar(t *testing.T) {
	r := newRecorder(t)
	u := writable(r)
	for name, body := range map[string]string{
		"no summary":            `{"start":"2026-09-18T18:30:00Z","end":"2026-09-18T20:00:00Z"}`,
		"no start":              `{"summary":"x"}`,
		"ends before it starts": `{"summary":"x","start":"2026-09-18T20:00:00Z","end":"2026-09-18T18:30:00Z"}`,
		"start is not a time":   `{"summary":"x","start":"next thursday"}`,
		"summary is a novel":    fmt.Sprintf(`{"summary":%q,"start":"2026-09-18T18:30:00Z"}`, strings.Repeat("a", maxSummaryBytes+1)),
	} {
		if _, err := act(t, u, CapCreateEvent, body); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if len(r.requests) != 0 {
		t.Errorf("a refused write still reached the calendar (%d requests)", len(r.requests))
	}
}

// An update that names nothing to change is an error rather than a
// write that rewrites the event with itself and burns an etag.
func TestAnUpdateWithNothingToChangeIsRefused(t *testing.T) {
	r := newRecorder(t)
	r.stored = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:x\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if _, err := act(t, writable(r), CapUpdateEvent, `{"uid":"x"}`); err == nil {
		t.Fatal("an empty update was accepted")
	}
	for _, req := range r.requests {
		if req.Method == http.MethodPut {
			t.Error("an empty update still wrote to the calendar")
		}
	}
}

// A long summary folds, and unfolding it gives back exactly what was
// written — which is the only property of folding that matters.
func TestLongLinesFoldAndUnfoldBackToThemselves(t *testing.T) {
	summary := "Parents' evening for the twins, then the school play, then supper with Giulia and her brother"
	r := newRecorder(t)
	body, _ := json.Marshal(map[string]any{
		"summary": summary, "start": "2026-09-18T18:30:00Z", "end": "2026-09-18T21:00:00Z",
	})
	if _, err := act(t, writable(r), CapCreateEvent, string(body)); err != nil {
		t.Fatal(err)
	}
	_, written := r.lastRequest(t, http.MethodPut)

	for _, line := range strings.Split(strings.TrimRight(written, "\r\n"), "\r\n") {
		if len(line) > 75 {
			t.Errorf("a %d-octet line was written unfolded: %q", len(line), line)
		}
	}
	events := parseICS(written)
	if len(events) != 1 {
		t.Fatalf("the written object did not parse back to one event (%d)", len(events))
	}
	if events[0].Summary != summary {
		t.Errorf("round trip changed the summary:\n got %q\nwant %q", events[0].Summary, summary)
	}
}

// A calendar that is not answering is `unavailable`, so that dependent
// work waits rather than concluding the write failed for good.
func TestAnUnreachableCalendarIsUnavailableOnAWrite(t *testing.T) {
	u := &Upstream{
		CollectionURL: "http://127.0.0.1:1/", Username: "sam",
		Writable: true, Now: func() time.Time { return testNow }, NewUID: func() string { return "u" },
	}
	_, err := act(t, u, CapCreateEvent, `{"summary":"x","start":"2026-09-18T18:30:00Z"}`)
	if !errors.Is(err, adapter.ErrUnavailable) {
		t.Fatalf("a write to an unreachable calendar = %v, want ErrUnavailable", err)
	}
}

// An unknown action is ErrNoSuchCapability, like an unknown query.
func TestAnUnknownActionIsNoSuchCapability(t *testing.T) {
	r := newRecorder(t)
	if _, err := act(t, writable(r), "caldav.rsvp", `{}`); !errors.Is(err, adapter.ErrNoSuchCapability) {
		t.Fatalf("an unknown action = %v, want ErrNoSuchCapability", err)
	}
}
