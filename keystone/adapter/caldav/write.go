package caldav

// The write half of the CalDAV adapter (ADR-W034 phase 2): create,
// change and delete one event.
//
// Two things in here are the whole reason it is a file of Go rather than
// a prompt.
//
// ICALENDAR IS GENERATED HERE, DETERMINISTICALLY. An agent resolves
// "dinner on Thursday at half six" into a start and an end and stops
// there; it never writes VCALENDAR. Folding at 75 octets, CRLF line
// endings, and the escaping rules for a summary containing a comma are
// not judgement calls, and a model asked to make them will be right most
// of the time — which is the worst available outcome for a household
// calendar, because the failures are rare, silent, and land on somebody's
// phone.
//
// A CHANGE IS A REWRITE OF THE SERVER'S OWN COPY, NOT A REPLACEMENT.
// This package's reader understands four properties. An event on a
// household's calendar routinely carries a dozen more — attendees,
// recurrence, alarms, the organiser — and regenerating the file from what
// we parsed would silently drop every one of them. So an update fetches
// the stored object and edits the lines it was asked to change, leaving
// everything it does not understand exactly where it found it. Dropping
// the alarm on somebody's medication reminder is not a acceptable cost of
// correcting a typo in its title.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
)

const (
	// CapCreateEvent puts a new event in the collection.
	CapCreateEvent = "caldav.create_event"
	// CapUpdateEvent changes the fields it is given on one event, and
	// leaves the rest of that event alone.
	CapUpdateEvent = "caldav.update_event"
	// CapDeleteEvent removes one event.
	CapDeleteEvent = "caldav.delete_event"
)

// ActionCapabilities is the closed set of things this adapter can do, as
// opposed to answer. Exported so a caller listing what a connection
// would gain by being widened does not have to keep its own copy.
var ActionCapabilities = []string{CapCreateEvent, CapUpdateEvent, CapDeleteEvent}

// maxSummaryBytes bounds the text this adapter will write into one
// property. It is not a limit anybody will meet writing down a dinner;
// it is a limit on what a caller that has gone wrong can put into a file
// the whole household syncs.
const maxSummaryBytes = 1024

// errReadOnly is the refusal a non-writable adapter gives. It is an
// ErrNoSuchCapability rather than a permission error on purpose: the
// runtime maps that onto `unavailable`, which is what "this adapter
// cannot do that" means to a caller, and it is indistinguishable from
// an adapter that was never built with a write path — which is the
// honest thing for a caller to be told.
var errReadOnly = fmt.Errorf("%w: this CalDAV connection is read-only. "+
	"Calendar writes are off until somebody widens the connection and the adapter is run with writes enabled",
	adapter.ErrNoSuchCapability)

// eventWrite is the request body for a create or an update.
//
// It is the plain-language shape an agent tool hands over — a title and
// two times — and NOT iCalendar. Every field is optional on an update
// and only the ones present are touched.
type eventWrite struct {
	// Href addresses an existing event, as a read reported it. Preferred
	// over UID for anything this adapter did not create itself.
	Href string `json:"href,omitempty"`
	// UID addresses an event this adapter created, whose file it named.
	UID string `json:"uid,omitempty"`
	// ETag, when given, makes the write conditional: the change is
	// refused if somebody else has touched the event since it was read.
	ETag string `json:"etag,omitempty"`

	Summary     *string `json:"summary,omitempty"`
	Location    *string `json:"location,omitempty"`
	Description *string `json:"description,omitempty"`
	// Start and End are RFC3339. AllDay switches both to a DATE value,
	// which is what a birthday is and what a 00:00-to-00:00 timed event
	// is not.
	Start  string `json:"start,omitempty"`
	End    string `json:"end,omitempty"`
	AllDay bool   `json:"allDay,omitempty"`
}

