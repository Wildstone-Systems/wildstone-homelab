package reconcile

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func tailnetRoute(name, host string) resource.Resource {
	return resource.Resource{
		Kind:     resource.KindRoute,
		Metadata: resource.Metadata{Name: name, Owner: "capability/" + name, Node: "node-a"},
		Route:    &resource.RouteSpec{Host: host, Upstream: "container/" + name + ":80", Auth: "authelia_oidc", Classification: "family_public"},
	}
}

func tailnetIngress(name string) resource.Resource {
	r := ingressRes("tailscale")
	r.Ingress.TailnetName = name
	return r
}

func fourRoutes() []resource.Resource {
	return []resource.Resource{
		tailnetRoute("home", "home.example.com"),
		tailnetRoute("auth", "auth.example.com"),
		tailnetRoute("jellyfin", "jellyfin.example.com"),
		tailnetRoute("photos", "photos.example.com"),
	}
}

// TestTailnetPutsTheDashboardOn443: a family opens one address, and it
// should be the one with no port in it.
func TestTailnetPutsTheDashboardOn443(t *testing.T) {
	desired := append(fourRoutes(), tailnetIngress("wildstone.tailnet.ts.net"))
	p := postureOf(desired)
	if got := routeSiteAddress(p, "home.example.com"); got != "wildstone.tailnet.ts.net:443" {
		t.Errorf("dashboard should hold 443, got %q", got)
	}
}

// TestTailnetGivesEveryRouteADistinctPort. Sharing one would route two
// services to whichever site Caddy matched first — a silent failure that
// looks like the right page for one household member and the wrong one
// for another.
func TestTailnetGivesEveryRouteADistinctPort(t *testing.T) {
	desired := append(fourRoutes(), tailnetIngress("wildstone.tailnet.ts.net"))
	p := postureOf(desired)
	seen := map[string]string{}
	for _, h := range []string{"home.example.com", "auth.example.com", "jellyfin.example.com", "photos.example.com"} {
		addr := routeSiteAddress(p, h)
		if prev, dup := seen[addr]; dup {
			t.Fatalf("%s and %s both resolve to %s", prev, h, addr)
		}
		seen[addr] = h
	}
}

// TestInstallingAServiceDoesNotMoveTheOthers is the bookmark test. Ports
// assigned purely by sorted position would shift every service after any
// alphabetically-earlier install — a whole household's saved links broken
// by adding something unrelated.
func TestInstallingAServiceDoesNotMoveTheOthers(t *testing.T) {
	before := postureOf(append(fourRoutes(), tailnetIngress("wildstone.tailnet.ts.net")))
	// "aaa" sorts before every existing host.
	withNew := append(fourRoutes(), tailnetRoute("aaa", "aaa.example.com"), tailnetIngress("wildstone.tailnet.ts.net"))
	after := postureOf(withNew)
	for _, h := range []string{"home.example.com", "auth.example.com", "jellyfin.example.com", "photos.example.com"} {
		if routeSiteAddress(before, h) != routeSiteAddress(after, h) {
			t.Errorf("%s moved from %s to %s when an unrelated service was installed",
				h, routeSiteAddress(before, h), routeSiteAddress(after, h))
		}
	}
}

// TestPortAssignmentIsDeterministic: the reconciler decides NoOp vs
// Update by comparing a live file's bytes against a fresh render, so an
// assignment that varied between renders of identical desired state
// would rewrite the config forever.
func TestPortAssignmentIsDeterministic(t *testing.T) {
	desired := append(fourRoutes(), tailnetIngress("wildstone.tailnet.ts.net"))
	first := string(renderRouteFragment(desired[0].Route, "authelia:9091", postureOf(desired)))
	for i := 0; i < 20; i++ {
		if got := string(renderRouteFragment(desired[0].Route, "authelia:9091", postureOf(desired))); got != first {
			t.Fatalf("render %d differs:\n%s\nvs\n%s", i, got, first)
		}
	}
}

// TestOtherPosturesKeepTheirHostnames: the tailnet addressing must not
// leak into a deployment that has real hostnames. This is the regression
// that would silently move an existing install onto ports.
func TestOtherPosturesKeepTheirHostnames(t *testing.T) {
	for _, tls := range []string{"off", "internal"} {
		p := postureOf(append(fourRoutes(), ingressRes(tls)))
		if got := routeSiteAddress(p, "home.example.com"); got != "home.example.com" {
			t.Errorf("posture %q rewrote the host to %q", tls, got)
		}
	}
}

