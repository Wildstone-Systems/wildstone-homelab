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
