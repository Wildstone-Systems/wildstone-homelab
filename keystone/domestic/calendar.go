package domestic

// The calendar executors (ADR-W034 phase 4): the household actions
// that make `packages/calendar-radicale` usable by an agent the moment
// it is installed.
//
// They are the ntfy notifier's siblings — declared by a package,
// dispatched through this gateway, tier-checked, timed out and audited —
// and they follow its precedent on layering too: Notifier speaks ntfy's
// HTTP API directly rather than behind an injected seam, and these speak
// CalDAV through keystone/adapter/caldav. The seam that IS injected in
// chat.go exists because that executor needs the runtime DB; this one
// needs a URL and a password.
//
// # Three things are deliberate
//
// THE SUMMARY IS THE WHOLE ANSWER. A family session calls these through
// butler.do_household_action, which hands the model `Result.Summary` and
// drops `Result.Detail` on the floor. So the summary is written to be
// read aloud — "Tomorrow: 3 things. 08:00 school run, 12:30 lunch with
// Ada, 18:30 dinner" — rather than as a log line with the answer in a
// field nobody downstream can see.
//
// AN EVENT IS NAMED BY WHAT IT IS, NOT BY AN ID. Changing or deleting
// takes a title and a date and resolves them against the calendar. Two
// reasons: a CalDAV UID is not something a person says out loud, and it
// would have to survive a round trip through a chat message to be
// useful. Ambiguity is the safety property that falls out of it — "three
// things on Friday match 'dinner'" is a refusal that names the
// candidates, where an id would have silently picked one.
//
// NOTHING HERE PARSES ENGLISH. "Next Thursday at half six" is resolved
// by the model, into a date and a time, before it ever reaches this
// package. What arrives is a value, and a value that is not one is a
// refusal rather than a guess.

import (
	"context"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/householdtz"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav"
)

// CalendarCapability is the capability these executors belong to. It is
// the `metadata.capability` of the package that declares the tools, and
// the two must agree or the gateway refuses every call —
// TestCalendarToolsAreDeclaredByThePackage pins them together.
const CalendarCapability = "calendar"

// Calendar tool names, without the capability prefix.
const (
	CalToolAgenda       = "get_agenda"
	CalToolSearch       = "search_events"
	CalToolAvailability = "check_availability"
	CalToolCreate       = "create_event"
	CalToolUpdate       = "update_event"
	CalToolDelete       = "delete_event"
	// CalToolSetup answers "how do I get this on my phone". It touches
	// the calendar not at all — it reads where the calendar IS — which
	// is why it is a read and why it still belongs here rather than in
	// a page somebody has to know to open.
	CalToolSetup = "how_to_add"
)

// CalendarTools maps each tool to the tier it must be declared at.
//
// Nothing here is above what a household surface may carry (ADR-W024
// A5): none of these touches a container, a node or a plan.
//
// Deleting is at `confirm`, the tier ADR-W024 A5.1 adds. It wanted "do
// it, but ask first", there was no such thing when this capability was
// first written, and the answer was to add it to W024 rather than to
// invent a confirmation primitive inside a calendar executor — the want
// is every destructive household action, not a calendar quirk. Two
// things follow from the tier: a rule firing on its own cannot carry it
// at all, because there is nobody to ask, and an attended call is
// refused until the member has said yes to THAT call.
var CalendarTools = map[string]string{
	CalToolAgenda:       TierRead,
	CalToolSearch:       TierRead,
	CalToolAvailability: TierRead,
	CalToolCreate:       TierDomestic,
	CalToolUpdate:       TierDomestic,
	CalToolDelete:       TierConfirm,
	CalToolSetup:        TierRead,
}

// maxAgendaDays bounds a range read. A household question is about today,
// tomorrow, or this week; a year is a report, and a report is a screen in
// the Console.
const maxAgendaDays = 14

// maxListedEvents bounds what one answer names individually. Past it the
// summary gives a count and the first few, because a model handed eighty
// lines will read eighty lines back to somebody.
const maxListedEvents = 12

