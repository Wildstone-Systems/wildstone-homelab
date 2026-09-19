package api

// The household's own calendar page (/calendar/).
//
// The home page's card shows three days and hands off to a phone; this
// is the page that card opens, where a member sees the month and adds,
// moves or removes an event without leaving Wildstone. Radicale's own
// web UI manages COLLECTIONS, not events, so without this page the only
// way to put something on the family calendar from a browser was to ask
// the assistant.
//
// IT IS THE SHARED CALENDAR ONLY. Keystone reaches Radicale as the
// calendar's service account, which holds the household collection and
// nobody's personal ones — and a portal member arrives by forwarded
// identity, with no CalDAV password Keystone could use on their behalf.
// Personal calendars stay where their owner's own login reaches them.
//
// THE GATE IS THE CALENDAR'S OWN GROUP. The surface is keyed on the
// `calendar` capability, so portalGroup finds calendar_users — the
// LdapGroup that package declares and the group Radicale's own filter
// admits. One grant, two doors, the same people through both.
//
// AN EVENT IS ADDRESSED ONLY INSIDE THE COLLECTION. The CalDAV adapter
// will follow any absolute href it is given, with the calendar's
// credential attached, because its other caller — the assistant — only
// ever passes hrefs Radicale itself reported. A browser can send
// anything. So every href from this page is resolved here and refused
// unless it is a path under the household collection.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

var surfaceCalendar = portalSurface{capability: calendarCapabilityName, title: "Calendar"}

// CalendarBook is the shared household calendar, read and written.
//
// A seam for the same reason CalendarEvents is one: this package holds
// no calendar credential. cmd/keystone hands down the upstream the
// assistant's calendar actions already use, so the page and the
// assistant write through one client with one credential.
type CalendarBook interface {
	Events(ctx context.Context, from, to time.Time) ([]caldav.Event, error)
	CreateEvent(ctx context.Context, req caldav.EventWrite) (adapter.Answer, error)
	UpdateEvent(ctx context.Context, req caldav.EventWrite) (adapter.Answer, error)
	DeleteEvent(ctx context.Context, req caldav.EventWrite) (adapter.Answer, error)
	// EventAt reads one event by the address a create returned; nil when
	// it has been deleted. It is how a booking follows its appointment.
	EventAt(ctx context.Context, href string) (*caldav.Event, error)
}

// maxCalendarPageDays bounds one read. A month view with its leading
// and trailing weeks is 42 days; this leaves room without letting a
// query string ask Radicale for a decade.
const maxCalendarPageDays = 62

// maxCalendarWriteBytes bounds one add or change: a title, two dates,
// two times and a place, with plenty of room to spare.
const maxCalendarWriteBytes = 16 << 10