// Act performs one declared write.
func (u *Upstream) Act(ctx context.Context, capability string, request json.RawMessage) (adapter.Answer, error) {
	if !u.Writable {
		return adapter.Answer{}, errReadOnly
	}
	var req eventWrite
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return adapter.Answer{}, fmt.Errorf("caldav: %s: the request is not an event: %w", capability, err)
		}
	}
	switch capability {
	case CapCreateEvent:
		return u.createEvent(ctx, req)
	case CapUpdateEvent:
		return u.updateEvent(ctx, req)
	case CapDeleteEvent:
		return u.deleteEvent(ctx, req)
	default:
		return adapter.Answer{}, fmt.Errorf("%w: %s", adapter.ErrNoSuchCapability, capability)
	}
}

func (u *Upstream) newUID() string {
	if u.NewUID != nil {
		return u.NewUID()
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A UID that repeats would overwrite an existing event, so
		// falling back to the clock alone is not acceptable; the clock
		// PLUS the process's own time is unique enough for one
		// collection and this path is unreachable in practice.
		return fmt.Sprintf("wildstone-%d", time.Now().UnixNano())
	}
	return "wildstone-" + hex.EncodeToString(b[:])
}

// createEvent writes a new event and refuses to overwrite anything.
func (u *Upstream) createEvent(ctx context.Context, req eventWrite) (adapter.Answer, error) {
	summary := ""
	if req.Summary != nil {
		summary = *req.Summary
	}
	if strings.TrimSpace(summary) == "" {
		return adapter.Answer{}, fmt.Errorf("caldav: %s needs a summary: an event with no title is one nobody "+
			"can recognise on a phone", CapCreateEvent)
	}
	if strings.TrimSpace(req.Start) == "" {
		return adapter.Answer{}, fmt.Errorf("caldav: %s needs a start: an event with no DTSTART is not a valid "+
			"calendar object, and a zero time would write one dated in the year 1", CapCreateEvent)
	}
	start, end, err := parseWindow(req)
	if err != nil {
		return adapter.Answer{}, err
	}
	if err := boundText(summary, req.Location, req.Description); err != nil {
		return adapter.Answer{}, err
	}

	uid := req.UID
	if strings.TrimSpace(uid) == "" {
		uid = u.newUID()
	}
	if !safeUIDSegment(uid) {
		return adapter.Answer{}, fmt.Errorf("caldav: %s: %q is not usable as a resource name", CapCreateEvent, uid)
	}
	href := strings.TrimRight(u.CollectionURL, "/") + "/" + url.PathEscape(uid) + ".ics"

	body := buildICS(uid, summary, deref(req.Location), deref(req.Description), start, end, req.AllDay, u.now())
	// If-None-Match: * makes this a create rather than a write. Without
	// it a colliding UID silently replaces an event the household has
	// and nothing anywhere reports a loss.
	etag, err := u.put(ctx, href, body, map[string]string{"If-None-Match": "*"})
	if err != nil {
		return adapter.Answer{}, err
	}
	return adapter.Answer{Data: map[string]any{
		"uid": uid, "href": href, "etag": etag,
		"summary": summary, "start": start, "end": end, "allDay": req.AllDay,
		"writtenAt": u.now().UTC(),
	}}, nil
}

// updateEvent changes the named fields on an existing event.
func (u *Upstream) updateEvent(ctx context.Context, req eventWrite) (adapter.Answer, error) {
	href, err := u.resolveHref(req)
	if err != nil {
		return adapter.Answer{}, err
	}
	if err := boundText(deref(req.Summary), req.Location, req.Description); err != nil {
		return adapter.Answer{}, err
	}
	var start, end time.Time
	if req.Start != "" || req.End != "" {
		if start, end, err = parseWindow(req); err != nil {
			return adapter.Answer{}, err
		}
	}

	current, currentETag, err := u.get(ctx, href)
	if err != nil {
		return adapter.Answer{}, err
	}
	// The caller's etag wins when it gave one: it is saying "change the
	// event I read", and a write that silently lands on a newer version
	// is the lost update this header exists to prevent.
	ifMatch := req.ETag
	if ifMatch == "" {
		ifMatch = currentETag
	}

	updated, changed := applyChanges(current, req, start, end, u.now())
	if !changed {
		return adapter.Answer{}, fmt.Errorf("caldav: %s was given nothing to change", CapUpdateEvent)
	}
	headers := map[string]string{}
	if ifMatch != "" {
		headers["If-Match"] = ifMatch
	}
	etag, err := u.put(ctx, href, updated, headers)
	if err != nil {
		return adapter.Answer{}, err
	}
	return adapter.Answer{Data: map[string]any{
		"href": href, "etag": etag, "writtenAt": u.now().UTC(),
	}}, nil
}

