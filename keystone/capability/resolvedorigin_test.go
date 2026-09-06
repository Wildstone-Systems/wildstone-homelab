package capability

import (
	"strings"
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

// identityProvider is the shipped identity package reduced to the part
// these tests are about: the exports it hands its consumers.
//
// The three expressions are the ones packages/identity/package.yaml
// actually ships, and TestShippedIdentityPackageStillExportsThePortal
// (exports_test.go) loads the real file and asserts it resolves to the
// same values — so this fixture cannot quietly drift into testing a
// mechanism the catalog no longer uses.
func identityProvider() *pkgspec.Package {
	return &pkgspec.Package{
		Metadata: pkgspec.Metadata{Name: "identity", Capability: "identity"},
		Provides: []pkgspec.Provide{{Trait: "identity", Mode: "oidc", Exports: map[string]string{
			"IssuerURL":     `{{ with .Params.public_domain }}{{ origin (printf "auth.%s" .) }}{{ end }}`,
			"SessionDomain": `{{ with .Params.public_domain }}{{ sessionDomain . }}{{ end }}`,
			"DashboardURL":  `{{ with .Params.public_domain }}{{ origin (printf "home.%s" .) }}{{ end }}`,
		}}},
		Params: []pkgspec.ParamDecl{{Name: "public_domain", Default: "example.com"}},
	}
}

// resolve is buildResolved with its error asserted away. An export that
// fails to render is a package bug, not an expected outcome, so every
// test here wants it to stop the run rather than to silently yield a map
// with a key missing.
func resolve(t *testing.T, pkg *pkgspec.Package, params map[string]string, cat *Catalog) map[string]string {
	t.Helper()
	got, err := buildResolved(pkg, params, cat)
	if err != nil {
		t.Fatalf("buildResolved: %v", err)
	}
	return got
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
	got := resolve(t, consumer(), map[string]string{}, cat)["IssuerURL"]
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
		if got := resolve(t, consumer(), map[string]string{}, cat)["IssuerURL"]; got != "https://auth.example.com" {
			t.Errorf("%s: issuer changed to %q", name, got)
		}
	}
}

// A catalog built the old way must still work: WithCurrent is additive,
// and every caller that does not know about addressing keeps behaving
// exactly as it did.
func TestCatalogWithoutCurrentStateStillResolves(t *testing.T) {
	if got := resolve(t, consumer(), map[string]string{}, NewCatalog(identityProvider()))["IssuerURL"]; got != "https://auth.example.com" {
		t.Errorf("plain catalog regressed: %q", got)
	}
}

// TailnetHost is always present so a template may test it without
// tripping missingkey=error, and empty when there is no tailnet.
func TestTailnetHostIsAlwaysDefined(t *testing.T) {
	got, ok := resolve(t, consumer(), map[string]string{}, NewCatalog(identityProvider()))["TailnetHost"]
	if !ok {
		t.Fatal("TailnetHost must exist even with no tailnet, or templates referencing it fail to render")
	}
	if got != "" {
		t.Errorf("want empty without a tailnet, got %q", got)
	}
	if got := resolve(t, consumer(), map[string]string{}, NewCatalog(identityProvider()).WithCurrent(tailnetState()))["TailnetHost"]; got != "wildstone.tailnet.ts.net" {
		t.Errorf("TailnetHost = %q", got)
	}
}

// Authelia's own configuration is browser-visible, and nothing in the
// system compares it against what is actually served — so a wrong value
// here passes every check and fails only at the moment a person tries to
// log in. These assert the three strings follow the posture.

func TestPortalConfigFollowsTheTailnetPosture(t *testing.T) {
	cat := NewCatalog(identityProvider()).WithCurrent(tailnetState())
	got := resolve(t, consumer(), map[string]string{}, cat)
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
		got := resolve(t, consumer(), map[string]string{}, cat)
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
	got := resolve(t, consumer(), map[string]string{}, NewCatalog(identityProvider()).WithCurrent(tailnetState()))["SessionDomain"]
	if got == "tailnet.ts.net" || got == "ts.net" {
		t.Fatalf("SessionDomain %q is a public suffix or its parent; browsers refuse such a cookie", got)
	}
}