// defaultDurationMinutes is how long an event with a start and no end
// lasts. An hour is the ordinary meeting, and guessing is better than
// refusing here: a calendar entry with no end renders as a point in time
// on most phones, which is not what anybody meant by "dinner at 6:30".
const defaultDurationMinutes = 60

// Calendar is the household's calendar, as the action gateway sees it.
//
// One instance backs every tool; Executors returns the registrations.
type Calendar struct {
	Authority HouseholdExecutionAuthority
	// SecretUse is checked for every credential-bearing calendar call. A bound
	// secret never falls back to the deployment credential on denial.
	SecretUse func(context.Context, Call, func(string) error) (bool, error)

	// Upstream is the collection these tools act on. It must be
	// Writable, or the three write tools refuse every call — which is a
	// legitimate posture (a read-only calendar capability) and is
	// reported as such rather than as a failure.
	Upstream *caldav.Upstream
	// Location is the household timezone. It decides what "today" means
	// and how a wall-clock time with no offset is read; a calendar
	// answered in UTC is wrong for most of the day in most of the world.
	Location *time.Location
	// Zone, when set, is the household's live timezone and wins over
	// Location, so an approved change applies without a restart.
	Zone *householdtz.Zone
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// Handoff answers "where is the calendar and how do I add it", for
	// ONE member. nil leaves calendar.how_to_add answering that this
	// deployment cannot say.
	//
	// A seam rather than a field, because the answer is derived from
	// desired state — where a Route is actually served changes with the
	// ingress posture — and this package must not hold a Statefile to
	// find that out. cmd/keystone binds it to the API server's own
	// resolver, which is the same code the dashboard's links use.
	Handoff func(member string) (CalendarHandoff, bool, error)
}

// CalendarHandoff is where the household's calendar is, for one person.
//
// A local shape rather than an import: this package is reached by the
// action gateway and should not learn the API server's types to answer a
// question about three strings.
type CalendarHandoff struct {
	Server            string
	Username          string
	OwnCalendars      string
	HouseholdCalendar string
	ProfileURL        string
}

func (c *Calendar) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Calendar) location() *time.Location {
	if l := c.Zone.Location(); l != nil {
		return l
	}
	if c.Location != nil {
		return c.Location
	}
	return time.UTC
}

// Executors returns one registration per tool, in a stable order.
func (c *Calendar) Executors() []Executor {
	names := make([]string, 0, len(CalendarTools))
	for name := range CalendarTools {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Executor, 0, len(names))
	for _, name := range names {
		out = append(out, &calendarTool{cal: c, tool: name})
	}
	return out
}

// calendarTool is one registered tool. It carries the name rather than
// being a type per tool, because they share every seam and
// differ only in which branch of Execute they take.
type calendarTool struct {
	cal  *Calendar
	tool string
}

func (t *calendarTool) Name() string { return CalendarCapability + "." + t.tool }

// calendarUsage is each tool's params, in the words a model reads. It
// mirrors the Params comment above each handler; times with no offset
// are the household's clock (see moment).
var calendarUsage = map[string]string{
	CalToolAgenda: "What is on, with each event's times, place and notes. params: date " +
		"(today | tomorrow | YYYY-MM-DD, default today), days (1-14, default 1). For \"my next " +
		"event\" or \"what's coming up\", ask for days=14 rather than today alone.",
	CalToolSearch: "Find events by title. params: query (required), from (YYYY-MM-DD, default today), " +
		"days (1-14, default 14).",
	CalToolAvailability: "Is anything on between two times. params: start, end " +
		"(required, times like 2026-09-25T18:30).",
	CalToolCreate: "Add an event. params: title (required), start (required: a time like " +
		"2026-09-25T18:30, or a date with all_day), end or duration_minutes (default 60), " +
		"location, notes, all_day (true for a birthday or a holiday).",
	CalToolUpdate: "Change an event, named by what it is. params: title and date (required: the " +
		"event as it is now), new_title, start, end or duration_minutes, location, notes.",
	CalToolDelete: "Remove an event; the person is asked first. params: title, date (both required).",
	CalToolSetup:  "How to put the household calendar on a phone or laptop. No params.",
}