// deleteEvent removes one event.
func (u *Upstream) deleteEvent(ctx context.Context, req eventWrite) (adapter.Answer, error) {
	href, err := u.resolveHref(req)
	if err != nil {
		return adapter.Answer{}, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodDelete, href, nil)
	if err != nil {
		return adapter.Answer{}, err
	}
	if req.ETag != "" {
		r.Header.Set("If-Match", req.ETag)
	}
	res, err := u.do(r)
	if err != nil {
		return adapter.Answer{}, err
	}
	defer res.Body.Close()
	if err := writeStatus(res, CapDeleteEvent); err != nil {
		return adapter.Answer{}, err
	}
	return adapter.Answer{Data: map[string]any{"href": href, "deletedAt": u.now().UTC()}}, nil
}

// resolveHref turns a request's address into a URL.
//
// An href the server reported is used as given; a bare UID is only ever
// resolved to the <uid>.ics this adapter would itself have written,
// because that guess is right for its own events and wrong for
// everybody else's.
func (u *Upstream) resolveHref(req eventWrite) (string, error) {
	base := strings.TrimRight(u.CollectionURL, "/")
	if h := strings.TrimSpace(req.Href); h != "" {
		if strings.HasPrefix(h, "http://") || strings.HasPrefix(h, "https://") {
			return h, nil
		}
		root, err := url.Parse(base)
		if err != nil {
			return "", fmt.Errorf("caldav: the collection URL is not a URL: %w", err)
		}
		rel, err := url.Parse(h)
		if err != nil {
			return "", fmt.Errorf("caldav: %q is not a usable event address: %w", h, err)
		}
		return root.ResolveReference(rel).String(), nil
	}
	if !safeUIDSegment(req.UID) {
		return "", fmt.Errorf("caldav: this write names no event: give the href a read reported, or the uid of an " +
			"event this connection created")
	}
	return base + "/" + url.PathEscape(req.UID) + ".ics", nil
}

// applyChanges rewrites the properties it was asked to and leaves every
// other line of the stored object untouched. See the file comment.
func applyChanges(current string, req eventWrite, start, end, now time.Time) (string, bool) {
	replacements := map[string]string{}
	if req.Summary != nil {
		replacements["SUMMARY"] = "SUMMARY:" + escapeICS(*req.Summary)
	}
	if req.Location != nil {
		replacements["LOCATION"] = "LOCATION:" + escapeICS(*req.Location)
	}
	if req.Description != nil {
		replacements["DESCRIPTION"] = "DESCRIPTION:" + escapeICS(*req.Description)
	}
	if !start.IsZero() {
		replacements["DTSTART"] = icsTimeProperty("DTSTART", start, req.AllDay)
	}
	if !end.IsZero() {
		replacements["DTEND"] = icsTimeProperty("DTEND", end, req.AllDay)
	}
	if len(replacements) == 0 {
		return current, false
	}

	// The edit applies to properties of the VEVENT ITSELF and to nothing
	// else in the file. A stored object routinely carries a VTIMEZONE,
	// which has a DTSTART of its own, and a VALARM, which has its own
	// properties too — rewriting either by name would move a timezone's
	// standard-time rule to the evening somebody is having dinner.
	var out []string
	seen := map[string]bool{}
	var changed bool
	var stack []string
	for _, line := range unfold(current) {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			out = append(out, line)
			continue
		}
		key, _, _ := strings.Cut(name, ";")
		key = strings.ToUpper(strings.TrimSpace(key))
		switch key {
		case "BEGIN":
			stack = append(stack, strings.ToUpper(strings.TrimSpace(value)))
			out = append(out, line)
			continue
		case "END":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			out = append(out, line)
			continue
		}
		if len(stack) == 0 || stack[len(stack)-1] != "VEVENT" {
			out = append(out, line)
			continue
		}
		if repl, want := replacements[key]; want {
			out = append(out, repl)
			seen[key] = true
			changed = true
			continue
		}
		// The stamp says when the object last changed, and a client
		// that syncs on it will not notice an edit that leaves it
		// alone.
		if key == "DTSTAMP" || key == "LAST-MODIFIED" {
			out = append(out, key+":"+icsUTC(now))
			continue
		}
		out = append(out, line)
	}

	// A property the event did not have yet — a location on an event
	// that never had one — goes in just before the event ends, which is
	// the only place it is unambiguously part of THIS component.
	var missing []string
	for key, repl := range replacements {
		if !seen[key] {
			missing = append(missing, key+"\x00"+repl)
		}
	}
	if len(missing) > 0 {
		out, changed = insertBeforeEnd(out, missing), true
	}
	return fold(out), changed
}

