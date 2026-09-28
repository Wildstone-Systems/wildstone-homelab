package sharedcalendar

import (
	"fmt"
	"regexp"
	"strings"
)

// ServiceMarker is the first line of a rendered rights file. Keystone
// reads the service account back from it when it re-renders the file with
// the household's Persons resolved (see Grants), because that account is a
// package parameter and not part of the registry.
const ServiceMarker = "# wildstone-calendar-service: "

// Grants turns what the registry names into the directory usernames
// Radicale checks. Radicale knows accounts and directory groups; it has
// never heard of a person_id or a household group.
//
// The package template renders without Grants, because a template has no
// database: a handle is then taken as the username it was when it was
// written, and a person_id or household group grants nothing. Keystone's
// reconcile loop renders again with Grants on every tick, so a rename or a
// change of group membership reaches Radicale without anybody editing the
// registry (#372).
type Grants struct {
	// Person returns the current directory username of the Person a
	// member reference names. known is false when the reference names
	// nobody on record; username is "" for a Person who may not sign in,
	// such as one who has been offboarded.
	Person func(ref string) (username string, known bool)
	// Group returns the current directory usernames of a household
	// group's members. ok is false for a group that does not exist.
	Group func(id string) (usernames []string, ok bool)
}

// Rights is what the package template renders: the same grant rules as
// Keystone, with nothing resolved (see Grants). Write implies read.
//
// The service account reaches declared shared collections, and each
// person's own calendars, so that the Calendar page can show a member
// their own calendars (ADR-W034 C2). It is never given an undeclared
// shared collection: a shared calendar removed from the registry stops
// being Keystone's to read.
func (r Registry) Rights(service string) (string, error) { return r.RightsWith(service, nil) }

// RightsWith is Rights with the household's Persons and groups resolved.
func (r Registry) RightsWith(service string, g *Grants) (string, error) {
	if !idPattern.MatchString(service) {
		return "", fmt.Errorf("invalid calendar service account")
	}
	var b strings.Builder
	b.WriteString(ServiceMarker + service + "\n\n")
	b.WriteString("[root]\nuser: .+\ncollection:\npermissions: R\n\n")
	for _, c := range r.Calendars {
		collection := regexp.QuoteMeta(strings.Trim(c.Path(), "/"))
		fmt.Fprintf(&b, "[%s-service]\nuser: %s\ncollection: %s\npermissions: rw\n\n", c.ID, regexp.QuoteMeta(service), collection)
		for _, write := range []bool{true, false} {
			members, groups, permission := c.ReadMembers, c.ReadGroups, "r"
			if write {
				members, groups, permission = c.WriteMembers, c.WriteGroups, "rw"
			}
			users, directoryGroups := g.resolve(members, groups)
			escaped := []string{}
			for _, u := range users {
				escaped = append(escaped, regexp.QuoteMeta(u))
			}
			// An empty user expression means groups alone decide access.
			fmt.Fprintf(&b, "[%s-%s]\nuser: %s\ngroups: %s\ncollection: %s\npermissions: %s\n\n", c.ID, permission, strings.Join(escaped, "|"), strings.Join(directoryGroups, ","), collection, permission)
		}
	}
	// Personal calendars, for the service account only. The look-ahead
	// keeps GROUPS out: only a declared shared collection, above, is the
	// service's. R lists a person's calendars; W lets the Calendar page
	// create a person's principal when they have never signed in to the
	// calendar yet, so that it can make them a calendar.
	fmt.Fprintf(&b, "[personal-principals-service]\nuser: %s\ncollection: (?!GROUPS$)[^/]+\npermissions: RW\n\n", regexp.QuoteMeta(service))
	fmt.Fprintf(&b, "[personal-calendars-service]\nuser: %s\ncollection: (?!GROUPS/)[^/]+/[^/]+\npermissions: rw\n\n", regexp.QuoteMeta(service))
	b.WriteString("[own-principal]\nuser: .+\ncollection: {user}\npermissions: RW\n\n[own-calendars]\nuser: .+\ncollection: {user}/[^/]+\npermissions: rw\n")
	return b.String(), nil
}

// resolve splits one grant into the usernames and directory groups
// Radicale can check. Radicale lower-cases every username it
// authenticates (lc_username), so the names are lower-cased to match.
func (g *Grants) resolve(members, groups []string) (users, directoryGroups []string) {
	seen := map[string]bool{}
	add := func(u string) {
		u = strings.ToLower(strings.TrimSpace(u))
		if u != "" && !seen[u] {
			seen[u] = true
			users = append(users, u)
		}
	}
	for _, m := range members {
		if g != nil && g.Person != nil {
			if u, known := g.Person(m); known {
				add(u)
				continue
			}
		}
		// A handle that names nobody on record is a directory account
		// Wildstone does not manage, and keeps the grant it always had,
		// exactly as Calendar.Allows does. A person_id is never a username.
		if !IsPersonID(m) {
			add(m)
		}
	}
	for _, id := range groups {
		if IsHouseholdGroup(id) && g != nil && g.Group != nil {
			if names, ok := g.Group(id); ok {
				for _, u := range names {
					add(u)
				}
				continue
			}
		}
		directoryGroups = append(directoryGroups, id)
	}
	return users, directoryGroups
}

// ServiceFromRights reads back the service account a rights file was
// rendered for, or "" when it carries no marker.
func ServiceFromRights(rights string) string {
	first, _, _ := strings.Cut(rights, "\n")
	service, ok := strings.CutPrefix(first, ServiceMarker)
	if !ok || !idPattern.MatchString(service) {
		return ""
	}
	return service
}
