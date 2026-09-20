package api

// One person's day, assembled from what the household already knows.
//
// PROTOTYPE. This is the shape described in ADR-W033 A8–A10 and nothing
// past it: a household surface with a subject selector (A10), reading
// records the household already holds. It adds no per-member calendar
// credential (ADR-W034 C2 stands), no child login and no child Agent
// surface (ADR-W030 Part C stands).
//
// WHERE THE ROWS COME FROM, and why these three:
//
//   - Calendar events the subject is an ATTENDEE of. The shared
//     collection already holds them; what was missing was reading who
//     an event is for, which caldav.Event.Attendees now reports.
//   - Commitments about the subject. `about` already becomes the
//     commitment's owner (handlers_portal_authoring.go), so "Maya's
//     reading book" is already a record with Maya's name on it.
//   - The subject's own unread notices, when they have any. A person
//     who cannot sign in has none, which is correct rather than a gap.
//
// WHAT IS DELIBERATELY NOT HERE. No recurring daily routine — "make
// your bed before school" is a rhythm neither commitments (date and
// meter driven) nor ADR-W024 automations (event driven) express, and
// inventing it inside a read-only view would be the wrong place. The
// agenda will show routines when there is a routine to show.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/upkeep"
)

// agendaDays is how far ahead a day view looks. Long enough that "later
// this week" is answerable, short enough that it is still a day and not
// a planner.
const agendaDays = 7

// maxAgendaNotices bounds the notice rows one person's day can carry.
type agendaPage struct {
	homeCard
	// People are who this household can show a day for: every member on
	// record, children included. Children appear here without being
	// admitted, which is ADR-W033 A8 — being scheduled is not signing
	// in.
	People []agendaPerson `json:"people"`
	// Who is the subject this page was assembled for.
	Who string `json:"who"`
	// Days are consecutive from today in the household's zone.
	Days []agendaDay `json:"days"`
}

type agendaPerson struct {
	Handle string `json:"handle"`
	// Child marks the constrained-Member profile, so a surface can say
	// whose day it is showing without inventing a vocabulary for it.
	Child bool `json:"child,omitempty"`
	// Yours marks the person reading the page, so the selector can open
	// on them.
	Yours bool `json:"yours,omitempty"`
}

type agendaDay struct {
	Date  string       `json:"date"`
	Label string       `json:"label"`
	Items []agendaItem `json:"items"`
}

type agendaItem struct {
	// Kind is event | commitment | notice, so the page can draw each in
	// its own idiom rather than flattening three things into one list
	// of strings.
	Kind   string      `json:"kind"`
	Title  string      `json:"title"`
	At     string      `json:"at,omitempty"`
	AllDay bool        `json:"allDay,omitempty"`
	Detail string      `json:"detail,omitempty"`
	Tone   portal.Tone `json:"tone,omitempty"`
	// Name addresses a commitment, so the page can tick it off through
	// the door that already exists (/home/commitments/{name}/done).
	// Empty for the kinds that are not answerable.
	Name string `json:"name,omitempty"`
	// PartOfDay is "morning" | "afternoon" | "evening" for a routine
	// that belongs somewhere in the day, empty otherwise.
	PartOfDay string `json:"partOfDay,omitempty"`
	// Done reports whether this reader may tick it off. A day that
	// offers a tick it will refuse is worse than one that offers none.
	Done bool `json:"done,omitempty"`
	// Moved marks a cycle that has been shifted off its usual day, so
	// the row can say so. A chore silently appearing on the wrong day
	// looks like a bug in the schedule rather than a decision somebody
	// made.
	Moved bool `json:"moved,omitempty"`
	// sortAt orders the day. Unexported: the page draws what it is
	// given, in the order it is given.
	sortAt time.Time
	// rank is the part-of-day order within a day, so a morning routine
	// sits above an afternoon one even though neither has a clock time.
	rank int
}