// insertBeforeEnd places new properties immediately before END:VEVENT.
func insertBeforeEnd(lines []string, pending []string) []string {
	// Deterministic order, so the same edit produces the same file.
	sortStrings(pending)
	var out []string
	inserted := false
	for _, line := range lines {
		if !inserted && strings.EqualFold(strings.TrimSpace(line), "END:VEVENT") {
			for _, p := range pending {
				_, repl, _ := strings.Cut(p, "\x00")
				out = append(out, repl)
			}
			inserted = true
		}
		out = append(out, line)
	}
	if !inserted {
		// No VEVENT to be inside. Appending would produce an object no
		// client can read, so the safer answer is to change nothing —
		// the caller learns from the unchanged etag.
		return lines
	}
	return out
}

// sortStrings is a three-line insertion sort rather than an import: the
// slice is at most five entries long and this file's only other use of
// sort would be this.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// --- HTTP ---

func (u *Upstream) do(req *http.Request) (*http.Response, error) {
	if u.Username != "" {
		req.SetBasicAuth(u.Username, u.Password)
	}
	res, err := u.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", adapter.ErrUnavailable, err)
	}
	return res, nil
}

func (u *Upstream) get(ctx context.Context, href string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, href, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "text/calendar")
	res, err := u.do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return "", "", fmt.Errorf("caldav: there is no event at %s", href)
	}
	if res.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 256))
		return "", "", fmt.Errorf("%w: reading the event answered %s: %s", adapter.ErrUnavailable,
			res.Status, strings.TrimSpace(string(snippet)))
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", adapter.ErrUnavailable, err)
	}
	return string(raw), res.Header.Get("ETag"), nil
}

func (u *Upstream) put(ctx context.Context, href, body string, headers map[string]string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, href, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := u.do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if err := writeStatus(res, "the write"); err != nil {
		return "", err
	}
	return res.Header.Get("ETag"), nil
}

// writeStatus turns a response into the error a caller can act on.
//
// The three cases are genuinely different and a household hears them
// differently: 412 means somebody else changed it, 403/401 means this
// credential may not, and anything else means the calendar is not
// answering — which is the one the runtime must report as unavailable so
// that dependent work waits instead of concluding.
func writeStatus(res *http.Response, what string) error {
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return nil
	case res.StatusCode == http.StatusPreconditionFailed:
		return fmt.Errorf("caldav: %s was refused because the event changed since it was read — read it again "+
			"and re-apply the change", what)
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return fmt.Errorf("caldav: %s was refused: this calendar credential may not write here", what)
	default:
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 256))
		return fmt.Errorf("%w: %s answered %s: %s", adapter.ErrUnavailable, what,
			res.Status, strings.TrimSpace(string(snippet)))
	}
}

// --- iCalendar generation ---

// buildICS renders a whole VCALENDAR for a new event.
func buildICS(uid, summary, location, description string, start, end time.Time, allDay bool, now time.Time) string {
	lines := []string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		// A PRODID naming Wildstone is what lets a household — and this
		// adapter's own update path — tell an event the house wrote from
		// one a person typed on a phone.
		"PRODID:-//Wildstone//Calendar//EN",
		"BEGIN:VEVENT",
		"UID:" + escapeICS(uid),
		"DTSTAMP:" + icsUTC(now),
		icsTimeProperty("DTSTART", start, allDay),
	}
	if !end.IsZero() {
		lines = append(lines, icsTimeProperty("DTEND", end, allDay))
	}
	lines = append(lines, "SUMMARY:"+escapeICS(summary))
	if location != "" {
		lines = append(lines, "LOCATION:"+escapeICS(location))
	}
	if description != "" {
		lines = append(lines, "DESCRIPTION:"+escapeICS(description))
	}
	lines = append(lines, "END:VEVENT", "END:VCALENDAR")
	return fold(lines)
}

