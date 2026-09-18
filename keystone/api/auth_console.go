package api

// Console sign-in by forwarded identity (ADR-W032).
//
// Every other surface in this house is reached by being in a group a
// package declared. The Console was the exception: reaching it required
// a bearer token minted by hand for each person, which does not scale
// past the operator, has no expiry story, and leaves a credential that
// outlives the directory account it was minted for.
//
// This file is the bridge. It is small on purpose and it is separate on
// purpose: it is the one authentication path that builds an
// administrator out of two request headers, and it should be readable
// in one screen without the routing table around it.

import (
	"net/http"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// ConsoleGroupsHeader is the group list the identity proxy forwards, and
// ConsoleGroup is the one that grants this surface.
//
// The group name is the LDAP one (underscore), matching what
// packages/wildstone-console declares and what the identity capability's
// provisioning job creates.
const (
	ConsoleGroupsHeader = "Remote-Groups"
	ConsoleGroup        = "console_users"
)

// consoleForwardAuth authenticates a person the identity provider signed
// in, from the group they are in.
//
// This is what makes the checkbox in "Add member" mean something. Before
// it, reaching the Console required a bearer token minted by hand for
// each person — which does not scale past the operator, has no expiry
// story, and leaves a credential that outlives the directory account it
// was minted for. Here, directory membership is what OFFERS the Console,
// and the household's own record of the person is what can withdraw it.
// (An earlier version of this comment said group membership alone was
// the permission, on the grounds that removal revokes. This deployment
// cannot remove anyone from a group — see forwardedUserMayHoldConsole.)
//
// Four conditions, each necessary:
//
//  1. The peer is the trusted ingress. A header anyone can set is not an
//     authentication, and this one grants the whole control plane. With
//     no TrustedIngress configured this path is unavailable rather than
//     open, which is the same fail-closed posture the household review
//     route takes (ADR-W030 A8.1).
//  2. Remote-User names somebody.
//  3. Remote-Groups carries the Console group. Absence is refusal, not a
//     lesser role — there is no narrower thing to be on this surface
//     yet, and inventing one would ship a screen whose every control
//     answers 403, because the Butler endpoints are written for a caller
//     entitled to all of them (ADR-W032 A3).
//
// The identity is ephemeral on purpose. No principal row is created,
// because the group is the permission and a persisted row would be a
// second place to revoke and a second place to forget — the same shape
// of hole as the case-sensitive member names in ADR-W032 A4.
func (s *Server) consoleForwardAuth(r *http.Request) (principal.Identity, bool) {
	if !s.peerIsTrustedIngress(r) {
		return principal.Identity{}, false
	}
	user := strings.TrimSpace(r.Header.Get(TrustedIngressHeader))
	if user == "" {
		return principal.Identity{}, false
	}
	if !hasGroup(r.Header.Get(ConsoleGroupsHeader), ConsoleGroup) {
		return principal.Identity{}, false
	}
	//  4. The household does not already know this person to be someone
	//     who may not hold the Console.
	if !s.forwardedUserMayHoldConsole(user) {
		return principal.Identity{}, false
	}
	// ActingFor carries the person as well as Name so every audit row,
	// every Butler grant and every plan approval reads their directory
	// account rather than a shared administrator.
	return principal.Identity{Name: user, Role: runtimedb.RoleAdmin, ActingFor: user}, true
}

// forwardedUserMayHoldConsole refuses a forwarded identity the household
// has a record of and that record says must not administer it: someone
// disabled, offboarded, un-admitted, or holding role 'member' rather
// than 'owner' (design §3.2, §18.1 — "A Member cannot access Console",
// "Demoting an Owner immediately invalidates Console access/session").
//
// The condition above it says group membership IS the permission,
// because removing somebody from the group revokes them. That is not
// true of this deployment. handlers_household_members.go states the
// reason in its own words: "Groups may only be ADDED. The provisioning
// job is additive ... and never removes either." So the one revocation
// path that comment relies on is not reachable through any surface
// Wildstone offers, and without this check a person who is demoted,
// disabled or offboarded keeps the whole control plane.
//
// Why a name match is allowed to REFUSE when A2.1 forbids it granting.
// Equal names are not proof of identity, so a handle may never be the
// reason somebody is let in — and it is not: the three conditions above
// still decide that. It is sound as a reason to keep somebody OUT,
// because the two mistakes are not symmetrical. Refusing the wrong
// person costs them a screen they can be given back. Admitting the
// wrong person costs the household its control plane.
//
// What this deliberately does NOT do is require a trusted-ingress
// identity link, which is what A2.1 would otherwise ask for and what
// handlers_household_review.go does for its own surface. Links and
// Person records are only ever created through the admin API, which
// lives behind this very check — so requiring one would lock every
// existing Console holder out of the surface they would need in order
// to grant themselves the link. The design resolves that at §2.4, where
// the installer creates the first Owner and their directory identity
// before setup completes, and §2.6, where root CLI recovery exists as
// the break-glass. Neither is built yet. Until they are, this closes
// the hole it can close without a lockout it cannot undo: a Person the
// household knows about is held to their role and lifecycle, and a
// forwarded name with no Person record behind it is still governed by
// the directory group alone.
func (s *Server) forwardedUserMayHoldConsole(user string) bool {
	// Matched case-INSENSITIVELY, and that is load-bearing rather than
	// tidy. Handles are compared exactly everywhere else, so `Admin` and
	// `admin` are two records (ADR-W032 A4, still open). An exact lookup
	// here would miss the Person whenever the proxy asserted a different
	// capitalisation, fall into the "no record" branch below, and hand
	// the Console to somebody the household had just disabled. The
	// permissive branch is the one an attacker would aim for, so nothing
	// that reaches it may depend on spelling.
	//
	// Looser matching is only ever used to refuse. It cannot grant: no
	// match at all still means the directory group decides, exactly as
	// before.
	people, err := s.DB.ButlerMembersByHandleFold(user)
	if err != nil {
		// A lookup that failed is not a lookup that passed.
		return false
	}
	if len(people) == 0 {
		return true
	}
	// Every match must qualify. With two records differing only by case
	// there is no way to tell which one the proxy meant, and an
	// authorization check that guesses is not a check.
	for _, m := range people {
		if !m.Active() || m.Role != runtimedb.RoleOwner {
			return false
		}
	}
	return true
}

// hasGroup reports whether a forwarded group list contains want.
//
// Authelia sends a comma-separated list. The comparison is exact after
// trimming: a prefix or substring match would make "console_users_ro"
// grant what "console_users" grants, and group names are somebody else's
// namespace to extend.
func hasGroup(header, want string) bool {
	for _, g := range strings.Split(header, ",") {
		if strings.TrimSpace(g) == want {
			return true
		}
	}
	return false
}
