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
var wellKnown = map[string]int{
	"home":     443,   // the dashboard: the address people actually type
	"auth":     7443,  // Authelia, which needs its own origin for OIDC
	"jellyfin": 8443,  //
	"photos":   9443,  //
	"docs":     10443, //
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
		return host
	}
	return p.TailnetName + ":" + strconv.Itoa(port)
}

// OriginFor is SiteAddress as a browser-facing URL. This is what an OIDC
// issuer and a dashboard href must both be built from.
func (p Posture) OriginFor(host string) string {
	return "https://" + p.SiteAddress(host)
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
