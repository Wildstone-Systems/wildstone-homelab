package api

// Getting the household's calendar onto a phone (ADR-W034).
//
// This is one endpoint and it exists because of one fact: a CalDAV
// client finds the calendars under the principal it signed in as, and
// nothing else. The household's SHARED calendar lives under its own
// principal — deliberately, so that it belongs to the house rather than
// to whoever happened to create it — and there is no way in iOS's own
// settings to add a writable account rooted anywhere else.
//
// An Apple configuration profile can say it. So the hand-off a household
// member gets is a link, and the link is this: signed in as them, served
// as a profile, naming both principals they are entitled to, carrying no
// password.
//
// The door is the portal's, which means three things are already true
// before this file runs: the request came from the identity proxy, it
// carries a forwarded member, and that member is in the group the
// dashboard capability declares. The profile is built for THAT member —
// there is no parameter naming a person, because a URL that took one
// would be a way to ask for somebody else's.

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"github.com/chickenrat17/wildstone-homelab/keystone/mobileconfig"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// householdCalendarName is what the shared calendar is called on a
// phone. A person scrolling their calendar list should be able to tell
// which entries everybody can see.
const householdCalendarName = "Household"

// calendarSetup is what the card and the profile both need: where the
// calendar is, as a phone reaches it, and which principals this member
// may be pointed at.
type calendarSetup struct {
	// Host and Port are the server as a phone reaches it.
	Host string
	Port int
	// Origin is the same thing as a URL, for a person to read or paste.
	Origin string
	// OwnPrincipal is this member's own space.
	OwnPrincipal string
	// HouseholdCalendar is the shared calendar's own address, empty when
	// this deployment has not configured one.
	HouseholdCalendar string
}

