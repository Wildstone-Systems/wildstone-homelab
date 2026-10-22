package reconcile

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// routeFixtures covers every value in the closed RouteSpec.Auth set plus
// both upstream shapes, so the validator sees each rendering branch.
var routeFixtures = []struct {
	name string
	spec resource.RouteSpec
}{
	{"forward-auth", resource.RouteSpec{
		Host: "kuma.example.internal", Upstream: "container/uptime-kuma:3001",
		Auth: "authelia_oidc", Classification: "admin",
	}},
	{"app-oidc", resource.RouteSpec{
		Host: "photos.example.internal", Upstream: "container/immich-server:2283",
		Auth: "app_oidc", Classification: "family_public",
	}},
	{"app-ldap", resource.RouteSpec{
		Host: "media.example.internal", Upstream: "container/jellyfin:8096",
		Auth: "app_ldap", Classification: "family_public",
	}},
	{"portal", resource.RouteSpec{
		Host: "auth.example.internal", Upstream: "container/authelia:9091",
		Auth: "portal", Classification: "family_public",
	}},
	{"none", resource.RouteSpec{
		Host: "ntfy.example.internal", Upstream: "container/ntfy:80",
		Auth: "none", Classification: "family_private",
	}},
	{"ip-upstream", resource.RouteSpec{
		Host: "nas.example.internal", Upstream: "10.0.0.4:5000",
		Auth: "none", Classification: "admin",
	}},
	{"host-upstream", resource.RouteSpec{
		Host: "home.example.internal", Upstream: "host/8123",
		Auth: "app_oidc", Classification: "family_tailscale",
	}},
}

// TestUpstreamForms pins what each upstream form renders to. The two
// forms that predate host/<port> are byte-for-byte what they were; the
// host form is the alias and never an address, because the address is
// the node's bridge gateway and belongs to Docker, not to desired state
// (#433).
func TestUpstreamForms(t *testing.T) {
	for up, want := range map[string]string{
		"container/immich-server:2283": "immich-server:2283",
		"10.0.0.4:5000":                "10.0.0.4:5000",
		"host/8123":                    resource.HostGatewayAlias + ":8123",
		"host/1":                       resource.HostGatewayAlias + ":1",
	} {
		if got := caddyUpstream(up); got != want {
			t.Errorf("caddyUpstream(%q) = %q, want %q", up, got, want)
		}
	}
	spec := &resource.RouteSpec{Host: "home.example.internal", Upstream: "host/8123", Auth: "none", Classification: "admin"}
	got := string(renderRouteFragment(spec, DefaultAutheliaAddr, terminatedUpstream))
	if !strings.Contains(got, "\treverse_proxy "+resource.HostGatewayAlias+":8123\n") {
		t.Errorf("host upstream not rendered to the host-gateway alias:\n%s", got)
	}
	if strings.Contains(got, "host/") || strings.Contains(got, "172.17.") {
		t.Errorf("host upstream leaked its source form or a literal gateway:\n%s", got)
	}
}

