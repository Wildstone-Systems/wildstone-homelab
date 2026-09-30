package capability

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// The shipped mqtt-mosquitto package against consumers of mqtt:broker.
// The engine knows nothing about MQTT: everything asserted here comes
// from the package's own `provides` block, the installed-state export
// functions, and the generic exported-secret placement rule.

func loadMosquitto(t *testing.T) *pkgspec.Package {
	t.Helper()
	pkg, err := pkgspec.Load(filepath.Join(repoRoot(t), "packages", "mqtt-mosquitto", "package.yaml"))
	if err != nil {
		t.Fatalf("loading packages/mqtt-mosquitto/package.yaml: %v", err)
	}
	return pkg
}

// installedBroker is the deployment's desired state after the broker's
// plan was approved: what the broker expanded to, as a statefile holds it.
func installedBroker(t *testing.T, broker *pkgspec.Package, node string) []resource.Resource {
	t.Helper()
	out, err := NewEngine().Expand(broker, map[string]string{"node": node, "listen_addr": "192.168.1.10"}, NewCatalog(broker))
	if err != nil {
		t.Fatalf("expanding the broker: %v", err)
	}
	return out
}

// bridgeConsumer is a Zigbee2MQTT-shaped consumer: a container on the
// Wildstone network, configured through env.
func bridgeConsumer() *pkgspec.Package {
	return &pkgspec.Package{
		ApiVersion: pkgspec.ApiVersion,
		Kind:       pkgspec.Kind,
		Metadata:   pkgspec.Metadata{Name: "zigbee-fixture", Version: "1.0.0", Capability: "zigbee"},
		Requires:   []pkgspec.Requirement{{Trait: "mqtt", Mode: "broker"}},
		Params:     []pkgspec.ParamDecl{{Name: "node", Required: true}},
		Templates: []string{`apiVersion: wildstone.dev/v1
kind: Container
metadata: { name: zigbee2mqtt, node: "{{ .Params.node }}" }
spec:
  image: "example/z2m@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  env:
    ZIGBEE2MQTT_CONFIG_MQTT_SERVER: "{{ .Resolved.MqttURL }}"
    ZIGBEE2MQTT_CONFIG_MQTT_USER: "{{ .Resolved.MqttUsername }}"
    ZIGBEE2MQTT_CONFIG_MQTT_PASSWORD: "{{ .Resolved.MqttPasswordSecretRef }}"
`},
	}
}

// hostConsumer is a Home-Assistant-shaped consumer: it reads its broker
// from a managed file, and would run on host networking, where a
// container name resolves to nothing.
func hostConsumer() *pkgspec.Package {
	return &pkgspec.Package{
		ApiVersion: pkgspec.ApiVersion,
		Kind:       pkgspec.Kind,
		Metadata:   pkgspec.Metadata{Name: "ha-fixture", Version: "1.0.0", Capability: "home-assistant"},
		Requires:   []pkgspec.Requirement{{Trait: "mqtt", Mode: "broker"}},
		Params:     []pkgspec.ParamDecl{{Name: "node", Required: true}},
		Templates: []string{`apiVersion: wildstone.dev/v1
kind: ConfigFile
metadata: { name: ha-mqtt, node: "{{ .Params.node }}" }
spec:
  filename: mqtt.yaml
  content: |
    broker: {{ .Resolved.MqttHost }}
    port: {{ .Resolved.MqttPort }}
    username: {{ .Resolved.MqttUsername }}
    password: {{ .Resolved.MqttPasswordSecretRef }}
`},
	}
}

func TestBrokerAdvertisesItsInstalledAddressAndAPerConsumerCredential(t *testing.T) {
	broker := loadMosquitto(t)
	cat := NewCatalog(broker).WithCurrent(installedBroker(t, broker, "n1"))

	for _, tc := range []struct {
		consumer *pkgspec.Package
		user     string
	}{{bridgeConsumer(), "zigbee"}, {hostConsumer(), "home-assistant"}} {
		got := resolve(t, tc.consumer, map[string]string{"node": "n1"}, cat)
		for key, want := range map[string]string{
			"MqttHost":              "192.168.1.10",
			"MqttPort":              "1883",
			"MqttURL":               "mqtt://192.168.1.10:1883",
			"MqttUsername":          tc.user,
			"MqttPasswordSecretRef": "secret/mqtt-" + tc.user,
		} {
			if got[key] != want {
				t.Errorf("%s: %s = %q, want %q", tc.consumer.Metadata.Name, key, got[key], want)
			}
		}
		// And the whole expansion goes through: same node, issued secret.
		if _, err := NewEngine().Expand(tc.consumer, map[string]string{"node": "n1"}, cat); err != nil {
			t.Errorf("%s on the broker's node: %v", tc.consumer.Metadata.Name, err)
		}
	}
}

// The port the operator chose is the port advertised: the export reads
// what was published, not a constant.
func TestBrokerAdvertisesAChosenPort(t *testing.T) {
	broker := loadMosquitto(t)
	installed, err := NewEngine().Expand(broker,
		map[string]string{"node": "n1", "listen_addr": "10.0.0.2", "listen_port": "11883"}, NewCatalog(broker))
	if err != nil {
		t.Fatal(err)
	}
	got := resolve(t, bridgeConsumer(), map[string]string{"node": "n1"}, NewCatalog(broker).WithCurrent(installed))
	if got["MqttURL"] != "mqtt://10.0.0.2:11883" {
		t.Errorf("MqttURL = %q", got["MqttURL"])
	}
}

