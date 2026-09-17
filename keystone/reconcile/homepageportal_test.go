package reconcile

// The dashboard's side of #197 and #198: a household surface renders as
// a card with a live frame in it, at the address a browser can actually
// reach, laid out so a person can use it.

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// portalRoute is a Route as one of the household-surface packages
// declares it: served by Keystone (an address upstream, not a
// container), and carrying the package name the card table is keyed on.
func portalRoute(name, host, pkg string) resource.Resource {
	return resource.Resource{
		Kind: resource.KindRoute,
		Metadata: resource.Metadata{
			Name: name, Owner: "capability/" + name, Package: pkg, Node: "node-a",
		},
		Route: &resource.RouteSpec{
			Host: host, Upstream: "192.168.1.10:9441",
			Auth: "authelia_oidc", Classification: "family_private",
		},
	}
}

func portalDesired() []resource.Resource {
	return []resource.Resource{
		tailnetIngress("wildstone.tailnet.ts.net"),
		tailnetRoute("home", "home.example.com"),
		portalRoute("filedrop", "drop.example.com", "filedrop-wildstone"),
		portalRoute("assistant", "assistant.example.com", "assistant-wildstone"),
	}
}

// The whole integration, in the rendered file: a card per surface, a
// frame whose src is the widget, and a link to the full page.
func TestTheHouseholdSurfacesRenderAsFramedCards(t *testing.T) {
	got := string(renderHomepageServices(portalDesired()))

	for _, want := range []string{
		"- Quick actions:",
		"    - Assistant:",
		"        href: https://wildstone.tailnet.ts.net:50443/assistant/",
		"          src: https://wildstone.tailnet.ts.net:50443/assistant/widget",
		"    - Files:",
		"        href: https://wildstone.tailnet.ts.net:49443/filedrop/",
		"          src: https://wildstone.tailnet.ts.net:49443/filedrop/widget",
		"          type: iframe",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("services.yaml is missing %q:\n%s", want, got)
		}
	}
	// The frame's address and the card's link are the same origin. Two
	// origins here would mean one of them is wrong, and the wrong one
	// renders as an empty box with no error anywhere.
	if strings.Count(got, "https://wildstone.tailnet.ts.net:49443/") != 2 {
		t.Errorf("the Files card and its frame disagree about where the surface is:\n%s", got)
	}
}

// A household surface is not an application, and must not be listed as
// one: a second plain card for the same Route would mean two Files on
// the dashboard, one of which does nothing.
func TestAHouseholdSurfaceIsNotAlsoAPlainCard(t *testing.T) {
	got := string(renderHomepageServices(portalDesired()))
	if n := strings.Count(got, "    - Files:"); n != 1 {
		t.Errorf("the Files surface rendered %d cards:\n%s", n, got)
	}
	// Within the group itself: the rest of the page may legitimately
	// contain an unknown application, and this is about these two.
	quick := got[strings.Index(got, "- "+portalGroupName+":"):]
	if next := strings.Index(quick[1:], "\n- "); next >= 0 {
		quick = quick[:next+1]
	}
	if strings.Contains(quick, homepageFallbackIcon) {
		t.Errorf("a household surface fell through to the unknown-app fallback:\n%s", quick)
	}
}

// The frames get their own row width. Three across is a chat box nobody
// can type in on a tablet.
func TestTheQuickActionsGroupIsLaidOutWider(t *testing.T) {
	got := string(renderHomepageSettings(portalDesired()))
	if !strings.Contains(got, "  Quick actions:\n    style: row\n    columns: 2\n") {
		t.Errorf("the household surfaces are not laid out two across:\n%s", got)
	}
	if !strings.Contains(got, "  Household:\n    style: row\n    columns: 3\n") {
		t.Errorf("the ordinary services stopped being three across:\n%s", got)
	}
	// Quick actions first: they are the only cards on the page a person
	// can use without leaving it.
	if strings.Index(got, "Quick actions:") > strings.Index(got, "  Household:") {
		t.Errorf("the household surfaces are laid out below the applications:\n%s", got)
	}
}

// With neither surface installed the group does not exist at all — an
// empty heading is how a dashboard looks broken.
func TestNoHouseholdSurfaceMeansNoGroup(t *testing.T) {
	desired := []resource.Resource{
		tailnetIngress("wildstone.tailnet.ts.net"),
		tailnetRoute("home", "home.example.com"),
		tailnetRoute("jellyfin", "jellyfin.example.com"),
	}
	if got := string(renderHomepageServices(desired)); strings.Contains(got, "Quick actions") {
		t.Errorf("a deployment with no household surface still rendered the group:\n%s", got)
	}
	if got := string(renderHomepageSettings(desired)); strings.Contains(got, "Quick actions") {
		t.Errorf("the layout names a group the service list does not have:\n%s", got)
	}
}

// Every card in the table must be reachable from a package that ships,
// and every surface that ships must have a card. The two halves are
// declared in different files by different mechanisms, and the failure
// mode is a blank frame on the family's dashboard.
func TestEveryPortalCardNamesAShippedPackage(t *testing.T) {
	for pkg, card := range portalCards {
		if !strings.HasPrefix(card.widget, card.page) {
			t.Errorf("%s: the widget path %q is not under the page path %q, so the frame's address "+
				"cannot be derived from the card's link", pkg, card.widget, card.page)
		}
		if card.title == "" || card.icon == "" || card.description == "" {
			t.Errorf("%s: a card with no title, icon or description renders as a placeholder", pkg)
		}
	}
}