// Usage implements UsageDescriber.
func (t *calendarTool) Usage() string { return calendarUsage[t.tool] }

func (t *calendarTool) Execute(ctx context.Context, c Call) (Result, error) {
	if CalendarTools[t.tool] != TierRead {
		if err := checkExecution(t.cal.Authority); err != nil {
			return Result{}, err
		}
	}
	// how_to_add is about where the calendar IS, so it answers on a
	// deployment that has a calendar and has not pointed the agent
	// tools at it — which is a real posture and the one a household is
	// in between installing the capability and wiring the credential.
	if t.tool != CalToolSetup && (t.cal.Upstream == nil || t.cal.Upstream.CollectionURL == "") {
		return Result{}, fmt.Errorf("no calendar is configured on this control plane")
	}
	if CalendarTools[t.tool] != TierRead && t.cal.Upstream != nil {
		local := *t.cal
		upstream := *t.cal.Upstream
		previous := upstream.BeforeWrite
		upstream.BeforeWrite = func() error {
			if err := checkExecution(local.Authority); err != nil {
				return err
			}
			if previous != nil {
				return previous()
			}
			return nil
		}
		local.Upstream = &upstream
		t = &calendarTool{cal: &local, tool: t.tool}
	}
	if t.tool != CalToolSetup && t.cal.SecretUse != nil {
		var result Result
		bound, err := t.cal.SecretUse(ctx, c, func(value string) error {
			local := *t.cal
			upstream := *t.cal.Upstream
			upstream.Password = value
			upstream.HTTP = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			local.Upstream = &upstream
			local.SecretUse = nil
			var err error
			result, err = (&calendarTool{cal: &local, tool: t.tool}).Execute(ctx, c)
			// Even a malicious upstream must not reflect the credential into inference.
			result.Summary = strings.ReplaceAll(result.Summary, value, "[secret omitted]")
			for k, v := range result.Detail {
				result.Detail[k] = strings.ReplaceAll(v, value, "[secret omitted]")
			}
			return err
		})
		if bound || err != nil {
			return result, err
		}
	}
	switch t.tool {
	case CalToolAgenda:
		return t.cal.agenda(ctx, c.Params)
	case CalToolSearch:
		return t.cal.search(ctx, c.Params)
	case CalToolAvailability:
		return t.cal.availability(ctx, c.Params)
	case CalToolCreate:
		return t.cal.create(ctx, c.Params)
	case CalToolUpdate:
		return t.cal.update(ctx, c.Params)
	case CalToolDelete:
		return t.cal.remove(ctx, c.Params)
	case CalToolSetup:
		return t.cal.howToAdd(c)
	default:
		return Result{}, fmt.Errorf("calendar has no tool %q", t.tool)
	}
}

// --- reads ---

// agenda answers "what is on".
//
// Params:
//
//	date  (optional) today | tomorrow | YYYY-MM-DD; default today
//	days  (optional) 1..14; default 1
func (c *Calendar) agenda(ctx context.Context, params map[string]string) (Result, error) {
	day, err := c.day(params["date"])
	if err != nil {
		return Result{}, err
	}
	days := 1
	if v := strings.TrimSpace(params["days"]); v != "" {
		if days, err = strconv.Atoi(v); err != nil || days < 1 || days > maxAgendaDays {
			return Result{}, fmt.Errorf("days must be a whole number from 1 to %d, got %q", maxAgendaDays, v)
		}
	}
	to := day.AddDate(0, 0, days)

	events, err := c.Upstream.Events(ctx, day, to)
	if err != nil {
		return Result{}, err
	}
	sortEvents(events)

	label := c.dayLabel(day, days)
	if len(events) == 0 {
		// An empty day is a real answer, and the one most easily
		// reported as a failure. Saying it plainly is the whole point.
		return Result{
			Summary: label + ": nothing on the calendar.",
			Detail:  map[string]string{"from": day.Format(time.RFC3339), "to": to.Format(time.RFC3339), "count": "0"},
		}, nil
	}
	return Result{
		Summary: fmt.Sprintf("%s: %s. %s", label, countOf(len(events)), c.render(events, days > 1)),
		Detail: map[string]string{
			"from": day.Format(time.RFC3339), "to": to.Format(time.RFC3339),
			"count": strconv.Itoa(len(events)),
		},
	}, nil
}

