package reconcile

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func ownedRoute(name, owner, host, auth string) resource.Resource {
	r := routeRes(host, auth, "family_public")
	r.Metadata = resource.Metadata{Name: name, Owner: "capability/" + owner, Node: "node-a"}
	return r
}

// ownedRouteFull is ownedRoute with the two fields presentation turns
// on: the upstream (which names the app) and the classification (which
// decides the group).
func ownedRouteFull(name, owner, host, upstream, class string) resource.Resource {
	r := routeRes(host, "authelia_oidc", class)
	r.Route.Upstream = upstream
	r.Metadata = resource.Metadata{Name: name, Owner: "capability/" + owner, Node: "node-a"}
	return r
}

// dashboardContainer is what makes a status check renderable: the
// monitor address is only emitted for a Route on Homepage's own node.
func dashboardContainer(node string) resource.Resource {
	return resource.Resource{
		Kind:      resource.KindContainer,
		Metadata:  resource.Metadata{Name: "homepage", Owner: "capability/dashboard", Node: node},
		Container: &resource.ContainerSpec{Image: "example@sha256:abc"},
	}
}

// TestHomepageListsRoutedServices pins F55. The dashboard shipped
// Homepage's own sample file on a healthy install because nothing
// rendered a service list, while Keystone knew every Route all along.
func TestHomepageListsRoutedServices(t *testing.T) {
	got := string(renderHomepageServices([]resource.Resource{
		ownedRoute("photos", "photos", "photos.example.com", "app_oidc"),
		ownedRoute("uptime-kuma", "monitoring", "status.example.com", "authelia_oidc"),
	}))
	for _, want := range []string{
		"- Household:",
		"href: https://photos.example.com",
		"href: https://status.example.com",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// TestHomepageSkipsThePortalAndItself: a login page and the page being
// rendered are not services a person clicks.
func TestHomepageSkipsThePortalAndItself(t *testing.T) {
	got := string(renderHomepageServices([]resource.Resource{
		ownedRoute("auth", "identity", "auth.example.com", "portal"),
		ownedRoute("home", "dashboard", "home.example.com", "authelia_oidc"),
		ownedRoute("photos", "photos", "photos.example.com", "app_oidc"),
	}))
	for _, unwanted := range []string{"auth.example.com", "home.example.com"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("listed %s, which is not a service to click:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, "photos.example.com") {
		t.Errorf("dropped a real service:\n%s", got)
	}
}

// TestHomepageEmptyRendersGuidanceNotABlankPage.
//
// The file must never be absent — Homepage seeds its sample config into
// any file it does not find, which is the whole of F55. It must also not
// be blank, which is the F87 half: zero services is the NORMAL state of
// a correct fresh install (ingress, identity and the dashboard are all
// excluded from this list by design), so the operator's reward for a
// clean install was a page with nothing on it and no way to tell working
// from broken.
func TestHomepageEmptyRendersGuidanceNotABlankPage(t *testing.T) {
	got := string(renderHomepageServices(nil))
	if !strings.Contains(got, "- Household:") {
		t.Errorf("want a real group so the page is not blank:\n%s", got)
	}
	if !strings.Contains(got, "Nothing installed yet") {
		t.Errorf("want a card saying so:\n%s", got)
	}
	if !strings.Contains(got, "wild capability enable") {
		t.Errorf("want the command that fixes it:\n%s", got)
	}
	// Still a list Homepage will parse, so it never falls back to its own
	// sample services.
	if strings.Contains(got, "My First Service") {
		t.Errorf("rendered Homepage's sample content:\n%s", got)
	}
}

// TestHomepageOrderIsStable: a churning prereq replaces the container.
func TestHomepageOrderIsStable(t *testing.T) {
	a := renderHomepageServices([]resource.Resource{
		ownedRoute("zeta", "z", "z.example.com", "app_oidc"),
		ownedRoute("alpha", "a", "a.example.com", "app_oidc"),
	})
	b := renderHomepageServices([]resource.Resource{
		ownedRoute("alpha", "a", "a.example.com", "app_oidc"),
		ownedRoute("zeta", "z", "z.example.com", "app_oidc"),
	})
	if string(a) != string(b) {
		t.Errorf("render is order-dependent:\n%s\n---\n%s", a, b)
	}
}

// ---------------------------------------------------------------- F87

// TestHomepagePresentsAKnownApp pins the whole entry, in order. The
// alpha VM showed a bare link with the capability slug under it; every
// line below is one of the things that were missing.
func TestHomepagePresentsAKnownApp(t *testing.T) {
	got := string(renderHomepageServices([]resource.Resource{
		dashboardContainer("node-a"),
		ownedRouteFull("photos", "photos", "photos.example.com", "container/immich-server:2283", "family_public"),
	}))
	want := "    - Photos:\n" +
		"        href: https://photos.example.com\n" +
		"        description: Every photo and video, searchable\n" +
		"        icon: immich\n" +
		"        siteMonitor: http://immich-server:2283\n"
	if !strings.Contains(got, want) {
		t.Errorf("want entry:\n%s\ngot:\n%s", want, got)
	}
}

// TestHomepageUnknownAppStillRendersFully: an app with no row in
// homepageApps gets a complete card, not a broken one. A guessed
// dashboard-icons slug would render as a broken image.
func TestHomepageUnknownAppStillRendersFully(t *testing.T) {
	got := string(renderHomepageServices([]resource.Resource{
		dashboardContainer("node-a"),
		ownedRouteFull("wiki", "knowledge", "wiki.example.com", "container/wiki-js:3000", "family_public"),
	}))
	for _, want := range []string{
		"    - Wiki:",
		"        description: Knowledge",
		"        icon: " + homepageFallbackIcon,
		"        siteMonitor: http://wiki-js:3000",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// TestHomepageGroupsByClassification: admin surfaces belong under Admin,
// derived from the field policy already uses to say who a surface is
// for, and Household is rendered first.
func TestHomepageGroupsByClassification(t *testing.T) {
	got := string(renderHomepageServices([]resource.Resource{
		ownedRouteFull("uptime", "monitoring", "uptime.example.com", "container/uptime-kuma:3001", "admin_tailscale"),
		ownedRouteFull("jellyfin", "media", "media.example.com", "container/jellyfin:8096", "family_public"),
	}))
	house, admin := strings.Index(got, "- Household:"), strings.Index(got, "- Admin:")
	if house < 0 || admin < 0 {
		t.Fatalf("want both groups:\n%s", got)
	}
	if house > admin {
		t.Errorf("Household must render before Admin:\n%s", got)
	}
	if !strings.Contains(got[admin:], "Uptime Kuma") {
		t.Errorf("the admin service is not under Admin:\n%s", got)
	}
	if !strings.Contains(got[house:admin], "Jellyfin") {
		t.Errorf("the family service is not under Household:\n%s", got)
	}
}

// TestHomepageEmptyGroupIsNotRendered: a heading with nothing under it
// makes a working dashboard look broken.
func TestHomepageEmptyGroupIsNotRendered(t *testing.T) {
	got := string(renderHomepageServices([]resource.Resource{
		ownedRouteFull("jellyfin", "media", "media.example.com", "container/jellyfin:8096", "family_public"),
	}))
	if strings.Contains(got, "- Admin:") {
		t.Errorf("opened a group with no services in it:\n%s", got)
	}
}

// TestHomepageMonitorsTheContainerNotTheBrowserURL. Two reasons the
// https:// link is the wrong probe target, and both make the dot lie:
// under tls internal the household CA is not trusted inside the Homepage
// container, and a forward-auth Route answers a login redirect that says
// nothing about the app.
func TestHomepageMonitorsTheContainerNotTheBrowserURL(t *testing.T) {
	got := string(renderHomepageServices([]resource.Resource{
		dashboardContainer("node-a"),
		ownedRouteFull("jellyfin", "media", "media.example.com", "container/jellyfin:8096", "family_public"),
	}))
	if !strings.Contains(got, "siteMonitor: http://jellyfin:8096") {
		t.Errorf("want the container address as the probe target:\n%s", got)
	}
	if strings.Contains(got, "siteMonitor: https://") {
		t.Errorf("probed the browser-facing URL:\n%s", got)
	}
}

// TestHomepageDoesNotMonitorAnotherNode: a container name resolves on
// its own node's Docker network and nowhere else, so the alternative to
// no dot is a permanently red one.
func TestHomepageDoesNotMonitorAnotherNode(t *testing.T) {
	elsewhere := ownedRouteFull("jellyfin", "media", "media.example.com", "container/jellyfin:8096", "family_public")
	elsewhere.Metadata.Node = "node-b"
	got := string(renderHomepageServices([]resource.Resource{dashboardContainer("node-a"), elsewhere}))
	if strings.Contains(got, "siteMonitor") {
		t.Errorf("monitored a container on another node:\n%s", got)
	}
	if !strings.Contains(got, "href: https://media.example.com") {
		t.Errorf("the service itself must still be listed:\n%s", got)
	}
}

// TestHomepageDoesNotMonitorWithoutADashboard: with no Homepage
// container in desired state there is nothing to do the dialing.
func TestHomepageDoesNotMonitorWithoutADashboard(t *testing.T) {
	got := string(renderHomepageServices([]resource.Resource{
		ownedRouteFull("jellyfin", "media", "media.example.com", "container/jellyfin:8096", "family_public"),
	}))
	if strings.Contains(got, "siteMonitor") {
		t.Errorf("emitted a probe with no Homepage to run it:\n%s", got)
	}
}

// TestHomepageDoesNotMonitorANonContainerUpstream: an "ip:port" Route
// points at something Wildstone does not run and cannot name.
func TestHomepageDoesNotMonitorANonContainerUpstream(t *testing.T) {
	got := string(renderHomepageServices([]resource.Resource{
		dashboardContainer("node-a"),
		ownedRouteFull("printer", "hardware", "printer.example.com", "192.0.2.10:631", "family_public"),
	}))
	if strings.Contains(got, "siteMonitor") {
		t.Errorf("probed a non-container upstream:\n%s", got)
	}
}

// hostHub is a host-networked Container and its "host/<port>" Route, from
// one package on node-a (#433).
func hostHub(pkg, container string) []resource.Resource {
	route := ownedRouteFull("home", "home-automation", "home.example.com", "host/8123", "family_public")
	route.Metadata.Package = pkg
	return []resource.Resource{route, {
		Kind:      resource.KindContainer,
		Metadata:  resource.Metadata{Name: container, Owner: "capability/home-automation", Node: "node-a", Package: pkg},
		Container: &resource.ContainerSpec{Image: "example@sha256:abc", NetworkMode: resource.NetworkModeHost},
	}}
}

// TestHomepagePresentsAHostServiceAsTheKnownApp: a host Route names no
// container, but its package runs exactly one host-networked container,
// and that is the app — its row, its icon, and a status dot dialled the
// way Caddy dials it, never an anonymous address card.
func TestHomepagePresentsAHostServiceAsTheKnownApp(t *testing.T) {
	desired := append([]resource.Resource{dashboardContainer("node-a")}, hostHub("home-assistant", "home-assistant")...)
	got := string(renderHomepageServices(desired))
	want := "    - Home Assistant:\n" +
		"        href: https://home.example.com\n" +
		"        description: Lights, sensors and automations\n" +
		"        icon: home-assistant\n" +
		"        siteMonitor: http://" + resource.HostGatewayAlias + ":8123\n"
	if !strings.Contains(got, want) {
		t.Errorf("want entry:\n%s\ngot:\n%s", want, got)
	}
	for _, e := range homepageEntries(desired) {
		if e.name == "home" && e.container != "home-assistant" {
			t.Errorf("entry container = %q, want the host-networked container", e.container)
		}
	}
}

// TestHomepageDoesNotGuessBetweenTwoHostContainers: with two candidates
// the port is the only thing saying which, so the card stays unbranded
// rather than carrying the wrong app's name.
func TestHomepageDoesNotGuessBetweenTwoHostContainers(t *testing.T) {
	desired := append([]resource.Resource{dashboardContainer("node-a")}, hostHub("hub", "home-assistant")...)
	desired = append(desired, resource.Resource{
		Kind:      resource.KindContainer,
		Metadata:  resource.Metadata{Name: "matter-server", Owner: "capability/home-automation", Node: "node-a", Package: "hub"},
		Container: &resource.ContainerSpec{Image: "example@sha256:abc", NetworkMode: resource.NetworkModeHost},
	})
	got := string(renderHomepageServices(desired))
	if strings.Contains(got, "Home Assistant") {
		t.Errorf("guessed which host container the Route serves:\n%s", got)
	}
	if !strings.Contains(got, "siteMonitor: http://"+resource.HostGatewayAlias+":8123") {
		t.Errorf("the host service is still dialled at its alias:\n%s", got)
	}
}

// TestHomepageDisambiguatesDuplicateTitles. A title is a YAML KEY: two
// Routes to the same app collapse into one card and a service vanishes
// from the dashboard silently. Resource names are unique per kind, so
// qualifying with the name always separates them.
func TestHomepageDisambiguatesDuplicateTitles(t *testing.T) {
	got := string(renderHomepageServices([]resource.Resource{
		ownedRouteFull("jellyfin", "media", "media.example.com", "container/jellyfin:8096", "family_public"),
		ownedRouteFull("jellyfin-lan", "media", "lan.example.com", "container/jellyfin:8096", "family_public"),
	}))
	for _, want := range []string{"- Jellyfin (jellyfin):", "- Jellyfin (jellyfin-lan):"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q — a collapsed key loses a service:\n%s", want, got)
		}
	}
	if strings.Count(got, "href:") != 2 {
		t.Errorf("want two entries, got:\n%s", got)
	}
}

// TestHomepageTitleComesFromTheHousehold. The name is a param on the
// dashboard package, carried through the container spec because a param
// is known only at expansion and this renderer runs later, against
// desired state.
func TestHomepageTitleComesFromTheHousehold(t *testing.T) {
	c := dashboardContainer("node-a")
	c.Container.Env = map[string]string{dashboardTitleEnv: "The Reed Family"}
	got := string(renderHomepageSettings([]resource.Resource{c}))
	if !strings.Contains(got, `title: "The Reed Family"`) {
		t.Errorf("want the household's own name:\n%s", got)
	}
}

// TestHomepageTitleFallsBackToADefault: a dashboard with a plain title
// is working, one that fails to render is not. Every unusable value
// takes the default rather than the page.
func TestHomepageTitleFallsBackToADefault(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unset":    nil,
		"empty":    {dashboardTitleEnv: "   "},
		"a secret": {dashboardTitleEnv: "secret/household-name"},
		"controls": {dashboardTitleEnv: "\x00\x01"},
		"newlines": {dashboardTitleEnv: "\n\n"},
	} {
		c := dashboardContainer("node-a")
		c.Container.Env = env
		got := string(renderHomepageSettings([]resource.Resource{c}))
		if !strings.Contains(got, `title: "Household"`) {
			t.Errorf("%s: want the default title:\n%s", name, got)
		}
	}
}

// TestHomepageTitleIsSafeInYAML. The title is the one genuinely free-text
// value this package writes into a file a third party parses: everything
// else is a validated resource name, a host, or a constant.
func TestHomepageTitleIsSafeInYAML(t *testing.T) {
	c := dashboardContainer("node-a")
	c.Container.Env = map[string]string{dashboardTitleEnv: `Bob: "the" builder` + "\nstatusStyle: basic"}
	got := string(renderHomepageSettings([]resource.Resource{c}))
	// Parsing is the assertion, not a substring search: the injected text
	// legitimately APPEARS in the file, safely inside a quoted scalar.
	// What matters is that YAML reads it as part of the title and the
	// real statusStyle key is untouched.
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("title made settings.yaml unparsable: %v\n%s", err, got)
	}
	if doc["title"] != `Bob: "the" builder statusStyle: basic` {
		t.Errorf("title round-trip = %q", doc["title"])
	}
	if doc["statusStyle"] != "dot" {
		t.Errorf("statusStyle = %v, want dot", doc["statusStyle"])
	}
}

// TestHomepageTitleIsBounded: a page heading, not a paragraph.
func TestHomepageTitleIsBounded(t *testing.T) {
	c := dashboardContainer("node-a")
	c.Container.Env = map[string]string{dashboardTitleEnv: strings.Repeat("x", 500)}
	got := string(renderHomepageSettings([]resource.Resource{c}))
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatal(err)
	}
	if n := len(doc["title"].(string)); n > maxDashboardTitle {
		t.Errorf("title is %d runes, want <= %d", n, maxDashboardTitle)
	}
}
