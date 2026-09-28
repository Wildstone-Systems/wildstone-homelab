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
	// Personal marks one of a person's own calendars, which the
	// Calendar page lists beside the declared ones (ADR-W034 C2). It is
	// never part of a registry: a declaration cannot name one, and only
	// its owner is ever shown it.
	Personal bool `json:"-"`
}
type Registry struct {
	Calendars       []Calendar `json:"calendars"`
	BookingCalendar string     `json:"bookingCalendar,omitempty"`
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,47}$`)

// memberPattern admits the two ways a registry names a Person (#372): a
// person_id (`psn_` and 16 hex digits), or a handle they carry or once
// carried. Handles keep their case because the household's do.
var memberPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// householdGroupPattern is a group in the household's one namespace
// (ADR-W033 B8.1), named by its permanent id rather than its display name
// so that renaming `Adults` re-points nothing. Anything else in a group
// list is a directory group, such as calendar_users.
var householdGroupPattern = regexp.MustCompile(`^grp_[0-9a-f]{16}$`)

// IsHouseholdGroup reports whether a group entry names a household group
// rather than a directory group.
func IsHouseholdGroup(g string) bool { return householdGroupPattern.MatchString(g) }

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
		for _, list := range [][]string{c.ReadGroups, c.WriteGroups} {
			for _, v := range list {
				if !idPattern.MatchString(v) {
					return r, fmt.Errorf("invalid calendar access group %q", v)
				}
			}
		}
		for _, list := range [][]string{c.ReadMembers, c.WriteMembers} {
			for _, v := range list {
				if !memberPattern.MatchString(v) {
					return r, fmt.Errorf("invalid calendar access member %q", v)
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

// Subject is who is asking, as the caller resolved them. Groups holds
// both directory group names and household group ids; the caller fills
// in the household groups from Wildstone's own records, never from what a
// request claims.
type Subject struct {
	Member string
	// Person is the member's person_id, or "" for a directory account
	// with no Person on record.
	Person string
	Groups []string
}

// PersonResolver answers which Person a member reference names: a
// person_id, or "" for nobody on record.
type PersonResolver func(ref string) string

// Allows applies the calendar's grants. Write implies read.
//
// A member reference that names a Person matches that Person and nobody
// else, whatever they are called now (#372). Only a reference that names
// nobody on record falls back to comparing the handle, which is how a
// directory account without a Person kept its grant before; a person_id
// never matches by spelling. With no resolver, as in a test with no
// database, references are compared as handles.
func (c Calendar) Allows(s Subject, write bool, resolve PersonResolver) bool {
	if s.Member == "" {
		return false
	}
	names := func(ref string) bool {
		if resolve != nil {
			if id := resolve(ref); id != "" {
				return s.Person != "" && id == s.Person
			}
		}
		return !IsPersonID(ref) && ref == s.Member
	}
	match := func(members, allowed []string) bool {
		if slices.ContainsFunc(members, names) {
			return true
		}
		for _, g := range s.Groups {
			if slices.Contains(allowed, g) {
				return true
			}
		}
		return false
	}
	return match(c.WriteMembers, c.WriteGroups) || (!write && match(c.ReadMembers, c.ReadGroups))
}

var personIDPattern = regexp.MustCompile(`^psn_[0-9a-f]{16}$`)

// IsPersonID reports whether a member reference is a person_id.
func IsPersonID(ref string) bool { return personIDPattern.MatchString(ref) }

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
