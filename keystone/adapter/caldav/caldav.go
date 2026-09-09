// Package caldav is the read-only CalDAV adapter (ADR-W030 A5.1, work
// package P12).
//
// It reads events from a calendar collection and does nothing else.
// Calendar WRITES — creating events, RSVPing to invitations — are
// explicitly later, separately controlled work in ADR-W030's follow-up
// list, so Act is not implemented and cannot be turned on by
// configuration.
//
// The credential is personal. It arrives from the node's secret path
// into this process's environment, having been onboarded through the
// authenticated secret-input surface (never through chat), and it never
// travels in a work payload or reaches a model. Where the provider
// cannot issue a read-only app password, the wider power is disclosed at
// onboarding and this adapter's read-only operation is what actually
// constrains it.
package caldav

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
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
}

func (u *Upstream) Name() string { return "caldav" }

func (u *Upstream) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Upstream) location() *time.Location {
	if u.Location != nil {
		return u.Location
	}
	return time.UTC
}

func (u *Upstream) client() *http.Client {
	if u.HTTP != nil {
		return u.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
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
		Propstat []struct {
			Prop struct {
				CalendarData string `xml:"calendar-data"`
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
func parseEvents(raw string) ([]Event, error) {
	var ms multistatus
	if err := xml.Unmarshal([]byte(raw), &ms); err != nil {
		return nil, fmt.Errorf("caldav: parsing the calendar response: %w", err)
	}
	var events []Event
	for _, r := range ms.Response {
		for _, ps := range r.Propstat {
			events = append(events, parseICS(ps.Prop.CalendarData)...)
		}
	}
	return events, nil
}

func parseICS(data string) []Event {
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
		case "DTSTART":
			if cur != nil {
				cur.Start, cur.AllDay = parseICSTime(value, params)
			}
		case "DTEND":
			if cur != nil {
				cur.End, _ = parseICSTime(value, params)
			}
		}
	}
	return out
}

// unfold joins iCalendar continuation lines, which begin with a space or
// tab. A long SUMMARY arrives folded, and reading it a line at a time
// truncates every event with a long name.
func unfold(data string) []string {
	raw := strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
	var out []string
	for _, line := range raw {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if len(out) > 0 {
				out[len(out)-1] += strings.TrimLeft(line, " \t")
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

func parseICSTime(value, params string) (time.Time, bool) {
	v := strings.TrimSpace(value)
	if strings.Contains(strings.ToUpper(params), "VALUE=DATE") {
		if t, err := time.Parse("20060102", v); err == nil {
			return t, true
		}
	}
	for _, layout := range []string{"20060102T150405Z", "20060102T150405", "20060102"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, layout == "20060102"
		}
	}
	return time.Time{}, false
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
	caps := make([]adapter.Capability, 0, 2)
	for _, name := range []string{CapEvents, CapToday} {
		caps = append(caps, adapter.Capability{
			Kind: "query", Name: name, Available: available, Detail: detail,
		})
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

	raw, err := u.report(ctx, from, to)
	if err != nil {
		return adapter.Answer{}, err
	}
	events, err := parseEvents(raw)
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

// Act performs nothing. Calendar writes and RSVPs are separately
// controlled later work, not a flag on this adapter.
func (u *Upstream) Act(context.Context, string, json.RawMessage) (adapter.Answer, error) {
	return adapter.Answer{}, fmt.Errorf("%w: this CalDAV adapter is read-only; "+
		"calendar writes and invitations are separately controlled", adapter.ErrNoSuchCapability)
}

var _ adapter.Upstream = (*Upstream)(nil)