// writeRenderedConfig lays out a managed prefix exactly as the reconciler
// does — Caddyfile at the root, fragments under routes/ — and returns the
// Caddyfile path. The layout matters: `import routes/*.caddy` resolves
// relative to the importing file.
func writeRenderedConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "routes"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(base, renderBaseCaddyfile(terminatedUpstream), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range routeFixtures {
		spec := f.spec
		frag := renderRouteFragment(&spec, DefaultAutheliaAddr, terminatedUpstream)
		if err := os.WriteFile(filepath.Join(dir, "routes", f.name+".caddy"), frag, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

// TestRenderedConfigIsValidCaddy runs the real Caddyfile adapter over the
// real rendering, so a syntax error, an unknown directive or a malformed
// site block cannot ship.
//
// Note what it does NOT catch, because it is worth knowing before trusting
// it: the pre-#40 rendering — "reverse_proxy container/immich-server:2283",
// an upstream nothing could ever dial — passes `caddy validate` cleanly.
// Caddy resolves upstream addresses when it dials, not when it parses.
// That bug is caught by TestRouteFragmentShape's container/ assertion, and
// the missing import by TestBaseCaddyfileImportsFragments; both run
// everywhere. This test covers the class of breakage those cannot see.
//
// Skips when caddy is absent so a local `go test` still works — but FAILS
// rather than skips under CI, where the workflow installs it. A guard that
// can silently stop running is the same shape of problem as the bug it
// guards against: green, and doing nothing. If this fails in CI, the
// install step or its PATH broke, not the rendering.
func TestRenderedConfigIsValidCaddy(t *testing.T) {
	caddy, err := exec.LookPath("caddy")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("caddy not on PATH under CI: the install step in " +
				".github/workflows/ci.yml did not put it there, so this check " +
				"would have skipped silently")
		}
		t.Skip("caddy not on PATH; CI installs it (see .github/workflows/ci.yml)")
	}
	base := writeRenderedConfig(t)
	out, err := exec.Command(caddy, "validate", "--adapter", "caddyfile", "--config", base).CombinedOutput()
	if err != nil {
		t.Fatalf("caddy rejected the rendered config: %v\n%s", err, out)
	}
}

// TestRouteFragmentShape asserts the properties the validator cannot: that
// upstreams are translated rather than passed through, and that only
// authelia_oidc asks the proxy to enforce anything. These run everywhere,
// with or without a caddy binary.
func TestRouteFragmentShape(t *testing.T) {
	for _, f := range routeFixtures {
		t.Run(f.name, func(t *testing.T) {
			spec := f.spec
			got := string(renderRouteFragment(&spec, DefaultAutheliaAddr, terminatedUpstream))

			if strings.Contains(got, "container/") {
				t.Errorf("upstream prefix survived into the config:\n%s", got)
			}
			// The site address must carry an explicit scheme, or Caddy
			// tries to provision a certificate for a hostname it cannot
			// validate from inside the tunnel.
			if !strings.Contains(got, "http://"+spec.Host+" {") {
				t.Errorf("site address is not an explicit http:// address:\n%s", got)
			}
			wantAuth := spec.Auth == "authelia_oidc"
			if gotAuth := strings.Contains(got, "forward_auth "+DefaultAutheliaAddr); gotAuth != wantAuth {
				t.Errorf("auth %q: forward_auth present = %v, want %v:\n%s",
					spec.Auth, gotAuth, wantAuth, got)
			}
			// Classification drives policy, not Caddy — it belongs in a
			// comment, never as a directive.
			if !strings.Contains(got, "# classification: "+spec.Classification) {
				t.Errorf("classification comment missing:\n%s", got)
			}
		})
	}
}

// TestBaseCaddyfileImportsFragments pins the one line whose absence made
// every fragment dead weight.
func TestBaseCaddyfileImportsFragments(t *testing.T) {
	got := string(renderBaseCaddyfile(terminatedUpstream))
	// ":80 {\n\tabort" is the default deny (F26): without it Caddy answers
	// every unrouted Host with an empty 200, so a front door with one
	// Route still looks alive for any hostname pointed at it. Verified
	// behaviourally against a real Caddy: routed host 200, unrouted host
	// connection aborted.
	// trusted_proxies (ADR-W019 A6): without it Caddy overwrites
	// X-Forwarded-Proto with its own inbound scheme and Authelia rejects
	// every OIDC request with "invalid X-Forwarded-Proto header value
	// 'http'". Invisible to any test that speaks to Authelia directly,
	// which is how it survived to a VM.
	for _, want := range []string{"import routes/*.caddy", "auto_https off", "admin off", ":80 {\n\tabort\n}", "trusted_proxies static private_ranges"} {
		if !strings.Contains(got, want) {
			t.Errorf("base Caddyfile missing %q:\n%s", want, got)
		}
	}
}

// TestFragmentsAreNeverEmpty pins F30. Caddy's --watch re-adapts the whole
// config every poll, so an adaptation warning is not logged once — it is
// logged every second for as long as the state lasts, into an unbounded
// json-file log. Both warning states are ones Wildstone creates itself, so
// both are Wildstone's to avoid: a fresh install before its first Route,
// and every Route ever deleted.
//
// Measured against a real Caddy at ~1 warning/second for each, and silent
// once the file holds a comment. The assertion is "non-empty and
// comment-only", because that is exactly the property that quiets it.
func TestFragmentsAreNeverEmpty(t *testing.T) {
	for name, got := range map[string][]byte{
		"placeholder": renderPlaceholderFragment(),
		"deleted":     renderDeletedFragment("kuma"),
	} {
		t.Run(name, func(t *testing.T) {
			if len(got) == 0 {
				t.Fatal("fragment is empty: Caddy warns once a second, forever")
			}
			for _, line := range strings.Split(strings.TrimSpace(string(got)), "\n") {
				if line != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") {
					t.Errorf("fragment contributes configuration, it must only be a comment: %q", line)
				}
			}
		})
	}
	if !strings.Contains(string(renderDeletedFragment("kuma")), "kuma") {
		t.Error("deleted fragment does not name the Route it replaced")
	}
}

// TestRenderedConfigWithPlaceholderAndDeletionIsValid runs the real adapter
// over the layout a node actually reaches after a Route is deleted: base
// Caddyfile, the placeholder, a live route, and a deleted one.
func TestRenderedConfigWithPlaceholderAndDeletionIsValid(t *testing.T) {
	caddy, err := exec.LookPath("caddy")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("caddy not on PATH under CI: the install step in .github/workflows/ci.yml did not put it there")
		}
		t.Skip("caddy not on PATH; CI installs it")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "routes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Caddyfile"), renderBaseCaddyfile(terminatedUpstream), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := routeFixtures[0].spec
	files := map[string][]byte{
		"00-wildstone-placeholder.caddy": renderPlaceholderFragment(),
		"live.caddy":                     renderRouteFragment(&spec, DefaultAutheliaAddr, terminatedUpstream),
		"gone.caddy":                     renderDeletedFragment("gone"),
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(dir, "routes", n), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command(caddy, "validate", "--adapter", "caddyfile",
		"--config", filepath.Join(dir, "Caddyfile")).CombinedOutput()
	if err != nil {
		t.Fatalf("caddy rejected the post-deletion layout: %v\n%s", err, out)
	}
}
