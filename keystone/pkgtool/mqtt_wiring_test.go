package pkgtool

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/trait"
)

// The secured broker (#438). Each test pins one half of "anonymous
// connections fail and a materialized client credential works": the
// config refuses anonymous clients, every credential the broker hands
// out is one the password file actually holds, and the listener is on
// the address the operator named rather than on every interface.

func renderedMosquitto(t *testing.T) []resource.Resource {
	t.Helper()
	rendered, ok := renderShippedPackages(t)["mqtt-mosquitto"]
	if !ok {
		t.Fatal("mqtt-mosquitto is not in the catalog")
	}
	return rendered
}

func mosquittoConfigFile(t *testing.T, rendered []resource.Resource, filename string) string {
	t.Helper()
	for _, r := range rendered {
		if r.Kind == resource.KindConfigFile && r.ConfigFile != nil && r.ConfigFile.Filename == filename {
			return r.ConfigFile.Content
		}
	}
	t.Fatalf("mqtt-mosquitto renders no %s", filename)
	return ""
}

func TestMosquittoRefusesAnonymousClients(t *testing.T) {
	conf := mosquittoConfigFile(t, renderedMosquitto(t), "mosquitto.conf")
	lines := map[string]bool{}
	for _, l := range strings.Split(conf, "\n") {
		lines[strings.TrimSpace(l)] = true
	}
	for _, want := range []string{"allow_anonymous false", "password_file /mosquitto/config/passwd"} {
		if !lines[want] {
			t.Errorf("mosquitto.conf lacks %q:\n%s", want, conf)
		}
	}
	if lines["allow_anonymous true"] {
		t.Error("mosquitto.conf still allows anonymous clients")
	}
	// per_listener_settings true would scope the two lines above to the
	// listener they follow, and a second listener added later would be
	// anonymous by default.
	if lines["per_listener_settings true"] {
		t.Error("per_listener_settings true makes a later listener anonymous by default")
	}
}

// Every password-file entry is a Mosquitto hash derived on the node from
// a Secret this package declares on the broker's own node — never a
// literal, never the plain form (which would put the password itself in
// the file), and never a secret some other node holds.
func TestMosquittoPasswordFileIsMaterializedFromItsOwnSecrets(t *testing.T) {
	rendered := renderedMosquitto(t)
	secrets := map[string]string{}
	var brokerNode string
	for _, r := range rendered {
		if r.Kind == resource.KindSecret && r.Secret != nil {
			if r.Secret.Provided {
				t.Errorf("secret %s is provided: a client password is generated on the node, not typed", r.Secret.Name)
			}
			secrets[r.Secret.Name] = r.Metadata.Node
		}
		if r.Kind == resource.KindContainer && r.Metadata.Name == "mosquitto" {
			brokerNode = r.Metadata.Node
		}
	}
	entry := regexp.MustCompile(`^([a-z][a-z0-9-]*):mosquitto:secret/(mqtt-[a-z0-9-]+)$`)
	users := 0
	for _, l := range strings.Split(strings.TrimSpace(mosquittoConfigFile(t, rendered, "passwd")), "\n") {
		m := entry.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil {
			t.Errorf("password file line %q is not <user>:mosquitto:secret/mqtt-<user>", l)
			continue
		}
		users++
		// The export hands consumer <capability> the reference
		// secret/mqtt-<capability> under username <capability>; a line
		// pairing them any other way is a credential that never matches.
		if m[2] != "mqtt-"+m[1] {
			t.Errorf("user %s is checked against secret/%s; the export hands it secret/mqtt-%s", m[1], m[2], m[1])
		}
		node, ok := secrets[m[2]]
		if !ok {
			t.Errorf("password file names secret/%s, which the package does not declare", m[2])
		} else if node != brokerNode {
			t.Errorf("secret/%s is on node %q, the broker on %q: the node materializing the file cannot read it", m[2], node, brokerNode)
		}
	}
	if users == 0 {
		t.Fatal("the password file issues no credential at all")
	}
	for _, want := range []string{"zigbee", "cameras", "home-assistant"} {
		if _, ok := secrets["mqtt-"+want]; !ok {
			t.Errorf("no credential for %s, which consumes mqtt:broker in #431", want)
		}
	}
}

