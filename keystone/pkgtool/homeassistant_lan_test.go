package pkgtool

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// Home Assistant 2.x on the host's network (#440), expanded for real
// against the deployments brokerDeployment builds.

func homeAssistantParams(node string) map[string]string {
	return map[string]string{"public_domain": "home.example.internal", "node": node}
}

func findRoute(t *testing.T, rendered []resource.Resource, name string) *resource.RouteSpec {
	t.Helper()
	for _, r := range rendered {
		if r.Kind == resource.KindRoute && r.Metadata.Name == name && r.Route != nil {
			return r.Route
		}
	}
	t.Fatalf("no Route/%s rendered", name)
	return nil
}

func TestHomeAssistantRunsOnTheHostNetwork(t *testing.T) {
	out, err := capability.NewEngine().Expand(loadShipped(t, "home-assistant"), homeAssistantParams("n1"), brokerDeployment(t, ""))
	if err != nil {
		t.Fatalf("home-assistant with MQTT off needs no broker: %v", err)
	}
	c := findContainer(t, out, "home-assistant")
	if !c.HostNetwork() {
		t.Errorf("networkMode = %q, want host: LAN discovery never crosses a bridge", c.NetworkMode)
	}
	if len(c.Ports) != 0 {
		t.Errorf("ports = %v: a host-networked container publishes nothing", c.Ports)
	}
	if len(c.Devices) != 0 {
		t.Errorf("devices = %+v with serial_device unset", c.Devices)
	}
	for k := range c.Env {
		if strings.HasPrefix(k, "WILDSTONE_MQTT_") {
			t.Errorf("%s rendered with mqtt_enabled off", k)
		}
	}
	// The proxy reaches it through the host gateway; a container/ upstream
	// would name something that has no address on the Wildstone network.
	route := findRoute(t, out, "hass")
	if route.Upstream != "host/8123" {
		t.Errorf("Route upstream = %q, want host/8123", route.Upstream)
	}
	if route.Host != "hass.home.example.internal" || route.Auth != "none" || route.Classification != "family_private" {
		t.Errorf("Route = %+v", route)
	}
}

