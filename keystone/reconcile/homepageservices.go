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
// the Model: a package declaring its own icon and blurb restates what
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
	"unicode"

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

	// The catalog expansion of 2026-09: one row per new package's upstream
	// container, in the order a household would meet them. Icon slugs are
	// dashboard-icons names, checked against that repository's index at
	// authoring time; a slug that later disappears renders as a broken
	// image rather than failing a test, which is the trade the header of
	// this table records.
	"frigate":        {"Cameras", "frigate", "Camera recordings and who was at the door"},
	"plex":           {"Plex", "plex", "Films, series and music"},
	"pihole":         {"Pi-hole", "pi-hole", "Ad blocking for the whole network"},
	"adguard-home":   {"AdGuard Home", "adguard-home", "Ad blocking for the whole network"},
	"vaultwarden":    {"Passwords", "vaultwarden", "The household password vault"},
	"zigbee2mqtt":    {"Zigbee", "zigbee2mqtt", "Zigbee bulbs, sensors and switches"},
	"open-webui":     {"Chat", "open-webui", "Talk to an AI that runs at home"},
	"grafana":        {"Grafana", "grafana", "Graphs of how everything is doing"},
	"qbittorrent":    {"Downloads", "qbittorrent", "The download queue"},
	"radarr":         {"Radarr", "radarr", "Films on the way"},
	"sonarr":         {"Sonarr", "sonarr", "Series on the way"},
	"lidarr":         {"Lidarr", "lidarr", "Albums on the way"},
	"prowlarr":       {"Prowlarr", "prowlarr", "Where downloads are searched for"},
	"bazarr":         {"Bazarr", "bazarr", "Subtitles for the library"},
	"seerr":          {"Requests", "jellyseerr", "Ask for a film or a series"},
	"homarr":         {"Homarr", "homarr", "Your own start page"},
	"audiobookshelf": {"Audiobooks", "audiobookshelf", "Audiobooks and podcasts"},
	"navidrome":      {"Music", "navidrome", "Your music collection"},
	"beszel":         {"Beszel", "beszel", "How the machines are doing"},
	"calibre-web":    {"Books", "calibre-web", "The e-book library"},
	"kavita":         {"Kavita", "kavita", "Books, comics and manga"},
	"mealie":         {"Recipes", "mealie", "Recipes, meal plans and the shopping list"},
	"linkding":       {"Bookmarks", "linkding", "Links worth keeping"},
	"syncthing":      {"Syncthing", "syncthing", "Folders kept in step between devices"},
	"freshrss":       {"Feeds", "freshrss", "Every site you follow, in one page"},
	"stirling-pdf":   {"PDF tools", "stirling-pdf", "Merge, split and sign PDFs"},
	"it-tools":       {"Tools", "it-tools", "QR codes, converters and generators"},
	"filebrowser":    {"Files", "filebrowser", "Browse and share files"},
	"gitea":          {"Gitea", "gitea", "The household's code and dotfiles"},
	"homebox":        {"Inventory", "homebox", "What we own and where it is"},
	"actual":         {"Budget", "actual-budget", "The household budget"},
	"podgrab":        {"Podcasts", "podgrab", "Podcasts, downloaded as they appear"},

	// The mail capability presents two surfaces, so it has two rows.
	// Davis has no dashboard-icons slug of its own, so it takes a
	// Material Design icon in the shape of the thing it serves rather
	// than a guess at a slug that would render as a broken image —
	// the same reasoning as homepageFallbackIcon below.
	// Both of these open on a login box, and the credential is the
	// person's own mailbox rather than anything Wildstone made — so the
	// description is where that gets said. Without it the household
	// meets a username and password field with no answer anywhere on
	// the page, tries their account password, and is told only
	// "invalid credentials" (ADR-W029 A9).
	"roundcube": {"Mail", "roundcube", "Your email — sign in with your address and your mail password"},
	"davis":     {"Calendars", "mdi-calendar-month", "Calendars and contacts on every phone — same login as Mail"},

	// The calendar capability. Radicale has no dashboard-icons slug of
	// its own, so it takes a Material Design icon in the shape of the
	// thing it serves rather than a guess at a slug that would render
	// as a broken image — the same reasoning as Davis above. The
	// description says which password to type, because this one is the
	// household sign-in and Davis's, one card away, is not.
	"radicale": {"Calendar", "mdi-calendar-blank", "The household's own calendars, on every phone — your household sign-in"},
}