// TestHomepageLinksMatchWhatCaddyServes. A dashboard whose links
// disagree with the proxy is worse than no dashboard: every link looks
// deliberate, so the failure reads as the service being broken.
func TestHomepageLinksMatchWhatCaddyServes(t *testing.T) {
	desired := append(fourRoutes(), tailnetIngress("wildstone.tailnet.ts.net"))
	page := string(renderHomepageServices(desired))
	p := postureOf(desired)
	// jellyfin is listed (home is the dashboard's own Route, excluded).
	want := "https://" + routeSiteAddress(p, "jellyfin.example.com")
	if !strings.Contains(page, want) {
		t.Errorf("dashboard must link %s:\n%s", want, page)
	}
	if strings.Contains(page, "https://jellyfin.example.com\n") {
		t.Errorf("dashboard linked the logical host, which nothing serves:\n%s", page)
	}
}

// TestTailnetPostureAddsNoCertificateConfiguration. Caddy fetches
// *.ts.net certificates from tailscaled with no configuration; saying it
// again in the Caddyfile would be a second place to get it wrong.
func TestTailnetPostureAddsNoCertificateConfiguration(t *testing.T) {
	base := string(renderBaseCaddyfile(postureOf([]resource.Resource{tailnetIngress("wildstone.tailnet.ts.net")})))
	for _, unwanted := range []string{"local_certs", "acme_dns", "auto_https off"} {
		if strings.Contains(base, unwanted) {
			t.Errorf("tailnet posture must leave automatic HTTPS alone, found %q:\n%s", unwanted, base)
		}
	}
}

// containerPublishing is a container in desired state that already holds
// a host port — the situation F63 records.
func containerPublishing(name, owner, portMapping string) resource.Resource {
	return resource.Resource{
		Kind:     resource.KindContainer,
		Metadata: resource.Metadata{Name: name, Owner: owner, Node: "node-a"},
		Container: &resource.ContainerSpec{
			Image: "ghcr.io/example/app@sha256:" + strings.Repeat("a", 64),
			Ports: []string{portMapping},
		},
	}
}

// TestAssignmentAvoidsPortsOtherContainersPublish is F63. Assigning a
// Route a port something else already holds produces a service that is
// unreachable while every resource reports healthy — the plan applies,
// the container starts, and only that one binding silently failed.
func TestAssignmentAvoidsPortsOtherContainersPublish(t *testing.T) {
	desired := append(fourRoutes(),
		tailnetIngress("wildstone.tailnet.ts.net"),
		containerPublishing("some-other-stack", "capability/monitoring", "0.0.0.0:9443:9443"))
	p := postureOf(desired)
	// 9443 is photos' pinned port, and it is taken.
	if got := routeSiteAddress(p, "photos.example.com"); strings.HasSuffix(got, ":9443") {
		t.Errorf("photos was assigned a port another container publishes: %s", got)
	}
	// It must still get a working port rather than none at all.
	if got := routeSiteAddress(p, "photos.example.com"); got == "photos.example.com" {
		t.Error("photos was left unserved instead of moved to a free port")
	}
}

// The ingress proxy publishes the whole block itself, so counting its own
// publications as conflicts would rule out every port there is.
func TestIngressOwnPortsAreNotConflicts(t *testing.T) {
	desired := append(fourRoutes(),
		tailnetIngress("wildstone.tailnet.ts.net"),
		containerPublishing("caddy-tailnet-ts", "capability/ingress", "0.0.0.0:443:443"))
	p := postureOf(desired)
	if got := routeSiteAddress(p, "home.example.com"); got != "wildstone.tailnet.ts.net:443" {
		t.Errorf("the dashboard lost 443 to the ingress container's own publication: %s", got)
	}
}

// A pin is a preference, not a guarantee: a stable address nothing can
// bind is worse than an unfamiliar one that works.
func TestPinnedPortYieldsWhenTaken(t *testing.T) {
	desired := append(fourRoutes(),
		tailnetIngress("wildstone.tailnet.ts.net"),
		containerPublishing("squatter", "capability/monitoring", "0.0.0.0:443:443"))
	p := postureOf(desired)
	got := routeSiteAddress(p, "home.example.com")
	if got == "wildstone.tailnet.ts.net:443" {
		t.Error("the dashboard kept a pinned port another container publishes")
	}
	if got == "home.example.com" {
		t.Error("the dashboard was left unserved rather than moved")
	}
}
