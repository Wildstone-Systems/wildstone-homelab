package reconcile

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func routeRes(host, auth, classification string) resource.Resource {
	return resource.Resource{
		Kind:  resource.KindRoute,
		Route: &resource.RouteSpec{Host: host, Upstream: "container/x:80", Auth: auth, Classification: classification},
	}
}

// TestAccessRulesComeFromRoutes pins the half that was missing: the
// identity package sets default_policy deny and says "Route fragments are
// what grant access", and nothing generated a rule — so every
// forward-auth'd Route answered 403 to everyone, forever.
func TestAccessRulesComeFromRoutes(t *testing.T) {
	got := string(renderAutheliaAccess([]resource.Resource{
		routeRes("home.example.com", "authelia_oidc", "family_public"),
		routeRes("auth.example.com", "portal", "family_public"),
	}))
	for _, want := range []string{
		"default_policy: deny",
		"- domain: auth.example.com\n      policy: bypass",
		"- domain: home.example.com\n      policy: one_factor",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("access file missing %q:\n%s", want, got)
		}
	}
}

// TestAccessSkipsRoutesAuthelisNeverSees asserts the rule set stays
// minimal. forward_auth is emitted only for authelia_oidc, so a rule for
// any other Auth would name a host Authelia is never consulted about —
// dead config that reads as policy.
func TestAccessSkipsRoutesAutheliaNeverSees(t *testing.T) {
	got := string(renderAutheliaAccess([]resource.Resource{
		routeRes("photos.example.com", "app_oidc", "family_public"),
		routeRes("media.example.com", "app_ldap", "family_public"),
		routeRes("open.example.com", "none", "family_public"),
	}))
	if strings.Contains(got, "example.com") {
		t.Errorf("rules named a host Authelia never decides:\n%s", got)
	}
}

// TestUnknownClassificationGetsTheStrongestPolicy pins the closed
// mapping. A classification nobody has considered must not inherit the
// weakest policy by falling through.
func TestUnknownClassificationGetsTheStrongestPolicy(t *testing.T) {
	got := string(renderAutheliaAccess([]resource.Resource{
		routeRes("admin.example.com", "authelia_oidc", "something_nobody_planned_for"),
	}))
	if !strings.Contains(got, "policy: two_factor") {
		t.Errorf("unknown classification did not get two_factor:\n%s", got)
	}
}

// TestNoRoutesStillDenies: an absent access_control hands the decision to
// whatever Authelia's built-in default happens to be. The file is always
// a valid deny-everything block instead.
func TestNoRoutesStillDenies(t *testing.T) {
	got := string(renderAutheliaAccess(nil))
	if !strings.Contains(got, "default_policy: deny") || !strings.Contains(got, "rules: []") {
		t.Errorf("empty desired state did not render a deny-all block:\n%s", got)
	}
}

// TestAccessOrderIsStable: an unstable order rewrites the file, and a
// rewritten prereq restarts Authelia, on ticks where nothing changed.
func TestAccessOrderIsStable(t *testing.T) {
	a := renderAutheliaAccess([]resource.Resource{
		routeRes("zeta.example.com", "authelia_oidc", "family_public"),
		routeRes("alpha.example.com", "authelia_oidc", "family_public"),
	})
	b := renderAutheliaAccess([]resource.Resource{
		routeRes("alpha.example.com", "authelia_oidc", "family_public"),
		routeRes("zeta.example.com", "authelia_oidc", "family_public"),
	})
	if string(a) != string(b) {
		t.Errorf("render is order-dependent:\n%s\n---\n%s", a, b)
	}
}
