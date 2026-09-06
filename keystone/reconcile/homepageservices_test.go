package reconcile

import (
	"strings"
	"testing"

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

// TestHomepageEmptyIsAValidList, not an absent file: Homepage seeds its
// sample config into any file it does not find, which is the whole bug.
func TestHomepageEmptyIsAValidList(t *testing.T) {
	got := string(renderHomepageServices(nil))
	if !strings.Contains(got, "[]") {
		t.Errorf("want an empty YAML list, got:\n%s", got)
	}
	if strings.Contains(got, "Household") {
		t.Errorf("empty render should not open a group:\n%s", got)
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