// icsTimeProperty renders DTSTART/DTEND in the form the value calls for.
//
// An all-day event is a DATE and not a midnight-to-midnight DATETIME.
// The two look interchangeable and are not: a phone in another timezone
// shows a DATETIME birthday on the wrong day.
func icsTimeProperty(name string, t time.Time, allDay bool) string {
	if allDay {
		return name + ";VALUE=DATE:" + t.Format("20060102")
	}
	return name + ":" + icsUTC(t)
}

func icsUTC(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// escapeICS applies RFC 5545's text escaping. Backslash first, or every
// escape this adds is escaped again by the ones after it.
func escapeICS(v string) string {
	r := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`, "\r", `\n`)
	return r.Replace(v)
}

// fold joins lines with CRLF and breaks any longer than 75 OCTETS.
//
// Octets, not characters: the limit is on the encoded line, and a
// continuation that lands in the middle of a UTF-8 sequence produces
// mojibake at best. The loop therefore steps by rune and measures by
// byte.
func fold(lines []string) string {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(foldLine(line))
		b.WriteString("\r\n")
	}
	return b.String()
}

func foldLine(line string) string {
	const limit = 75
	if len(line) <= limit {
		return line
	}
	var b strings.Builder
	width := 0
	for i, r := range line {
		size := len(string(r))
		// After the first, a continuation line opens with a space, which
		// counts toward its own 75.
		if width+size > limit {
			b.WriteString("\r\n ")
			width = 1
		}
		b.WriteString(line[i : i+size])
		width += size
	}
	return b.String()
}

// --- request validation ---

func parseWindow(req eventWrite) (time.Time, time.Time, error) {
	layout := time.RFC3339
	var start, end time.Time
	var err error
	if req.Start != "" {
		if req.AllDay {
			if start, err = parseDateOrTime(req.Start); err != nil {
				return start, end, fmt.Errorf("caldav: start %q is not a date: %w", req.Start, err)
			}
		} else if start, err = time.Parse(layout, req.Start); err != nil {
			return start, end, fmt.Errorf("caldav: start %q is not an RFC3339 time: %w", req.Start, err)
		}
	}
	if req.End != "" {
		if req.AllDay {
			if end, err = parseDateOrTime(req.End); err != nil {
				return start, end, fmt.Errorf("caldav: end %q is not a date: %w", req.End, err)
			}
		} else if end, err = time.Parse(layout, req.End); err != nil {
			return start, end, fmt.Errorf("caldav: end %q is not an RFC3339 time: %w", req.End, err)
		}
	}
	if !start.IsZero() && !end.IsZero() && !end.After(start) {
		return start, end, fmt.Errorf("caldav: the event ends at or before it starts (%s to %s)", req.Start, req.End)
	}
	return start, end, nil
}

func parseDateOrTime(v string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, v)
}

func boundText(summary string, location, description *string) error {
	for name, v := range map[string]string{
		"summary": summary, "location": deref(location), "description": deref(description),
	} {
		if len(v) > maxSummaryBytes {
			return fmt.Errorf("caldav: %s is %d bytes, over the %d this adapter will write",
				name, len(v), maxSummaryBytes)
		}
	}
	return nil
}

// safeUIDSegment reports whether a UID can be the last segment of a URL
// this adapter builds.
//
// It is a positive list rather than an escape, because the value becomes
// a path and a UID is not free text a caller should be able to steer
// with. url.PathEscape would make "../../x" safe to send and still name
// somebody else's resource on a server that normalises.
func safeUIDSegment(uid string) bool {
	uid = strings.TrimSpace(uid)
	if uid == "" || len(uid) > 128 || uid == "." || uid == ".." {
		return false
	}
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '@':
		default:
			return false
		}
	}
	return true
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