// homeAgenda answers one person's day.
func (s *Server) homeAgenda(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this page is not ready yet")
	}
	// A deployment that was never given -household-timezone has no
	// zone, and Time.In(nil) panics. The same fallback homeActivity
	// makes: a day boundary in the server's own zone is wrong by hours
	// at worst, and a panicking dashboard is wrong altogether.
	loc := s.householdLoc()
	if loc == nil {
		loc = time.Local
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	people, err := s.agendaPeople(member)
	if err != nil {
		s.Log.Error("agenda people", "err", err)
		return agendaPage{homeCard: homeCard{Problem: "Can't tell who lives here."}}, http.StatusOK, nil
	}

	// The subject. An unknown or absent `who` falls back to the reader
	// rather than erroring: the page opens on your own day.
	who := strings.TrimSpace(r.URL.Query().Get("who"))
	if !knownPerson(people, who) {
		who = member
	}

	page := agendaPage{homeCard: homeCard{Available: true}, People: people, Who: who}
	byDay := map[string][]agendaItem{}

	s.agendaEvents(r.Context(), who, today, loc, byDay)
	s.agendaCommitments(who, memberWho(r, member), today, loc, byDay)
	s.agendaNotices(who, member, today, loc, byDay)

	for i := 0; i < agendaDays; i++ {
		d := today.AddDate(0, 0, i)
		key := d.Format("2006-01-02")
		items := byDay[key]
		// A day with nothing on it is still a day, for the first two:
		// "nothing today" is an answer a household wants. Beyond that an
		// empty day is noise.
		if len(items) == 0 && i > 1 {
			continue
		}
		// A day is read top to bottom, so it sorts the way a day
		// happens: by part of day first, then by clock time within it.
		// An appointment at four o'clock and an after-dinner chore are
		// both "evening", and the appointment has a time to place it.
		sort.SliceStable(items, func(a, b int) bool {
			if items[a].rank != items[b].rank {
				return items[a].rank < items[b].rank
			}
			return items[a].sortAt.Before(items[b].sortAt)
		})
		page.Days = append(page.Days, agendaDay{Date: key, Label: agendaLabel(d, today), Items: items})
	}
	return page, http.StatusOK, nil
}

// agendaLabel names a day the way somebody standing in a kitchen would.
func agendaLabel(d, today time.Time) string {
	switch d.Sub(today) / (24 * time.Hour) {
	case 0:
		return "Today"
	case 1:
		return "Tomorrow"
	}
	return d.Format("Monday 2 January")
}

