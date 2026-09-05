package reconcile

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func ingressRes(tls string, trusted ...string) resource.Resource {
	return resource.Resource{
		Kind:     resource.KindIngress,
		Metadata: resource.Metadata{Name: "ingress", Owner: "capability/ingress", Node: "node-a"},
		Ingress:  &resource.IngressSpec{TLS: tls, TrustedProxies: trusted},
	}
}

// TestPostureDefaultsToWhatShippedBefore: a node with no Ingress declared
// must render exactly what it rendered before the kind existed. An
// upgrade that silently changes how the front door terminates TLS is the
// worst possible way to learn about a new resource kind.
func TestPostureDefaultsToWhatShippedBefore(t *testing.T) {
	p := postureOf(nil)
	if p.tls != "off" || p.trustedProxies != defaultTrustedProxies {
		t.Fatalf("want the pre-Ingress posture, got %+v", p)
	}
	base := string(renderBaseCaddyfile(p))
	if !strings.Contains(base, "auto_https off") || strings.Contains(base, "local_certs") {
		t.Errorf("default posture should terminate nothing:\n%s", base)
	}
}

// TestInternalPostureTerminatesTLS is #65: the tailnet profile has
// nothing in front of it, so it must hold the certificate itself
// (ADR-W019 A3). Before this, both profiles rendered the same
// terminated-upstream config and the LAN profile could not serve https
// at all, which is why SSO needed a hand-made container in front.
func TestInternalPostureTerminatesTLS(t *testing.T) {
	p := postureOf([]resource.Resource{ingressRes("internal", "private_ranges")})
	base := string(renderBaseCaddyfile(p))
	if !strings.Contains(base, "local_certs") {
		t.Errorf("internal posture must use Caddy's own CA:\n%s", base)
	}
	if strings.Contains(base, "auto_https off") {
		t.Errorf("internal posture must not disable https:\n%s", base)
	}
	// The default-deny site has to be on the port the profile listens
	// on, or an unrouted host answers an empty 200 instead (F26).
	if !strings.Contains(base, "https://:443 {\n\tabort\n}") {
		t.Errorf("default deny is on the wrong port:\n%s", base)
	}

	frag := string(renderRouteFragment(&resource.RouteSpec{
		Host: "photos.example.com", Upstream: "container/immich:2283",
	}, "authelia:9091", p))
	if !strings.Contains(frag, "https://photos.example.com {") {
		t.Errorf("fragments must match the scheme the browser uses (ADR-W019 A2):\n%s", frag)
	}
}

// TestTrustedProxiesComeFromTheProfile is ADR-W019 B5: the renderer is
// told what to trust and by whom, rather than compiling in a default
// that is right for one profile and wrong for the next.
func TestTrustedProxiesComeFromTheProfile(t *testing.T) {
	p := postureOf([]resource.Resource{ingressRes("off", "10.0.0.0/8", "172.16.0.0/12")})
	base := string(renderBaseCaddyfile(p))
	if !strings.Contains(base, "trusted_proxies static 10.0.0.0/8 172.16.0.0/12") {
		t.Errorf("profile's trusted proxies not rendered:\n%s", base)
	}
}

// TestOffPostureKeepsHttp guards the Cloudflare profile: TLS arrives
// already terminated from the tunnel, so this listener stays plain http
// and its fragments with it (ADR-W019 A4).
func TestOffPostureKeepsHttp(t *testing.T) {
	p := postureOf([]resource.Resource{ingressRes("off", "private_ranges")})
	frag := string(renderRouteFragment(&resource.RouteSpec{
		Host: "photos.example.com", Upstream: "container/immich:2283",
	}, "authelia:9091", p))
	if !strings.Contains(frag, "http://photos.example.com {") {
		t.Errorf("terminated-upstream profile must stay http:\n%s", frag)
	}
}

// publicIngressRes is the DNS-01 posture: a public certificate needs a
// provider to answer the challenge with, so the two travel together.
func publicIngressRes(provider string, trusted ...string) resource.Resource {
	r := ingressRes("public", trusted...)
	r.Ingress.ACMEDNS = provider
	return r
}

// TestPublicPostureAsksACMEForTheCertificate is ADR-W019 A9. The tailnet
// profile's internal CA is real TLS that every device must be told to
// trust by hand — a step a household does not complete, which turns the
// padlock into a warning people click through. This posture gets a
// certificate from a CA the world already trusts.
func TestPublicPostureAsksACMEForTheCertificate(t *testing.T) {
	p := postureOf([]resource.Resource{publicIngressRes("cloudflare", "private_ranges")})
	base := string(renderBaseCaddyfile(p))
	if !strings.Contains(base, "acme_dns cloudflare {env.CF_API_TOKEN}") {
		t.Errorf("public posture must answer the challenge over DNS-01:\n%s", base)
	}
	// auto_https off would disable the very thing this posture exists to
	// do, and local_certs would quietly issue from the internal CA again
	// — the exact outcome an operator chose this profile to avoid.
	if strings.Contains(base, "auto_https off") || strings.Contains(base, "local_certs") {
		t.Errorf("public posture must not suppress or self-issue certificates:\n%s", base)
	}
}

// TestPublicPostureServesHTTPSLikeInternal: "public" and "internal"
// differ only in who signed the certificate. Every rendering decision
// downstream — the scheme in Route fragments, the default-deny port —
// turns on whether THIS proxy terminates, so the two must agree.
func TestPublicPostureServesHTTPSLikeInternal(t *testing.T) {
	pub := postureOf([]resource.Resource{publicIngressRes("cloudflare")})
	internal := postureOf([]resource.Resource{ingressRes("internal")})
	if pub.siteScheme() != "https" || pub.siteScheme() != internal.siteScheme() {
		t.Errorf("public must render https like internal, got %q vs %q",
			pub.siteScheme(), internal.siteScheme())
	}
	if defaultDenyPort(pub) != "443" || defaultDenyPort(pub) != defaultDenyPort(internal) {
		t.Errorf("public must default-deny on 443 like internal, got %q vs %q",
			defaultDenyPort(pub), defaultDenyPort(internal))
	}
}

// TestTokenNeverRendersIntoTheCaddyfile: the credential is an env
// reference, never a value. A zone-editing token written into a managed
// file would be readable by anything that can read the node's config,
// and the file is world-readable by design.
func TestTokenNeverRendersIntoTheCaddyfile(t *testing.T) {
	p := postureOf([]resource.Resource{publicIngressRes("cloudflare")})
	base := string(renderBaseCaddyfile(p))
	if strings.Contains(base, "secret/") {
		t.Errorf("a secret reference must not reach the Caddyfile:\n%s", base)
	}
	if !strings.Contains(base, "{env.CF_API_TOKEN}") {
		t.Errorf("the token must be read from the environment:\n%s", base)
	}
}
