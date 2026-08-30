package reconcile

import (
	"fmt"
	"path"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// routeFragmentPath returns the managed-file path Route/<name> is rendered
// to: <managedPrefix>/routes/<name>.caddy (ADR-W004 A1's Route apply
// description). managedPrefix is expected to be one of the target agent's
// configured ManagedPrefixes.
func routeFragmentPath(managedPrefix, name string) string {
	return path.Join(managedPrefix, "routes", name+".caddy")
}

// baseCaddyfilePath returns the managed path of the Caddyfile that
// imports every route fragment: <managedPrefix>/Caddyfile. Both ingress
// profiles mount the managed prefix read-only, so this file IS Caddy's
// configuration — before it existed the fragments were written and never
// read by anything (#40).
func baseCaddyfilePath(managedPrefix string) string {
	return path.Join(managedPrefix, "Caddyfile")
}

// renderBaseCaddyfile is the whole Caddy config apart from the per-route
// fragments it imports.
//
// auto_https is off because Wildstone does not terminate TLS at Caddy in
// either shipped profile: the public path arrives already-terminated from
// a Cloudflare tunnel, and the private path is reached over a tailnet or
// LAN that provides its own transport security. Sites are therefore
// rendered with an explicit http:// scheme so Caddy binds :80 and does
// not try to provision certificates for a hostname it cannot validate.
//
// admin off: the admin API is a write surface on the same container that
// serves family traffic, and nothing in Wildstone drives Caddy through
// it — the reconciler's mechanism is files plus a reload.
func renderBaseCaddyfile() []byte {
	return []byte("# managed by wildstone-keystone — do not edit\n" +
		"{\n" +
		"\tadmin off\n" +
		"\tauto_https off\n" +
		"}\n" +
		"\n" +
		"import routes/*.caddy\n" +
		"\n" +
		// Default deny (F26). Caddy answers an unmatched site with an empty
		// 200, which on an internet-facing front door reads as "something is
		// here" for every hostname anyone points at it. A Route is the only
		// thing that makes a host served; everything else gets the connection
		// closed. This block is last so any real site address, being more
		// specific, still wins.
		":80 {\n" +
		"\tabort\n" +
		"}\n")
}

// caddyUpstream translates a RouteSpec upstream into an address Caddy can
// actually dial.
//
// Packages declare "container/<name>:<port>". That form was previously
// written into the config verbatim, which is not a host:port at all — and
// there was no mechanism behind it either, because containers sat on
// Docker's default bridge, which has no embedded DNS. Since protocol
// minor 8 every managed container is attached to a user-defined network
// where Docker registers its name, so the container name resolves and the
// prefix is simply dropped. An "ip:port" upstream passes through.
func caddyUpstream(upstream string) string {
	return strings.TrimPrefix(upstream, "container/")
}

// renderRouteFragment deterministically renders a RouteSpec to a managed
// Caddy config fragment. Deterministic rendering matters here beyond
// style: the reconciler's Route observe step (route.go / reconcile.go)
// compares the live file's bytes against a fresh render of the desired
// spec to decide NoOp vs Update, so any non-determinism (e.g. map
// iteration order) would show as permanent drift.
//
// autheliaAddr is the forward-auth endpoint; only auth "authelia_oidc"
// asks the proxy to enforce anything. The other closed-set values are
// pass-through AT THE PROXY by definition — app_oidc and app_ldap mean
// the app runs its own login, portal is the login surface itself, and
// none is honestly unauthenticated (which policy already refuses on a
// public classification, ADR-W009 §9.2-2). The classification is emitted
// as a comment because it drives policy, not Caddy.
func renderRouteFragment(spec *resource.RouteSpec, autheliaAddr string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# managed by wildstone-keystone — do not edit\n")
	fmt.Fprintf(&b, "# classification: %s\n", spec.Classification)
	fmt.Fprintf(&b, "http://%s {\n", spec.Host)
	if spec.Auth == "authelia_oidc" {
		// Authelia's forward-auth contract: it answers /api/authz/forward-auth
		// with 2xx to allow and a redirect to its portal otherwise, and the
		// copied headers are what downstream apps read the identity from.
		fmt.Fprintf(&b, "\tforward_auth %s {\n", autheliaAddr)
		fmt.Fprintf(&b, "\t\turi /api/authz/forward-auth\n")
		fmt.Fprintf(&b, "\t\tcopy_headers Remote-User Remote-Groups Remote-Name Remote-Email\n")
		fmt.Fprintf(&b, "\t}\n")
	}
	fmt.Fprintf(&b, "\treverse_proxy %s\n", caddyUpstream(spec.Upstream))
	fmt.Fprintf(&b, "}\n")
	return []byte(b.String())
}
