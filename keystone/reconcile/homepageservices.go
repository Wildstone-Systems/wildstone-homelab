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
// Since F87 the entry is a presented service rather than a bare link:
// display name, icon, one line of plain English, a group, and a status
// check. All five are derived, and the ONE place a new app declares them
// is homepageApps below — a single row per upstream container. That is
// the whole integration contract, and it is deliberately not a field on
// the Route: a package declaring its own icon and blurb restates what
// its own Route already implies, which is the "declaring half of a
// relationship" trap ADR-W021 A6 rejects for the href itself.
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
// (F73 later added the file to the digest for the DASHBOARD container
// specifically, via the mount-prefix carve-out — see configdigest.go.)

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

// homepageApp is how one application is presented to a person: what to
// call it, what to draw next to it, and what to say it does.
//
// Keyed by the UPSTREAM CONTAINER NAME rather than by capability,
// because the capability is the job and the container is the app — two
// media profiles (Jellyfin, Plex) share a capability and share nothing a
// person looks at. The container name is already in the Route, already
// unique on the node's Docker network, and already the thing the status
// check dials.
//
// Icon values are dashboard-icons slugs, which is what Homepage resolves
// a bare name to. Homepage fetches them from a CDN, so a household with
// no egress sees the name and no picture — which is exactly what it sees
// today, so this is never worse than the status quo. Shipping the icon
// files into the config volume is the fix when that matters, and it does
// not change this table.
type homepageApp struct {
	title       string
	icon        string
	description string
}

// homepageApps is the presentation table: ONE row per app Wildstone
// ships or expects to meet. Adding an app to the dashboard is adding a
// row here — nothing else in this file is per-app.
//
// Descriptions are written for the person holding the tablet, not for
// the operator: "Films, series and music", not "media". The old renderer
// emitted the capability slug as the description, which told a family
// member the word Keystone happens to use internally.
var homepageApps = map[string]homepageApp{
	"jellyfin":       {"Jellyfin", "jellyfin", "Films, series and music"},
	"immich-server":  {"Photos", "immich", "Every photo and video, searchable"},
	"paperless":      {"Documents", "paperless-ngx", "Scanned paperwork you can search"},
	"ntfy":           {"Notifications", "ntfy", "Alerts pushed to your phone"},
	"uptime-kuma":    {"Uptime Kuma", "uptime-kuma", "Is everything still up?"},
	"authelia":       {"Sign-in", "authelia", "Accounts and two-factor"},
	"lldap":          {"Directory", "lldap", "People and groups"},
	"home-assistant": {"Home Assistant", "home-assistant", "Lights, sensors and automations"},
	"homeassistant":  {"Home Assistant", "home-assistant", "Lights, sensors and automations"},
	"homepage":       {"Home", "homepage", "This dashboard"},
}

// homepageFallbackIcon is what an app with no row gets. A generic
// Material Design icon rather than a guess at a dashboard-icons slug:
// guessing is right often enough to be tempting and wrong silently — a
// missing slug renders as a broken image, which looks worse than a plain
// one that was never claimed to be the app's own.
const homepageFallbackIcon = "mdi-application-outline"

// homepageGroups is the rendered group order. Household first because it
// is what the family opens the page for; Admin second because it is the
// operator's, and burying it is the point.
var homepageGroups = []string{"Household", "Admin"}

// homepageGroup places a Route by its CLASSIFICATION, which is the field
// that already says who a surface is for and is mandatory on every Route
// (ADR-W009). Deriving the grouping from it means the dashboard cannot
// disagree with policy about which services are the operator's.
func homepageGroup(classification string) string {
	if classification == "admin_tailscale" {
		return "Admin"
	}
	return "Household"
}

// homepageTitle turns a resource name into something to show a person.
// "photos" -> "Photos", "uptime-kuma" -> "Uptime Kuma". Only used for an
// app with no row in homepageApps.
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

// upstreamContainerName is the container half of "container/<name>:<port>",
// or "" for an upstream that is not a container (an "ip:port" Route).
func upstreamContainerName(upstream string) string {
	if !strings.HasPrefix(upstream, "container/") {
		return ""
	}
	addr := strings.TrimPrefix(upstream, "container/")
	if i := strings.IndexByte(addr, ':'); i >= 0 {
		return addr[:i]
	}
	return addr
}

// dashboardNode is the node Homepage itself runs on, or "" if the
// dashboard capability is not installed.
//
// Needed because the status check below dials the container network, and
// a container on another node is not on this one's. Read from desired
// state rather than from the node the file is being written to, so the
// render stays a pure function of desired state — one file, identical on
// every node, which is what nodePrereqFiles assumes.
func dashboardNode(desired []resource.Resource) string {
	node := ""
	for _, r := range desired {
		if r.Kind != resource.KindContainer || r.Container == nil {
			continue
		}
		if strings.TrimPrefix(r.Metadata.Owner, "capability/") != "dashboard" {
			continue
		}
		if r.Metadata.Node == "" {
			continue
		}
		// Smallest wins, so two dashboard containers cannot make the
		// render depend on slice order.
		if node == "" || r.Metadata.Node < node {
			node = r.Metadata.Node
		}
	}
	return node
}

