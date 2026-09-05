package capability

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// .Resolved origins must describe where a Route is SERVED, not where it
// was declared (ADR-W019 A10). An OIDC issuer is a string Authelia
// compares byte for byte against what the browser saw, so a value
// derived from the declared host under a posture that does not serve it
// there produces a login loop — and nothing in the failure names the
// disagreement.

func identityProvider() *pkgspec.Package {
	return &pkgspec.Package{
		Metadata: pkgspec.Metadata{Name: "identity", Capability: "identity"},
		Provides: []pkgspec.Provide{{Trait: "identity", Mode: "oidc"}},
		Params:   []pkgspec.ParamDecl{{Name: "public_domain", Default: "example.com"}},
	}
}

func consumer() *pkgspec.Package {
	return &pkgspec.Package{Metadata: pkgspec.Metadata{Name: "photos", Capability: "photos"}}
}

func tailnetState() []resource.Resource {
	return []resource.Resource{
		{Kind: resource.KindIngress, Metadata: resource.Metadata{Name: "ingress", Node: "n1"},
			Ingress: &resource.IngressSpec{TLS: "tailscale", TailnetName: "wildstone.tailnet.ts.net"}},
		{Kind: resource.KindRoute, Metadata: resource.Metadata{Name: "auth", Node: "n1"},
			Route: &resource.RouteSpec{Host: "auth.example.com", Upstream: "container/authelia:9091", Auth: "portal", Classification: "family_public"}},
		{Kind: resource.KindRoute, Metadata: resource.Metadata{Name: "home", Node: "n1"},
			Route: &resource.RouteSpec{Host: "home.example.com", Upstream: "container/homepage:3000", Auth: "authelia_oidc", Classification: "family_public"}},
	}
}

func TestIssuerFollowsTheTailnetPosture(t *testing.T) {
	cat := NewCatalog(identityProvider()).WithCurrent(tailnetState())
	got := buildResolved(consumer(), map[string]string{}, cat)["IssuerURL"]
	want := "https://wildstone.tailnet.ts.net:7443"
	if got != want {
		t.Errorf("issuer must be where Authelia is actually served\n got %q\nwant %q", got, want)
	}
}

// The regression that matters most: every deployment that exists today
// uses a posture where a Route IS served at its declared host, and this
// change must be invisible to all of them.
func TestIssuerUnchangedWithoutTheTailnetPosture(t *testing.T) {
	for name, current := range map[string][]resource.Resource{
		"no state at all": nil,
		"internal CA": {{Kind: resource.KindIngress, Metadata: resource.Metadata{Name: "ingress", Node: "n1"},
			Ingress: &resource.IngressSpec{TLS: "internal"}}},
		"terminated upstream": {{Kind: resource.KindIngress, Metadata: resource.Metadata{Name: "ingress", Node: "n1"},
			Ingress: &resource.IngressSpec{TLS: "off"}}},
	} {
		cat := NewCatalog(identityProvider()).WithCurrent(current)
		if got := buildResolved(consumer(), map[string]string{}, cat)["IssuerURL"]; got != "https://auth.example.com" {
			t.Errorf("%s: issuer changed to %q", name, got)
		}
	}
}

// A catalog built the old way must still work: WithCurrent is additive,
// and every caller that does not know about addressing keeps behaving
// exactly as it did.
func TestCatalogWithoutCurrentStateStillResolves(t *testing.T) {
	if got := buildResolved(consumer(), map[string]string{}, NewCatalog(identityProvider()))["IssuerURL"]; got != "https://auth.example.com" {
		t.Errorf("plain catalog regressed: %q", got)
	}
}

// TailnetHost is always present so a template may test it without
// tripping missingkey=error, and empty when there is no tailnet.
func TestTailnetHostIsAlwaysDefined(t *testing.T) {
	got, ok := buildResolved(consumer(), map[string]string{}, NewCatalog(identityProvider()))["TailnetHost"]
	if !ok {
		t.Fatal("TailnetHost must exist even with no tailnet, or templates referencing it fail to render")
	}
	if got != "" {
		t.Errorf("want empty without a tailnet, got %q", got)
	}
	if got := buildResolved(consumer(), map[string]string{}, NewCatalog(identityProvider()).WithCurrent(tailnetState()))["TailnetHost"]; got != "wildstone.tailnet.ts.net" {
		t.Errorf("TailnetHost = %q", got)
	}
}

// Authelia's own configuration is browser-visible, and nothing in the
// system compares it against what is actually served — so a wrong value
// here passes every check and fails only at the moment a person tries to
// log in. These assert the three strings follow the posture.

func TestPortalConfigFollowsTheTailnetPosture(t *testing.T) {
	cat := NewCatalog(identityProvider()).WithCurrent(tailnetState())
	got := buildResolved(consumer(), map[string]string{}, cat)
	for key, want := range map[string]string{
		// Cookies ignore ports, so the bare host covers every service.
		"SessionDomain": "wildstone.tailnet.ts.net",
		"IssuerURL":     "https://wildstone.tailnet.ts.net:7443",
		"DashboardURL":  "https://wildstone.tailnet.ts.net:443",
	} {
		if got[key] != want {
			t.Errorf("%s\n got %q\nwant %q", key, got[key], want)
		}
	}
}

func TestPortalConfigUnchangedUnderHostnamePostures(t *testing.T) {
	for _, tls := range []string{"off", "internal"} {
		cat := NewCatalog(identityProvider()).WithCurrent([]resource.Resource{
			{Kind: resource.KindIngress, Metadata: resource.Metadata{Name: "ingress", Node: "n1"},
				Ingress: &resource.IngressSpec{TLS: tls}},
		})
		got := buildResolved(consumer(), map[string]string{}, cat)
		for key, want := range map[string]string{
			"SessionDomain": "example.com",
			"IssuerURL":     "https://auth.example.com",
			"DashboardURL":  "https://home.example.com",
		} {
			if got[key] != want {
				t.Errorf("posture %q: %s got %q, want %q", tls, key, got[key], want)
			}
		}
	}
}

// A cookie cannot be scoped to a public suffix, and ts.net is one. If
// SessionDomain ever returned a parent of the tailnet name, every login
// would silently fail to set a session.
func TestSessionDomainIsNeverAParentOfATsNetName(t *testing.T) {
	got := buildResolved(consumer(), map[string]string{}, NewCatalog(identityProvider()).WithCurrent(tailnetState()))["SessionDomain"]
	if got == "tailnet.ts.net" || got == "ts.net" {
		t.Fatalf("SessionDomain %q is a public suffix or its parent; browsers refuse such a cookie", got)
	}
}
