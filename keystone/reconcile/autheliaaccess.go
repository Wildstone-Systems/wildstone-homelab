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
	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
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

// tailnetPolicy is the policy actually enforced under a posture.
//
// Under the tailnet posture a password is the whole sign-in: two_factor
// becomes one_factor, everything else is unchanged ("deny" stays deny).
//
// Two reasons, and both are about that posture specifically. The
// network is already a device factor: nothing reaches these ports but a
// machine the household enrolled in its tailnet. And the posture cannot
// honour a mixed policy anyway: every Route shares one hostname and
// Authelia matches rules by hostname, so a single two_factor surface —
// the Console — raised the family's homepage and every app to two_factor
// with it. Enrolling a second factor then needs a one-time code the file
// notifier writes to a file on the host that no family member can read,
// so the household was locked out of its own front door.
//
// Every other posture has real hostnames, can tell surfaces apart, and
// keeps the policy each package declared.
func tailnetPolicy(policy string, posture ingressorigin.Posture) string {
	if posture.TLS == "tailscale" && policy == "two_factor" {
		return "one_factor"
	}
	return policy
}

// policyRank orders Authelia policies from most permissive to least, so
// collapsing several Routes onto one domain can take the strictest.
// Erring strict means a service can end up better protected than
// declared; erring permissive means one is exposed. Only one of those is
// survivable.
var policyRank = map[string]int{"": 0, "bypass": 1, "one_factor": 2, "two_factor": 3, "deny": 4}

func stricter(candidate, current string) bool {
	return policyRank[candidate] > policyRank[current]
}

// AccessPolicies is the policy Authelia will enforce per domain, derived
// from desired state.
//
// Exported because the install preview must be able to ask the same
// question this file answers: installing a service can RAISE the policy
// for every other service, and an operator should be told before
// approving rather than discovering it when their household cannot log
// in (F77). Two implementations of "what policy will apply" would be two
// chances to disagree, which is F70's lesson.
func AccessPolicies(desired []resource.Resource) map[string]string {
	posture := ingressorigin.Of(desired)
	hostOf := func(declared string) string {
		served := posture.SiteAddress(declared)
		if h, _, ok := strings.Cut(served, ":"); ok {
			return h
		}
		return served
	}
	byDomain := map[string]string{}
	for _, r := range desired {
		if r.Kind != resource.KindRoute || r.Route == nil {
			continue
		}
		var policy string
		switch r.Route.Auth {
		case "portal":
			policy = "bypass"
		case "authelia_oidc":
			policy = accessPolicyFor(r.Route.Classification)
		default:
			continue
		}
		policy = tailnetPolicy(policy, posture)
		host := hostOf(r.Route.Host)
		if stricter(policy, byDomain[host]) {
			byDomain[host] = policy
		}
	}
	return byDomain
}

// PolicyStricter reports whether a is stricter than b, for callers
// comparing a before and after.
func PolicyStricter(a, b string) bool { return stricter(a, b) }

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
	// Rules must name the host a BROWSER sends, not the one a package
	// declared (F71). Under the tailnet posture those differ, and rules
	// naming the declared host match nothing at all — so default_policy
	// catches every request and a correctly-configured login page answers
	// 403 to everyone.
	//
	// The hostname only, never host:port. Under that posture every Route
	// shares one hostname and differs by port, and Authelia matches
	// `domain` against the hostname — so a port here would match nothing
	// and we would have traded one silent deny for another.
	type rule struct {
		domain, policy string
		subjects       []string
	}
	byDomain := AccessPolicies(desired)
	subjectsByDomain := AccessSubjects(desired)

	rules := make([]rule, 0, len(byDomain))
	for domain, policy := range byDomain {
		rules = append(rules, rule{domain, policy, subjectsByDomain[domain]})
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
		// A subject list turns "any authenticated household member" into
		// "a member of one of these groups". Emitted ONLY where the
		// capability declared directory groups, so a host that declared
		// none behaves exactly as it did before slice 8.
		//
		// Authelia reads the subject list as OR, which is what an
		// entitlement means: a capability may declare more than one
		// group, and being in any of them admits you.
		if len(r.subjects) > 0 {
			b.WriteString("      subject:\n")
			for _, subject := range r.subjects {
				b.WriteString("        - 'group:" + subject + "'\n")
			}
		}
	}
	return []byte(b.String())
}

// AccessSubjects maps a served hostname to the directory groups that may
// reach it (ADR-W033 design §4, slice 8).
//
// THE ENFORCEMENT #209 ASKS FOR, for forward-auth Routes. Until now a
// rule carried a policy and no subject, so any household member Authelia
// admitted at that factor reached every such app — access was whatever
// the LDAP groups happened to say, and nothing said it.
//
// SILENT WHERE NOTHING WAS DECLARED, and that restraint is the whole
// safety of it. A subject list on a host whose group is empty denies
// EVERYONE, so the rule is emitted only for a capability that declared
// directory groups of its own. A capability that declared none keeps the
// behaviour it has today rather than being locked shut by an upgrade
// that added a field.
//
// The groups come from the LdapGroup resources the capability owns —
// the same derivation the membership projection uses, so what Authelia
// demands and what the directory is given cannot disagree.
func AccessSubjects(desired []resource.Resource) map[string][]string {
	groupsByCapability := map[string][]string{}
	for _, r := range desired {
		if r.Kind != resource.KindLdapGroup || r.LdapGroup == nil || r.LdapGroup.Name == "" {
			continue
		}
		if c, ok := strings.CutPrefix(r.Metadata.Owner, "capability/"); ok && c != "" {
			groupsByCapability[c] = append(groupsByCapability[c], r.LdapGroup.Name)
		}
	}
	if len(groupsByCapability) == 0 {
		return nil
	}

	posture := ingressorigin.Of(desired)
	out := map[string][]string{}
	for _, r := range desired {
		if r.Kind != resource.KindRoute || r.Route == nil {
			continue
		}
		// Only forward-auth. `app_oidc` is gated by the OIDC client's
		// own policy, `app_ldap` by the app's search filter, and
		// `portal` bypasses Authelia because Keystone decides it
		// in-process — a subject on any of those would be enforcement
		// in the wrong place or none at all.
		if r.Route.Auth != "authelia_oidc" {
			continue
		}
		capability, ok := strings.CutPrefix(r.Metadata.Owner, "capability/")
		if !ok {
			continue
		}
		groups := groupsByCapability[capability]
		if len(groups) == 0 {
			continue
		}
		host := posture.SiteAddress(r.Route.Host)
		if h, _, cut := strings.Cut(host, ":"); cut {
			host = h
		}
		out[host] = append(out[host], groups...)
	}
	// Several Routes may share a hostname under the tailnet posture, so
	// the lists merge — and a union is right: each Route's own
	// capability admits its own people, and a host serving two services
	// admits the union of them. Sorted and deduped so the file is a pure
	// function of desired state and does not restart Authelia on ticks
	// where nothing changed.
	for host, groups := range out {
		sort.Strings(groups)
		out[host] = dedupeStrings(groups)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func dedupeStrings(sorted []string) []string {
	if len(sorted) == 0 {
		return nil
	}
	out := sorted[:1]
	for _, s := range sorted[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