// calendarPageEvent is one event as the page draws it. Day and EndDay
// are in the household's timezone, so a tablet with its clock set
// elsewhere still files dinner under the right date.
type calendarPageEvent struct {
	// Href is the event's address in the collection, under a key the
	// redactor shelters (redact.calendarEventRefScanRegexp): its file
	// name is a UID, which is token-shaped.
	Href   string    `json:"calendarEvent"`
	Title  string    `json:"title"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end,omitzero"`
	AllDay bool      `json:"allDay,omitempty"`
	// Day is the first day the event is on; LastDay the last, inclusive.
	Day     string `json:"day"`
	LastDay string `json:"lastDay"`
	// StartTime and EndTime are wall-clock "15:04" in the household's
	// zone, empty for an all-day event.
	StartTime string `json:"startTime,omitempty"`
	EndTime   string `json:"endTime,omitempty"`
	// Location and Notes are the event's place and description.
	Location string `json:"location,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

func (s *Server) calendarLoc() *time.Location {
	if l := s.householdLoc(); l != nil {
		return l
	}
	return time.UTC
}

// calendarReady says whether the page has a calendar behind it, and if
// not, why — in words a member can act on.
func (s *Server) calendarReady() (int, error) {
	if s.CalendarBook == nil || s.CalendarCollectionURL == "" {
		return http.StatusServiceUnavailable, fmt.Errorf(
			"the calendar is installed, but Wildstone hasn't been given its sign-in yet, so it can't show or add events")
	}
	return 0, nil
}

// calendarPageEvents answers GET /api/v1/calendar/events?from=&to=,
// both YYYY-MM-DD in the household's zone, to inclusive.
func (s *Server) calendarPageEvents(_ http.ResponseWriter, r *http.Request, _ string, _ []resource.Resource) (any, int, error) {
	if status, err := s.calendarReady(); err != nil {
		return nil, status, err
	}
	loc := s.calendarLoc()
	from, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("from"), loc)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("from must be a date (YYYY-MM-DD)")
	}
	to, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("to"), loc)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("to must be a date (YYYY-MM-DD)")
	}
	to = to.AddDate(0, 0, 1) // inclusive of the last day
	if !to.After(from) || to.Sub(from) > maxCalendarPageDays*24*time.Hour {
		return nil, http.StatusBadRequest, fmt.Errorf("ask for between 1 and %d days", maxCalendarPageDays)
	}

	events, err := s.CalendarBook.Events(r.Context(), from, to)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("the calendar didn't answer: %w", err)
	}
	out := make([]calendarPageEvent, 0, len(events))
	for _, e := range events {
		out = append(out, pageEvent(e, loc))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		// All-day first, then by time — the order a paper calendar has.
		if out[i].AllDay != out[j].AllDay {
			return out[i].AllDay
		}
		return out[i].Start.Before(out[j].Start)
	})
	now := time.Now().In(loc)
	return map[string]any{
		"events":   out,
		"today":    now.Format("2006-01-02"),
		"timezone": loc.String(),
	}, http.StatusOK, nil
}

func pageEvent(e caldav.Event, loc *time.Location) calendarPageEvent {
	p := calendarPageEvent{Href: e.Href, Title: e.Summary, Start: e.Start, End: e.End, AllDay: e.AllDay,
		Location: e.Location, Notes: e.Description}
	if e.AllDay {
		// A DATE is a calendar day, not an instant: read its fields as
		// they are rather than converting them through a zone. DTEND is
		// exclusive, so the last day is the one before it.
		p.Day = e.Start.Format("2006-01-02")
		last := e.Start
		if !e.End.IsZero() && e.End.After(e.Start) {
			last = e.End.AddDate(0, 0, -1)
		}
		p.LastDay = last.Format("2006-01-02")
		return p
	}
	start := e.Start.In(loc)
	p.Day, p.StartTime = start.Format("2006-01-02"), start.Format("15:04")
	p.LastDay = p.Day
	if !e.End.IsZero() {
		end := e.End.In(loc)
		p.EndTime = end.Format("15:04")
		// An event ending at midnight ends on the day before, as far as
		// a person reading a calendar is concerned.
		lastInstant := end.Add(-time.Nanosecond)
		if d := lastInstant.Format("2006-01-02"); d > p.Day {
			p.LastDay = d
		}
	}
	return p
}

// calendarPageWrite is what the page sends to add or change an event:
// dates and wall-clock times in the household's zone, which the browser
// can produce without knowing that zone's offset on the day in question.
type calendarPageWrite struct {
	Href      string `json:"href,omitempty"`
	Title     string `json:"title"`
	Date      string `json:"date"`
	EndDate   string `json:"endDate,omitempty"`
	AllDay    bool   `json:"allDay,omitempty"`
	StartTime string `json:"startTime,omitempty"`
	EndTime   string `json:"endTime,omitempty"`
	Location  string `json:"location,omitempty"`
	Notes     string `json:"notes,omitempty"`
}

// toEventWrite turns the page's dates and times into the adapter's
// RFC3339 window. All-day ends are exclusive, as iCalendar has them.
//
// On a change (existing), place and notes are always sent, so emptying
// one on the page clears it; on an add, only what was filled in is.
func (w calendarPageWrite) toEventWrite(loc *time.Location, existing bool) (caldav.EventWrite, error) {
	title := strings.TrimSpace(w.Title)
	if title == "" {
		return caldav.EventWrite{}, fmt.Errorf("give the event a name")
	}
	day, err := time.ParseInLocation("2006-01-02", w.Date, loc)
	if err != nil {
		return caldav.EventWrite{}, fmt.Errorf("pick a date")
	}
	lastDay := day
	if w.EndDate != "" {
		if lastDay, err = time.ParseInLocation("2006-01-02", w.EndDate, loc); err != nil {
			return caldav.EventWrite{}, fmt.Errorf("the end date isn't a date")
		}
		if lastDay.Before(day) {
			return caldav.EventWrite{}, fmt.Errorf("the event ends before it starts")
		}
	}
	out := caldav.EventWrite{Summary: &title, AllDay: w.AllDay}
	where, notes := strings.TrimSpace(w.Location), strings.TrimSpace(w.Notes)
	if existing || where != "" {
		out.Location = &where
	}
	if existing || notes != "" {
		out.Description = &notes
	}
	if w.AllDay {
		out.Start = day.Format("2006-01-02")
		out.End = lastDay.AddDate(0, 0, 1).Format("2006-01-02")
		return out, nil
	}
	start, err := wallClock(day, w.StartTime, loc)
	if err != nil {
		return caldav.EventWrite{}, fmt.Errorf("pick a start time")
	}
	var end time.Time
	if w.EndTime == "" {
		end = start.Add(time.Hour)
	} else if end, err = wallClock(lastDay, w.EndTime, loc); err != nil {
		return caldav.EventWrite{}, fmt.Errorf("the end time isn't a time")
	}
	if !end.After(start) {
		return caldav.EventWrite{}, fmt.Errorf("the event ends before it starts")
	}
	out.Start, out.End = start.Format(time.RFC3339), end.Format(time.RFC3339)
	return out, nil
}

func wallClock(day time.Time, hhmm string, loc *time.Location) (time.Time, error) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return time.Time{}, err
	}
	return time.Date(day.Year(), day.Month(), day.Day(), t.Hour(), t.Minute(), 0, 0, loc), nil
}

// calendarHref resolves an href from the page and refuses anything that
// is not an event inside the household collection. See the file header.
func (s *Server) calendarHref(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("which event? (no href)")
	}
	base, err := url.Parse(strings.TrimRight(s.CalendarCollectionURL, "/") + "/")
	if err != nil {
		return "", fmt.Errorf("the calendar's address is not a URL")
	}
	ref, err := url.Parse(raw)
	if err != nil || ref.IsAbs() || ref.Host != "" || !strings.HasPrefix(ref.Path, "/") {
		return "", fmt.Errorf("that is not an event in the household calendar")
	}
	clean := path.Clean(ref.Path)
	if !strings.HasPrefix(clean, base.Path) || clean == strings.TrimRight(base.Path, "/") ||
		strings.Contains(strings.TrimPrefix(clean, base.Path), "/") {
		return "", fmt.Errorf("that is not an event in the household calendar")
	}
	return (&url.URL{Scheme: base.Scheme, Host: base.Host, Path: clean}).String(), nil
}

func decodeCalendarWrite(r *http.Request) (calendarPageWrite, error) {
	var req calendarPageWrite
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxCalendarWriteBytes)).Decode(&req); err != nil {
		return req, fmt.Errorf("the request is not an event: %w", err)
	}
	return req, nil
}

// calendarWriteStatus maps an upstream refusal to a status. The
// adapter's own words reach the member: they are written to be read.
func calendarWriteStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

func (s *Server) calendarPageCreate(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if status, err := s.calendarReady(); err != nil {
		return nil, status, err
	}
	req, err := decodeCalendarWrite(r)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	w, err := req.toEventWrite(s.calendarLoc(), false)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	ans, err := s.CalendarBook.CreateEvent(r.Context(), w)
	if err != nil {
		return nil, calendarWriteStatus(err), err
	}
	s.event("calendar.create", "", principal.Identity{Name: portalPrincipal, ActingFor: member},
		fmt.Sprintf("%s added %q on %s", member, strings.TrimSpace(req.Title), req.Date))
	return ans.Data, http.StatusOK, nil
}

func (s *Server) calendarPageUpdate(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if status, err := s.calendarReady(); err != nil {
		return nil, status, err
	}
	req, err := decodeCalendarWrite(r)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	href, err := s.calendarHref(req.Href)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	w, err := req.toEventWrite(s.calendarLoc(), true)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	w.Href = href
	ans, err := s.CalendarBook.UpdateEvent(r.Context(), w)
	if err != nil {
		return nil, calendarWriteStatus(err), err
	}
	s.event("calendar.update", "", principal.Identity{Name: portalPrincipal, ActingFor: member},
		fmt.Sprintf("%s changed %q", member, strings.TrimSpace(req.Title)))
	return ans.Data, http.StatusOK, nil
}

func (s *Server) calendarPageDelete(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if status, err := s.calendarReady(); err != nil {
		return nil, status, err
	}
	req, err := decodeCalendarWrite(r)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	href, err := s.calendarHref(req.Href)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	ans, err := s.CalendarBook.DeleteEvent(r.Context(), caldav.EventWrite{Href: href})
	if err != nil {
		return nil, calendarWriteStatus(err), err
	}
	s.event("calendar.delete", "", principal.Identity{Name: portalPrincipal, ActingFor: member},
		fmt.Sprintf("%s removed %q", member, strings.TrimSpace(req.Title)))
	return ans.Data, http.StatusOK, nil
}
