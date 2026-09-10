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
// was minted for. Here, membership IS the permission: removing somebody
// from the group revokes them on their next request, in the same place
// an operator already manages access to every other service in the
// house.
//
// Three conditions, each necessary:
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
	// ActingFor carries the person as well as Name so every audit row,
	// every Butler grant and every plan approval reads their directory
	// account rather than a shared administrator.
	return principal.Identity{Name: user, Role: runtimedb.RoleAdmin, ActingFor: user}, true
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
