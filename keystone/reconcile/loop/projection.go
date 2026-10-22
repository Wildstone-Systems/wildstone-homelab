package loop

// Projecting entitlements into the directory (ADR-W033 design §4; slice 8).
//
// §4 makes Wildstone the source of truth for who may reach what, and
// requires it to project effective membership outward. Slices 6 and 8
// built the truth — a group namespace and an entitlement table — and
// nothing read either of them. This is the bridge.
//
// WHY IT LIVES IN THE LOOP. Two registries have to meet, and the loop is
// the only place that holds both: the Statefile's declared LdapUser
// resources and the runtimedb's Persons, groups and entitlements. The
// Reconciler below it has no database and should not grow one — it
// converges declared state against a node, and "who may use the photo
// library" is not a fact about a node.
//
// IT EDITS A COPY, NEVER THE STATEFILE. Desired state says what the
// household and its packages declared; the projection is derived, and
// re-derived every tick. Writing it back would make the Statefile
// disagree with itself the moment an entitlement changed without a plan.
// renderAutheliaClients already takes this posture for tailnetPolicy,
// for the same reason.
//
// RECONCILING, NOT FIRE-AND-FORGET (#209). It runs on every tick and
// recomputes from scratch, so a membership a person removed by hand in
// LLDAP's own UI comes back on the next pass, and a withdrawn
// entitlement is taken away on the next pass. Neither waits for an
// event nobody sends.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// MembershipDrift is something projection found and did not do.
//
// Reported rather than silently acted on or silently skipped, because
// each of these is a household-visible fact: a directory account
// Wildstone cannot safely place, or a membership somebody made by hand.
type MembershipDrift struct {
	// Account is the directory username the finding is about.
	Account string
	// Reason says what stopped projection, in words meant for a log a
	// household's operator reads.
	Reason string
}

func (d MembershipDrift) String() string { return d.Account + ": " + d.Reason }

// projectEntitlements returns a copy of desired in which every LdapUser
// carries the memberships its Person's entitlements imply, and a
// revocation list for the ones Wildstone placed and no longer wants.
//
// The mapping, end to end:
//
//	person -> slice-6 groups -> entitlement rows -> capability
//	capability -> the LdapGroup resources that capability declared
//	           -> directory group names
//
// The second half is why no new declaration was needed for it: an
// LdapGroup resource already records which capability produced it, in
// Metadata.Owner, so "the directory groups that belong to `documents`"
// is a fact about desired state rather than a table somebody maintains.
func projectEntitlements(db *runtimedb.DB, desired []resource.Resource) ([]resource.Resource, []MembershipDrift, error) {
	if db == nil {
		return desired, nil, nil
	}
	groupsByCapability, err := directoryGroupsByCapability(desired)
	if err != nil {
		return nil, nil, err
	}
	if len(groupsByCapability) == 0 {
		// Nothing declares a directory group, so there is nothing to
		// project INTO. Returning desired untouched matters more than it
		// reads: a node whose resources have not loaded yet must not
		// look like a household that withdrew every entitlement.
		return desired, nil, nil
	}
	placed, err := db.PlacedMemberships()
	if err != nil {
		return nil, nil, err
	}

	var drift []MembershipDrift
	out := make([]resource.Resource, len(desired))
	copy(out, desired)

	for i, r := range out {
		if r.Kind != resource.KindLdapUser || r.LdapUser == nil || r.LdapUser.Username == "" {
			continue
		}
		account := r.LdapUser.Username
		personID, ambiguous, err := db.PersonForDirectoryAccount(account)
		if err != nil {
			return nil, nil, err
		}
		if ambiguous {
			// Two Person records answer to this name. auth_console.go's
			// rule is that a name collision must never make two subjects
			// one, and placing a membership on a guess is exactly that
			// — so projection does nothing here and says so loudly.
			drift = append(drift, MembershipDrift{Account: account,
				Reason: "more than one active Person answers to this name, so no membership " +
					"was projected; a directory account and a Person are joined by nothing " +
					"but a string, and guessing which one is meant is how somebody reaches " +
					"another person's service"})
			continue
		}
		if personID == "" {
			// A directory account Wildstone does not manage — the
			// installer makes some, and a household may make others.
			// Not drift and not an error: it simply keeps whatever
			// memberships a human gave it.
			continue
		}

		want, err := entitledDirectoryGroups(db, personID, groupsByCapability)
		if err != nil {
			return nil, nil, err
		}

		// Declared memberships are the household's own word and are
		// never projected away. `console_users` is put there by the
		// installer and by the members API, not by an entitlement, and
		// an entitlement pass that removed it would lock the Owner out
		// of the Console.
		declared := map[string]bool{}
		for _, g := range r.LdapUser.Groups {
			declared[g] = true
		}

		spec := *r.LdapUser
		spec.Groups = union(r.LdapUser.Groups, want)
		spec.Revoke = revocations(placed[personID], want, declared)
		out[i].LdapUser = &spec

		// Claim what is now asserted, so the next pass can tell what it
		// placed from what a person placed. Recorded BEFORE the job
		// runs: a claim slightly ahead of the directory converges on the
		// next tick, while a claim behind it would leave a membership
		// Wildstone placed looking like somebody's hand edit — which it
		// would then refuse to remove, forever.
		if err := db.ClaimMemberships(personID, want); err != nil {
			return nil, nil, err
		}
	}
	sort.Slice(drift, func(i, j int) bool { return drift[i].Account < drift[j].Account })
	return out, drift, nil
}

