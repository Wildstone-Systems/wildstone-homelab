package api

import (
	"net/http"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

func (f *haFixture) continuity(t *testing.T) haContinuityView {
	t.Helper()
	var v haContinuityView
	resp := f.h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/home-automation/continuity", nil, &v)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET continuity = %d", resp.StatusCode)
	}
	return v
}

func (v haContinuityView) comp(id string) *haContinuity {
	for i := range v.Components {
		if v.Components[i].Component == id {
			return &v.Components[i]
		}
	}
	return nil
}

// Only installed parts are described; nothing installed is an empty list,
// not null.
func TestHomeAutomationContinuityEmpty(t *testing.T) {
	f := newHAFixture(t)
	v := f.continuity(t)
	if v.Components == nil || len(v.Components) != 0 {
		t.Fatalf("components = %+v", v.Components)
	}
}

func TestHomeAutomationContinuity(t *testing.T) {
	f := newHAFixture(t)
	// A second Zigbee stick is plugged in, free: the replacement offered.
	spare := &wsv1.Radio{Id: "serial-by-id:usb-Spare_Zigbee-if00", Kind: wsv1.RadioKind_RADIO_KIND_ZIGBEE, KernelName: "ttyUSB2",
		DevPath: "/dev/ttyUSB2", ByIdPaths: []string{"/dev/serial/by-id/usb-Spare_Zigbee-if00"}}
	f.radios = append(f.radios, spare)
	f.install(t, "mqtt-test", "2.0.0", map[string]string{"node": "n1"}, haContainer("mosquitto", "mqtt", "mqtt-test", "n1"))
	data := haVolume("zigbee2mqtt-data", "zigbee", "zigbee-test", "n1")
	repo := haVolume("zigbee-backup-repo", "zigbee", "zigbee-test", "n1")
	bp := haBackupPlan("zigbee", "zigbee", "zigbee-test", "n1", "zigbee2mqtt-data", "zigbee-backup-repo")
	bp.BackupPlan.Class = "important"
	f.install(t, "zigbee-test", "2.0.0", map[string]string{"node": "n1", "usb_device": haZigbeeAlias},
		haContainer("zigbee2mqtt", "zigbee", "zigbee-test", "n1", haZigbeeAlias), data, repo, bp)
	f.observe(t, resource.KindContainer, "mosquitto", true, "", "")
	f.observe(t, resource.KindContainer, "zigbee2mqtt", true, "healthy", "")

	v := f.continuity(t)
	if len(v.Components) != 2 || v.comp(haZWave) != nil {
		t.Fatalf("components = %+v", v.Components)
	}
	z := v.comp(haZigbee)
	if z == nil || z.Package != "zigbee-test" || z.Holds == "" {
		t.Fatalf("zigbee = %+v", z)
	}
	// Data first, the backup copy after it; the class comes from the
	// BackupPlan the package rendered.
	if len(z.Data) != 2 || z.Data[0].Volume != "zigbee2mqtt-data" || z.Data[0].Class != "important" ||
		z.Data[0].BackupPlan != "zigbee" || z.Data[0].Schedule != "daily" || !z.Data[1].Repository {
		t.Errorf("data = %+v", z.Data)
	}
	if z.Radio == nil || z.Radio.NetworkOn != "radio_and_machine" || z.Radio.Moved.Outcome != haKept ||
		z.Radio.Replaced.Outcome != haRestore || z.Radio.Replaced.Otherwise == "" || z.Radio.Param != "usb_device" {
		t.Fatalf("radio = %+v", z.Radio)
	}
	// Its own stick is not a replacement; the free spare is, by its
	// stable by-id path.
	if len(z.Radio.Replacements) != 1 || z.Radio.Replacements[0].RadioID != spare.Id ||
		z.Radio.Replacements[0].Path != "/dev/serial/by-id/usb-Spare_Zigbee-if00" {
		t.Errorf("replacements = %+v", z.Radio.Replacements)
	}
	// Removal stops the container and keeps both volumes, never lists
	// them as deleted.
	if len(z.Removal.Stops) != 1 || z.Removal.Stops[0] != "zigbee2mqtt" || len(z.Removal.Keeps) != 2 || z.Removal.Consequence == "" {
		t.Errorf("removal = %+v", z.Removal)
	}
	// Removing the broker would stop Zigbee: named as a dependent.
	m := v.comp(haMQTT)
	if m == nil || m.Radio != nil || len(m.Removal.Dependents) != 1 || m.Removal.Dependents[0] != haZigbee {
		t.Errorf("mqtt = %+v", m)
	}
}

// Bluetooth is a Home Assistant setting: turned off, never removed.
func TestHomeAutomationContinuityModeComponent(t *testing.T) {
	f := newHAFixture(t)
	f.install(t, "hass-test", "1.2.0", map[string]string{"node": "n1", "bluetooth": "host-bluez"},
		haContainer("home-assistant", "home-assistant", "hass-test", "n1"),
		haVolume("home-assistant-config", "home-assistant", "hass-test", "n1"))
	f.observe(t, resource.KindContainer, "home-assistant", true, "healthy", "")
	v := f.continuity(t)
	bt := v.comp(haBluetooth)
	if bt == nil || !bt.Removal.Setting || len(bt.Removal.Stops) != 0 || len(bt.Removal.Keeps) != 0 {
		t.Fatalf("bluetooth = %+v", bt)
	}
	hass := v.comp(haHomeAssistant)
	if hass == nil || len(hass.Removal.Keeps) != 1 || hass.Removal.Keeps[0] != "home-assistant-config" ||
		len(hass.Removal.Dependents) != 1 || hass.Removal.Dependents[0] != haBluetooth {
		t.Fatalf("home-assistant = %+v", hass)
	}
}