// search finds events by title.
//
// Params:
//
//	query (required) text to look for in the title, case-insensitive
//	from  (optional) YYYY-MM-DD; default today
//	days  (optional) 1..14; default 14
func (c *Calendar) search(ctx context.Context, params map[string]string) (Result, error) {
	query := strings.TrimSpace(params["query"])
	if query == "" {
		return Result{}, fmt.Errorf("param %q is required: say what to look for", "query")
	}
	from, err := c.day(params["from"])
	if err != nil {
		return Result{}, err
	}
	days := maxAgendaDays
	if v := strings.TrimSpace(params["days"]); v != "" {
		if days, err = strconv.Atoi(v); err != nil || days < 1 || days > maxAgendaDays {
			return Result{}, fmt.Errorf("days must be a whole number from 1 to %d, got %q", maxAgendaDays, v)
		}
	}
	to := from.AddDate(0, 0, days)

	events, err := c.Upstream.Events(ctx, from, to)
	if err != nil {
		return Result{}, err
	}
	matched := matching(events, query)
	sortEvents(matched)

	window := fmt.Sprintf("%s to %s", from.Format("Mon 2 Jan"), to.AddDate(0, 0, -1).Format("Mon 2 Jan"))
	if len(matched) == 0 {
		return Result{
			Summary: fmt.Sprintf("Nothing matching %q between %s.", query, window),
			Detail:  map[string]string{"query": query, "count": "0"},
		}, nil
	}
	return Result{
		Summary: fmt.Sprintf("%s matching %q between %s. %s",
			countOf(len(matched)), query, window, c.render(matched, true)),
		Detail: map[string]string{"query": query, "count": strconv.Itoa(len(matched))},
	}, nil
}

// availability answers "is that time free".
//
// Params:
//
//	start (required) a time
//	end   (required) a time after start
func (c *Calendar) availability(ctx context.Context, params map[string]string) (Result, error) {
	start, err := c.moment(params["start"], "start")
	if err != nil {
		return Result{}, err
	}
	end, err := c.moment(params["end"], "end")
	if err != nil {
		return Result{}, err
	}
	if !end.After(start) {
		return Result{}, fmt.Errorf("the window ends at or before it starts")
	}

	events, err := c.Upstream.Events(ctx, start, end)
	if err != nil {
		return Result{}, err
	}
	// The server filters by time-range already; overlapping is what that
	// means, so anything returned is a clash.
	sortEvents(events)
	window := fmt.Sprintf("%s to %s", start.In(c.location()).Format("Mon 2 Jan 15:04"), end.In(c.location()).Format("15:04"))
	if len(events) == 0 {
		return Result{
			Summary: "Free: nothing on the calendar from " + window + ".",
			Detail:  map[string]string{"free": "true", "conflicts": "0"},
		}, nil
	}
	return Result{
		Summary: fmt.Sprintf("Busy from %s. %s already there: %s", window, countOf(len(events)), c.render(events, false)),
		Detail:  map[string]string{"free": "false", "conflicts": strconv.Itoa(len(events))},
	}, nil
}

// --- writes ---