// TestOriginsResolveBeforeTheirRoutesExist is the bug the first clean
// install of v0.1.8-alpha found, and it is a sequencing problem rather
// than an arithmetic one.
//
// .Resolved feeds the templates that DECLARE a capability's Routes, so
// it necessarily runs before those Routes are in desired state. Port
// assignment reads desired state. At the one moment these values are
// computed — installing the capability — there is nothing to read.
//
// The old fallback returned the declared host, which is not merely
// imprecise: Authelia refuses to start when authelia_url does not share
// a cookie scope with its session domain, so the plan applied and then
// the container crash-looped, with the error naming a cookie scope and
// nothing naming the posture.
func TestOriginsResolveBeforeTheirRoutesExist(t *testing.T) {
	// Desired state has the ingress posture and NO Routes — exactly the
	// state during the identity capability's own installation.
	current := []resource.Resource{
		{Kind: resource.KindIngress, Metadata: resource.Metadata{Name: "ingress", Node: "n1"},
			Ingress: &resource.IngressSpec{TLS: "tailscale", TailnetName: "wildstone.tailnet.ts.net"}},
	}
	got := resolve(t, consumer(), map[string]string{}, NewCatalog(identityProvider()).WithCurrent(current))
	for key, want := range map[string]string{
		"IssuerURL":     "https://wildstone.tailnet.ts.net:7443",
		"DashboardURL":  "https://wildstone.tailnet.ts.net:443",
		"SessionDomain": "wildstone.tailnet.ts.net",
	} {
		if got[key] != want {
			t.Errorf("%s with no Routes yet\n got %q\nwant %q", key, got[key], want)
		}
	}
}

// Authelia's specific rule, asserted directly: the portal URL must sit
// inside the session cookie's scope. Under this posture that means the
// same host, which is what makes ports work at all — cookies ignore
// ports.
func TestPortalURLSharesTheSessionCookieScope(t *testing.T) {
	got := resolve(t, consumer(), map[string]string{}, NewCatalog(identityProvider()).WithCurrent(tailnetState()))
	domain, issuer := got["SessionDomain"], got["IssuerURL"]
	if !strings.Contains(issuer, "://"+domain) {
		t.Errorf("Authelia refuses to start unless authelia_url is within the session domain\n  domain=%q\n  issuer=%q", domain, issuer)
	}
}

// TestAbsentRouteHonoursOccupiedPorts is F70, raised by automated review
// on the F68 fix and confirmed to be real.
//
// The two halves of addressing must agree. assign() treats a pinned port
// as a preference and moves a Route when something else publishes it
// (F63). If the pre-Route fallback returned the pin unconditionally,
// then installing identity while another container held 7443 would give
// Authelia an issuer on 7443, and the Route — once it existed — would be
// served somewhere else. Both halves internally consistent, disagreeing
// with each other, and the symptom is a login that fails.
func TestAbsentRouteHonoursOccupiedPorts(t *testing.T) {
	current := []resource.Resource{
		{Kind: resource.KindIngress, Metadata: resource.Metadata{Name: "ingress", Node: "n1"},
			Ingress: &resource.IngressSpec{TLS: "tailscale", TailnetName: "wildstone.tailnet.ts.net"}},
		// Something unrelated already holds auth's pinned port.
		{Kind: resource.KindContainer, Metadata: resource.Metadata{Name: "squatter", Owner: "capability/monitoring", Node: "n1"},
			Container: &resource.ContainerSpec{
				Image: "ghcr.io/example/app@sha256:" + strings.Repeat("a", 64),
				Ports: []string{"0.0.0.0:7443:7443"},
			}},
	}
	issuer := resolve(t, consumer(), map[string]string{}, NewCatalog(identityProvider()).WithCurrent(current))["IssuerURL"]
	if strings.HasSuffix(issuer, ":7443") {
		t.Errorf("issuer took a port another container publishes: %s", issuer)
	}
	if !strings.HasPrefix(issuer, "https://wildstone.tailnet.ts.net:") {
		t.Errorf("issuer should still be on the tailnet name with some free port, got %s", issuer)
	}
}
