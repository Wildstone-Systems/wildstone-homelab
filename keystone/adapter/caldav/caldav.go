// Package caldav is the CalDAV adapter (ADR-W030 A5.1, work package P12;
// ADR-W034 for the write path).
//
// It reads events from a calendar collection, and — only when two
// separate things both say so — creates, changes and deletes them
// (ADR-W034 phase 2). Reading is the default and writing is not:
//
//   - Keystone refuses action work for a connection nobody has widened
//     (runtimedb: `w.Kind == WorkAction && !conn.ActionsEnabled`), so an
//     adapter never even receives one until a person decides;
//   - and Writable below must be on, so an adapter deployed to answer
//     questions cannot be talked into changing a calendar by anything
//     that reaches it over the wire.
//
// The second is a deliberate second copy of the first, for the reason
// keystone/domestic keeps its own copy of the tier rule: a call that
// reached here some other way — a hand-edited row, a future path nobody
// has thought of — still cannot write to the household's calendar.
//
// RSVPing to invitations is still NOT here. It is a different act with a
// different blast radius (it sends mail, to people, as the household)
// and stays on ADR-W030's follow-up list.
//
// The credential is personal. It arrives from the node's secret path
// into this process's environment, having been onboarded through the
// authenticated secret-input surface (never through chat), and it never
// travels in a work payload or reaches a model. Where the provider
// cannot issue a read-only app password, the wider power is disclosed at
// onboarding, and what actually constrains it is the pair of gates
// above rather than anything the provider was asked for.
package caldav

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/householdtz"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
)

const (
	// CapEvents reads events in a window.
	CapEvents = "caldav.events"
	// CapToday is the window a household actually asks about, so a
	// routine does not have to compute dates.
	CapToday = "caldav.today"
)

// Upstream is one CalDAV calendar collection.
type Upstream struct {
	// BeforeWrite is the live control plane store fence; standalone adapters
	// are authorized by their claimed integration work instead.
	BeforeWrite func() error
	// CollectionURL is the calendar collection, e.g.
	// https://dav.example.com/calendars/sam/default/
	CollectionURL string
	Username      string
	Password      string
	HTTP          *http.Client
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// Location is the household timezone, used to decide what "today"
	// means. A calendar question answered in UTC is wrong for most of
	// the day in most of the world.
	Location *time.Location
	// Zone, when set, is the household's live timezone and wins over
	// Location, so an approved change applies without a restart.
	Zone *householdtz.Zone
	// Writable turns the action capabilities on. Off by default, and
	// off is the honest posture for an adapter nobody has decided to
	// widen — see the package comment for why this is a second copy of
	// a check Keystone already makes.
	Writable bool
	// NewUID mints the identifier for a created event, injectable so a
	// test can assert what was written rather than that something was.
	NewUID func() string
}

func (u *Upstream) Name() string { return "caldav" }

func (u *Upstream) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Upstream) location() *time.Location {
	if l := u.Zone.Location(); l != nil {
		return l
	}
	if u.Location != nil {
		return u.Location
	}
	return time.UTC
}

