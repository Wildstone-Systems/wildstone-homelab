package resource

// Route upstreams on the node host itself (#433).
//
// A Route's upstream has had two forms: "container/<name>:<port>" for a
// container on the Wildstone network, reached by Docker DNS, and a
// literal "address:port" for something Wildstone does not run. A
// host-networked container (networkMode: host, #432) is neither. It has
// no name on the Wildstone network, and the literal address the proxy
// would have to dial — the node's bridge gateway — is a Docker
// implementation detail that differs per machine and per daemon config.
// Writing it into desired state would make a package that works on one
// node silently wrong on the next.
//
// So the third form names the place, not the address: "host/<port>".
// Keystone renders it to HostGatewayAlias, and the agent maps that one
// name to Docker's own "host-gateway" value when it creates the proxy
// container, so the address is resolved by the daemon that owns it, on
// the node where it is true, every time the container is created.
//
// The proxy stays on the bridge. Moving it to host networking would
// cost it Docker DNS, and with it every container upstream that exists
// today.

import (
	"fmt"
	"strconv"
	"strings"
)

// HostUpstreamPrefix introduces a Route upstream on the node host.
const HostUpstreamPrefix = "host/"

// HostGatewayAlias is the name a bridge-networked container uses to reach
// the node host. The agent writes it as "<alias>:host-gateway" into the
// container's /etc/hosts; Docker substitutes the gateway address itself.
// Docker Desktop resolves the same name natively, so the rendered Caddy
// config is not Wildstone-specific syntax either.
const HostGatewayAlias = "host.docker.internal"

// HostGatewayProtocolMinor is the wardstone protocol minor whose agent
// maps HostGatewayAlias to Docker's host-gateway. An older agent treats
// it as an ordinary host alias, fails to resolve it on the node, and
// skips it — the proxy then starts with no route to the host and every
// host Route answers 502 with nothing reporting why. The reconciler
// therefore refuses a host Route on such a node and leaves the alias out
// of container specs there (so their hashes do not change until the
// agent can honour it).
const HostGatewayProtocolMinor = 24

// ParseHostUpstream reports whether upstream is the "host/<port>" form
// and, if so, its port. isHost is true for anything that STARTS like the
// form, so a malformed one is reported as an error rather than falling
// through to the literal-address form.
//
// The grammar is closed: exactly "host/" followed by a decimal port in
// 1..65535, no leading zeros, no sign, no host part and no path. Every
// looser reading has a second meaning — "host/localhost:80" could be the
// proxy's own loopback, "host/8123/api" is a path Caddy would not take
// from an upstream — and a form that means two things is the kind that
// works on the author's machine.
func ParseHostUpstream(upstream string) (port int, isHost bool, err error) {
	if !strings.HasPrefix(strings.ToLower(upstream), HostUpstreamPrefix) {
		return 0, false, nil
	}
	if !strings.HasPrefix(upstream, HostUpstreamPrefix) {
		return 0, true, fmt.Errorf("%q: the prefix is lower-case %q", upstream, HostUpstreamPrefix)
	}
	rest := strings.TrimPrefix(upstream, HostUpstreamPrefix)
	if rest == "" {
		return 0, true, fmt.Errorf("%q: missing port, want %s<port>", upstream, HostUpstreamPrefix)
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return 0, true, fmt.Errorf("%q: want %s<port> with a decimal port and nothing else (no host, scheme or path)", upstream, HostUpstreamPrefix)
		}
	}
	if len(rest) > 1 && rest[0] == '0' {
		return 0, true, fmt.Errorf("%q: port has a leading zero", upstream)
	}
	n, convErr := strconv.Atoi(rest)
	if convErr != nil || n < 1 || n > 65535 {
		return 0, true, fmt.Errorf("%q: port must be 1..65535", upstream)
	}
	return n, true, nil
}

// IsHostUpstream reports whether upstream is a well-formed "host/<port>".
func IsHostUpstream(upstream string) bool {
	_, isHost, err := ParseHostUpstream(upstream)
	return isHost && err == nil
}

// validateRouteUpstream refuses a malformed host upstream, and a literal
// upstream that spells the host by the names the host form renders to.
//
// "host:8123" and "host.docker.internal:8123" look like they reach the
// node, and would bypass everything that makes that work: the alias is
// only written into the proxy when a host Route exists, and only on an
// agent that maps it. Written literally, they render, pass validation,
// and 502 — or resolve to whatever an upstream DNS server says "host"
// is. Refusing them points the author at the one form that is wired up.
//
// Every other literal passes unchanged: an existing "ip:port" or
// "container/<name>:<port>" upstream means today exactly what it meant
// before this form existed.
func validateRouteUpstream(upstream string) error {
	if _, isHost, err := ParseHostUpstream(upstream); isHost {
		return err
	}
	if strings.HasPrefix(upstream, "container/") {
		return nil
	}
	h := upstream
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	switch strings.ToLower(strings.TrimSuffix(h, ".")) {
	case "host", HostGatewayAlias, "host-gateway":
		return fmt.Errorf("%q is ambiguous: to reach a service on this node's host network write %s<port>", upstream, HostUpstreamPrefix)
	}
	return nil
}