// The LAN reaches port 8123 directly now, so the seeded trust must cover
// the proxy's bridge address and nothing a household LAN usually is.
func TestHomeAssistantTrustsTheBridgeNotTheLAN(t *testing.T) {
	out, err := capability.NewEngine().Expand(loadShipped(t, "home-assistant"), homeAssistantParams("n1"), brokerDeployment(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	var store struct {
		Data struct {
			Stable struct {
				TrustedProxies []string `json:"trusted_proxies"`
			} `json:"stable"`
		} `json:"data"`
	}
	for _, r := range out {
		if r.Kind == resource.KindSeedFile && r.SeedFile.Path == ".storage/http" {
			if err := json.Unmarshal([]byte(r.SeedFile.Content), &store); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := []string{"172.16.0.0/12", "127.0.0.1/32"}
	if !reflect.DeepEqual(store.Data.Stable.TrustedProxies, want) {
		t.Errorf("trusted_proxies = %v, want %v (HostNetworkProxyTrustCIDRs)", store.Data.Stable.TrustedProxies, want)
	}
}

// An install from 1.x keeps everything Home Assistant holds: the same
// config volume at /config, backed up by the same plan to the same
// repository, the same Route name and hostname, and the same seed paths
// (which the node never rewrites). These are the values 1.1.2 rendered.
func TestHomeAssistantUpgradeKeepsConfigAndBackups(t *testing.T) {
	// A 1.x install recorded exactly these params; the new ones default.
	params := homeAssistantParams("n1")
	params["hass_subdomain"] = "hass"
	params["timezone"] = "Europe/London"
	out, err := capability.NewEngine().Expand(loadShipped(t, "home-assistant"), params, brokerDeployment(t, ""))
	if err != nil {
		t.Fatalf("a 1.x install's params must still expand: %v", err)
	}
	c := findContainer(t, out, "home-assistant")
	if len(c.Mounts) != 1 || c.Mounts[0].Source != "home-assistant-config" || c.Mounts[0].Target != "/config" || c.Mounts[0].ReadOnly {
		t.Errorf("mounts = %+v, want home-assistant-config at /config, writable", c.Mounts)
	}
	if c.Env["TZ"] != "Europe/London" {
		t.Errorf("TZ = %q", c.Env["TZ"])
	}
	volumes := map[string]bool{}
	seeds := map[string]bool{}
	var plan *resource.BackupPlanSpec
	for _, r := range out {
		switch r.Kind {
		case resource.KindVolume:
			volumes[r.Volume.Name] = true
		case resource.KindSeedFile:
			seeds[r.SeedFile.Path] = true
		case resource.KindBackupPlan:
			plan = r.BackupPlan
		}
	}
	for _, v := range []string{"home-assistant-config", "home-assistant-backup-repo"} {
		if !volumes[v] {
			t.Errorf("volume %s is no longer declared", v)
		}
	}
	for _, p := range []string{"configuration.yaml", ".storage/http", ".HA_VERSION", "automations.yaml", "scripts.yaml", "scenes.yaml"} {
		if !seeds[p] {
			t.Errorf("seed %s is gone", p)
		}
	}
	if plan == nil || !reflect.DeepEqual(plan.Volumes, []string{"home-assistant-config"}) ||
		plan.Repository != "home-assistant-backup-repo" || plan.PasswordSecret != "secret/restic-home-assistant" ||
		plan.Schedule != "daily" {
		t.Errorf("backup plan = %+v, want the 1.x plan unchanged", plan)
	}
	if r := findRoute(t, out, "hass"); r.Host != "hass.home.example.internal" {
		t.Errorf("Route host = %q", r.Host)
	}
}

func TestHomeAssistantAuthenticatesAgainstTheBroker(t *testing.T) {
	params := homeAssistantParams("n1")
	params["mqtt_enabled"] = "true"
	out, err := capability.NewEngine().Expand(loadShipped(t, "home-assistant"), params, brokerDeployment(t, "n1"))
	if err != nil {
		t.Fatalf("home-assistant on the broker's node: %v", err)
	}
	env := findContainer(t, out, "home-assistant").Env
	for key, want := range map[string]string{
		// The broker's private address, never the name "mosquitto", which
		// a host-networked container cannot resolve.
		"WILDSTONE_MQTT_HOST":     "192.168.1.10",
		"WILDSTONE_MQTT_PORT":     "1883",
		"WILDSTONE_MQTT_USERNAME": "home-assistant",
		"WILDSTONE_MQTT_PASSWORD": "secret/mqtt-home-assistant", // the reference; the node substitutes it
	} {
		if env[key] != want {
			t.Errorf("%s = %q, want %q", key, env[key], want)
		}
	}
}

func TestHomeAssistantMQTTOnAnotherNodeIsRefused(t *testing.T) {
	params := homeAssistantParams("n2")
	params["mqtt_enabled"] = "true"
	_, err := capability.NewEngine().Expand(loadShipped(t, "home-assistant"), params, brokerDeployment(t, "n1"))
	var pe *capability.SecretPlacementError
	if !errors.As(err, &pe) {
		t.Fatalf("home-assistant on n2, broker on n1: want a SecretPlacementError, got %v", err)
	}
	for _, want := range []string{`"n1"`, `"n2"`, "mqtt-home-assistant"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message does not name %s: %v", want, err)
		}
	}
	// With MQTT off, the broker's placement is none of its business.
	if _, err := capability.NewEngine().Expand(loadShipped(t, "home-assistant"), homeAssistantParams("n2"), brokerDeployment(t, "n1")); err != nil {
		t.Errorf("MQTT off on n2: %v", err)
	}
}

func TestHomeAssistantMQTTWithoutABrokerIsAnUnsatisfiedRequirement(t *testing.T) {
	params := homeAssistantParams("n1")
	params["mqtt_enabled"] = "true"
	_, err := capability.NewEngine().Expand(loadShipped(t, "home-assistant"), params, brokerDeployment(t, ""))
	var ue *capability.UnsatisfiedRequirementError
	if !errors.As(err, &ue) || ue.Trait != "mqtt" || ue.Mode != "broker" {
		t.Fatalf("want an unsatisfied mqtt:broker requirement, got %v", err)
	}
}

func TestHomeAssistantSerialDeviceIsAllowlisted(t *testing.T) {
	const stick = "/dev/serial/by-id/usb-Nabu_Casa_ZBT-1_0001-if00-port0"
	params := homeAssistantParams("n1")
	params["serial_device"] = stick
	out, err := capability.NewEngine().Expand(loadShipped(t, "home-assistant"), params, brokerDeployment(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if d := findContainer(t, out, "home-assistant").Devices; len(d) != 1 || d[0].Source != stick {
		t.Errorf("devices = %+v, want the stick", d)
	}
	// The same closed allowlist as every package: never a block device.
	params["serial_device"] = "/dev/sda"
	if _, err := capability.NewEngine().Expand(loadShipped(t, "home-assistant"), params, brokerDeployment(t, "")); err == nil {
		t.Error("serial_device=/dev/sda expanded; a block device must be refused")
	}
}
