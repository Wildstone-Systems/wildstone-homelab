// Package reconcile — Authelia access control, rendered from Routes.
//
// Authelia's access_control is what decides a forward-auth request, and
// the identity package sets `default_policy: deny` on the correct
// reasoning that an unlisted host must not be quietly reachable. Its
// comment then says "Route fragments are what grant access, per-host" —
// but nothing generated a rule, so every forward-auth'd Route answered
// 403 to everyone, permanently, including the operator's own dashboard.
// The default was right and the other half was missing.
//
// This is the symmetric counterpart to renderAutheliaClients: one merged
// managed file, derived from desired state, that Authelia is pointed at
// with its own --config.
package reconcile

import (
	"path"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// autheliaAccessPath is the merged access-control file.
func autheliaAccessPath(managedPrefix string) string {
	return path.Join(managedPrefix, "authelia", "access.yml")
}

// accessPolicyFor maps a Route's classification to an Authelia policy.
//
// A CLOSED mapping, deliberately: a classification nobody has considered
// must not silently pick up the weakest policy, so anything unrecognised
// gets the strongest one. family_public is one_factor because it is the
// household's shared surface — a password, not a second factor, is what
// a family member has on a phone in the kitchen.
func accessPolicyFor(classification string) string {
	switch classification {
	case "family_public":
		return "one_factor"
	default:
		return "two_factor"
	}
}

// renderAutheliaAccess builds the access_control block from desired Routes.
//
// Only Auth == "authelia_oidc" needs a rule: that is the one value for
// which renderRouteFragment emits forward_auth, so it is the only one for
// which Authelia is ever consulted. Auth == "portal" is the provider's own
// host and is bypassed — it must answer anonymously or nobody can ever
// reach the login page to stop being anonymous.
//
// With no Routes at all the file is still a valid deny-everything block
// rather than an absent one. Nothing is exposed at that point anyway, and
// an absent access_control would hand the decision to whatever Authelia's
// built-in default happens to be in the version we shipped against.
func renderAutheliaAccess(desired []resource.Resource) []byte {
	type rule struct{ domain, policy string }
	var rules []rule
	for _, r := range desired {
		if r.Kind != resource.KindRoute || r.Route == nil {
			continue
		}
		switch r.Route.Auth {
		case "portal":
			rules = append(rules, rule{r.Route.Host, "bypass"})
		case "authelia_oidc":
			rules = append(rules, rule{r.Route.Host, accessPolicyFor(r.Route.Classification)})
		}
	}
	// Sorted so the file is a pure function of desired state: an
	// unstable order would rewrite the file, and restart Authelia,
	// on ticks where nothing changed.
	sort.Slice(rules, func(i, j int) bool { return rules[i].domain < rules[j].domain })

	var b strings.Builder
	b.WriteString("# managed by wildstone-keystone — do not edit\n")
	b.WriteString("access_control:\n")
	b.WriteString("  default_policy: deny\n")
	if len(rules) == 0 {
		b.WriteString("  # no Routes are authenticated on this node yet\n")
		b.WriteString("  rules: []\n")
		return []byte(b.String())
	}
	b.WriteString("  rules:\n")
	for _, r := range rules {
		b.WriteString("    - domain: " + r.domain + "\n")
		b.WriteString("      policy: " + r.policy + "\n")
	}
	return []byte(b.String())
}