// calendarSetupFor derives the setup from desired state and the
// configured collection.
//
// The address comes from `ingressorigin` through calendarServer, which
// is the same helper the launcher's links use — so under the tailnet
// posture it is the node's certified name and this Route's port rather
// than the host the Route declares (ADR-W019 A10). A profile built any
// other way installs cleanly and then fails to connect, which is the
// worst of the available failures: it looks like it worked.
func (s *Server) calendarSetupFor(member string, desired []resource.Resource) (calendarSetup, bool) {
	origin, found := calendarServer(desired)
	if !found || strings.TrimSpace(member) == "" {
		return calendarSetup{}, false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Hostname() == "" {
		return calendarSetup{}, false
	}
	port := 443
	if p := parsed.Port(); p != "" {
		if n, perr := strconv.Atoi(p); perr == nil {
			port = n
		}
	}
	return calendarSetup{
		Host:              parsed.Hostname(),
		Port:              port,
		Origin:            origin,
		OwnPrincipal:      "/" + member + "/",
		HouseholdCalendar: householdCalendarPath(s.CalendarCollectionURL),
	}, true
}

// householdCalendarPath is the shared calendar's own address, taken from
// the collection Keystone was pointed at.
//
// Derived rather than configured separately, because the two would
// disagree: `-calendar-url` already names the collection the agent tools
// write to, and a second setting for "and this is where it is" is a
// second thing to keep in step.
//
// The whole path, not its parent. The shared calendar is a GROUP
// collection — /GROUPS/<base64 of the group name>/ — so there is no
// principal above it to point a client at, and there does not need to
// be: Radicale's discovery yields a group's collection inside the
// principal of everybody in that group, so a member finds it by signing
// in as themselves. This address is for SAYING, not for mounting.
func householdCalendarPath(collection string) string {
	collection = strings.TrimSpace(collection)
	if collection == "" {
		return ""
	}
	parsed, err := url.Parse(collection)
	if err != nil || strings.Trim(parsed.Path, "/") == "" {
		return ""
	}
	return "/" + strings.Trim(parsed.Path, "/") + "/"
}

// calendarProfile serves the configuration profile.
func (s *Server) calendarProfile(w http.ResponseWriter, _ *http.Request, member string, desired []resource.Resource) (any, int, error) {
	setup, ok := s.calendarSetupFor(member, desired)
	if !ok {
		return nil, http.StatusNotFound, fmt.Errorf("this household has no calendar to add")
	}

	// ONE account, and that is the point of the group collection rather
	// than a shortcut.
	//
	// An earlier version pinned a second account at the shared
	// calendar's own principal, because a CalDAV client discovers only
	// the calendars under the principal it signed in as. The shared
	// calendar is now a GROUP collection, which Radicale's discovery
	// yields inside the principal of everybody in that group — so this
	// one account carries the member's own calendars AND the
	// household's, and a second would mount the same calendar twice.
	profile := mobileconfig.Profile{
		Host: setup.Host, Port: setup.Port, Username: member,
		Accounts: []mobileconfig.Account{{Name: member, PrincipalURL: setup.OwnPrincipal}},
	}

	body, err := profile.Build()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	// iOS decides whether to hand a download to Settings on the content
	// type and nothing else. Served as XML it renders as a page of
	// markup and the household concludes the calendar is broken.
	w.Header().Set("Content-Type", mobileconfig.ContentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+mobileconfig.Filename+`"`)
	// A profile carries a member's username and their household's
	// addresses. It is not secret, and it is not something a shared
	// cache should hand to the next person on the sofa either.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return nil, 0, nil
}

// calendarSetupDetails answers "what do I type", for the surfaces that
// cannot install a profile — a laptop, an Android phone, a person who
// would rather see it written down.
//
// It names no password. The member signs in with the household password
// they already have, and a surface that told them anything else about it
// would be a surface teaching the household to expect a second one.
func (s *Server) calendarSetupDetails(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	setup, ok := s.calendarSetupFor(member, desired)
	if !ok {
		return nil, http.StatusNotFound, fmt.Errorf("this household has no calendar to add")
	}
	out := map[string]any{
		"server":       setup.Origin,
		"username":     member,
		"ownCalendars": setup.Origin + setup.OwnPrincipal,
		"profileURL":   profileURLFor(r.URL.Path),
		"password":     "your household password — the one you sign in here with",
	}
	if setup.HouseholdCalendar != "" {
		out["householdCalendar"] = setup.Origin + setup.HouseholdCalendar
		out["householdNote"] = "the shared household calendar appears beside your own once you sign in; " +
			"this address is only needed if a client does not show it"
	}
	return out, http.StatusOK, nil
}

// profileURLFor answers with the profile door on the SAME surface the
// question arrived through.
//
// This used to name the dashboard's copy unconditionally, which made
// the answer wrong for exactly the caller that needed it most: a member
// on the calendar page who is not in homepage_users would be handed a
// link to a route they are refused at. An address a surface gives out
// has to be one the person it is talking to can actually open.
func profileURLFor(requestPath string) string {
	if strings.HasPrefix(requestPath, "/api/v1/calendar/") {
		return "/api/v1/calendar/profile.mobileconfig"
	}
	return "/api/v1/home/calendar/profile.mobileconfig"
}

// CalendarHandoff is where the household's calendar is and how to add
// it, for one member.
//
// Exported, and it is the only exported thing in this file, because a
// second caller needs exactly this: the Butler's calendar tool, so that
// "how do I get this on my phone" can be answered in the conversation
// where it was asked rather than only on a page somebody has to know to
// open (ADR-W034).
type CalendarHandoff struct {
	// Server is the calendar's address as a phone reaches it.
	Server string
	// Username is the member the answer is for.
	Username string
	// OwnCalendars and HouseholdCalendar are the two addresses this
	// member is entitled to. HouseholdCalendar is empty when the
	// deployment has no shared calendar configured.
	OwnCalendars      string
	HouseholdCalendar string
	// ProfileURL is the configuration profile, as a browser on a PHONE
	// must reach it — an absolute URL, because the answer travels over
	// chat and lands somewhere that has no idea what host it is
	// relative to.
	ProfileURL string
}

// CalendarHandoffFor answers for one member, from current desired state.
//
// It takes only a member name and there is deliberately no variant that
// takes anything else: the caller is a tool the model can reach, so the
// ONE input is the one the surface already bound and the model cannot
// change (butler.HouseholdGateway holds its member for the session's
// life).
func (s *Server) CalendarHandoffFor(member string) (CalendarHandoff, bool, error) {
	if strings.TrimSpace(member) == "" {
		return CalendarHandoff{}, false, fmt.Errorf("a calendar hand-off needs to be for somebody")
	}
	desired, err := s.State.Load()
	if err != nil {
		return CalendarHandoff{}, false, err
	}
	setup, ok := s.calendarSetupFor(member, desired)
	if !ok || !hasWildstoneCalendar(desired) {
		return CalendarHandoff{}, false, nil
	}
	out := CalendarHandoff{
		Server:       setup.Origin,
		Username:     member,
		OwnCalendars: setup.Origin + setup.OwnPrincipal,
	}
	if setup.HouseholdCalendar != "" {
		out.HouseholdCalendar = setup.Origin + setup.HouseholdCalendar
	}
	// The profile is served by the HOME surface, not by the calendar
	// server, so its address is the dashboard's own — resolved the same
	// way every other household link is.
	if home, found := portalSurfaceOrigin(desired, surfaceHome.capability); found {
		out.ProfileURL = home + "/api/v1/home/calendar/profile.mobileconfig"
	}
	return out, true, nil
}

// portalSurfaceOrigin is where a capability's Route is actually served,
// as a browser must type it.
func portalSurfaceOrigin(desired []resource.Resource, capability string) (string, bool) {
	posture := ingressorigin.Of(desired)
	for _, r := range desired {
		if r.Kind != resource.KindRoute || r.Route == nil || r.Route.Host == "" {
			continue
		}
		if strings.TrimPrefix(r.Metadata.Owner, "capability/") == capability {
			return "https://" + posture.SiteAddress(r.Route.Host), true
		}
	}
	return "", false
}

// CalendarCardEvent is one event the home page's calendar card shows.
//
// Narrowed on purpose, and it is the same narrowing the CalDAV adapter
// makes: a title and a time. Description and attendees are the most
// sensitive part of a calendar and a card saying "three things today"
// needs none of it.
type CalendarCardEvent struct {
	Title  string
	Start  time.Time
	AllDay bool
}
