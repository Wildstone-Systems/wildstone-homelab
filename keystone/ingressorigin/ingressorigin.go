// Package ingressorigin answers one question that two very different
// parts of Keystone have to agree on: given a Route's declared host,
// where is it actually served?
//
// Under every posture but "tailscale" the answer is "at that host", and
// nobody needs a package to say so. Under the tailnet posture a Route is
// served at the node's one MagicDNS name plus a port (ADR-W019 A10), and
// the answer has to be identical in two places computed at completely
// different times:
//
//   - keystone/reconcile, rendering the Caddy fragment that serves it and
//     the dashboard links that point at it;
//   - keystone/capability, deriving .Resolved values like an OIDC issuer
//     at expansion time, long before any of that is rendered.
//
// They cannot disagree. An OIDC issuer is a string that must match what
// the browser saw, byte for byte, and a dashboard link that misses is a
// service that looks broken. Two implementations of "which port is auth
// on" would be two chances to be wrong, and the failure would surface as
// a login loop rather than as anything naming the disagreement — which
// is exactly the shape of the bugs ADR-W019 keeps recording.
package ingressorigin

import (
	"sort"
	"strconv"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// Posture is the part of an Ingress declaration that decides addressing.
type Posture struct {
	TLS         string
	TailnetName string
	// Ports maps a Route's declared host to the port it is served on.
	// Empty under every posture but "tailscale".
	Ports map[string]int
	// occupied is every host port some other container already publishes.
	// Carried so that resolving a host whose Route does not exist yet
	// obeys the same collision rules the eventual assignment will (F70).
	occupied map[int]bool
}

// PortBlock is the closed set of ports the tailnet ingress profile
// publishes, in assignment order.
//
// Closed and small on purpose. Every entry is a port the ingress
// container must publish, and publishing is declared statically in the
// package — so an unbounded range would mean either a container binding
// hundreds of ports it will never use, or an assignment that silently
// fails for the service that lands outside it. Twelve is more Routes
// than the shipped catalog has capabilities.
var PortBlock = []int{443, 7443, 8443, 9443, 10443, 11443, 12443, 13443, 14443, 15443, 16443, 17443}

// wellKnown pins the shipped capabilities to fixed ports.
//
// Without this, ports would follow sorted position, and installing
// anything alphabetically earlier would silently move every service
// after it — a household's bookmarks broken by an unrelated install.
// Pinning the names that ship means the common case never moves.
//
// Keyed on the FIRST LABEL of the declared host — "home" in
// "home.example.com" — because that label is what the package chose and
// the rest is the deployment's domain. Pinning the whole host would
// unpin everything the moment the domain changed.
// EVERY shipped capability is pinned, not just some (F82). An unpinned
// service takes the next free port, which moves when anything sorting
// before it is installed — so installing ntfy moved uptime-kuma from
// 10443 to 11443 and every bookmark to it pointed at the wrong service.
// TestInstallingAServiceDoesNotMoveTheOthers only ever covered the
// pinned ones; pinning them all is what makes the property true.
//
// It also fixes two quieter things. "docs" was pinned and never matched:
// documents-paperless labels its Route from docs_subdomain, which
// defaults to "paperless", so that pin has been dead. And an unpinned
// service cannot know its own origin at expansion time (F79), which is
// why its self-referential URLs come out wrong — a pin is what makes
// that knowable.
//
// Ports chosen to match a deployment that already has these installed,
// so landing this moves nothing that is currently running.
//
// A pin follows the DEFAULT label. Overriding media_subdomain or
// docs_subdomain makes that Route unpinned again, and it will take the
// next free port like any third-party package.
var wellKnown = map[string]int{
	"home":      443,   // the dashboard: the address people actually type
	"auth":      7443,  // Authelia, which needs its own origin for OIDC
	"jellyfin":  8443,  // media-jellyfin, via media_subdomain
	"photos":    9443,  // photos-immich, via photos_subdomain
	"ntfy":      10443, // notifications
	"uptime":    11443, // monitoring
	"paperless": 12443, // documents-paperless, via docs_subdomain
}

// Of derives the addressing posture from desired state.
func Of(desired []resource.Resource) Posture {
	var p Posture
	for _, r := range desired {
		if r.Kind == resource.KindIngress && r.Ingress != nil {
			p.TLS = r.Ingress.TLS
			p.TailnetName = r.Ingress.TailnetName
			break
		}
	}
	if p.TLS == "tailscale" {
		p.Ports = assign(desired)
		p.occupied = portsPublishedByOthers(desired)
	}
	return p
}

// assign maps declared host to port across the whole desired set.
//
// Determinism is not a style preference: the reconciler decides NoOp
// versus Update by comparing a live file's bytes to a fresh render, so an
// assignment that varied between two renders of identical desired state
// would show as permanent drift and rewrite the config forever.
func assign(desired []resource.Resource) map[string]int {
	var hosts []string
	seen := map[string]bool{}
	for _, r := range desired {
		if r.Kind == resource.KindRoute && r.Route != nil && r.Route.Host != "" && !seen[r.Route.Host] {
			hosts = append(hosts, r.Route.Host)
			seen[r.Route.Host] = true
		}
	}
	sort.Strings(hosts)

	ports := make(map[string]int, len(hosts))
	taken := portsPublishedByOthers(desired)
	// Pinned hosts first, so an unpinned Route can never take a port a
	// pinned one is entitled to merely by sorting earlier.
	for _, h := range hosts {
		// A pin is a preference, not a guarantee. If something else
		// already publishes that port the Route falls through to the
		// block below, because a stable address nothing can bind is
		// worse than an unfamiliar one that works.
		if p, ok := wellKnown[ServiceLabel(h)]; ok && !taken[p] {
			ports[h] = p
			taken[p] = true
		}
	}
	for _, h := range hosts {
		if _, done := ports[h]; done {
			continue
		}
		for _, p := range PortBlock {
			if !taken[p] {
				ports[h] = p
				taken[p] = true
				break
			}
		}
		// A Route beyond the block gets no port and is left unserved
		// rather than colliding. Silently sharing one would route two
		// services to whichever site Caddy matched first.
	}
	return ports
}

// portsPublishedByOthers is every host port some OTHER container in
// desired state already publishes (F63).
//
// Without this, ports are handed out from the block with no idea what is
// already bound, and a Route can be assigned a port its container will
// never manage to bind. The failure is remote from the cause and quiet:
// the plan applies, the resource reports healthy, and exactly one
// service is unreachable.
//
// Containers owned by the ingress capability are excluded, and must be:
// the ingress proxy is the thing being assigned these ports, so counting
// its own publications as conflicts would rule out the entire block.
//
// This sees only what Wildstone MANAGES. A process outside desired state
// holding a port — another stack on the same host, something started by
// hand — is invisible here and still collides, surfacing as the ingress
// container failing to start. That is a smaller and much louder failure
// than a silently unreachable service, but it is not nothing, and it is
// the reason this is a mitigation rather than a fix.
func portsPublishedByOthers(desired []resource.Resource) map[int]bool {
	taken := map[int]bool{}
	for _, r := range desired {
		if r.Kind != resource.KindContainer || r.Container == nil {
			continue
		}
		if r.Metadata.Owner == "capability/ingress" {
			continue
		}
		for _, p := range r.Container.Ports {
			if _, hostPort, _, ok := resource.SplitPortMapping(p); ok {
				taken[int(hostPort)] = true
			}
		}
	}
	return taken
}

// ServiceLabel is the leading label of a host: "home" in
// "home.example.com" — what a package named the service, with the
// deployment's domain stripped off.
func ServiceLabel(host string) string {
	if i := strings.IndexByte(host, '.'); i > 0 {
		return host[:i]
	}
	return host
}

// SiteAddress is the host:port a Route is served at — the declared host
// under every posture but "tailscale".
//
// A Route with no assigned port renders at its declared host, which will
// not be served. That is deliberate: the alternative is sharing a port,
// and a front door routing two services to whichever matched first is
// worse than one that does not answer.
func (p Posture) SiteAddress(host string) string {
	if p.TLS != "tailscale" || p.TailnetName == "" {
		return host
	}
	port, ok := p.Ports[host]
	if !ok {
		// The Routes being created by THIS expansion are not in desired
		// state yet — .Resolved feeds the templates that declare them, so
		// it necessarily runs first. Ports is therefore empty exactly when
		// a capability is being installed, which is the moment Authelia's
		// own URLs are computed.
		//
		// Falling back to the declared host looked harmless and was not:
		// it produced authelia_url https://auth.<domain> under a posture
		// that serves nothing there, and Authelia refused to start with
		// "does not share a cookie scope" — after the plan had applied.
		//
		// The pin is the answer because it does not depend on what else
		// exists. Every capability whose origin .Resolved describes is a
		// shipped, pinned one; an unpinned service has no .Resolved URL to
		// get wrong, and still falls through to the declared host.
		port = p.portForAbsentRoute(host)
		if port == 0 {
			return host
		}
	}
	return p.TailnetName + ":" + strconv.Itoa(port)
}

// OriginFor is SiteAddress as a browser-facing URL. This is what an OIDC
// issuer and a dashboard href must both be built from.
func (p Posture) OriginFor(host string) string {
	return "https://" + p.SiteAddress(host)
}

// portForAbsentRoute is the port a Route WOULD be assigned, for a host
// whose Route is not in desired state yet.
//
// It must agree with assign(), and agreeing is the whole difficulty. The
// pin alone is not enough: assign() treats a pin as a preference and
// moves the Route when something else already publishes that port (F63).
// A fallback returning the pin unconditionally would hand Authelia an
// issuer on 7443 while Caddy, once the Route existed, served it
// elsewhere — a login that fails with both halves internally consistent
// and disagreeing with each other (F70).
//
// So it applies the same two rules in the same order: the pin if free,
// otherwise the first free port in the block. Returns 0 when nothing is
// available, which the caller renders as the declared host.
func (p Posture) portForAbsentRoute(host string) int {
	if pinned, ok := wellKnown[ServiceLabel(host)]; ok && p.portFree(pinned) {
		return pinned
	}
	for _, candidate := range PortBlock {
		if p.portFree(candidate) {
			return candidate
		}
	}
	return 0
}

// portFree reports whether a port is neither published by another
// container nor already assigned to an existing Route.
func (p Posture) portFree(port int) bool {
	if p.occupied[port] {
		return false
	}
	for _, assigned := range p.Ports {
		if assigned == port {
			return false
		}
	}
	return true
}

// SessionDomain is the cookie domain a session must be scoped to for
// single sign-on to hold across every service.
//
// Under a hostname posture that is the shared parent — services are
// siblings under it, and a cookie set there is sent to all of them.
// Under the tailnet posture every service is the SAME host on different
// ports, and cookies ignore ports entirely, so the host itself is both
// necessary and sufficient. Scoping to a parent of a ts.net name would
// also simply fail: ts.net is a public suffix, so a browser refuses a
// cookie set on the suffix.
func (p Posture) SessionDomain(publicDomain string) string {
	if p.TLS == "tailscale" && p.TailnetName != "" {
		return p.TailnetName
	}
	return publicDomain
}

// WellKnownPortFor exposes the pin for a service label, so tests and
// callers can assert the shipped catalog is pinned in full (F82).
func WellKnownPortFor(label string) (int, bool) {
	p, ok := wellKnown[label]
	return p, ok
}
