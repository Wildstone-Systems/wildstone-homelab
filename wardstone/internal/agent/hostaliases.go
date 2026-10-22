package agent

// Host aliases: names a container must be able to resolve, resolved by
// the NODE and written into the container's /etc/hosts (protocol minor
// 14).
//
// The problem this solves is narrow and was invisible until a real login
// failed. Under the tailnet ingress posture the OIDC issuer is the node's
// MagicDNS name, because an issuer has to match what the browser saw byte
// for byte (ADR-W019 A10). But an app does not only redirect a browser
// there — it fetches the discovery document and exchanges the code
// server-side, from inside its container. Docker's embedded DNS forwards
// to the host's EXTERNAL nameservers and never learns a .ts.net name, so
// those calls fail with "Name or service not known" and the app answers
// 500 to a login it never got to attempt. On this deployment that was
// every OIDC consumer at once: open-webui, immich, homepage.
//
// The node resolves the names rather than Keystone supplying addresses,
// because the node is the only place they resolve — MagicDNS is
// configured on the host. It also means a changed tailnet address is
// picked up the next time the container is created, with nothing to
// update in desired state.

import (
	"context"
	"log/slog"
	"net"
	"sort"
)

// hostGatewayAlias is the one alias the node does NOT resolve (protocol
// minor 24, #433). It names the node host as seen from a bridge
// container, and the only party that knows that address is Docker: the
// daemon substitutes its bridge gateway for the literal "host-gateway"
// when it writes /etc/hosts. Keystone asks for it on the ingress proxy
// and the dashboard so a Route can reach a host-networked service
// ("host/<port>") with no machine-specific IP in desired state.
//
// It widens nothing. A bridge container can already route to its
// gateway; this gives that address a name. The value is a closed
// constant — Keystone cannot make the agent write any other magic value,
// because every other name still goes through the resolver below.
const hostGatewayAlias = "host.docker.internal"

// dockerHostGateway is Docker's own ExtraHosts value for the host.
const dockerHostGateway = "host-gateway"

// resolveHostAliases turns names into Docker "name:ip" ExtraHosts
// entries, in a deterministic order.
//
// A name that does not resolve is SKIPPED, not fatal. Refusing to start
// the container would turn a login that fails into a service that does
// not run — strictly worse, and for a name that may be irrelevant to
// most of what the container does. The app then reports the same
// resolution error it would have reported anyway.
func resolveHostAliases(ctx context.Context, names []string, log *slog.Logger) []string {
	if len(names) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if name == hostGatewayAlias {
			out = append(out, name+":"+dockerHostGateway)
			continue
		}
		addrs, err := net.DefaultResolver.LookupHost(ctx, name)
		if err != nil || len(addrs) == 0 {
			if log != nil {
				log.Warn("host alias did not resolve; container will start without it",
					"name", name, "err", err)
			}
			continue
		}
		// One address, chosen deterministically: /etc/hosts with several
		// entries for a name is legal but the order decides which is
		// tried first, and a set that varies per container recreate is a
		// difference nobody can see and everybody has to debug.
		sort.Strings(addrs)
		out = append(out, name+":"+addrs[0])
	}
	return out
}
