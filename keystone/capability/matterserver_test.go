package capability

import (
	"errors"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// The shipped matter-matterjs package against consumers of matter:server
// (#442). The provider is host-networked and binds its WebSocket to
// loopback, so the answer is read back from the installed container's
// env (hostNetworkEnv), and the engine holds a consumer to the one place
// a loopback URL answers: a host-networked container on the same node.

// matterConsumer is a Home-Assistant-shaped consumer: a container on
// the given network mode carrying the URL, as HA 2.x would for its
// one-time integration form.
func matterConsumer(networkMode string) *pkgspec.Package {
	return &pkgspec.Package{
		ApiVersion: pkgspec.ApiVersion,
		Kind:       pkgspec.Kind,
		Metadata:   pkgspec.Metadata{Name: "ha-matter-fixture", Version: "1.0.0", Capability: "home-assistant"},
		Requires:   []pkgspec.Requirement{{Trait: "matter", Mode: "server"}},
		Params:     []pkgspec.ParamDecl{{Name: "node", Required: true}},
		Templates: []string{`apiVersion: wildstone.dev/v1
kind: Container
metadata: { name: ha, node: "{{ .Params.node }}" }
spec:
  image: "ghcr.io/home-assistant/home-assistant@sha256:612d76760b544cb40b7ba01387fdac964c59a6a550a50a4d30b4773c822d2918"
  ` + networkMode + `
  env:
    WILDSTONE_MATTER_URL: "{{ .Resolved.MatterWsURL }}"
`},
	}
}

func matterCatalog(t *testing.T) (*pkgspec.Package, *Catalog) {
	t.Helper()
	m := loadShipped(t, "matter-matterjs")
	return m, NewCatalog(m)
}

func installedMatter(t *testing.T, m *pkgspec.Package, cat *Catalog, node string) []resource.Resource {
	t.Helper()
	out, err := NewEngine().Expand(m, map[string]string{"node": node}, cat)
	if err != nil {
		t.Fatalf("expanding matter-matterjs: %v", err)
	}
	return out
}

func TestMatterServerAdvertisesItsLoopbackListener(t *testing.T) {
	m, cat := matterCatalog(t)
	installed := installedMatter(t, m, cat, "n1")
	got := resolve(t, matterConsumer("networkMode: host"), map[string]string{"node": "n1"}, cat.WithCurrent(installed))
	for key, want := range map[string]string{
		"MatterWsHost": "127.0.0.1",
		"MatterWsPort": "5580",
		"MatterWsURL":  "ws://127.0.0.1:5580/ws",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
	for key, v := range got {
		if strings.Contains(v, "matter-server") {
			t.Errorf("%s advertises the container name (%q): a host-networked Home Assistant cannot resolve it", key, v)
		}
	}
	// And the consumer that fits — host network, same node — expands.
	if _, err := NewEngine().Expand(matterConsumer("networkMode: host"), map[string]string{"node": "n1"}, cat.WithCurrent(installed)); err != nil {
		t.Fatalf("a host-networked consumer on the server's node was refused: %v", err)
	}
}

// Not installed: no address, and the consumer fails on the key.
func TestMatterServerNotInstalledPublishesNoAddress(t *testing.T) {
	_, cat := matterCatalog(t)
	got := resolve(t, matterConsumer("networkMode: host"), map[string]string{"node": "n1"}, cat.WithCurrent(nil))
	for _, key := range []string{"MatterWsHost", "MatterWsPort", "MatterWsURL"} {
		if v, ok := got[key]; ok {
			t.Errorf("%s published as %q with no Matter server installed", key, v)
		}
	}
	if _, err := NewEngine().Expand(matterConsumer("networkMode: host"), map[string]string{"node": "n1"}, cat); err == nil ||
		!strings.Contains(err.Error(), "MatterWsURL") {
		t.Fatalf("want a template error naming MatterWsURL, got %v", err)
	}
}

// A consumer on another node would dial its OWN loopback: refused, with
// both nodes named.
func TestMatterConsumerOnAnotherNodeIsRefused(t *testing.T) {
	m, cat := matterCatalog(t)
	installed := installedMatter(t, m, cat, "n1")
	_, err := NewEngine().Expand(matterConsumer("networkMode: host"), map[string]string{"node": "n2"}, cat.WithCurrent(installed))
	var lp *LoopbackPlacementError
	if !errors.As(err, &lp) {
		t.Fatalf("want a LoopbackPlacementError, got %v", err)
	}
	if lp.Bridge || lp.Node != "n2" || len(lp.ProviderNodes) != 1 || lp.ProviderNodes[0] != "n1" || lp.Key != "MatterWsURL" {
		t.Errorf("refusal = %+v", lp)
	}
	for _, want := range []string{`"n2"`, `"n1"`, "MatterWsURL", "matter-matterjs", "loopback"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %s: %v", want, err)
		}
	}
}

// A consumer container on the Wildstone bridge would dial itself.
func TestMatterConsumerOnTheBridgeIsRefused(t *testing.T) {
	m, cat := matterCatalog(t)
	installed := installedMatter(t, m, cat, "n1")
	_, err := NewEngine().Expand(matterConsumer(""), map[string]string{"node": "n1"}, cat.WithCurrent(installed))
	var lp *LoopbackPlacementError
	if !errors.As(err, &lp) || !lp.Bridge {
		t.Fatalf("want a bridge LoopbackPlacementError, got %v", err)
	}
	if !strings.Contains(err.Error(), "networkMode: host") {
		t.Errorf("refusal does not say what to do: %v", err)
	}
	// Rehearsal nodes are stand-ins, but a bridge is a bridge anywhere.
	if _, err := NewEngine().Expand(matterConsumer(""), map[string]string{"node": "n1"}, cat.Rehearsal()); !errors.As(err, &lp) {
		t.Errorf("rehearsal let a bridge consumer through: %v", err)
	}
}

// Lint and the shipped walk get an answer from the provider's own
// defaults, and do not compare stand-in nodes.
func TestMatterServerRehearsalAnswers(t *testing.T) {
	_, cat := matterCatalog(t)
	got := resolve(t, matterConsumer("networkMode: host"), map[string]string{"node": "n1"}, cat.Rehearsal())
	if got["MatterWsURL"] != "ws://127.0.0.1:5580/ws" {
		t.Errorf("MatterWsURL = %q in rehearsal", got["MatterWsURL"])
	}
	if _, err := NewEngine().Expand(matterConsumer("networkMode: host"), map[string]string{"node": "elsewhere"}, cat.Rehearsal()); err != nil {
		t.Errorf("rehearsal compared stand-in nodes: %v", err)
	}
}

// hostNetworkEnv answers only for a host-networked container, and never
// with a secret reference.
func TestHostNetworkEnvAnswersOnlyForHostNetworkedContainers(t *testing.T) {
	c := func(name, mode string, env map[string]string) resource.Resource {
		return resource.Resource{Kind: resource.KindContainer, Metadata: resource.Metadata{Name: name, Node: "n1"},
			Container: &resource.ContainerSpec{Image: "x", NetworkMode: mode, Env: env}}
	}
	installed := []resource.Resource{
		c("bridge", "", map[string]string{"LISTEN_ADDRESS": "127.0.0.1"}),
		c("host", resource.NetworkModeHost, map[string]string{
			"LISTEN_ADDRESS": "127.0.0.1",
			"TOKEN":          "secret/matter-token",
			"PASSWORD":       "pbkdf2:secret/x",
		}),
	}
	for _, tc := range []struct{ container, key, want string }{
		{"host", "LISTEN_ADDRESS", "127.0.0.1"},
		{"host", "MISSING", ""},
		{"host", "TOKEN", ""},
		{"host", "PASSWORD", ""},
		{"bridge", "LISTEN_ADDRESS", ""},
		{"absent", "LISTEN_ADDRESS", ""},
	} {
		if got := hostNetworkEnv(installed, tc.container, tc.key); got != tc.want {
			t.Errorf("hostNetworkEnv(%s, %s) = %q, want %q", tc.container, tc.key, got, tc.want)
		}
	}
}

func TestLoopbackURL(t *testing.T) {
	for v, want := range map[string]bool{
		"ws://127.0.0.1:5580/ws": true,
		"ws://127.0.0.2:5580/ws": true,
		"http://localhost:8123":  true,
		"ws://[::1]:5580/ws":     true,
		"ws://192.168.1.10:3000": false,
		"127.0.0.1":              false, // a bare address is not judged
		"127.0.0.1/32":           false,
		"mqtt://mosquitto:1883":  false,
		"":                       false,
	} {
		if got := loopbackURL(v); got != want {
			t.Errorf("loopbackURL(%q) = %v, want %v", v, got, want)
		}
	}
}
