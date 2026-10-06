package pkgtool

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// Home Assistant's bluetooth mode (#445), expanded for real.

func renderHomeAssistant(t *testing.T, params map[string]string) []resource.Resource {
	t.Helper()
	out, err := capability.NewEngine().Expand(loadShipped(t, "home-assistant"), params, brokerDeployment(t, ""))
	if err != nil {
		t.Fatalf("expand %v: %v", params, err)
	}
	return out
}

// proxy, the default, and off render no host access at all; host-bluez
// renders exactly /run/dbus read-only plus NET_ADMIN and NET_RAW, and
// nothing else of the host (no USB bus, no device).
func TestHomeAssistantBluetoothModes(t *testing.T) {
	config := resource.Mount{Source: "home-assistant-config", Target: "/config"}
	for _, mode := range []string{"", "proxy", "off"} {
		params := homeAssistantParams("n1")
		if mode != "" {
			params["bluetooth"] = mode
		}
		c := findContainer(t, renderHomeAssistant(t, params), "home-assistant")
		if !reflect.DeepEqual(c.Mounts, []resource.Mount{config}) || len(c.CapAdd) != 0 || len(c.Devices) != 0 {
			t.Errorf("bluetooth=%q: mounts %+v capAdd %v devices %+v, want only the config volume", mode, c.Mounts, c.CapAdd, c.Devices)
		}
		if got := c.RequiredProtocolMinor(); got != resource.HostNetworkProtocolMinor {
			t.Errorf("bluetooth=%q needs minor %d, want %d as 2.0.0 did", mode, got, resource.HostNetworkProtocolMinor)
		}
	}

	params := homeAssistantParams("n1")
	params["bluetooth"] = "host-bluez"
	c := findContainer(t, renderHomeAssistant(t, params), "home-assistant")
	want := []resource.Mount{config, {Source: "/run/dbus", Target: "/run/dbus", ReadOnly: true}}
	if !reflect.DeepEqual(c.Mounts, want) {
		t.Errorf("host-bluez mounts = %+v, want %+v", c.Mounts, want)
	}
	if !reflect.DeepEqual(c.CapAdd, []string{"NET_ADMIN", "NET_RAW"}) {
		t.Errorf("host-bluez capAdd = %v, want exactly NET_ADMIN, NET_RAW", c.CapAdd)
	}
	if len(c.Devices) != 0 {
		t.Errorf("host-bluez devices = %+v: the USB bus is never passed for Bluetooth", c.Devices)
	}
	if got := c.RequiredProtocolMinor(); got != resource.HostSystemMountProtocolMinor {
		t.Errorf("host-bluez needs minor %d, want %d (the /run/dbus mount)", got, resource.HostSystemMountProtocolMinor)
	}

	// A serial radio alongside host BlueZ is still just that one device.
	params["serial_device"] = "/dev/serial/by-id/usb-Nabu_Casa_ZBT-1_0001-if00-port0"
	if d := findContainer(t, renderHomeAssistant(t, params), "home-assistant").Devices; len(d) != 1 {
		t.Errorf("devices = %+v, want only the serial radio", d)
	}

	// A closed choice: anything else is refused, not rendered as off.
	params = homeAssistantParams("n1")
	params["bluetooth"] = "usb"
	if _, err := capability.NewEngine().Expand(loadShipped(t, "home-assistant"), params, brokerDeployment(t, "")); err == nil {
		t.Error("bluetooth=usb expanded; the choice is closed")
	}
}

// Switching Bluetooth mode touches only the container: every other
// resource — the /config volume, its seeds, the backup plan, the Route —
// renders byte-identically in all three modes, and the container keeps
// the same volume at /config.
func TestHomeAssistantBluetoothSwitchKeepsConfig(t *testing.T) {
	others := func(mode string) map[string]string {
		t.Helper()
		params := homeAssistantParams("n1")
		params["bluetooth"] = mode
		got := map[string]string{}
		for _, r := range renderHomeAssistant(t, params) {
			if r.Kind == resource.KindContainer {
				if r.Container.Mounts[0] != (resource.Mount{Source: "home-assistant-config", Target: "/config"}) {
					t.Errorf("bluetooth=%s: first mount %+v, want the config volume at /config", mode, r.Container.Mounts[0])
				}
				continue
			}
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			got[string(r.Kind)+"/"+r.Metadata.Name] = string(b)
		}
		return got
	}
	proxy := others("proxy")
	if _, ok := proxy[string(resource.KindVolume)+"/home-assistant-config"]; !ok {
		t.Fatalf("no config volume rendered: %v", proxy)
	}
	for _, mode := range []string{"host-bluez", "off"} {
		if got := others(mode); !reflect.DeepEqual(got, proxy) {
			t.Errorf("bluetooth=%s changes resources other than the container:\n%v\nvs proxy\n%v", mode, got, proxy)
		}
	}
}
