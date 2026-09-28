package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/sharedcalendar"
)

var errCalendarAccess = errors.New("calendar is unavailable or access is not allowed")

func calendarRegistry(desired []resource.Resource) (sharedcalendar.Registry, error) {
	for _, r := range desired {
		if r.Kind == resource.KindConfigFile && r.Metadata.Name == sharedcalendar.ConfigName && r.ConfigFile != nil {
			return sharedcalendar.Parse(r.ConfigFile.Content)
		}
	}
	return sharedcalendar.Legacy(), nil
}
func (s *Server) calendarBookFor(c sharedcalendar.Calendar) (CalendarBook, string, error) {
	u, err := c.CollectionURL(s.CalendarCollectionURL)
	if err != nil {
		return nil, "", err
	}
	if s.CalendarBookFor != nil {
		return s.CalendarBookFor(u), u, nil
	}
	if c.ID == "household" && s.CalendarBook != nil {
		return s.CalendarBook, u, nil
	}
	return nil, "", fmt.Errorf("calendar is not connected")
}

// calendarSubject is who is asking, with their Person and household
// groups read from Wildstone's own records (#372). groups carries the
// directory groups the caller already established: the proxy's forwarded
// list for the portal, declared and projected membership for a Butler.
//
// The member is resolved by the handle they carry NOW, not through
// retired handles: a directory account that happens to share somebody's
// old name is not that somebody.
func (s *Server) calendarSubject(member string, groups []string) sharedcalendar.Subject {
	sub := sharedcalendar.Subject{Member: member, Groups: append([]string(nil), groups...)}
	if s.DB == nil || member == "" {
		return sub
	}
	m, err := s.DB.GetButlerMember(member)
	if err != nil {
		if !errors.Is(err, runtimedb.ErrNotFound) {
			s.Log.Warn("calendar: resolving the member's Person", "err", err)
		}
		return sub
	}
	sub.Person = m.PersonID
	households, err := s.DB.GroupsForPerson(m.PersonID)
	if err != nil {
		s.Log.Warn("calendar: reading the member's household groups", "err", err)
		return sub
	}
	for _, g := range households {
		sub.Groups = append(sub.Groups, g.ID)
	}
	return sub
}

// calendarPeople resolves a registry's member references to Persons. A
// lookup that fails matches nobody rather than falling back to comparing
// names.
func (s *Server) calendarPeople() sharedcalendar.PersonResolver {
	if s.DB == nil {
		return nil
	}
	return func(ref string) string {
		id, err := s.DB.ResolvePersonRef(ref)
		if err != nil {
			s.Log.Warn("calendar: resolving a calendar grant", "err", err)
			return "\x00unresolved"
		}
		return id
	}
}

func (s *Server) calendarAllows(c sharedcalendar.Calendar, member string, groups []string, write bool) bool {
	if c.Personal {
		// Only ever listed for their owner; see personalCalendars.
		return true
	}
	return c.Allows(s.calendarSubject(member, groups), write, s.calendarPeople())
}

func (s *Server) selectCalendar(desired []resource.Resource, member string, groups []string, id string, write bool) (sharedcalendar.Calendar, CalendarBook, string, error) {
	reg, err := calendarRegistry(desired)
	if err != nil {
		return sharedcalendar.Calendar{}, nil, "", err
	}
	if id == "" {
		id = reg.DefaultID()
	}
	for _, c := range reg.Calendars {
		if c.ID == id && s.calendarAllows(c, member, groups, write) {
			b, u, e := s.calendarBookFor(c)
			return c, b, u, e
		}
	}
	return sharedcalendar.Calendar{}, nil, "", errCalendarAccess
}

// CalendarForActor resolves current declared and projected directory membership
// for tools without proxy headers. A caller never supplies its own groups.
//
// Declared shared calendars only. A Butler never reaches a person's own
// calendar (ADR-W034 A9), so no personal identifier resolves here.
func (s *Server) CalendarForActor(member, id string, write bool) (sharedcalendar.Calendar, string, error) {
	desired, err := s.State.Load()
	if err != nil {
		return sharedcalendar.Calendar{}, "", err
	}
	groups := []string{}
	for _, r := range desired {
		if r.LdapUser != nil && r.LdapUser.Username == member {
			groups = append(groups, r.LdapUser.Groups...)
		}
	}
	if s.DB != nil {
		person, ambiguous, e := s.DB.PersonForDirectoryAccount(member)
		if e != nil {
			return sharedcalendar.Calendar{}, "", e
		}
		if !ambiguous && person != "" {
			services, e := s.DB.EntitledServices(person)
			if e != nil {
				return sharedcalendar.Calendar{}, "", e
			}
			for _, r := range desired {
				if r.LdapGroup != nil {
					for _, service := range services {
						if r.Metadata.Owner == "capability/"+service {
							groups = append(groups, r.LdapGroup.Name)
						}
					}
				}
			}
		}
	}
	c, _, u, e := s.selectCalendar(desired, member, groups, id, write)
	return c, u, e
}

