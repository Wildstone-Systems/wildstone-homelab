package pkgtool

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// The shipped MQTT consumers against the secured broker (#439). Each
// test expands the REAL package against a deployment in which
// mqtt-mosquitto was installed on n1 at 192.168.1.10, as a statefile
// would hold it — not the lint rehearsal — so the node rule and the
// installed address are both in play.

func loadShipped(t *testing.T, name string) *pkgspec.Package {
	t.Helper()
	p, err := pkgspec.Load(filepath.Join("..", "..", "packages", name, "package.yaml"))
	if err != nil {
		t.Fatalf("loading packages/%s: %v", name, err)
	}
	return p
}

// brokerDeployment is a catalog with identity and the broker, and the
// broker's expansion on brokerNode as the deployment's desired state.
// Without a broker (brokerNode == "") the catalog has identity only.
func brokerDeployment(t *testing.T, brokerNode string) *capability.Catalog {
	t.Helper()
	identity := loadShipped(t, "identity")
	if brokerNode == "" {
		return capability.NewCatalog(identity)
	}
	broker := loadShipped(t, "mqtt-mosquitto")
	installed, err := capability.NewEngine().Expand(broker,
		map[string]string{"node": brokerNode, "listen_addr": "192.168.1.10"}, capability.NewCatalog(broker))
	if err != nil {
		t.Fatalf("expanding the broker: %v", err)
	}
	return capability.NewCatalog(identity, broker).WithCurrent(installed)
}

func findContainer(t *testing.T, rendered []resource.Resource, name string) *resource.ContainerSpec {
	t.Helper()
	for _, r := range rendered {
		if r.Kind == resource.KindContainer && r.Metadata.Name == name && r.Container != nil {
			return r.Container
		}
	}
	t.Fatalf("no Container/%s rendered", name)
	return nil
}

func zigbeeParams(node string) map[string]string {
	return map[string]string{"admin_domain": "admin.example.internal", "node": node}
}

func TestZigbee2MQTTAuthenticatesAgainstTheBroker(t *testing.T) {
	out, err := capability.NewEngine().Expand(loadShipped(t, "zigbee-zigbee2mqtt"), zigbeeParams("n1"), brokerDeployment(t, "n1"))
	if err != nil {
		t.Fatalf("zigbee on the broker's node: %v", err)
	}
	env := findContainer(t, out, "zigbee2mqtt").Env
	for key, want := range map[string]string{
		"ZIGBEE2MQTT_CONFIG_MQTT_SERVER":   "mqtt://192.168.1.10:1883",
		"ZIGBEE2MQTT_CONFIG_MQTT_USER":     "zigbee",
		"ZIGBEE2MQTT_CONFIG_MQTT_PASSWORD": "secret/mqtt-zigbee", // exactly the reference: the agent substitutes nothing else
	} {
		if env[key] != want {
			t.Errorf("%s = %q, want %q", key, env[key], want)
		}
	}
}

// The broker's password is a node secret on n1; Zigbee2MQTT on n2 could
// never read it, so it is refused before a plan exists.
func TestZigbee2MQTTOnAnotherNodeIsRefused(t *testing.T) {
	_, err := capability.NewEngine().Expand(loadShipped(t, "zigbee-zigbee2mqtt"), zigbeeParams("n2"), brokerDeployment(t, "n1"))
	var pe *capability.SecretPlacementError
	if !errors.As(err, &pe) {
		t.Fatalf("zigbee on n2, broker on n1: want a SecretPlacementError, got %v", err)
	}
	for _, want := range []string{`"n1"`, `"n2"`, "mqtt-zigbee"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message does not name %s: %v", want, err)
		}
	}
}

func TestZigbee2MQTTWithoutABrokerIsAnUnsatisfiedRequirement(t *testing.T) {
	_, err := capability.NewEngine().Expand(loadShipped(t, "zigbee-zigbee2mqtt"), zigbeeParams("n1"), brokerDeployment(t, ""))
	var ue *capability.UnsatisfiedRequirementError
	if !errors.As(err, &ue) || ue.Trait != "mqtt" || ue.Mode != "broker" {
		t.Fatalf("want an unsatisfied mqtt:broker requirement, got %v", err)
	}
}

