package reconcile

// The rendered LLDAP user list.
//
// Same contract as the group list (lldapgroups.go), and merged for the
// same reason: the provisioning job consumes all of them at once.
//
// Tab-separated, one person per line:
//
//	username <TAB> displayName <TAB> email <TAB> group1,group2
//
// Tab rather than colon or space because a display name legitimately
// contains spaces and may contain a colon; it may not contain a tab,
// which validateLdapUser enforces by charset. The consumer is a POSIX
// shell with no JSON parser, and `IFS=<tab> read` is the whole parser it
// needs.
//
// Empty trailing fields are written as empty, never omitted: a reader
// splitting on a fixed field count must not have to guess which field is
// missing when a user has no email.

import (
	"path"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// lldapUsersPath is the merged user list the provisioning job reads.
func lldapUsersPath(managedPrefix string) string {
	return path.Join(managedPrefix, "lldap", "users")
}

// renderLdapUsers merges every LdapUser in desired into the list, sorted
// so the file — and therefore the job's spec hash — is stable across
// ticks.
func renderLdapUsers(desired []resource.Resource) []byte {
	lines := make([]string, 0, len(desired))
	seen := make(map[string]bool, len(desired))
	for _, r := range desired {
		if r.Kind != resource.KindLdapUser || r.LdapUser == nil || r.LdapUser.Username == "" {
			continue
		}
		u := r.LdapUser
		// Two packages may name the same person; the directory has one
		// account either way, so the list does too. First declaration
		// wins, and sorting makes "first" deterministic.
		if seen[u.Username] {
			continue
		}
		seen[u.Username] = true
		groups := append([]string(nil), u.Groups...)
		sort.Strings(groups)
		lines = append(lines, strings.Join([]string{
			u.Username, u.DisplayName, u.Email, strings.Join(groups, ","),
		}, "\t"))
	}
	sort.Strings(lines)

	var b strings.Builder
	b.WriteString("# managed by wildstone-keystone — do not edit\n")
	b.WriteString("# username<TAB>displayName<TAB>email<TAB>groups (comma-separated)\n")
	b.WriteString("# The identity capability's provisioning job creates any user the\n")
	b.WriteString("# directory does not have and adds any listed membership it is\n")
	b.WriteString("# missing. It never deletes a user and never removes a membership:\n")
	b.WriteString("# this file is not a roster of who is in a group, only of what\n")
	b.WriteString("# Wildstone has been asked to ensure.\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	return []byte(b.String())
}
