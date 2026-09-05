package reconcile

// The rendered Homepage service list.
//
// The dashboard capability shipped Homepage with its own sample file —
// "My First Service / Homepage is awesome" — on a fully healthy install,
// because the package declared a Volume, a Container and a Route and no
// ConfigFile, so nothing ever told it what the household runs (F55).
//
// Keystone already knows: every Route in desired state names a host and
// what serves it. This is the same shape as the Caddy fragments, the
// Authelia clients file, the access-control block and the LLDAP lists —
// one merged managed file, derived from desired state, written as a node
// prereq. It was the only one of that set with no renderer.
//
// Deliberately NOT in the config digest, for the reason route fragments
// are not: this file is derived from the Route set, and the ingress
// container mounts the whole managed prefix — so folding it in would
// replace Caddy on every Route change, which is exactly the trade
// TestRouteFragmentsAreNotInTheDigest exists to prevent. The cost is
// that Homepage shows a new service when it next re-reads its config
// rather than instantly; a stale dashboard is a much smaller problem
// than a fleet replaced on every route edit.
//
// A package declaring its own homepage entry was the alternative and is
// rejected in ADR-W021 A6: it would restate the host its own Route
// already carries, which is the "declaring half of a relationship" trap
// LdapGroupSpec names — two places that must agree, one of which nobody
// updates.

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// homepageServicesPath is the file the dashboard container bind-mounts
// over Homepage's own services.yaml.
func homepageServicesPath(managedPrefix string) string {
	return path.Join(managedPrefix, "dashboard", "services.yaml")
}

// homepageTitle turns a resource name into something to show a person.
// "photos" -> "Photos", "uptime-kuma" -> "Uptime Kuma".
func homepageTitle(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// renderHomepageServices builds Homepage's services.yaml from the Routes.
//
// Two exclusions, both so the page shows what a person would actually
// click. A `portal` Route is the login page — listing it as a service
// invites someone to log in to the thing they are already logged into.
// The dashboard's own Route is the page being rendered.
//
// The scheme is https, hardcoded. That is what a browser uses and what
// Authelia requires (ADR-W019 A2: the issuer is a string that must
// match), while the Caddy fragments render http:// because TLS
// terminates upstream. No ingress profile currently declares its
// browser-facing scheme, so this is the SECOND consumer of the trust
// context ADR-W019 A7 describes — the first being the CA root a package
// needs to trust. When that lands, this reads it instead of assuming.
func renderHomepageServices(desired []resource.Resource) []byte {
	type svc struct{ title, host, capability string }
	var svcs []svc
	for _, r := range desired {
		if r.Kind != resource.KindRoute || r.Route == nil || r.Route.Host == "" {
			continue
		}
		if r.Route.Auth == "portal" {
			continue
		}
		owner := strings.TrimPrefix(r.Metadata.Owner, "capability/")
		if owner == "dashboard" {
			continue
		}
		svcs = append(svcs, svc{homepageTitle(r.Metadata.Name), r.Route.Host, owner})
	}
	// Sorted, so the file is a pure function of desired state and does
	// not churn — a rewritten prereq replaces the container.
	sort.Slice(svcs, func(i, j int) bool { return svcs[i].title < svcs[j].title })

	var b strings.Builder
	b.WriteString("# managed by wildstone-keystone — do not edit\n")
	if len(svcs) == 0 {
		// A valid empty list, not an absent file. Homepage seeds its own
		// sample config into any config file it does not find, which is
		// how "My First Service" appears on a working install.
		b.WriteString("# no routed services on this node yet\n[]\n")
		return []byte(b.String())
	}
	b.WriteString("- Household:\n")
	for _, s := range svcs {
		fmt.Fprintf(&b, "    - %s:\n", s.title)
		fmt.Fprintf(&b, "        href: https://%s\n", s.host)
		if s.capability != "" {
			fmt.Fprintf(&b, "        description: %s\n", s.capability)
		}
	}
	return []byte(b.String())
}