type calendarChoice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Color    string `json:"color"`
	Default  bool   `json:"default"`
	Writable bool   `json:"writable"`
	Visible  bool   `json:"visible"`
	// Personal marks one of the member's own calendars.
	Personal bool `json:"personal,omitempty"`
}

// calendarChoices is every calendar the member may read, with their own
// display choices. personal is appended after the declared ones.
func (s *Server) calendarChoices(member string, groups []string, desired []resource.Resource, personal ...sharedcalendar.Calendar) ([]calendarChoice, error) {
	reg, err := calendarRegistry(desired)
	if err != nil {
		return nil, err
	}
	prefs := map[string]bool{}
	if s.DB != nil {
		prefs, err = s.DB.CalendarVisibility(member)
		if err != nil {
			return nil, err
		}
	}
	out := []calendarChoice{}
	for _, c := range append(append([]sharedcalendar.Calendar(nil), reg.Calendars...), personal...) {
		if !s.calendarAllows(c, member, groups, false) {
			continue
		}
		v, ok := prefs[c.ID]
		if !ok {
			v = true
		}
		out = append(out, calendarChoice{c.ID, c.Name, c.Color, c.Default, s.calendarAllows(c, member, groups, true), v, c.Personal})
	}
	return out, nil
}
func (s *Server) calendarPreference(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	var req struct {
		Calendar string `json:"calendar"`
		Visible  bool   `json:"visible"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1024)).Decode(&req); err != nil {
		return nil, 400, err
	}
	if req.Calendar == "" {
		return nil, 400, fmt.Errorf("choose a calendar")
	}
	if _, _, _, err := s.selectPageCalendar(r.Context(), desired, member, forwardedGroups(r), req.Calendar, false); err != nil {
		return nil, 403, err
	}
	if err := s.DB.SetCalendarVisibility(member, req.Calendar, req.Visible); err != nil {
		if errors.Is(err, runtimedb.ErrNoSuchPerson) {
			return nil, http.StatusForbidden, fmt.Errorf("your calendar choices can't be saved because you aren't on the household's list of people yet")
		}
		return nil, 500, err
	}
	return map[string]bool{"saved": true}, 200, nil
}

type calendarReadEvent struct {
	caldav.Event
	Calendar sharedcalendar.Calendar
}

// visibleCalendarEvents reads the declared shared calendars a member may
// read and has not hidden. It is what the dashboard and the agenda show,
// so it never includes anybody's own calendar: those screens are shared
// (ADR-W033 A10), and a person's own calendar belongs on their Calendar
// page only.
func (s *Server) visibleCalendarEvents(ctx context.Context, member string, groups []string, desired []resource.Resource, from, to time.Time, filter string) ([]calendarReadEvent, error) {
	events, _, err := s.calendarEvents(ctx, member, groups, desired, from, to, filter, nil)
	return events, err
}

// calendarEvents merges the member's visible calendars, declared and then
// personal, in a deterministic order.
func (s *Server) calendarEvents(ctx context.Context, member string, groups []string, desired []resource.Resource, from, to time.Time, filter string, personal []personalCalendar) ([]calendarReadEvent, []calendarChoice, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	own := make([]sharedcalendar.Calendar, 0, len(personal))
	for _, p := range personal {
		own = append(own, p.Calendar)
	}
	choices, err := s.calendarChoices(member, groups, desired, own...)
	if err != nil {
		return nil, nil, err
	}
	if filter != "" && !slicesContainsChoice(choices, filter) {
		return nil, nil, errCalendarAccess
	}
	out := []calendarReadEvent{}
	for _, choice := range choices {
		if (filter != "" && choice.ID != filter) || (filter == "" && !choice.Visible) {
			continue
		}
		var c sharedcalendar.Calendar
		var b CalendarBook
		if choice.Personal {
			p := findPersonal(personal, choice.ID)
			if p == nil {
				return nil, nil, errCalendarAccess
			}
			c, b = p.Calendar, p.book
		} else if c, b, _, err = s.selectCalendar(desired, member, groups, choice.ID, false); err != nil {
			return nil, nil, err
		}
		events, err := b.Events(ctx, from, to)
		if err != nil {
			return nil, nil, err
		}
		for _, e := range events {
			out = append(out, calendarReadEvent{e, c})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		if a.Calendar.ID != b.Calendar.ID {
			return a.Calendar.ID < b.Calendar.ID
		}
		return a.Href < b.Href
	})
	return out, choices, nil
}

func slicesContainsChoice(choices []calendarChoice, id string) bool {
	for _, c := range choices {
		if c.ID == id {
			return true
		}
	}
	return false
}

func (s *Server) bookingCalendar(id string) (sharedcalendar.Calendar, CalendarBook, string, error) {
	var desired []resource.Resource
	var err error
	if s.State != nil {
		desired, err = s.State.Load()
		if err != nil {
			return sharedcalendar.Calendar{}, nil, "", err
		}
	}
	reg, err := calendarRegistry(desired)
	if err != nil {
		return sharedcalendar.Calendar{}, nil, "", err
	}
	if id == "" {
		id = reg.BookingCalendar
	}
	for _, c := range reg.Calendars {
		if c.ID == id {
			b, u, e := s.calendarBookFor(c)
			return c, b, u, e
		}
	}
	return sharedcalendar.Calendar{}, nil, "", fmt.Errorf("booking calendar is no longer declared")
}

// ---- A person's own calendars (ADR-W034 C2) ----
//
// Keystone reaches them with the calendar service account, which the
// generated rights file lets into every person's own collections. What
// keeps one member out of another's is here: the principal is derived
// from the member the identity proxy proved, and nothing a request
// carries can name a different one. Only the Calendar page uses these.
// The dashboard, the agenda, bookings and every Butler tool read declared
// shared calendars only.

// personalCalendarPrefix marks a personal calendar's identifier,
// `personal:<principal>/<collection>`. A colon cannot appear in a declared
// calendar id, so the two never collide. The identifier only ever selects
// from the member's own listing, so naming another principal in it
// reaches nothing.
const personalCalendarPrefix = "personal:"

// defaultPersonalColor is used for a calendar whose client set none.
const defaultPersonalColor = "#10b981"

// principalPattern is the directory usernames this addresses. Radicale
// lower-cases every login (lc_username), so its principal is the
// lower-cased username.
var principalPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._@-]{0,63}$`)

// calendarCollections is the part of the CalDAV upstream that lists and
// makes calendars under a principal.
type calendarCollections interface {
	Calendars(ctx context.Context, principalURL string) ([]caldav.Collection, error)
	MakeCalendar(ctx context.Context, collectionURL, name, color string) error
	MakePrincipal(ctx context.Context, principalURL string) error
}

type personalCalendar struct {
	sharedcalendar.Calendar
	URL  string
	book CalendarBook
}

func findPersonal(list []personalCalendar, id string) *personalCalendar {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

// personalPrincipal is the address of the principal a login name signs in
// to, and the collections client that reaches it. Callers pass only the
// member's own names.
func (s *Server) personalPrincipal(login string) (string, calendarCollections, error) {
	name := strings.ToLower(strings.TrimSpace(login))
	if !principalPattern.MatchString(name) || strings.Trim(name, ".") == "" {
		return "", nil, fmt.Errorf("your sign-in name can't be used for a calendar")
	}
	origin, err := url.Parse(s.CalendarCollectionURL)
	if err != nil || origin.Host == "" {
		return "", nil, fmt.Errorf("the calendar is not connected")
	}
	principal := (&url.URL{Scheme: origin.Scheme, Host: origin.Host, Path: "/" + name + "/"}).String()
	if s.CalendarBookFor == nil {
		return "", nil, fmt.Errorf("the calendar is not connected")
	}
	collections, ok := s.CalendarBookFor(principal).(calendarCollections)
	if !ok {
		return "", nil, fmt.Errorf("this calendar can't list personal calendars")
	}
	return principal, collections, nil
}

// personalLogins is every login the member's calendars can be under: the
// name they sign in with now, then the names they signed in with before a
// rename. Radicale keeps a person's calendars under the login that made
// them and does not move them when a login changes, so after a rename the
// page still finds the ones made under the old name (#372). They stay
// reachable here; a phone signed in with the new name sees only the new
// principal, which is where new calendars are made.
func (s *Server) personalLogins(member string) []string {
	if s.DB == nil {
		return []string{member}
	}
	handles, err := s.DB.PersonHandles(member)
	if err != nil {
		s.Log.Warn("calendar: reading a member's earlier names", "err", err)
		return []string{member}
	}
	seen := map[string]bool{}
	var out []string
	for _, h := range handles {
		if l := strings.ToLower(strings.TrimSpace(h)); l != "" && !seen[l] {
			seen[l] = true
			out = append(out, h)
		}
	}
	return out
}

// personalCalendars lists the member's own event calendars.
func (s *Server) personalCalendars(ctx context.Context, member string) ([]personalCalendar, error) {
	var out []personalCalendar
	for i, login := range s.personalLogins(member) {
		principal, collections, err := s.personalPrincipal(login)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			// An earlier name that can't be a principal holds nothing.
			continue
		}
		found, err := collections.Calendars(ctx, principal)
		if err != nil {
			return nil, err
		}
		name := strings.ToLower(strings.TrimSpace(login))
		for _, c := range found {
			// Confined again here, against the principal this derived: a
			// collection is used only if it is directly under it.
			if !strings.HasPrefix(c.URL, principal) || strings.Contains(strings.TrimSuffix(strings.TrimPrefix(c.URL, principal), "/"), "/") {
				continue
			}
			color := c.Color
			if color == "" {
				color = defaultPersonalColor
			}
			out = append(out, personalCalendar{
				Calendar: sharedcalendar.Calendar{ID: personalCalendarPrefix + name + "/" + c.Segment, Name: c.Name, Color: color, Personal: true},
				URL:      c.URL,
				book:     s.CalendarBookFor(c.URL),
			})
		}
	}
	return out, nil
}

// selectPageCalendar is selectCalendar for the Calendar page, which also
// reaches the member's own calendars.
func (s *Server) selectPageCalendar(ctx context.Context, desired []resource.Resource, member string, groups []string, id string, write bool) (sharedcalendar.Calendar, CalendarBook, string, error) {
	if !strings.HasPrefix(id, personalCalendarPrefix) {
		return s.selectCalendar(desired, member, groups, id, write)
	}
	own, err := s.personalCalendars(ctx, member)
	if err != nil {
		return sharedcalendar.Calendar{}, nil, "", err
	}
	if p := findPersonal(own, id); p != nil {
		return p.Calendar, p.book, p.URL, nil
	}
	return sharedcalendar.Calendar{}, nil, "", errCalendarAccess
}

// maxPersonalCalendarName bounds a new calendar's name.
const maxPersonalCalendarName = 120

var calendarColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// calendarPersonalCreate answers POST /api/v1/calendar/personal: make
// the member a new calendar of their own. It is created in Radicale under
// their own account, so it is on their phone too.
func (s *Server) calendarPersonalCreate(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if status, err := s.calendarReady(); err != nil {
		return nil, status, err
	}
	var req struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1024)).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("the request is not a calendar: %w", err)
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > maxPersonalCalendarName || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("give the calendar a name of up to %d characters", maxPersonalCalendarName)
	}
	color := strings.ToLower(strings.TrimSpace(req.Color))
	if color == "" {
		color = defaultPersonalColor
	}
	if !calendarColorPattern.MatchString(color) {
		return nil, http.StatusBadRequest, fmt.Errorf("pick a colour")
	}
	principal, collections, err := s.personalPrincipal(member)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	segment := hex.EncodeToString(raw)
	collection := principal + segment + "/"
	err = collections.MakeCalendar(r.Context(), collection, name, color)
	if errors.Is(err, caldav.ErrNoPrincipal) {
		// They have never signed in to the calendar, so their account
		// has no folder yet. Make it, then the calendar.
		if err = collections.MakePrincipal(r.Context(), principal); err == nil {
			err = collections.MakeCalendar(r.Context(), collection, name, color)
		}
	}
	if err != nil {
		return nil, calendarWriteStatus(err), err
	}
	// The household activity names the act and not the calendar: its
	// name is the member's own business.
	s.event("calendar.personal.create", "", portalActor(member), fmt.Sprintf("%s added a calendar of their own", member))
	login := strings.ToLower(strings.TrimSpace(member))
	return calendarChoice{ID: personalCalendarPrefix + login + "/" + segment, Name: name, Color: color, Writable: true, Visible: true, Personal: true}, http.StatusOK, nil
}

// portalActor is the acting identity the portal records events under.
func portalActor(member string) principal.Identity {
	return principal.Identity{Name: portalPrincipal, ActingFor: member}
}