func (u *Upstream) client() *http.Client {
	client := &http.Client{Timeout: 15 * time.Second}
	if u.HTTP != nil {
		copy := *u.HTTP
		client = &copy
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

// Event is one calendar entry, narrowed to what a household question
// needs. Description and attendees are deliberately not carried: they
// are the most sensitive part of a calendar and a routine that says
// "you have three things today" needs none of it.
type Event struct {
	UID     string    `json:"uid"`
	Summary string    `json:"summary"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end,omitzero"`
	AllDay  bool      `json:"allDay,omitempty"`
	// Location and Description are the event's place and notes, as a
	// person typed them. Read so that "anything else about it?" has an
	// answer: without them a Butler could only ever repeat a title and
	// a time, while the note that mattered sat in the event.
	Location    string `json:"location,omitempty"`
	Description string `json:"description,omitempty"`
	// Href is where this event lives in the collection, relative to the
	// server root, as the server itself reported it.
	//
	// It is carried because a UID is NOT an address. A server names the
	// resource it stores, and only the events THIS adapter created are
	// at the <uid>.ics the write path would guess — one added on a phone
	// is wherever that phone put it. Changing or deleting an event the
	// household made therefore starts by reading it, and this is the
	// field that makes that possible.
	Href string `json:"href,omitempty"`
	// Attendees are the people this event is FOR, as the iCalendar
	// names them: the CN parameter where there is one, otherwise the
	// local part of the mailto. Read so a household can ask whose
	// Tuesday afternoon the swimming is, which is the one thing a
	// shared calendar cannot answer from a title alone.
	//
	// A NAME, NOT AN IDENTITY. This is whatever the event says, from
	// whichever phone wrote it. Matching one to a household Person is
	// a decision for the caller and deliberately not made here — the
	// adapter reports what the calendar holds.
	Attendees []string `json:"attendees,omitempty"`
}

// attendeeName reads a person's name out of one ATTENDEE line.
//
// CN first, because it is what a human typed and what every client
// shows. Failing that the local part of a mailto, which is a poor name
// but a better one than the whole URI. An ATTENDEE with neither is
// skipped rather than reported as an empty person.
func attendeeName(params, value string) string {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(p, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "CN") {
			continue
		}
		// A CN may be quoted, and the quotes are syntax, not name.
		if name := strings.Trim(strings.TrimSpace(v), `"`); name != "" {
			return unescapeICS(name)
		}
	}
	addr := strings.TrimSpace(value)
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		addr = addr[i+1:] // strip the mailto: scheme
	}
	if local, _, ok := strings.Cut(addr, "@"); ok && local != "" {
		return local
	}
	return ""
}

// report issues a CalDAV REPORT for a time range and returns the raw
// multistatus.
func (u *Upstream) report(ctx context.Context, from, to time.Time) (string, error) {
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8" ?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><D:getetag/><C:calendar-data/></D:prop>
  <C:filter>
    <C:comp-filter name="VCALENDAR">
      <C:comp-filter name="VEVENT">
        <C:time-range start="%s" end="%s"/>
      </C:comp-filter>
    </C:comp-filter>
  </C:filter>
</C:calendar-query>`, from.UTC().Format("20060102T150405Z"), to.UTC().Format("20060102T150405Z"))

	req, err := http.NewRequestWithContext(ctx, "REPORT", u.CollectionURL, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "1")
	if u.Username != "" {
		req.SetBasicAuth(u.Username, u.Password)
	}
	res, err := u.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", adapter.ErrUnavailable, err)
	}
	defer res.Body.Close()
	// 207 Multi-Status is the success case; anything else — including a
	// 401 from a rotated password — means this calendar cannot answer.
	if res.StatusCode != http.StatusMultiStatus && res.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 256))
		return "", fmt.Errorf("%w: the calendar answered %s: %s", adapter.ErrUnavailable,
			res.Status, strings.TrimSpace(string(snippet)))
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("%w: %v", adapter.ErrUnavailable, err)
	}
	return string(raw), nil
}

// multistatus is the sliver of DAV we need: the calendar-data payloads.
type multistatus struct {
	XMLName  xml.Name `xml:"multistatus"`
	Response []struct {
		Href     string `xml:"href"`
		Propstat []struct {
			Prop struct {
				CalendarData string `xml:"calendar-data"`
				ETag         string `xml:"getetag"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// parseEvents pulls VEVENTs out of a multistatus.
//
// It is a deliberately small iCalendar reader rather than a dependency.
// The fields a household question needs are four, the unfolding rule is
// one line, and a full parser would be a large surface reading text
// written by anyone who can put an event in the calendar.
func parseEvents(raw string, loc *time.Location) ([]Event, error) {
	var ms multistatus
	if err := xml.Unmarshal([]byte(raw), &ms); err != nil {
		return nil, fmt.Errorf("caldav: parsing the calendar response: %w", err)
	}
	var events []Event
	for _, r := range ms.Response {
		for _, ps := range r.Propstat {
			for _, ev := range parseICS(ps.Prop.CalendarData, loc) {
				ev.Href = strings.TrimSpace(r.Href)
				events = append(events, ev)
			}
		}
	}
	return events, nil
}

func parseICS(data string, loc *time.Location) []Event {
	var out []Event
	var cur *Event
	for _, line := range unfold(data) {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		// Strip parameters: DTSTART;VALUE=DATE is still DTSTART.
		key, params, _ := strings.Cut(name, ";")
		switch strings.ToUpper(strings.TrimSpace(key)) {
		case "BEGIN":
			if strings.EqualFold(strings.TrimSpace(value), "VEVENT") {
				cur = &Event{}
			}
		case "END":
			if strings.EqualFold(strings.TrimSpace(value), "VEVENT") && cur != nil {
				if cur.UID != "" || cur.Summary != "" {
					out = append(out, *cur)
				}
				cur = nil
			}
		case "UID":
			if cur != nil {
				cur.UID = strings.TrimSpace(value)
			}
		case "SUMMARY":
			if cur != nil {
				cur.Summary = unescapeICS(value)
			}
		case "LOCATION":
			if cur != nil {
				cur.Location = unescapeICS(value)
			}
		case "DESCRIPTION":
			if cur != nil {
				cur.Description = unescapeICS(value)
			}
		case "ATTENDEE":
			if cur != nil {
				if who := attendeeName(params, value); who != "" {
					cur.Attendees = append(cur.Attendees, who)
				}
			}
		case "DTSTART":
			if cur != nil {
				cur.Start, cur.AllDay = parseICSTime(value, params, loc)
			}
		case "DTEND":
			if cur != nil {
				cur.End, _ = parseICSTime(value, params, loc)
			}
		}
	}
	return out
}

// unfold joins iCalendar continuation lines, which begin with a space or
// tab. A long SUMMARY arrives folded, and reading it a line at a time
// truncates every event with a long name.
//
// EXACTLY ONE leading space or tab is removed, because exactly one was
// added: RFC 5545 folds by inserting CRLF and a single whitespace
// character, so every other leading space on a continuation line is the
// household's own text. Trimming them all reads "Dentist for\r\n  Sam"
// as "Dentist forSam" — which is what this did, and which is invisible
// until a fold happens to land next to a space. It also made the write
// path's own round trip lossy, since a summary long enough to fold is
// long enough to contain one.
func unfold(data string) []string {
	raw := strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
	var out []string
	for _, line := range raw {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if len(out) > 0 {
				out[len(out)-1] += line[1:]
				continue
			}
		}
		out = append(out, line)
	}
	return out
}