// homepageMonitor is the address Homepage polls to decide whether a
// service is up — the container, over the wildstone Docker network, in
// plain HTTP.
//
// NOT the browser-facing https:// URL, for two reasons that both make
// the dot lie. Under `tls: internal` the certificate is issued by the
// household CA, which the Homepage container does not trust, so every
// service would report down. And a Route behind authelia_oidc answers a
// redirect to the login page, which says the PROXY is up and nothing
// about the app.
//
// Dialing the container says the application itself is answering, which
// is the fact a person wants from a status dot. It is also the same
// address Caddy dials (caddyUpstream), so a green dot and a working link
// agree by construction.
func homepageMonitor(upstream string) string {
	if !strings.HasPrefix(upstream, "container/") {
		return ""
	}
	addr := strings.TrimPrefix(upstream, "container/")
	if !strings.Contains(addr, ":") {
		// No port: nothing to dial. A Route like this cannot be proxied
		// either, so silence is the honest answer.
		return ""
	}
	return "http://" + addr
}

// homepageEntry is one rendered service, fully resolved.
type homepageEntry struct {
	group   string
	title   string
	href    string
	icon    string
	desc    string
	monitor string
	// name is the Route's resource name, kept only to disambiguate two
	// services that resolve to the same title.
	name string
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
	entries := homepageEntries(desired)

	var b strings.Builder
	b.WriteString("# managed by wildstone-keystone — do not edit\n")
	if len(entries) == 0 {
		// A valid empty list, not an absent file. Homepage seeds its own
		// sample config into any config file it does not find, which is
		// how "My First Service" appears on a working install.
		b.WriteString("# no routed services on this node yet\n[]\n")
		return []byte(b.String())
	}
	for _, group := range homepageGroups {
		if !homepageGroupUsed(entries, group) {
			continue
		}
		fmt.Fprintf(&b, "- %s:\n", group)
		for _, e := range entries {
			if e.group != group {
				continue
			}
			fmt.Fprintf(&b, "    - %s:\n", e.title)
			fmt.Fprintf(&b, "        href: %s\n", e.href)
			if e.desc != "" {
				fmt.Fprintf(&b, "        description: %s\n", e.desc)
			}
			fmt.Fprintf(&b, "        icon: %s\n", e.icon)
			if e.monitor != "" {
				fmt.Fprintf(&b, "        siteMonitor: %s\n", e.monitor)
			}
		}
	}
	return []byte(b.String())
}

// homepageEntries resolves the Route set into the presented services, in
// render order. Shared with the settings renderer, which needs to know
// which groups exist before it can lay them out — two renderers deriving
// the group list separately is how a layout ends up naming a group the
// service list does not have.
func homepageEntries(desired []resource.Resource) []homepageEntry {
	// The address a browser must use, which under the tailnet posture is
	// the node's one name and this Route's port rather than the declared
	// host (ADR-W019 A10). Getting this from the same helper the Caddy
	// fragments use is the point: a dashboard whose links disagree with
	// the proxy is worse than no dashboard, because every link looks
	// deliberate.
	posture := postureOf(desired)
	home := dashboardNode(desired)

	var entries []homepageEntry
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

		e := homepageEntry{
			group: homepageGroup(r.Route.Classification),
			href:  "https://" + routeSiteAddress(posture, r.Route.Host),
			name:  r.Metadata.Name,
		}
		if app, known := homepageApps[upstreamContainerName(r.Route.Upstream)]; known {
			e.title, e.icon, e.desc = app.title, app.icon, app.description
		} else {
			// No row: still a complete entry, just an unbranded one. The
			// capability is a poor description but it is the only thing
			// Keystone knows about an app nobody has described.
			e.title, e.icon, e.desc = homepageTitle(r.Metadata.Name), homepageFallbackIcon, homepageTitle(owner)
		}
		// Only a service on Homepage's own node is reachable by container
		// name; anything else gets no dot rather than a permanently red
		// one.
		if home != "" && r.Metadata.Node == home {
			e.monitor = homepageMonitor(r.Route.Upstream)
		}
		entries = append(entries, e)
	}

	// Sorted, so the file is a pure function of desired state and does
	// not churn — a rewritten prereq replaces the container.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].title != entries[j].title {
			return entries[i].title < entries[j].title
		}
		return entries[i].name < entries[j].name
	})
	// A title is a YAML KEY, so two services sharing one silently
	// collapse into a single card and a service disappears from the
	// dashboard. Two Routes to the same app (one family, one admin) is
	// enough to cause it. Resource names are unique per kind, so
	// qualifying with the name always separates them.
	// Flagged before any renaming: rewriting in place would make the
	// second of a pair compare against its own already-qualified
	// predecessor and look unique.
	dup := make([]bool, len(entries))
	for i := range entries {
		dup[i] = (i > 0 && entries[i-1].title == entries[i].title) ||
			(i+1 < len(entries) && entries[i+1].title == entries[i].title)
	}
	for i := range entries {
		if dup[i] {
			entries[i].title = fmt.Sprintf("%s (%s)", entries[i].title, entries[i].name)
		}
	}
	return entries
}

// homepageGroupUsed reports whether any entry lands in a group. An empty
// group rendered as a heading with nothing under it is the kind of
// detail that makes a dashboard look broken.
func homepageGroupUsed(entries []homepageEntry, group string) bool {
	for _, e := range entries {
		if e.group == group {
			return true
		}
	}
	return false
}
