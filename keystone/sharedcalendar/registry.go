// Package sharedcalendar defines the credential-free desired calendar registry.
package sharedcalendar

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

const ConfigName = "calendar-registry"
const HouseholdPath = "/GROUPS/Y2FsZW5kYXJfdXNlcnM=/"

type Calendar struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Color        string   `json:"color"`
	Default      bool     `json:"default,omitempty"`
	ReadGroups   []string `json:"readGroups"`
	WriteGroups  []string `json:"writeGroups"`
	ReadMembers  []string `json:"readMembers,omitempty"`
	WriteMembers []string `json:"writeMembers,omitempty"`
}
type Registry struct {
	Calendars       []Calendar `json:"calendars"`
	BookingCalendar string     `json:"bookingCalendar,omitempty"`
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,47}$`)
var colorPattern = regexp.MustCompile(`^#[a-fA-F0-9]{6}$`)

func Legacy() Registry {
	return Registry{Calendars: []Calendar{{ID: "household", Name: "Household", Color: "#3b82f6", Default: true, ReadGroups: []string{"calendar_users"}, WriteGroups: []string{"calendar_users"}}}, BookingCalendar: "household"}
}
func Parse(raw string) (Registry, error) {
	if strings.TrimSpace(raw) == "" {
		return Legacy(), nil
	}
	var r Registry
	if len(raw) > 32768 {
		return r, fmt.Errorf("calendar registry exceeds 32 KiB")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return r, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return r, fmt.Errorf("expected one registry")
	}
	if len(r.Calendars) < 1 || len(r.Calendars) > 32 {
		return r, fmt.Errorf("declare between 1 and 32 calendars")
	}
	seen := map[string]bool{}
	defaults := 0
	for _, c := range r.Calendars {
		if !idPattern.MatchString(c.ID) || seen[c.ID] {
			return r, fmt.Errorf("invalid or duplicate calendar id %q", c.ID)
		}
		seen[c.ID] = true
		if strings.TrimSpace(c.Name) == "" || len(c.Name) > 120 || strings.IndexFunc(c.Name, unicode.IsControl) >= 0 || !colorPattern.MatchString(c.Color) {
			return r, fmt.Errorf("invalid calendar name or color for %s", c.ID)
		}
		for _, list := range [][]string{c.ReadGroups, c.WriteGroups, c.ReadMembers, c.WriteMembers} {
			for _, v := range list {
				if !idPattern.MatchString(v) {
					return r, fmt.Errorf("invalid calendar access subject")
				}
			}
		}
		if len(c.ReadGroups)+len(c.ReadMembers)+len(c.WriteGroups)+len(c.WriteMembers) == 0 {
			return r, fmt.Errorf("calendar %s has no readers", c.ID)
		}
		if c.Default {
			defaults++
		}
	}
	if !seen["household"] {
		return r, fmt.Errorf("retain the household calendar for existing event links")
	}
	if defaults != 1 {
		return r, fmt.Errorf("choose exactly one default calendar")
	}
	if r.BookingCalendar == "" {
		r.BookingCalendar = "household"
	}
	if !seen[r.BookingCalendar] {
		return r, fmt.Errorf("unknown booking calendar")
	}
	return r, nil
}
func (r Registry) DefaultID() string {
	for _, c := range r.Calendars {
		if c.Default {
			return c.ID
		}
	}
	return "household"
}
func (c Calendar) Group() string {
	if c.ID == "household" {
		return "calendar_users"
	}
	return "wildstone_calendar_" + c.ID
}
func (c Calendar) Path() string {
	return "/GROUPS/" + base64.StdEncoding.EncodeToString([]byte(c.Group())) + "/"
}
func (c Calendar) Allows(member string, groups []string, write bool) bool {
	if member == "" {
		return false
	}
	match := func(members, allowed []string) bool {
		if slices.Contains(members, member) {
			return true
		}
		for _, g := range groups {
			if slices.Contains(allowed, g) {
				return true
			}
		}
		return false
	}
	return match(c.WriteMembers, c.WriteGroups) || (!write && match(c.ReadMembers, c.ReadGroups))
}
func (r Registry) JSON() string { b, _ := json.Marshal(r); return string(b) }

// CollectionURL preserves the existing Household URL and derives all other
// addresses from its origin. Neither a declaration nor a browser supplies a URL.
func (c Calendar) CollectionURL(legacy string) (string, error) {
	u, e := url.Parse(legacy)
	if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid configured calendar origin")
	}
	if c.ID != "household" {
		u.Path = c.Path()
		u.RawPath = ""
	}
	return strings.TrimRight(u.String(), "/") + "/", nil
}

// EventURL confines even upstream-supplied hrefs to a single collection.
func EventURL(collection, raw string) (string, error) {
	base, e := url.Parse(strings.TrimRight(collection, "/") + "/")
	if e != nil || base.Host == "" {
		return "", fmt.Errorf("invalid calendar origin")
	}
	ref, e := url.Parse(raw)
	if e != nil || raw == "" || ref.User != nil || ref.RawQuery != "" || ref.Fragment != "" || strings.Contains(ref.Path, "\\") {
		return "", fmt.Errorf("invalid event address")
	}
	for _, part := range strings.Split(ref.Path, "/") {
		if part == "." || part == ".." {
			return "", fmt.Errorf("invalid event path")
		}
	}
	target := base.ResolveReference(ref)
	target.RawPath = ""
	leaf := strings.TrimPrefix(target.Path, base.Path)
	if target.Scheme != base.Scheme || target.Host != base.Host || !strings.HasPrefix(target.Path, base.Path) || leaf == "" || leaf == "." || leaf == ".." || strings.Contains(leaf, "/") || strings.Contains(leaf, "%") {
		return "", fmt.Errorf("event is outside the selected calendar")
	}
	return target.String(), nil
}