// directoryGroupsByCapability maps a capability to the directory groups
// it declared, from Metadata.Owner ("capability/<name>").
//
// A resource with no capability owner is skipped rather than guessed at.
// `system` owns resources too, and a system-owned group is not something
// a household entitles anybody to.
func directoryGroupsByCapability(desired []resource.Resource) (map[string][]string, error) {
	out := map[string][]string{}
	for _, r := range desired {
		if r.Kind != resource.KindLdapGroup || r.LdapGroup == nil || r.LdapGroup.Name == "" {
			continue
		}
		capability, ok := strings.CutPrefix(r.Metadata.Owner, "capability/")
		if !ok || capability == "" {
			continue
		}
		out[capability] = append(out[capability], r.LdapGroup.Name)
	}
	for c := range out {
		sort.Strings(out[c])
	}
	return out, nil
}

// entitledDirectoryGroups is every directory group this Person's
// entitlements admit them to.
//
// Asked through EntitledTo per capability rather than joined in SQL: the
// precedence rules — explicit beats inherited, restrict beats allow
// within a level, dormant grants nothing — have one implementation, and
// a join here would be a second one that agreed until one was edited.
func entitledDirectoryGroups(db *runtimedb.DB, personID string, byCapability map[string][]string) ([]string, error) {
	capabilities := make([]string, 0, len(byCapability))
	for c := range byCapability {
		capabilities = append(capabilities, c)
	}
	sort.Strings(capabilities)

	var want []string
	for _, c := range capabilities {
		ok, err := db.EntitledTo(personID, c)
		if err != nil {
			return nil, fmt.Errorf("projecting %q: %w", c, err)
		}
		if !ok {
			continue
		}
		want = append(want, byCapability[c]...)
	}
	sort.Strings(want)
	return dedupe(want), nil
}

// revocations is what Wildstone placed, no longer wants, and may take
// away: claimed − wanted − declared.
//
// All three terms are load-bearing.
//
//   - claimed, because a membership Wildstone never placed is somebody's
//     hand edit and is not its to remove. That subtraction is the whole
//     safety argument for reversing the additive-only posture.
//   - wanted, because a membership that is still entitled obviously
//     stays.
//   - declared, because the Statefile's own groups outrank the
//     projection. A group that is both claimed and declared — the
//     installer's console_users, say, if an entitlement once implied it
//     too — must not be revoked on the strength of the entitlement
//     going away, or withdrawing one would silently undo the other.
func revocations(claimed, wanted []string, declared map[string]bool) []string {
	keep := map[string]bool{}
	for _, g := range wanted {
		keep[g] = true
	}
	var out []string
	for _, g := range claimed {
		if keep[g] || declared[g] {
			continue
		}
		out = append(out, g)
	}
	sort.Strings(out)
	return dedupe(out)
}

func union(a, b []string) []string {
	out := append(append([]string(nil), a...), b...)
	sort.Strings(out)
	return dedupe(out)
}

func dedupe(sorted []string) []string {
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

// seedCapabilityEntitlements keeps the entitlement table in step with
// what is installed (§17.3).
//
// Done here, from desired state, rather than in the install handler —
// because enabling a capability only PROPOSES a plan, and a plan may
// never be approved. Seeding at proposal time would create entitlements
// for a service that was never installed. Desired state is what the
// household actually committed to, and reading it every tick makes
// install, uninstall and reinstall one mechanism instead of three hooks.
//
// accessGroups is capability -> the seed keys its manifest declares,
// supplied by the caller because the loop holds no catalog. A catalog
// conformance test asserts every profile of a capability declares the
// same audience, so one entry per capability is a complete answer rather
// than a guess about which profile is running.
func seedCapabilityEntitlements(db *runtimedb.DB, accessGroups map[string][]string, desired []resource.Resource) error {
	if db == nil {
		return nil
	}
	installed := map[string]bool{}
	for _, r := range desired {
		if capability, ok := strings.CutPrefix(r.Metadata.Owner, "capability/"); ok && capability != "" {
			installed[capability] = true
		}
	}
	// Nothing in desired state at all is a loop that has not loaded yet,
	// not a household that uninstalled everything. Making every service
	// dormant on the strength of it would withdraw all access on the
	// first tick after a restart with an unreadable statefile.
	if len(installed) == 0 {
		return nil
	}

	names := make([]string, 0, len(installed))
	for c := range installed {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		if err := db.SeedServiceEntitlements(c, accessGroups[c], "system"); err != nil {
			return err
		}
	}

	// And a capability that has entitlements but is no longer installed
	// goes dormant rather than losing them (§17.3, §17.4).
	known, err := db.ServicesWithEntitlements()
	if err != nil {
		return err
	}
	for _, c := range known {
		if installed[c] {
			continue
		}
		if err := db.MakeServiceDormant(c); err != nil {
			return err
		}
	}
	return nil
}
