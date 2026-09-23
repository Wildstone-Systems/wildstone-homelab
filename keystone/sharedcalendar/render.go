package sharedcalendar

import (
	"fmt"
	"regexp"
	"strings"
)

// Rights uses the same grant rules as Keystone. Write implies read. The service
// account reaches declared shared collections only; personal principals retain
// Radicale's authenticated-user boundary.
func (r Registry) Rights(service string) (string, error) {
	if !idPattern.MatchString(service) {
		return "", fmt.Errorf("invalid calendar service account")
	}
	var b strings.Builder
	b.WriteString("[root]\nuser: .+\ncollection:\npermissions: R\n\n")
	for _, c := range r.Calendars {
		collection := regexp.QuoteMeta(strings.Trim(c.Path(), "/"))
		fmt.Fprintf(&b, "[%s-service]\nuser: %s\ncollection: %s\npermissions: rw\n\n", c.ID, regexp.QuoteMeta(service), collection)
		for _, write := range []bool{true, false} {
			members, groups, permission := c.ReadMembers, c.ReadGroups, "r"
			if write {
				members, groups, permission = c.WriteMembers, c.WriteGroups, "rw"
			}
			escaped := []string{}
			for _, m := range members {
				escaped = append(escaped, regexp.QuoteMeta(m))
			}
			// An empty user expression means groups alone decide access.
			fmt.Fprintf(&b, "[%s-%s]\nuser: %s\ngroups: %s\ncollection: %s\npermissions: %s\n\n", c.ID, permission, strings.Join(escaped, "|"), strings.Join(groups, ","), collection, permission)
		}
	}
	b.WriteString("[own-principal]\nuser: .+\ncollection: {user}\npermissions: RW\n\n[own-calendars]\nuser: .+\ncollection: {user}/[^/]+\npermissions: rw\n")
	return b.String(), nil
}
