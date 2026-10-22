package capability

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// The shipped zwave-zwave-js-ui package against a consumer of
// zwave:server (#444). Unlike the broker, this provider itself requires
// something — identity:oidc, for its admin Route — which is what the
// nested rehearsal in installed() exists for.

func loadShipped(t *testing.T, name string) *pkgspec.Package {
	t.Helper()
	pkg, err := pkgspec.Load(filepath.Join(repoRoot(t), "packages", name, "package.yaml"))
	if err != nil {
		t.Fatalf("loading packages/%s/package.yaml: %v", name, err)
	}
	return pkg
}

// zwaveConsumer is a Home-Assistant-shaped consumer of the WS server.
func zwaveConsumer() *pkgspec.Package {
	return &pkgspec.Package{
		ApiVersion: pkgspec.ApiVersion,
		Kind:       pkgspec.Kind,
		Metadata:   pkgspec.Metadata{Name: "ha-zwave-fixture", Version: "1.0.0", Capability: "home-assistant"},
		Requires:   []pkgspec.Requirement{{Trait: "zwave", Mode: "server"}},
		Params:     []pkgspec.ParamDecl{{Name: "node", Required: true}},
		Templates: []string{`apiVersion: wildstone.dev/v1
kind: ConfigFile
metadata: { name: ha-zwave, node: "{{ .Params.node }}" }
spec:
  filename: zwave.yaml
  content: |
    url: {{ .Resolved.ZwaveWsURL }}
`},
	}
}

func zwaveCatalog(t *testing.T) (*pkgspec.Package, *Catalog) {
	t.Helper()
	zw := loadShipped(t, "zwave-zwave-js-ui")
	return zw, NewCatalog(zw, loadShipped(t, "identity"))
}

func installedZwave(t *testing.T, zw *pkgspec.Package, cat *Catalog, params map[string]string) []resource.Resource {
	t.Helper()
	base := map[string]string{
		"node":         "n1",
		"admin_domain": "admin.example.internal",
		"usb_device":   "/dev/serial/by-id/usb-Zooz_800_Z-Wave_Stick_0001-if00",
		"listen_addr":  "192.168.1.20",
	}
	for k, v := range params {
		base[k] = v
	}
	out, err := NewEngine().Expand(zw, base, cat)
	if err != nil {
		t.Fatalf("expanding zwave-zwave-js-ui: %v", err)
	}
	return out
}

func TestZwaveServerAdvertisesItsInstalledAddress(t *testing.T) {
	zw, cat := zwaveCatalog(t)
	installed := installedZwave(t, zw, cat, map[string]string{"ws_port": "3300"})
	got := resolve(t, zwaveConsumer(), map[string]string{"node": "n1"}, cat.WithCurrent(installed))
	for key, want := range map[string]string{
		"ZwaveWsHost": "192.168.1.20",
		"ZwaveWsPort": "3300",
		"ZwaveWsURL":  "ws://192.168.1.20:3300",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
	for key, v := range got {
		if strings.Contains(v, "zwave-js-ui") {
			t.Errorf("%s advertises the container name (%q): a host-networked Home Assistant cannot resolve it", key, v)
		}
	}
}

// Not installed: no address, and the consumer fails on the key rather
// than connecting somewhere nobody chose.
func TestZwaveServerNotInstalledPublishesNoAddress(t *testing.T) {
	_, cat := zwaveCatalog(t)
	got := resolve(t, zwaveConsumer(), map[string]string{"node": "n1"}, cat.WithCurrent(nil))
	for _, key := range []string{"ZwaveWsHost", "ZwaveWsPort", "ZwaveWsURL"} {
		if v, ok := got[key]; ok {
			t.Errorf("%s published as %q with no Z-Wave server installed", key, v)
		}
	}
	if _, err := NewEngine().Expand(zwaveConsumer(), map[string]string{"node": "n1"}, cat); err == nil ||
		!strings.Contains(err.Error(), "ZwaveWsURL") {
		t.Fatalf("want a template error naming ZwaveWsURL, got %v", err)
	}
}

// A rehearsal renders a provider that itself has requirements against
// the same catalog, so lint and the shipped walk can check a Home
// Assistant that consumes it. Before #444 the provider was rendered with
// no catalog, its identity:oidc requirement failed, and the consumer
// got no answer at all.
func TestRehearsalRendersAProviderThatHasRequirements(t *testing.T) {
	_, cat := zwaveCatalog(t)
	got := resolve(t, zwaveConsumer(), map[string]string{"node": "n1"}, cat.Rehearsal())
	if got["ZwaveWsURL"] != "ws://192.168.1.10:3000" {
		t.Errorf("ZwaveWsURL = %q, want the provider's declared example address", got["ZwaveWsURL"])
	}
	// Still exactly one level deep: without its own requirement in the
	// catalog the provider has no rehearsal answer, rather than a guess.
	alone := NewCatalog(loadShipped(t, "zwave-zwave-js-ui")).Rehearsal()
	if v, ok := resolve(t, zwaveConsumer(), map[string]string{"node": "n1"}, alone)["ZwaveWsURL"]; ok {
		t.Errorf("ZwaveWsURL = %q from a provider whose own requirement is unsatisfied", v)
	}
}
