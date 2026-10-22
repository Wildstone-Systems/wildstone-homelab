package reconcile

// The rendered LLDAP user list.
//
// Same contract as the group list (lldapgroups.go), and merged for the
// same reason: the provisioning job consumes all of them at once.
//
// Tab-separated, one person per line:
//
//	username <TAB> displayName <TAB> email <TAB> group1,group2 <TAB> revoke1,revoke2
//
// An empty list is written "-", never as an empty field. See emptyList.
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

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// emptyList is what an empty group or revoke column is written as.
//
// NEVER an empty field, for the reason the display-name fallback below
// exists: tab is IFS whitespace, so two tabs in a row are ONE separator
// to `IFS=<tab> read`. An empty groups column would shift the
// revocations into it, and the job would then ADD every membership it
// was told to take away — the precise inversion of the intent, from one
// absent character.
//
// That is not hypothetical here. It is what the first version of the
// fifth field did, and the test that caught it reported the revocation
// as "already in photos_users".
const emptyList = "-"

func orDash(items []string) string {
	if len(items) == 0 {
		return emptyList
	}
	return strings.Join(items, ",")
}

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
		revoke := append([]string(nil), u.Revoke...)
		sort.Strings(revoke)
		// Never an empty field before the last. The job reads this with
		// `IFS=<tab> read`, and tab is IFS whitespace, so two tabs in a
		// row are ONE separator: an empty display name shifted the email
		// into it, the groups into the email, and left no groups at all.
		// The account was created with its group list as its address and
		// was never put in a group, and the first Owner the installer
		// created — who has no display name — was refused the homepage
		// the install sent them to. The username is what LLDAP shows
		// when there is no display name anyway.
		display := u.DisplayName
		if display == "" {
			display = u.Username
		}
		lines = append(lines, strings.Join([]string{
			u.Username, display, u.Email,
			orDash(groups), orDash(revoke),
		}, "\t"))
	}
	sort.Strings(lines)

	var b strings.Builder
	b.WriteString("# managed by wildstone-keystone — do not edit\n")
	b.WriteString("# username<TAB>displayName<TAB>email<TAB>groups<TAB>revoke (comma-separated)\n")
	b.WriteString("# The identity capability's provisioning job creates any user the\n")
	b.WriteString("# directory does not have and adds any listed membership it is\n")
	b.WriteString("# missing. It never deletes a user.\n")
	b.WriteString("#\n")
	b.WriteString("# `revoke` names memberships to REMOVE, and every one of them is a\n")
	b.WriteString("# membership Wildstone placed itself and no longer wants (slice 8).\n")
	b.WriteString("# This file is still not a roster of who is in a group: a name that\n")
	b.WriteString("# appears in neither column is a membership somebody made by hand,\n")
	b.WriteString("# and the job reports it rather than touching it.\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	return []byte(b.String())
}