// create adds an event.
//
// Params:
//
//	title            (required)
//	start            (required) a time, or a date with all_day
//	end              (optional) a time; defaults to start + duration_minutes
//	duration_minutes (optional) default 60
//	location         (optional)
//	all_day          (optional) true for a birthday, a holiday, a bin day
func (c *Calendar) create(ctx context.Context, params map[string]string) (Result, error) {
	title := strings.TrimSpace(params["title"])
	if title == "" {
		return Result{}, fmt.Errorf("param %q is required: an event with no title is one nobody can recognise", "title")
	}
	allDay := isTrue(params["all_day"])

	write := caldav.EventWrite{Summary: &title, AllDay: allDay}
	if loc := strings.TrimSpace(params["location"]); loc != "" {
		write.Location = &loc
	}
	if notes := strings.TrimSpace(params["notes"]); notes != "" {
		write.Description = &notes
	}

	if allDay {
		day, err := c.dateOnly(params["start"], "start")
		if err != nil {
			return Result{}, err
		}
		end := day.AddDate(0, 0, 1)
		if v := strings.TrimSpace(params["end"]); v != "" {
			if end, err = c.dateOnly(v, "end"); err != nil {
				return Result{}, err
			}
		}
		write.Start, write.End = day.Format("2006-01-02"), end.Format("2006-01-02")
	} else {
		start, err := c.moment(params["start"], "start")
		if err != nil {
			return Result{}, err
		}
		end, err := c.endOf(start, params)
		if err != nil {
			return Result{}, err
		}
		write.Start, write.End = start.Format(time.RFC3339), end.Format(time.RFC3339)
	}

	if _, err := c.Upstream.CreateEvent(ctx, write); err != nil {
		return Result{}, err
	}
	when := c.describeWrite(write)
	return Result{
		Summary: fmt.Sprintf("Added %q, %s.", title, when),
		Detail:  map[string]string{"title": title, "start": write.Start, "allDay": strconv.FormatBool(allDay)},
	}, nil
}

// update changes one event, found by what it is called and when it is.
//
// Params:
//
//	title            (required) the event to change
//	date             (required) the day it is on
//	new_title        (optional)
//	start, end       (optional) a new time
//	duration_minutes (optional) with start, instead of end
//	location         (optional)
func (c *Calendar) update(ctx context.Context, params map[string]string) (Result, error) {
	found, day, err := c.resolve(ctx, params, CalToolUpdate)
	if err != nil {
		return Result{}, err
	}

	write := caldav.EventWrite{Href: found.Href, UID: found.UID}
	var changes []string
	if v := strings.TrimSpace(params["new_title"]); v != "" {
		write.Summary = &v
		changes = append(changes, "title")
	}
	if v := strings.TrimSpace(params["location"]); v != "" {
		write.Location = &v
		changes = append(changes, "location")
	}
	if v := strings.TrimSpace(params["notes"]); v != "" {
		write.Description = &v
		changes = append(changes, "notes")
	}
	if v := strings.TrimSpace(params["start"]); v != "" {
		start, merr := c.momentOnDay(v, day)
		if merr != nil {
			return Result{}, merr
		}
		end, eerr := c.endOfOnDay(start, params, day)
		if eerr != nil {
			return Result{}, eerr
		}
		write.Start, write.End = start.Format(time.RFC3339), end.Format(time.RFC3339)
		changes = append(changes, "time")
	}
	if len(changes) == 0 {
		return Result{}, fmt.Errorf("nothing to change: give new_title, start or location")
	}

	if _, err := c.Upstream.UpdateEvent(ctx, write); err != nil {
		return Result{}, err
	}
	return Result{
		Summary: fmt.Sprintf("Changed the %s of %q on %s.", strings.Join(changes, " and "), found.Summary, day.Format("Mon 2 Jan")),
		Detail:  map[string]string{"title": found.Summary, "changed": strings.Join(changes, ",")},
	}, nil
}

// remove deletes one event, found the same way.
//
// Params:
//
//	title (required)
//	date  (required)
func (c *Calendar) remove(ctx context.Context, params map[string]string) (Result, error) {
	found, day, err := c.resolve(ctx, params, CalToolDelete)
	if err != nil {
		return Result{}, err
	}
	if _, err := c.Upstream.DeleteEvent(ctx, caldav.EventWrite{Href: found.Href, UID: found.UID}); err != nil {
		return Result{}, err
	}
	return Result{
		Summary: fmt.Sprintf("Deleted %q from %s.", found.Summary, day.Format("Mon 2 Jan")),
		Detail:  map[string]string{"title": found.Summary, "date": day.Format("2006-01-02")},
	}, nil
}