func unescapeICS(v string) string {
	r := strings.NewReplacer(`\,`, ",", `\;`, ";", `\n`, "\n", `\N`, "\n", `\\`, `\`)
	return r.Replace(strings.TrimSpace(v))
}

func parseICSTime(value, params string, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.UTC
	}
	v := strings.TrimSpace(value)
	upper := strings.ToUpper(params)
	// A DATE is a calendar day, and a calendar day is the HOUSEHOLD's:
	// midnight here, not midnight UTC, which is the evening before in
	// the Americas.
	if strings.Contains(upper, "VALUE=DATE") && !strings.Contains(upper, "VALUE=DATE-TIME") || len(v) == 8 {
		if t, err := time.ParseInLocation("20060102", v, loc); err == nil {
			return t, true
		}
	}
	if t, err := time.Parse("20060102T150405Z", v); err == nil {
		return t, false
	}
	// A local time is in its TZID when it names one — what a phone
	// writes — and otherwise floating, which RFC 5545 defines as the
	// wall clock of whoever reads it: here, the household.
	zone := loc
	if tzid := icsParam(params, "TZID"); tzid != "" {
		if z, err := time.LoadLocation(tzid); err == nil {
			zone = z
		}
	}
	if t, err := time.ParseInLocation("20060102T150405", v, zone); err == nil {
		return t, false
	}
	return time.Time{}, false
}

// icsParam returns one property parameter's value, unquoted.
func icsParam(params, name string) string {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(p, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

// Events reads the events overlapping [from, to).
//
// It is Query's own body, exported for the same reason EventWrite is:
// Keystone's action gateway is in this process and should not have to
// unwrap a map[string]any to read a list it could have been handed.
func (u *Upstream) Events(ctx context.Context, from, to time.Time) ([]Event, error) {
	raw, err := u.report(ctx, from, to)
	if err != nil {
		return nil, err
	}
	events, err := parseEvents(raw, u.location())
	if err != nil {
		return nil, err
	}
	return within(events, from, to), nil
}

// within keeps the events that overlap [from, to), judged on the times
// as parsed here rather than on the server's own time-range filter.
//
// The server's filter is not enough on its own. An all-day event is a
// DATE, which has no zone, and Radicale matches it as if it were UTC —
// so asked for the household's today (05:00Z to 05:00Z in Chicago), it
// also returned tomorrow's all-day event, whose midnight-UTC falls
// inside that window. A Butler then told a member the Work Day on the
// 19th was on the 18th.
func within(events []Event, from, to time.Time) []Event {
	out := events[:0]
	for _, e := range events {
		end := e.End
		if !end.After(e.Start) {
			if e.AllDay {
				end = e.Start.AddDate(0, 0, 1)
			} else {
				end = e.Start
			}
		}
		// An instant event (end == start) is in the window when it
		// starts in it; anything longer, when it overlaps it.
		if e.Start.Before(to) && (end.After(from) || (end.Equal(e.Start) && !e.Start.Before(from))) {
			out = append(out, e)
		}
	}
	return out
}

// Capabilities verifies against the live collection.
func (u *Upstream) Capabilities(ctx context.Context) ([]adapter.Capability, error) {
	available := true
	detail := ""
	now := u.now()
	if _, err := u.report(ctx, now, now.Add(time.Minute)); err != nil {
		available = false
		detail = err.Error()
	}
	caps := make([]adapter.Capability, 0, 2+len(ActionCapabilities))
	for _, name := range []string{CapEvents, CapToday} {
		caps = append(caps, adapter.Capability{
			Kind: "query", Name: name, Available: available, Detail: detail,
		})
	}
	// The write path is REPORTED whether or not it is on, with the
	// reason when it is not. Recording a capability says "this works",
	// never "this is allowed" (ADR-W030 A5) — and a capability that
	// simply vanished when writes were off would leave whoever is
	// deciding whether to widen a connection with nothing to decide
	// about.
	for _, name := range ActionCapabilities {
		c := adapter.Capability{Kind: "action", Name: name, Available: available && u.Writable, Detail: detail}
		if available && !u.Writable {
			c.Detail = "this connection is read-only: calendar writes are off until somebody turns them on"
		}
		caps = append(caps, c)
	}
	return caps, nil
}

// Query answers a calendar read.
func (u *Upstream) Query(ctx context.Context, capability string, request json.RawMessage) (adapter.Answer, error) {
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if len(request) > 0 {
		_ = json.Unmarshal(request, &req)
	}
	var from, to time.Time
	switch capability {
	case CapToday:
		now := u.now().In(u.location())
		from = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, u.location())
		to = from.AddDate(0, 0, 1)
	case CapEvents:
		var err error
		if from, err = time.Parse(time.RFC3339, req.From); err != nil {
			return adapter.Answer{}, fmt.Errorf("%s needs an RFC3339 from: %w", CapEvents, err)
		}
		if to, err = time.Parse(time.RFC3339, req.To); err != nil {
			return adapter.Answer{}, fmt.Errorf("%s needs an RFC3339 to: %w", CapEvents, err)
		}
	default:
		return adapter.Answer{}, fmt.Errorf("%w: %s", adapter.ErrNoSuchCapability, capability)
	}

	events, err := u.Events(ctx, from, to)
	if err != nil {
		return adapter.Answer{}, err
	}
	if len(events) == 0 {
		// A day with nothing on it is a real answer, and the one most
		// likely to be misreported: "your calendar is empty" and "your
		// calendar is unreachable" must never come out the same.
		return adapter.Answer{Empty: true, Data: map[string]any{
			"events": []Event{}, "from": from, "to": to,
		}}, nil
	}
	return adapter.Answer{Data: map[string]any{
		"events": events, "from": from, "to": to, "observedAt": u.now().UTC(),
	}}, nil
}

var _ adapter.Upstream = (*Upstream)(nil)