// Before the broker is installed there is no address to hand out, and
// handing out none makes the consumer fail on the key rather than
// connect somewhere nobody chose.
func TestBrokerNotInstalledPublishesNoAddress(t *testing.T) {
	broker := loadMosquitto(t)
	got := resolve(t, bridgeConsumer(), map[string]string{"node": "n1"}, NewCatalog(broker).WithCurrent(nil))
	for _, key := range []string{"MqttHost", "MqttPort", "MqttURL"} {
		if v, ok := got[key]; ok {
			t.Errorf("%s published as %q with no broker installed", key, v)
		}
	}
	_, err := NewEngine().Expand(bridgeConsumer(), map[string]string{"node": "n1"}, NewCatalog(broker))
	if err == nil || !strings.Contains(err.Error(), "MqttURL") {
		t.Fatalf("want a template error naming MqttURL, got %v", err)
	}
}

// The same-node rule. Secrets are node-local, so a consumer elsewhere
// would install, apply, and then never authenticate; expansion refuses it
// naming both nodes.
func TestConsumerOnAnotherNodeIsRefusedWithBothNodes(t *testing.T) {
	broker := loadMosquitto(t)
	cat := NewCatalog(broker).WithCurrent(installedBroker(t, broker, "n1"))
	for _, consumer := range []*pkgspec.Package{bridgeConsumer(), hostConsumer()} {
		_, err := NewEngine().Expand(consumer, map[string]string{"node": "n2"}, cat)
		var pe *SecretPlacementError
		if !errors.As(err, &pe) {
			t.Fatalf("%s on n2: want a SecretPlacementError, got %v", consumer.Metadata.Name, err)
		}
		msg := err.Error()
		for _, want := range []string{`"n1"`, `"n2"`, "node-local", "mqtt-mosquitto", consumer.Metadata.Name} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: message does not mention %s: %s", consumer.Metadata.Name, want, msg)
			}
		}
	}
}

// A capability the broker issues no credential to is refused by name,
// not left to fail against the broker.
func TestConsumerWithoutAnIssuedCredentialIsRefused(t *testing.T) {
	broker := loadMosquitto(t)
	stranger := bridgeConsumer()
	stranger.Metadata.Name, stranger.Metadata.Capability = "energy-fixture", "energy"
	_, err := NewEngine().Expand(stranger, map[string]string{"node": "n1"},
		NewCatalog(broker).WithCurrent(installedBroker(t, broker, "n1")))
	if err == nil || !strings.Contains(err.Error(), "issues no such secret") || !strings.Contains(err.Error(), "mqtt-energy") {
		t.Fatalf("want an issues-no-such-secret error naming secret/mqtt-energy, got %v", err)
	}
}

// Lint and the shipped-package walk have no deployment: the rehearsal
// answers from the broker's own example address, and the node rule is
// not applied to stand-in nodes — but an unissued credential still is.
func TestRehearsalAnswersFromTheProvidersExample(t *testing.T) {
	broker := loadMosquitto(t)
	cat := NewCatalog(broker).Rehearsal()
	got := resolve(t, bridgeConsumer(), map[string]string{"node": "somewhere"}, cat)
	if got["MqttURL"] != "mqtt://192.168.1.10:1883" {
		t.Errorf("MqttURL = %q, want the broker's declared example", got["MqttURL"])
	}
	if _, err := NewEngine().Expand(bridgeConsumer(), map[string]string{"node": "somewhere"}, cat); err != nil {
		t.Errorf("rehearsal must not compare stand-in nodes: %v", err)
	}
	stranger := bridgeConsumer()
	stranger.Metadata.Capability = "energy"
	if _, err := NewEngine().Expand(stranger, map[string]string{"node": "somewhere"}, cat); err == nil {
		t.Error("rehearsal must still refuse a consumer the broker issues nothing to")
	}
}

// The rule is about exported secrets, not about MQTT: a package that
// names some unrelated secret of its own is none of its business.
func TestPlacementRuleIgnoresSecretsNobodyExported(t *testing.T) {
	broker := loadMosquitto(t)
	own := bridgeConsumer()
	own.Requires = nil
	own.Templates = []string{`apiVersion: wildstone.dev/v1
kind: Container
metadata: { name: other, node: "{{ .Params.node }}" }
spec:
  image: "example/o@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  env: { TOKEN: secret/other-token }
`}
	if _, err := NewEngine().Expand(own, map[string]string{"node": "n2"},
		NewCatalog(broker).WithCurrent(installedBroker(t, broker, "n1"))); err != nil {
		t.Fatalf("a secret no provider exported must not be placement-checked: %v", err)
	}
}

// publishedHost renders nothing for an every-interface mapping: there is
// no one address to hand a consumer, and guessing one is worse than
// failing on the key.
func TestPublishedHostIsEmptyForEveryInterface(t *testing.T) {
	inst := []resource.Resource{{Kind: resource.KindContainer, Metadata: resource.Metadata{Name: "b"},
		Container: &resource.ContainerSpec{Ports: []string{"1883:1883", "10.0.0.2:8883:8883/udp"}}}}
	m, ok := publishedMapping(inst, "b", "1883")
	if !ok || m.HostIP != "" || m.HostPort != 1883 {
		t.Errorf("every-interface mapping: %+v %v", m, ok)
	}
	if _, ok := publishedMapping(inst, "b", "8883"); ok {
		t.Error("a UDP mapping must not answer for a TCP listener")
	}
	if _, ok := publishedMapping(inst, "nope", "1883"); ok {
		t.Error("another container's port must not answer")
	}
}