// resolve finds the ONE event a change names, or refuses.
//
// The refusals are the point. Nothing found and several found are
// different mistakes, and both are things the household can correct in
// the next sentence — so each says which it was and what it saw, rather
// than the executor picking a candidate and reporting success.
func (c *Calendar) resolve(ctx context.Context, params map[string]string, tool string) (caldav.Event, time.Time, error) {
	title := strings.TrimSpace(params["title"])
	if title == "" {
		return caldav.Event{}, time.Time{}, fmt.Errorf("param %q is required: name the event to %s", "title", verbOf(tool))
	}
	if strings.TrimSpace(params["date"]) == "" {
		return caldav.Event{}, time.Time{}, fmt.Errorf("param %q is required: say which day the event is on", "date")
	}
	day, err := c.day(params["date"])
	if err != nil {
		return caldav.Event{}, time.Time{}, err
	}

	events, err := c.Upstream.Events(ctx, day, day.AddDate(0, 0, 1))
	if err != nil {
		return caldav.Event{}, time.Time{}, err
	}
	matched := matching(events, title)
	sortEvents(matched)

	switch len(matched) {
	case 1:
		return matched[0], day, nil
	case 0:
		if len(events) == 0 {
			return caldav.Event{}, day, fmt.Errorf("there is nothing on the calendar on %s", day.Format("Mon 2 Jan"))
		}
		return caldav.Event{}, day, fmt.Errorf("nothing on %s matches %q. That day has: %s",
			day.Format("Mon 2 Jan"), title, titlesOf(events))
	default:
		// Named rather than picked. Choosing one here would report
		// success for a change the household did not ask for, and the
		// only place that surfaces is somebody's phone.
		return caldav.Event{}, day, fmt.Errorf("%d events on %s match %q: %s. Say which one",
			len(matched), day.Format("Mon 2 Jan"), title, titlesOf(matched))
	}
}

// --- time reading ---
//
// Everything a model sends arrives as a string, and these are the only
// three shapes accepted. A value that is none of them is a refusal: an
// executor that guessed would put an appointment on the wrong day and
// report that it had done what was asked.

// day reads a whole day: "today", "tomorrow", "yesterday" or YYYY-MM-DD.
func (c *Calendar) day(v string) (time.Time, error) {
	loc := c.location()
	now := c.now().In(loc)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "today":
		return midnight, nil
	case "tomorrow":
		return midnight.AddDate(0, 0, 1), nil
	case "yesterday":
		return midnight.AddDate(0, 0, -1), nil
	}
	return c.dateOnly(v, "date")
}

// dateOnly reads YYYY-MM-DD in the household's timezone.
func (c *Calendar) dateOnly(v, field string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(v), c.location())
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be a date as YYYY-MM-DD (or today/tomorrow), got %q", field, v)
	}
	return t, nil
}

// moment reads a point in time.
//
// Two shapes, and the second is the one that matters in practice: a
// model asked for a time usually produces "2026-09-25T18:30" with no
// offset, and reading that as UTC would put a six-thirty dinner on the
// household's phone at the wrong hour. With no offset it is the
// HOUSEHOLD's clock, which is what the person speaking meant.
func (c *Calendar) moment(v, field string) (time.Time, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return time.Time{}, fmt.Errorf("param %q is required", field)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, c.location()); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%s must be a time like 2026-09-25T18:30 or 2026-09-25T18:30:00-05:00, got %q", field, v)
}

// momentOnDay reads a time that may be given as a clock time alone,
// against the day the event was found on. "move it to 19:00" is the
// ordinary phrasing, and it carries no date.
func (c *Calendar) momentOnDay(v string, day time.Time) (time.Time, error) {
	s := strings.TrimSpace(v)
	for _, layout := range []string{"15:04", "3:04pm", "3pm"} {
		if t, err := time.ParseInLocation(layout, strings.ToLower(s), c.location()); err == nil {
			return time.Date(day.Year(), day.Month(), day.Day(),
				t.Hour(), t.Minute(), 0, 0, c.location()), nil
		}
	}
	return c.moment(v, "start")
}