// The listener is published on the one address the operator chose, and
// that address must be a private one: this is the whole of the LAN
// exposure, so it is explicit rather than a Docker default.
func TestMosquittoListenerIsBoundToAConfiguredPrivateAddress(t *testing.T) {
	rendered := renderedMosquitto(t)
	var ports []string
	for _, r := range rendered {
		if r.Kind == resource.KindRoute {
			t.Errorf("mqtt-mosquitto renders Route %s: the broker is not a web service and has no ingress", r.Metadata.Name)
		}
		if r.Kind == resource.KindContainer && r.Container != nil {
			ports = append(ports, r.Container.Ports...)
		}
	}
	if len(ports) != 1 {
		t.Fatalf("want exactly one published port, got %v", ports)
	}
	m, ok := resource.ParsePortMapping(ports[0])
	if !ok || m.HostIP == "" || m.ContainerPort != 1883 || m.Protocol != "tcp" {
		t.Errorf("published %q: want <private address>:<port>:1883", ports[0])
	}

	pkg := walkShipped(t)["mqtt-mosquitto"].pkg
	var found bool
	for _, d := range pkg.Params {
		if d.Name != "listen_addr" {
			continue
		}
		found = true
		if !d.Required || d.Default != "" {
			t.Error("listen_addr must be required with no default: exposure is the operator's explicit choice")
		}
		if d.Validate != "private_ipv4" {
			t.Errorf("listen_addr validates %q; want private_ipv4, which refuses 0.0.0.0, loopback and public addresses", d.Validate)
		}
	}
	if !found {
		t.Fatal("mqtt-mosquitto has no listen_addr param")
	}
}

// The broker provides mqtt:broker and fills every key the trait carries.
// A key it forgot is one a consumer's template fails on, three packages
// away.
func TestMosquittoFillsEveryBrokerExport(t *testing.T) {
	pkg := walkShipped(t)["mqtt-mosquitto"].pkg
	tr, _ := trait.Lookup("mqtt")
	mode, ok := tr.LookupMode("broker")
	if !ok {
		t.Fatal("trait mqtt has no broker mode")
	}
	var exports map[string]string
	for _, p := range pkg.Provides {
		if p.Trait == "mqtt" && p.Mode == "broker" {
			exports = p.Exports
		}
	}
	if exports == nil {
		t.Fatal("mqtt-mosquitto does not provide mqtt:broker")
	}
	for _, key := range mode.ExportKeys() {
		if _, ok := exports[key]; !ok {
			t.Errorf("mqtt-mosquitto does not export %s", key)
		}
	}
	// Never the container name: a host-networked consumer cannot resolve it.
	for key, expr := range exports {
		if strings.Contains(expr, "mosquitto:1883") || strings.Contains(expr, "//mosquitto") {
			t.Errorf("%s advertises the container name: %s", key, expr)
		}
	}
}

// A consumer linted against the broker (wild pkg lint -catalog) renders
// the broker's example address and its own credential reference, with no
// deployment behind it.
func TestLintRendersABrokerConsumerAgainstTheCatalog(t *testing.T) {
	dir := t.TempDir()
	body := `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: zigbee-fixture, version: 1.0.0, capability: zigbee }
requires:
  - { trait: mqtt, mode: broker }
params:
  - { name: node, required: true }
policies:
  classifications: [internal_only]
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: z2m-fixture, node: "{{ .Params.node }}" }
    spec:
      image: "example/z2m@sha256:0000000000000000000000000000000000000000000000000000000000000000"
      env:
        MQTT_SERVER: "{{ .Resolved.MqttURL }}"
        MQTT_USER: "{{ .Resolved.MqttUsername }}"
        MQTT_PASSWORD: "{{ .Resolved.MqttPasswordSecretRef }}"
`
	if err := os.WriteFile(filepath.Join(dir, "package.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	broker, err := filepath.Abs("../../packages/mqtt-mosquitto")
	if err != nil {
		t.Fatal(err)
	}
	issues, err := Lint(dir, LintOptions{CatalogDirs: []string{broker}})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range issues {
		if i.Rule == "expand" || i.Rule == "trait" {
			t.Errorf("%s: %s", i.Rule, i.Msg)
		}
	}
}
