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
		"    - Photos:\n        href: https://photos.example.com\n        description: photos",
		"    - Uptime Kuma:\n        href: https://status.example.com",
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
