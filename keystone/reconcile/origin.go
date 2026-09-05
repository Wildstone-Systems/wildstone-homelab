package reconcile

// Route origins: where a Route is actually served, as opposed to the
// hostname a package declared for it (ADR-W019 A10).
//
// Every posture but "tailscale" serves a Route at its own hostname, and
// origin is simply "https://<host>". The tailnet posture cannot: a
// tailnet certificate covers exactly ONE name — the node's own MagicDNS
// name, no wildcards and no subdomains — so four Routes that each want a
// hostname have to share one and be told apart some other way. Port is
// that other way.
//
// The declared host does not become meaningless when that happens. It
// stays the Route's identity — what packages template, what policy reads,
// what an operator recognises — and only the ADDRESS changes. Keeping the
// two separate is what lets every service package stay exactly as it is
// while the addressing model underneath changes completely.
//
// Ports are assigned here rather than declared by packages for the reason
// the hostnames are not declared either: a package knows what it is, not
// what else is installed beside it, and port collisions are precisely a
// fact about the neighbours. Keystone sees the whole desired set.

import (
	"sort"
	"strconv"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// tailnetPortBlock is the closed set of ports the tailnet ingress
// profile publishes, in assignment order.
//
// Closed and small on purpose. Every entry is a port the ingress
// container must publish, and publishing is declared statically in the
// package — so an unbounded range would mean either a container that
// binds hundreds of ports it will never use, or an assignment that
// silently fails for the service that lands outside the range. Twelve is
// more Routes than the shipped catalog has capabilities.
//
// 443 leads because the dashboard is what a family opens, and it should
// be the one address nobody has to be told a port for.
var tailnetPortBlock = []int{443, 7443, 8443, 9443, 10443, 11443, 12443, 13443, 14443, 15443, 16443, 17443}

// wellKnownTailnetPorts pins the shipped capabilities to stable ports.
//
// Without this, ports would come from sorted position in the desired
// set, and installing anything alphabetically earlier would silently
// move every service after it — every bookmark in the household broken
// by an unrelated install. Pinning the names that ship means the common
// case never moves; anything unpinned takes the next free port, which
// can move, and that is a far smaller surface.
//
// Keyed on the FIRST LABEL of the declared host — "home" in
// "home.example.com" — because that label is what the package chose and
// the rest is the deployment's domain. Pinning on the whole host would
// unpin every service the moment the domain changed.
var wellKnownTailnetPorts = map[string]int{
	"home":     443,   // the dashboard: the address people actually type
	"auth":     7443,  // Authelia, needed as its own origin for OIDC
	"jellyfin": 8443,  //
	"photos":   9443,  //
	"docs":     10443, //
}

// assignTailnetPorts maps Route NAME to port across the whole desired
// set, deterministically.
//
// Determinism is not a style preference here: the reconciler decides
// NoOp-versus-Update by comparing a live file's bytes to a fresh render,
// so an assignment that varied between two renders of identical desired
// state would show up as permanent drift and rewrite the config forever.
func assignTailnetPorts(desired []resource.Resource) map[string]int {
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
	taken := make(map[int]bool, len(hosts))
	// Pinned hosts first, so an unpinned Route can never take a port a
	// pinned one is entitled to just by sorting earlier.
	for _, n := range hosts {
		if p, ok := wellKnownTailnetPorts[serviceLabel(n)]; ok {
			ports[n] = p
			taken[p] = true
		}
	}
	for _, n := range hosts {
		if _, done := ports[n]; done {
			continue
		}
		for _, p := range tailnetPortBlock {
			if !taken[p] {
				ports[n] = p
				taken[p] = true
				break
			}
		}
		// A Route beyond the block gets no port and is left unserved
		// rather than colliding with one that has one. Silently sharing a
		// port would route two services to whichever Caddy matched first.
	}
	return ports
}

// serviceLabel is the leading label of a host: "home" in
// "home.example.com". It is what a package named the service, with the
// deployment's domain stripped off.
func serviceLabel(host string) string {
	if i := strings.IndexByte(host, '.'); i > 0 {
		return host[:i]
	}
	return host
}

// routeSiteAddress is the site address a Route's Caddy fragment opens
// with: the declared host under every posture but "tailscale", and the
// node's single tailnet name plus this Route's port under that one.
//
// A Route with no port assigned renders at its declared host, which will
// not be served — deliberately. The alternative is sharing a port with
// another service, and a front door that routes two households' services
// to whichever site matched first is worse than one that does not answer.
func routeSiteAddress(p ingressPosture, host string) string {
	if p.tls != "tailscale" || p.tailnetName == "" {
		return host
	}
	port, ok := p.tailnetPorts[host]
	if !ok {
		return host
	}
	return p.tailnetName + ":" + strconv.Itoa(port)
}