// endOf resolves an end from an explicit end or a duration.
func (c *Calendar) endOf(start time.Time, params map[string]string) (time.Time, error) {
	if v := strings.TrimSpace(params["end"]); v != "" {
		end, err := c.moment(v, "end")
		if err != nil {
			return time.Time{}, err
		}
		if !end.After(start) {
			return time.Time{}, fmt.Errorf("the event ends at or before it starts")
		}
		return end, nil
	}
	return c.durationEnd(start, params)
}

func (c *Calendar) endOfOnDay(start time.Time, params map[string]string, day time.Time) (time.Time, error) {
	if v := strings.TrimSpace(params["end"]); v != "" {
		end, err := c.momentOnDay(v, day)
		if err != nil {
			return time.Time{}, err
		}
		if !end.After(start) {
			return time.Time{}, fmt.Errorf("the event ends at or before it starts")
		}
		return end, nil
	}
	return c.durationEnd(start, params)
}

func (c *Calendar) durationEnd(start time.Time, params map[string]string) (time.Time, error) {
	minutes := defaultDurationMinutes
	if v := strings.TrimSpace(params["duration_minutes"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 60*24*7 {
			return time.Time{}, fmt.Errorf("duration_minutes must be a whole number of minutes, got %q", v)
		}
		minutes = n
	}
	return start.Add(time.Duration(minutes) * time.Minute), nil
}

// --- rendering, for a summary somebody hears read aloud ---

func (c *Calendar) render(events []caldav.Event, withDay bool) string {
	loc := c.location()
	shown := events
	suffix := ""
	if len(shown) > maxListedEvents {
		shown = shown[:maxListedEvents]
		suffix = fmt.Sprintf(", and %d more", len(events)-maxListedEvents)
	}
	parts := make([]string, 0, len(shown))
	for _, ev := range shown {
		var when string
		switch {
		case ev.AllDay && withDay:
			when = ev.Start.In(loc).Format("Mon 2 Jan") + " all day"
		case ev.AllDay:
			when = "all day"
		case withDay:
			when = ev.Start.In(loc).Format("Mon 2 Jan 15:04")
		default:
			when = ev.Start.In(loc).Format("15:04")
		}
		// The end, when the event has one of its own: "06:00-19:00" is
		// a work day, "06:00" is an alarm.
		if !ev.AllDay && ev.End.After(ev.Start) {
			when += "-" + ev.End.In(loc).Format("15:04")
		}
		part := when + " " + ev.Summary
		if where := strings.TrimSpace(ev.Location); where != "" {
			part += " (at: " + where + ")"
		}
		if notes := strings.TrimSpace(ev.Description); notes != "" {
			part += " [notes: " + notes + "]"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ") + suffix + "."
}

func (c *Calendar) dayLabel(day time.Time, days int) string {
	now := c.now().In(c.location())
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, c.location())
	if days > 1 {
		return fmt.Sprintf("%s to %s", day.Format("Mon 2 Jan"), day.AddDate(0, 0, days-1).Format("Mon 2 Jan"))
	}
	switch day.Sub(today) {
	case 0:
		return "Today"
	case 24 * time.Hour:
		return "Tomorrow"
	case -24 * time.Hour:
		return "Yesterday"
	}
	return day.Format("Mon 2 Jan")
}

func (c *Calendar) describeWrite(w caldav.EventWrite) string {
	if w.AllDay {
		if t, err := time.Parse("2006-01-02", w.Start); err == nil {
			return "all day on " + t.Format("Mon 2 Jan")
		}
		return "all day on " + w.Start
	}
	start, err := time.Parse(time.RFC3339, w.Start)
	if err != nil {
		return w.Start
	}
	return start.In(c.location()).Format("Mon 2 Jan at 15:04")
}

// --- small helpers ---

func sortEvents(events []caldav.Event) {
	sort.SliceStable(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })
}

// matching is a case-insensitive substring match on the title. It is
// deliberately not fuzzy: a near-match that silently picked the wrong
// event is the failure resolve's refusals exist to prevent.
func matching(events []caldav.Event, query string) []caldav.Event {
	q := strings.ToLower(strings.TrimSpace(query))
	var out []caldav.Event
	for _, ev := range events {
		if strings.Contains(strings.ToLower(ev.Summary), q) {
			out = append(out, ev)
		}
	}
	return out
}

func titlesOf(events []caldav.Event) string {
	shown := events
	suffix := ""
	if len(shown) > maxListedEvents {
		shown = shown[:maxListedEvents]
		suffix = ", …"
	}
	parts := make([]string, 0, len(shown))
	for _, ev := range shown {
		parts = append(parts, strconv.Quote(ev.Summary))
	}
	return strings.Join(parts, ", ") + suffix
}

func countOf(n int) string {
	if n == 1 {
		return "1 thing"
	}
	return fmt.Sprintf("%d things", n)
}

func verbOf(tool string) string {
	if tool == CalToolDelete {
		return "delete"
	}
	return "change"
}

func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "1":
		return true
	}
	return false
}

