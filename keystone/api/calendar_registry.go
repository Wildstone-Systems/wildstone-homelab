package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
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
func (s *Server) selectCalendar(desired []resource.Resource, member string, groups []string, id string, write bool) (sharedcalendar.Calendar, CalendarBook, string, error) {
	reg, err := calendarRegistry(desired)
	if err != nil {
		return sharedcalendar.Calendar{}, nil, "", err
	}
	if id == "" {
		id = reg.DefaultID()
	}
	for _, c := range reg.Calendars {
		if c.ID == id && c.Allows(member, groups, write) {
			b, u, e := s.calendarBookFor(c)
			return c, b, u, e
		}
	}
	return sharedcalendar.Calendar{}, nil, "", errCalendarAccess
}

// CalendarForActor resolves current declared and projected directory membership
// for tools without proxy headers. A caller never supplies its own groups.
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
}

func (s *Server) calendarChoices(member string, groups []string, desired []resource.Resource) ([]calendarChoice, error) {
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
	for _, c := range reg.Calendars {
		if !c.Allows(member, groups, false) {
			continue
		}
		v, ok := prefs[c.ID]
		if !ok {
			v = true
		}
		out = append(out, calendarChoice{c.ID, c.Name, c.Color, c.Default, c.Allows(member, groups, true), v})
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
	if _, _, _, err := s.selectCalendar(desired, member, forwardedGroups(r), req.Calendar, false); err != nil {
		return nil, 403, err
	}
	if err := s.DB.SetCalendarVisibility(member, req.Calendar, req.Visible); err != nil {
		return nil, 500, err
	}
	return map[string]bool{"saved": true}, 200, nil
}

type calendarReadEvent struct {
	caldav.Event
	Calendar sharedcalendar.Calendar
}

func (s *Server) visibleCalendarEvents(ctx context.Context, member string, groups []string, desired []resource.Resource, from, to time.Time, filter string) ([]calendarReadEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	choices, err := s.calendarChoices(member, groups, desired)
	if err != nil {
		return nil, err
	}
	if filter != "" {
		if _, _, _, err = s.selectCalendar(desired, member, groups, filter, false); err != nil {
			return nil, err
		}
	}
	out := []calendarReadEvent{}
	for _, choice := range choices {
		if (filter != "" && choice.ID != filter) || (filter == "" && !choice.Visible) {
			continue
		}
		c, b, _, err := s.selectCalendar(desired, member, groups, choice.ID, false)
		if err != nil {
			return nil, err
		}
		events, err := b.Events(ctx, from, to)
		if err != nil {
			return nil, err
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
	return out, nil
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