// The upgrade from 1.x keeps everything Zigbee2MQTT holds: the same
// volume at the same path (network key, every pairing), in the same
// BackupPlan, and the coordinator exactly as configured. These are the
// values 1.0.2 rendered; a change to any of them is a re-pair of every
// device in the house.
func TestZigbee2MQTTUpgradeKeepsItsNetworkAndRadio(t *testing.T) {
	pkg := loadShipped(t, "zigbee-zigbee2mqtt")
	const stick = "/dev/serial/by-id/usb-ITead_Sonoff_Zigbee_3.0_USB_Dongle_Plus_0001-if00-port0"

	// Params a 1.x install recorded, minus mqtt_server, which the install
	// form no longer offers (it lists declared params only).
	params := zigbeeParams("n1")
	params["usb_device"] = stick
	params["coordinator_adapter"] = "ember"
	out, err := capability.NewEngine().Expand(pkg, params, brokerDeployment(t, "n1"))
	if err != nil {
		t.Fatal(err)
	}
	c := findContainer(t, out, "zigbee2mqtt")
	if len(c.Mounts) != 1 || c.Mounts[0].Source != "zigbee2mqtt-data" || c.Mounts[0].Target != "/app/data" || c.Mounts[0].ReadOnly {
		t.Errorf("mounts = %+v, want zigbee2mqtt-data at /app/data, writable", c.Mounts)
	}
	// Since 2.0.1 the stick is /dev/zigbee inside the container (see
	// TestZigbee2MQTTRealByIDStickPassesPolicy): the same host device
	// under a fixed name, which the network key does not depend on.
	if len(c.Devices) != 1 || c.Devices[0].Source != stick || c.Devices[0].Target != "/dev/zigbee" {
		t.Errorf("devices = %+v, want the stick at /dev/zigbee", c.Devices)
	}
	if c.Env["ZIGBEE2MQTT_CONFIG_SERIAL_PORT"] != "/dev/zigbee" || c.Env["ZIGBEE2MQTT_CONFIG_SERIAL_ADAPTER"] != "ember" {
		t.Errorf("serial env = %q/%q", c.Env["ZIGBEE2MQTT_CONFIG_SERIAL_PORT"], c.Env["ZIGBEE2MQTT_CONFIG_SERIAL_ADAPTER"])
	}
	// Configuration stays environment, never a managed configuration.yaml
	// that would overwrite the pairings Zigbee2MQTT writes into it.
	var volume, plan bool
	for _, r := range out {
		switch {
		case r.Kind == resource.KindConfigFile:
			t.Errorf("zigbee renders ConfigFile/%s: Zigbee2MQTT owns its configuration.yaml", r.Metadata.Name)
		case r.Kind == resource.KindVolume && r.Volume != nil && r.Volume.Name == "zigbee2mqtt-data":
			volume = true
		case r.Kind == resource.KindBackupPlan && r.BackupPlan != nil:
			plan = len(r.BackupPlan.Volumes) == 1 && r.BackupPlan.Volumes[0] == "zigbee2mqtt-data"
		}
	}
	if !volume || !plan {
		t.Errorf("volume declared %v, backed up %v: the network key lives there", volume, plan)
	}

	// A network coordinator, unchanged too.
	params = zigbeeParams("n1")
	params["coordinator_url"] = "tcp://192.168.1.60:6638"
	out, err = capability.NewEngine().Expand(pkg, params, brokerDeployment(t, "n1"))
	if err != nil {
		t.Fatal(err)
	}
	c = findContainer(t, out, "zigbee2mqtt")
	if c.Env["ZIGBEE2MQTT_CONFIG_SERIAL_PORT"] != "tcp://192.168.1.60:6638" || len(c.Devices) != 0 {
		t.Errorf("network coordinator: port %q, devices %+v", c.Env["ZIGBEE2MQTT_CONFIG_SERIAL_PORT"], c.Devices)
	}
}

// A real stick's by-id name is a long mixed-case run with digits — the
// shape secret-values-forbidden refuses in env. Zigbee2MQTT is told the
// fixed in-container /dev/zigbee instead, and the device mapping alone
// carries the host path, so an operator typing the name ls printed gets
// a plan rather than a policy refusal. The serial is built at runtime
// and marked REPLACED: it is a fixture, not anyone's stick.
func TestZigbee2MQTTRealByIDStickPassesPolicy(t *testing.T) {
	stick := "/dev/serial/by-id/usb-" + strings.Join([]string{
		"ITead", "Sonoff", "Zigbee", "3.0", "USB", "Dongle", "Plus", "REPLACED",
	}, "_") + "-if00-port0"
	if !redact.SecretShaped(stick) {
		t.Fatalf("fixture %q is not secret-shaped; it no longer reproduces the refusal", stick)
	}
	params := zigbeeParams("n1")
	params["usb_device"] = stick
	out, err := capability.NewEngine().Expand(loadShipped(t, "zigbee-zigbee2mqtt"), params, brokerDeployment(t, "n1"))
	if err != nil {
		t.Fatal(err)
	}
	c := findContainer(t, out, "zigbee2mqtt")
	if len(c.Devices) != 1 || c.Devices[0].Source != stick || c.Devices[0].Target != "/dev/zigbee" {
		t.Errorf("devices = %+v, want the stick mapped to /dev/zigbee", c.Devices)
	}
	if got := c.Env["ZIGBEE2MQTT_CONFIG_SERIAL_PORT"]; got != "/dev/zigbee" {
		t.Errorf("ZIGBEE2MQTT_CONFIG_SERIAL_PORT = %q, want the fixed /dev/zigbee", got)
	}
	for k, v := range c.Env {
		if strings.Contains(v, "/dev/serial/") {
			t.Errorf("env %s carries the host path %q", k, v)
		}
	}
	violations, _ := policy.NewEngine(nil).Validate(policy.Change{Proposed: out})
	for _, v := range violations {
		if v.Rule == policy.RuleSecretValuesForbidden {
			t.Errorf("refused: %v", v)
		}
	}
}