// homepageFallbackIcon is what an app with no row gets. A generic
// Material Design icon rather than a guess at a dashboard-icons slug:
// guessing is right often enough to be tempting and wrong silently — a
// missing slug renders as a broken image, which looks worse than a plain
// one that was never claimed to be the app's own.
const homepageFallbackIcon = "mdi-application-outline"

// homepageGroups is the rendered group order. Quick actions first
// because they are the only cards a person can use without leaving the
// page (#197, #198); Household next because it is what the family opens
// the page for; Admin last because it is the operator's, and burying it
// is the point.
var homepageGroups = []string{portalGroupName, householdGroupName, "Admin"}

// householdGroupName is where an ordinary service lands, and where the
// empty state lands. Named rather than repeated since Quick actions
// took the first position: "the group at index 0" and "the group a
// household's services are in" stopped being the same string.
const householdGroupName = "Household"

// homepageGroup places a Route by its CLASSIFICATION, which is the field
// that already says who a surface is for and is mandatory on every Route
// (ADR-W009). Deriving the grouping from it means the dashboard cannot
// disagree with policy about which services are the operator's.
func homepageGroup(classification string) string {
	if classification == "admin_tailscale" {
		return "Admin"
	}
	return householdGroupName
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

// dashboardTitleEnv is the environment variable the dashboard package
// sets from its `household_name` param, and the ONLY way a person's own
// words reach a rendered file.
//
// Homepage ignores it — the title it uses comes from settings.yaml,
// which this renderer writes. The variable exists because a param is
// known only during template expansion, and the renderers run later
// against desired state; the container spec is the one carrier the
// dashboard package has that survives that gap. Reading it here keeps
// the household's name declared in exactly one place, next to the other
// dashboard params, rather than hard-coded in Go.
const dashboardTitleEnv = "WILDSTONE_DASHBOARD_TITLE"

// defaultDashboardTitle is what an install that never set the param
// gets. It is also what an operator gets who sets it to something
// unusable, which is deliberate: a dashboard with a plain title is
// working, a dashboard that fails to render is not.
const defaultDashboardTitle = "Household"

// maxDashboardTitle bounds the title. A page heading, not a paragraph.
const maxDashboardTitle = 64

// dashboardTitle is the household's own name for the page.
func dashboardTitle(desired []resource.Resource) string {
	for _, r := range dashboardContainers(desired) {
		if v, ok := r.Container.Env[dashboardTitleEnv]; ok {
			if clean := sanitizeTitle(v); clean != "" {
				return clean
			}
		}
	}
	return defaultDashboardTitle
}

// sanitizeTitle reduces operator free text to something safe to place in
// a YAML scalar and sane to render.
//
// Control characters and newlines are dropped rather than escaped: this
// value is written into a file a third party parses, and the narrow
// answer ("what a page heading may contain") is a much smaller thing to
// get right than YAML quoting in general. An unresolved secret reference
// is refused outright — the dashboard is family-facing, and a title is
// not a place anyone should be putting one.
func sanitizeTitle(v string) string {
	if strings.HasPrefix(v, "secret/") {
		return ""
	}
	var b strings.Builder
	for _, r := range v {
		if r == '\n' || r == '\r' || r == '\t' {
			b.WriteRune(' ')
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len([]rune(out)) > maxDashboardTitle {
		out = strings.TrimSpace(string([]rune(out)[:maxDashboardTitle]))
	}
	return out
}

// yamlQuote emits a double-quoted YAML scalar. Every other value this
// package writes is drawn from a validated charset (a resource name, a
// host, a constant); the title is the one that is genuinely free text.
func yamlQuote(v string) string {
	esc := strings.ReplaceAll(v, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	return `"` + esc + `"`
}

// dashboardContainers is every container the dashboard capability owns,
// in a deterministic order.
func dashboardContainers(desired []resource.Resource) []resource.Resource {
	var out []resource.Resource
	for _, r := range desired {
		if r.Kind != resource.KindContainer || r.Container == nil {
			continue
		}
		if strings.TrimPrefix(r.Metadata.Owner, "capability/") != "dashboard" {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
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
	for _, r := range dashboardContainers(desired) {
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
	// container and node are what the Route points at, and they are here
	// for the Wildstone dashboard profile rather than for the rendered
	// YAML: Homepage discovers whether an app answers by dialling it
	// itself (siteMonitor), and a page served by Keystone has to join the
	// observation Keystone already holds instead. An empty container
	// means the Route names an address rather than a container, which is
	// a service with no observation to join.
	container string
	node      string
	// widget is the live frame a household surface renders instead of
	// being only a link (#197, #198). Empty on every ordinary service,
	// which is all of them but two.
	widget *portalCard
}

// widgetSrc is the frame's address: the same origin the card links to,
// at the widget's own path. Derived from the entry rather than stored,
// so a Route that moves port moves both at once.
func (e homepageEntry) widgetSrc() string {
	if e.widget == nil {
		return ""
	}
	// The theme rides in the URL: the frame is drawn inside Homepage's
	// card and has no other way to learn which palette it is landing
	// in (homepageTheme says why it cannot be inferred).
	return strings.TrimSuffix(e.href, e.widget.page) + e.widget.widget + "?theme=" + homepageTheme
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
			if e.href != "" {
				fmt.Fprintf(&b, "        href: %s\n", e.href)
			}
			if e.desc != "" {
				fmt.Fprintf(&b, "        description: %s\n", e.desc)
			}
			fmt.Fprintf(&b, "        icon: %s\n", e.icon)
			if e.monitor != "" {
				fmt.Fprintf(&b, "        siteMonitor: %s\n", e.monitor)
			}
			if e.widget != nil {
				// Homepage's own iframe widget, configured and not
				// extended: src, a name, and a height. Nothing here
				// tells Homepage what the frame contains, because it
				// is not Homepage's business and because the surface
				// behind it authenticates the person itself.
				b.WriteString("        widget:\n")
				b.WriteString("          type: iframe\n")
				fmt.Fprintf(&b, "          name: %s\n", e.widget.name)
				fmt.Fprintf(&b, "          src: %s\n", e.widgetSrc())
				fmt.Fprintf(&b, "          classes: %s\n", e.widget.classes)
				b.WriteString("          referrerPolicy: no-referrer\n")
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
		if card, portal := portalCardFor(r.Metadata.Package); portal {
			// A household surface, not an application: the card's link
			// opens the full page and the frame shows the compact one.
			e.group = portalGroupName
			e.title, e.icon, e.desc = card.title, card.icon, card.description
			e.widget = &card
			e.href += card.page
			entries = append(entries, e)
			continue
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
		e.container, e.node = upstreamContainerName(r.Route.Upstream), r.Metadata.Node
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
	if len(entries) == 0 {
		return []homepageEntry{homepageEmptyState}
	}
	return entries
}

// homepageEmptyState is what a household with no routed services sees.
//
// Zero services is the NORMAL state of a correct fresh install, not an
// error: ingress, identity and the dashboard all go in before anything a
// family uses, and all three are excluded from this list by design. So
// the operator's reward for a clean install was a blank page with
// nothing to distinguish "working, nothing installed" from "broken".
//
// An ENTRY rather than a special case inside renderHomepageServices,
// because settings.yaml lays out the groups homepageEntries reports: a
// card the layout has never heard of falls back to Homepage's default
// list style, which is the disagreement between the two files this
// design exists to prevent.
//
// No href — there is nowhere to send a browser, and Homepage renders a
// card without one perfectly well. The command is the useful part.
var homepageEmptyState = homepageEntry{
	group: householdGroupName,
	title: "Nothing installed yet",
	icon:  homepageFallbackIcon,
	desc:  "Add a capability with `wild capability enable <name>` and it appears here",
	name:  "empty",
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

// IconFor is the dashboard-icons slug for an app, by the name of the
// container it runs in — the same lookup the dashboard renders from, so
// the Console cannot disagree with the tablet on the wall about what an
// app's logo is.
//
// It exists because the alternative is a second name-to-slug table in
// the Console, and a second copy of this map is a copy that drifts. The
// Go one is already covered: homepagecatalog_test fails the build when a
// shipped Route has no row here.
//
// An unknown container gets the same generic fallback the dashboard
// gives it, never a guess at a slug: a guessed slug is right often
// enough to be tempting and wrong silently.
func IconFor(container string) string {
	if container == "" {
		return homepageFallbackIcon
	}
	if app, known := homepageApps[container]; known {
		return app.icon
	}
	return homepageFallbackIcon
}

// IconForUpstream is IconFor for a Route's `upstream` field, which is
// where the container name actually lives ("container/<name>:<port>").
func IconForUpstream(upstream string) string {
	return IconFor(upstreamContainerName(upstream))
}