var _ Executor = (*calendarTool)(nil)

// howToAdd answers "how do I get the calendar on my phone".
//
// It is written to be READ ALOUD or pasted into a chat message, because
// that is where it lands: the summary is the whole channel to a family
// session (see the file comment), and this particular answer is one a
// person acts on with a phone in their hand.
//
// It answers for c.Subject and for nobody else. There is no param naming
// a member, deliberately: the surface binds who is asking for the life
// of the session and the model cannot name a different person, so the
// one input this tool has is the one that cannot be steered.
func (c *Calendar) howToAdd(call Call) (Result, error) {
	member := strings.TrimSpace(call.Subject)
	if member == "" {
		return Result{}, fmt.Errorf("this session is not bound to a household member, so there is nobody to set up")
	}
	if c.Handoff == nil {
		return Result{}, fmt.Errorf("this control plane cannot say where the calendar is")
	}
	h, ok, err := c.Handoff(member)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, fmt.Errorf("this household has no Wildstone calendar to add")
	}

	var b strings.Builder
	if h.ProfileURL != "" {
		// The profile FIRST, because it is the only thing that puts the
		// shared household calendar on an iPhone — iOS will not add a
		// writable account rooted at a principal it did not discover,
		// and no amount of typing addresses gets round that.
		fmt.Fprintf(&b, "On an iPhone or iPad, open this on the device and install it: %s — "+
			"it adds both calendars and asks for your household password. ", h.ProfileURL)
	}
	fmt.Fprintf(&b, "On anything else, add a CalDAV account with server %s, username %s, "+
		"and your household password", h.Server, h.Username)
	b.WriteString(". The shared household calendar appears beside your own once you sign in — " +
		"you do not add it separately")
	if h.HouseholdCalendar != "" {
		fmt.Fprintf(&b, ", though its own address is %s if a client does not show it", h.HouseholdCalendar)
	}
	b.WriteString(".")

	return Result{
		Summary: b.String(),
		Detail: map[string]string{
			"member": member, "server": h.Server,
			"profile": h.ProfileURL, "household": h.HouseholdCalendar,
		},
	}, nil
}

// Describe implements Describer: what this call would do, in a sentence
// somebody can answer yes or no to.
//
// Only the confirm-tier tool needs it, and only deleting is at that
// tier. Resolving the event HERE is the good part: "three things on
// Friday match 'dinner'" is reported before anybody is asked to approve
// a deletion the gateway could not have carried out, so an ambiguous
// request costs one message rather than a question, an answer, and then
// a refusal.
func (t *calendarTool) Describe(ctx context.Context, c Call) (string, error) {
	if t.tool != CalToolDelete {
		return "", nil
	}
	if t.cal.Upstream == nil || t.cal.Upstream.CollectionURL == "" {
		return "", fmt.Errorf("no calendar is configured on this control plane")
	}
	found, day, err := t.cal.resolve(ctx, c.Params, CalToolDelete)
	if err != nil {
		return "", err
	}
	when := day.Format("Mon 2 Jan")
	if !found.AllDay && !found.Start.IsZero() {
		when = found.Start.In(t.cal.location()).Format("Mon 2 Jan") + " at " +
			found.Start.In(t.cal.location()).Format("15:04")
	}
	return fmt.Sprintf("Delete %q from %s?", found.Summary, when), nil
}

var _ Describer = (*calendarTool)(nil)