func frigateParams(node, mqtt string) map[string]string {
	p := map[string]string{
		"admin_domain":    "admin.example.internal",
		"node":            node,
		"camera_rtsp_url": "rtsp://192.168.1.50:554/stream1",
	}
	if mqtt != "" {
		p["mqtt_enabled"] = mqtt
	}
	return p
}

// frigateMQTT is the mqtt block of Frigate's managed config.
func frigateMQTT(t *testing.T, rendered []resource.Resource) map[string]any {
	t.Helper()
	for _, r := range rendered {
		if r.Kind == resource.KindConfigFile && r.ConfigFile != nil && r.ConfigFile.Filename == "config.yml" {
			var cfg struct {
				MQTT map[string]any `yaml:"mqtt"`
			}
			if err := yaml.Unmarshal([]byte(r.ConfigFile.Content), &cfg); err != nil {
				t.Fatalf("Frigate's config.yml is not YAML: %v\n%s", err, r.ConfigFile.Content)
			}
			return cfg.MQTT
		}
	}
	t.Fatal("Frigate renders no config.yml")
	return nil
}

// MQTT off is the default, and it needs no broker anywhere: not in the
// catalog, and not on this node.
func TestFrigateInstallsWithoutMQTT(t *testing.T) {
	pkg := loadShipped(t, "cameras-frigate")
	for label, tc := range map[string]struct {
		cat  *capability.Catalog
		node string
		mqtt string
	}{
		"no broker at all":        {brokerDeployment(t, ""), "n1", ""},
		"explicitly off":          {brokerDeployment(t, ""), "n1", "false"},
		"broker on another node":  {brokerDeployment(t, "n1"), "n2", ""},
		"broker on the same node": {brokerDeployment(t, "n1"), "n1", "false"},
	} {
		out, err := capability.NewEngine().Expand(pkg, frigateParams(tc.node, tc.mqtt), tc.cat)
		if err != nil {
			t.Errorf("%s: %v", label, err)
			continue
		}
		if m := frigateMQTT(t, out); m["enabled"] != false || len(m) != 1 {
			t.Errorf("%s: mqtt = %v, want only enabled: false", label, m)
		}
		if v, ok := findContainer(t, out, "frigate").Env["FRIGATE_MQTT_PASSWORD"]; ok {
			t.Errorf("%s: FRIGATE_MQTT_PASSWORD = %q with MQTT off", label, v)
		}
	}
}

func TestFrigateWithMQTTAuthenticatesAgainstTheBroker(t *testing.T) {
	out, err := capability.NewEngine().Expand(loadShipped(t, "cameras-frigate"), frigateParams("n1", "true"), brokerDeployment(t, "n1"))
	if err != nil {
		t.Fatalf("frigate with MQTT on the broker's node: %v", err)
	}
	m := frigateMQTT(t, out)
	for key, want := range map[string]any{
		"enabled":  true,
		"host":     "192.168.1.10",
		"port":     1883,
		"user":     "cameras",
		"password": "{FRIGATE_MQTT_PASSWORD}", // Frigate's own env substitution; no literal
	} {
		if m[key] != want {
			t.Errorf("mqtt.%s = %#v, want %#v", key, m[key], want)
		}
	}
	if got := findContainer(t, out, "frigate").Env["FRIGATE_MQTT_PASSWORD"]; got != "secret/mqtt-cameras" {
		t.Errorf("FRIGATE_MQTT_PASSWORD = %q, want exactly secret/mqtt-cameras", got)
	}
}

func TestFrigateWithMQTTNeedsTheBrokerOnItsNode(t *testing.T) {
	pkg := loadShipped(t, "cameras-frigate")
	_, err := capability.NewEngine().Expand(pkg, frigateParams("n1", "true"), brokerDeployment(t, ""))
	var ue *capability.UnsatisfiedRequirementError
	if !errors.As(err, &ue) || ue.Trait != "mqtt" {
		t.Errorf("MQTT on with no broker: want an unsatisfied mqtt:broker requirement, got %v", err)
	}
	_, err = capability.NewEngine().Expand(pkg, frigateParams("n2", "true"), brokerDeployment(t, "n1"))
	var pe *capability.SecretPlacementError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "mqtt-cameras") {
		t.Errorf("MQTT on, broker on n1, frigate on n2: want a SecretPlacementError for mqtt-cameras, got %v", err)
	}
}

// No shipped default or example points a client at the broker's
// container name: a host-networked Home Assistant cannot resolve it, and
// a consumer that needs the broker takes its address from mqtt:broker.
func TestNoShippedDefaultNamesTheBrokerContainer(t *testing.T) {
	for name, sp := range walkShipped(t) {
		for _, d := range sp.pkg.Params {
			for _, v := range []string{d.Default, d.Example} {
				low := strings.ToLower(v)
				if low == "mosquitto" || strings.Contains(low, "//mosquitto") || strings.Contains(low, "mosquitto:1883") {
					t.Errorf("%s: param %s defaults to %q, the broker's container name; require mqtt:broker instead", name, d.Name, v)
				}
			}
		}
	}
}
