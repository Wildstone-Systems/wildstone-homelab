package reconcile

import (
	"fmt"
	"path"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// routeFragmentPath returns the managed-file path Route/<name> is rendered
// to: <managedPrefix>/routes/<name>.caddy (ADR-W004 A1's Route apply
// description). managedPrefix is expected to be one of the target agent's
// configured ManagedPrefixes.
func routeFragmentPath(managedPrefix, name string) string {
	return path.Join(managedPrefix, "routes", name+".caddy")
}

// placeholderFragmentPath is a fragment that exists only so
// `import routes/*.caddy` always matches at least one non-empty file.
//
// Caddy's --watch re-adapts the whole config every second, so any
// adaptation warning repeats every second for as long as the state lasts
// (F30). Two states Wildstone creates itself trigger one: a fresh install
// before its first Route ("No files matching import glob pattern") and a
// deleted Route ("Import file is empty"). Measured against a real Caddy:
// one warning per second in both, silent with a comment-only file.
// Unbounded json-file logging turns that into gigabytes a year.
func placeholderFragmentPath(managedPrefix string) string {
	return routeFragmentPath(managedPrefix, resource.ReservedRouteName)
}

// renderPlaceholderFragment is comment-only on purpose: non-empty, so
// Caddy stops warning, and contributing no configuration.
func renderPlaceholderFragment() []byte {
	return []byte("# managed by wildstone-keystone — do not edit\n" +
		"# Placeholder so `import routes/*.caddy` always matches a non-empty\n" +
		"# file. Caddy warns once a second, forever, otherwise (F30).\n")
}

// renderDeletedFragment replaces a deleted Route's fragment. The file
// cannot be removed: the agent has no delete-file op, which is a
// consequence of the closed operation surface the product's security
// rests on (ADR-W003), not an oversight. It was previously truncated to
// zero bytes, which Caddy warns about once a second forever — so it now
// holds a comment saying what happened instead.
func renderDeletedFragment(name string) []byte {
	return []byte("# managed by wildstone-keystone — do not edit\n" +
		"# Route " + name + " was deleted. This file remains because the agent\n" +
		"# has no delete-file op; it is a comment rather than empty so Caddy\n" +
		"# does not warn once a second about an empty import (F30).\n")
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
// defaultTrustedProxies is what the base Caddyfile trusts to have set the
// X-Forwarded-* headers (ADR-W019 A6).
//
// Caddy replaces X-Forwarded-Proto with the scheme of the request IT
// received unless the sender is a trusted proxy. Without this, a request
// that was correctly terminated as https upstream reaches Authelia
// claiming http, and Authelia refuses it outright:
//
//	invalid X-Forwarded-Proto header value 'http'
//
// which is every OIDC request through either shipped ingress profile.
//
// private_ranges rather than 0.0.0.0/0: the terminator in front of this
// listener is always a container on the bridge or a host on the LAN —
// cloudflared, the tailnet Caddy, a local TLS front. A list that trusts
// everyone would let any caller assert any scheme and any client IP,
// which is the header-spoofing hole the list exists to close.
const defaultTrustedProxies = "private_ranges"

// ingressPosture is how the node's proxy is configured, read from the
// Ingress resource the installed ingress package declares (ADR-W019 B5).
//
// A node with no Ingress declared falls back to the posture that shipped
// before the kind existed: TLS terminated upstream, private ranges
// trusted. That keeps an older deployment rendering exactly what it
// rendered yesterday rather than silently changing how its front door
// behaves on upgrade.
type ingressPosture struct {
	tls            string
	trustedProxies string
	// acmeDNS is the DNS-01 provider module name when tls is "public";
	// empty for every other posture.
	acmeDNS string
	// tailnetName and tailnetPorts are the "tailscale" posture's
	// addressing: the one name the node's certificate covers, and which
	// port each declared host is served on behind it.
	tailnetName  string
	tailnetPorts map[string]int
}

// terminatedUpstream is the pre-Ingress posture: TLS handled in front,
// private ranges trusted. Named so tests and the fallback path say what
// they mean rather than repeating two fields.
var terminatedUpstream = ingressPosture{tls: "off", trustedProxies: defaultTrustedProxies}

func postureOf(desired []resource.Resource) ingressPosture {
	p := terminatedUpstream
	for _, r := range desired {
		if r.Kind != resource.KindIngress || r.Ingress == nil {
			continue
		}
		if r.Ingress.TLS != "" {
			p.tls = r.Ingress.TLS
		}
		if len(r.Ingress.TrustedProxies) > 0 {
			p.trustedProxies = strings.Join(r.Ingress.TrustedProxies, " ")
		}
		p.acmeDNS = r.Ingress.ACMEDNS
		p.tailnetName = r.Ingress.TailnetName
		break
	}
	if p.tls == "tailscale" {
		p.tailnetPorts = assignTailnetPorts(desired)
	}
	return p
}

// siteScheme is the scheme Route fragments are written with. It must
// match what the BROWSER uses, because an OIDC issuer is a string that
// has to be identical on both sides (ADR-W019 A2) — which is why this
// cannot be a free choice per package.
// terminatesTLS reports whether this proxy holds the certificate itself,
// whoever issued it. Both "internal" and "public" do; only the trust
// story differs, and every rendering decision below turns on the former.
func (p ingressPosture) terminatesTLS() bool {
	return p.tls == "internal" || p.tls == "public" || p.tls == "tailscale"
}

func (p ingressPosture) siteScheme() string {
	if p.terminatesTLS() {
		return "https"
	}
	return "http"
}

func renderBaseCaddyfile(p ingressPosture) []byte {
	global := "{\n" +
		"\tadmin off\n"
	// auto_https off when TLS terminates upstream; local_certs when this
	// proxy holds the certificate itself (ADR-W019 A3). local_certs is
	// Caddy's own internal CA, which is what makes a hostname no public
	// CA would ever sign serve real TLS on a LAN.
	switch p.tls {
	case "internal":
		global += "\tlocal_certs\n"
	case "tailscale":
		// Nothing to configure. Caddy fetches *.ts.net certificates from
		// a locally-running tailscaled automatically, so ordinary
		// automatic HTTPS is already correct — the only requirement is
		// that the socket is reachable and the site name is the node's.
		// Saying anything here would be saying it twice.
	case "public":
		// A real CA over DNS-01 (ADR-W019 A9). auto_https stays ON —
		// unlike local_certs this is the ordinary ACME path, and the only
		// thing that has to change is HOW the challenge is answered,
		// because the listener is on an address no CA can reach. The
		// token comes from the environment rather than the file: managed
		// files under the prefix are world-readable to anything that can
		// read the node's config, and a zone-editing credential is not
		// something to leave there.
		global += "\tacme_dns " + p.acmeDNS + " {env.CF_API_TOKEN}\n"
	default:
		global += "\tauto_https off\n"
	}
	if p.trustedProxies != "" {
		global += "\tservers {\n" +
			"\t\ttrusted_proxies static " + p.trustedProxies + "\n" +
			"\t}\n"
	}
	global += "}\n"
	return []byte("# managed by wildstone-keystone — do not edit\n" +
		global +
		"\n" +
		"import routes/*.caddy\n" +
		"\n" +
		// Default deny (F26). Caddy answers an unmatched site with an empty
		// 200, which on an internet-facing front door reads as "something is
		// here" for every hostname anyone points at it. A Route is the only
		// thing that makes a host served; everything else gets the connection
		// closed. This block is last so any real site address, being more
		// specific, still wins.
		p.siteScheme() + "://:" + defaultDenyPort(p) + " {\n" +
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
//
// "host/<port>" is a service on the node's own network namespace (#433).
// It renders to resource.HostGatewayAlias, never to an address: the
// agent writes that name into the proxy container's /etc/hosts as
// Docker's "host-gateway", so the bridge gateway is looked up by the
// daemon on the node where it is true, and no machine-specific IP ever
// reaches desired state or this file.
func caddyUpstream(upstream string) string {
	if port, isHost, err := resource.ParseHostUpstream(upstream); isHost && err == nil {
		return fmt.Sprintf("%s:%d", resource.HostGatewayAlias, port)
	}
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
func renderRouteFragment(spec *resource.RouteSpec, autheliaAddr string, p ingressPosture) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# managed by wildstone-keystone — do not edit\n")
	fmt.Fprintf(&b, "# classification: %s\n", spec.Classification)
	fmt.Fprintf(&b, "%s://%s {\n", p.siteScheme(), routeSiteAddress(p, spec.Host))
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

// defaultDenyPort keeps the catch-all on the port the profile actually
// listens on. Caddy binds :80 for an http site and :443 for an https
// one, and a default-deny block on the wrong port is not a default deny
// at all — it is an unrouted host answering an empty 200 (F26).
func defaultDenyPort(p ingressPosture) string {
	if p.terminatesTLS() {
		return "443"
	}
	return "80"
}
