package reconcile

// The rendered LLDAP group list.
//
// Every LdapGroup in the deployment is merged into ONE file on the node,
// for the same reason every OidcClient is merged into one Authelia
// clients file: the thing that consumes it consumes all of them at once,
// and a directory of fragments would mean the consumer had to reconcile
// them itself.
//
// The format is one group name per line, not JSON. The consumer is a
// POSIX shell script running in an image with no jq — see the identity
// package's provision-groups.sh — and a line-oriented file is the one
// format a shell reads without a parser. Comments and blank lines are
// skipped so the file can say what it is.
//
// This is Keystone's side of the contract; the script is the package's.
// They are deliberately on opposite sides of ADR-W005's line: WHICH
// groups exist is deployment-wide state the control plane owns, and HOW
// a group gets created is the identity profile's business.

import (
	"path"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// lldapGroupsPath is the merged group list, the file the identity
// capability's provisioning job reads.
func lldapGroupsPath(managedPrefix string) string {
	return path.Join(managedPrefix, "lldap", "groups")
}

// renderLdapGroups merges every LdapGroup in desired into the list.
// Sorted, so the file does not churn on map or slice order and the job's
// spec hash is stable across ticks.
func renderLdapGroups(desired []resource.Resource) []byte {
	names := make([]string, 0, len(desired))
	seen := make(map[string]bool, len(desired))
	for _, r := range desired {
		if r.Kind != resource.KindLdapGroup || r.LdapGroup == nil || r.LdapGroup.Name == "" {
			continue
		}
		// Two packages may legitimately declare the same group — a
		// shared family_users, say. The directory has one group either
		// way, so the list does too.
		if seen[r.LdapGroup.Name] {
			continue
		}
		seen[r.LdapGroup.Name] = true
		names = append(names, r.LdapGroup.Name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# managed by wildstone-keystone — do not edit\n")
	b.WriteString("# one group per line; the identity capability's provisioning job\n")
	b.WriteString("# creates any that the directory does not already have, and never\n")
	b.WriteString("# deletes one — removing a group would remove everyone in it.\n")
	for _, n := range names {
		b.WriteString(n)
		b.WriteString("\n")
	}
	return []byte(b.String())
}