// agendaPeople is everyone this household can show a day for.
func (s *Server) agendaPeople(reader string) ([]agendaPerson, error) {
	members, err := s.DB.ListButlerMembers()
	if err != nil {
		return nil, err
	}
	var out []agendaPerson
	for _, m := range members {
		// Offboarded people are gone; disabled ones are not, and a
		// disabled adult still has a dentist appointment.
		if !m.OffboardedAt.IsZero() {
			continue
		}
		out = append(out, agendaPerson{
			Handle: m.Member,
			Child:  m.Profile == runtimedb.ProfileChild,
			Yours:  m.Member == reader,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out, nil
}

func knownPerson(people []agendaPerson, who string) bool {
	for _, p := range people {
		if p.Handle == who {
			return true
		}
	}
	return false
}

// agendaEvents adds the calendar events this person is named on.
//
// Matched on the attendee name the calendar carries, case-insensitively
// against the handle. That is a loose match and it is the honest one:
// an event written on somebody's phone names "Maya", not a person id.
// A tighter binding is a job for the attribution work, not for a
// prototype that would otherwise show an empty day.
func (s *Server) agendaEvents(ctx context.Context, who string, today time.Time, loc *time.Location, byDay map[string][]agendaItem) {
	if s.CalendarBook == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	events, err := s.CalendarBook.Events(ctx, today, today.AddDate(0, 0, agendaDays))
	if err != nil {
		s.Log.Info("agenda events unavailable", "err", err)
		return
	}
	for _, e := range events {
		if !namesPerson(e.Attendees, who) {
			continue
		}
		start := e.Start.In(loc)
		key := start.Format("2006-01-02")
		item := agendaItem{
			Kind: "event", Title: e.Summary, AllDay: e.AllDay,
			Detail: e.Location, sortAt: start, rank: rankOfClock(start, e.AllDay),
		}
		if !e.AllDay {
			item.At = start.Format("3:04 PM")
		}
		byDay[key] = append(byDay[key], item)
	}
}

// rankOfClock places a timed event in the day's three parts, so an
// appointment and a routine sort against each other instead of forming
// two separate lists. Noon and six are where a household already draws
// those lines.
func rankOfClock(at time.Time, allDay bool) int {
	if allDay {
		return resource.PartOfDayRank(resource.PartOfDayMorning)
	}
	switch h := at.Hour(); {
	case h < 12:
		return resource.PartOfDayRank(resource.PartOfDayMorning)
	case h < 18:
		return resource.PartOfDayRank(resource.PartOfDayAfternoon)
	default:
		return resource.PartOfDayRank(resource.PartOfDayEvening)
	}
}

// namesPerson reports whether a calendar's attendee list means this
// household member.
func namesPerson(attendees []string, who string) bool {
	for _, a := range attendees {
		if strings.EqualFold(strings.TrimSpace(a), who) {
			return true
		}
	}
	return false
}

// agendaCommitments adds what this person is meant to be keeping up
// with, subject to who is asking.
func (s *Server) agendaCommitments(who string, asker who, today time.Time, loc *time.Location, byDay map[string][]agendaItem) {
	if s.Upkeep == nil {
		return
	}
	standings, err := s.Upkeep.Evaluate()
	if err != nil {
		s.Log.Info("agenda commitments unavailable", "err", err)
		return
	}
	for _, st := range standings {
		if commitmentOwner(st) != who || st.Disabled {
			continue
		}
		if !s.agendaMaySee(st, asker, who) {
			continue
		}
		// ON THE DAY IT IS DUE. A weekly routine anchored to a Thursday
		// is a Thursday thing, and showing it every day until somebody
		// does it is how a chore chart becomes wallpaper. The calendar
		// rule carries the date; a booking, where there is one, is more
		// specific still and wins.
		//
		// Undated commitments — the meter-driven ones, due at a mileage
		// rather than a date — land on today: "this is outstanding" is
		// the honest place for something with no date of its own, and
		// dropping it would make the agenda quietly incomplete.
		at := today
		scheduled, scheduledOK := calendarDue(st)
		if scheduledOK {
			at = scheduled.In(loc)
		}
		moved := false
		if b := st.Booking; b != nil && !b.At.IsZero() {
			at = b.At.In(loc)
			moved = !scheduledOK || !sameDay(at, scheduled.In(loc))
		}
		if at.Before(today) {
			at = today
		}
		if at.After(today.AddDate(0, 0, agendaDays)) {
			continue
		}
		key := at.Format("2006-01-02")
		item := agendaItem{
			Kind: "commitment", Title: st.Description, sortAt: at,
			Detail: st.Occurrence.Reason, Tone: commitmentTone(st),
			Name: st.Name, PartOfDay: st.PartOfDay,
			// The same predicate the upkeep page answers with, so a tick
			// offered here is a tick that will be accepted there.
			Done:  mayComplete(st, asker) || s.isGuardianOf(asker.member, who),
			Moved: moved,
			rank:  resource.PartOfDayRank(st.PartOfDay),
		}
		if item.Title == "" {
			item.Title = st.Name
		}
		byDay[key] = append(byDay[key], item)
	}
}

// calendarDue is when a clock-driven commitment next falls due.
//
// The soonest calendar rule that knows its own position: a commitment
// may watch several meters, and only the ones on the clock have a date
// to put on a day. A rule that cannot say where it stands is skipped
// rather than guessed at.
func calendarDue(st upkeep.Standing) (time.Time, bool) {
	var best time.Time
	for _, rule := range st.Occurrence.Rules {
		if !rule.Calendar || rule.DueTime.IsZero() {
			continue
		}
		if best.IsZero() || rule.DueTime.Before(best) {
			best = rule.DueTime
		}
	}
	return best, !best.IsZero()
}

// sameDay compares two instants by the day they fall on, which is the
// only comparison a chore's date deserves.
func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// commitmentOwner is the handle a commitment is about, with the
// household/ prefix the records carry stripped off.
func commitmentOwner(st upkeep.Standing) string {
	return strings.TrimPrefix(st.Owner, "household/")
}

// agendaMaySee applies ADR-W033 A9 on top of the existing rule.
//
// visibleTo already says a personal commitment is its owner's business.
// A9 adds the one relationship that rule could not express: a child's
// guardian has standing over the child's records. It is a RECORDED
// relationship, never "the adults" — which is exactly the distinction
// handlers_portal_commitments.go declined to invent for itself.
func (s *Server) agendaMaySee(st upkeep.Standing, asker who, subject string) bool {
	if visibleTo(st, asker) {
		return true
	}
	return s.isGuardianOf(asker.member, subject)
}

// isGuardianOf reports whether this member is the recorded guardian of
// that person.
func (s *Server) isGuardianOf(guardian, child string) bool {
	if guardian == "" || child == "" || guardian == child {
		return false
	}
	members, err := s.DB.ListButlerMembers()
	if err != nil {
		return false
	}
	for _, m := range members {
		if m.Member != child {
			continue
		}
		// Only a child record confers it. An ordinary Member's records
		// are their own however the column is filled in.
		return m.Profile == runtimedb.ProfileChild &&
			strings.TrimPrefix(m.Guardian, "household/") == guardian
	}
	return false
}

// agendaNotices adds the subject's own unread notices.
//
// Only their own, and only when the reader is entitled to them: a
// notice is addressed to a person, and the selector is not a way to
// read somebody else's post. A guardian may, for a child — the same
// standing A9 gives over their other records.
func (s *Server) agendaNotices(who, reader string, today time.Time, loc *time.Location, byDay map[string][]agendaItem) {
	if who != reader && !s.isGuardianOf(reader, who) {
		return
	}
	notices, err := s.DB.ListNotices(maxNotifications, runtimedb.NoticeFilter{
		Recipient: who, Deliverable: true, Unacknowledged: true,
	})
	if err != nil {
		s.Log.Info("agenda notices unavailable", "err", err)
		return
	}
	key := today.Format("2006-01-02")
	for _, n := range notices {
		byDay[key] = append(byDay[key], agendaItem{
			Kind: "notice", Title: s.noticeTitle(n.Kind),
			Detail: n.Kind, sortAt: today, rank: resource.PartOfDayRank(""),
		})
	}
}
