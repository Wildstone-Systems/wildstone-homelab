package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/redact"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// A stick's /dev/serial/by-id alias and the radio id built from it are
// its stable identity in the Console's radio picker. The response
// redactor used to take the mixed-case vendor/model/serial name for a
// token and serve both as [REDACTED], so the picker had nothing to tell
// two sticks apart by and nothing to submit.

// sonoffByIDName is a realistic udev by-id name — long, mixed case,
// carrying a USB serial — built at runtime so no serial number is
// written into the repo.
func sonoffByIDName() string {
	return "usb-ITead_Sonoff_Zigbee_3.0_USB_Dongle_Plus_" + "REPLACED" + strings.Repeat("0a", 8) + "-if00-port0"
}

// secretShaped is a genuine secret-shaped value; it must still redact.
const secretShaped = "aGVsbG9Xb3JsZFRoaXNJc0FTZWNyZXRWYWx1ZTEyMw"

func TestProbeRadiosKeepsSerialByIDIdentity(t *testing.T) {
	h := newHarness(t)
	alias := "/dev/serial/by-id/" + sonoffByIDName()
	id := "serial-by-id:" + sonoffByIDName()
	h.s.Agents = applierFunc(func(context.Context, string, *wsv1.Operation) (*wsv1.OpResult, error) {
		return &wsv1.OpResult{Result: &wsv1.OpResult_ListRadios{ListRadios: &wsv1.ListRadiosResult{
			PlatformSupported: true,
			Radios: []*wsv1.Radio{{
				Id: id, Kind: wsv1.RadioKind_RADIO_KIND_ZIGBEE, KernelName: "ttyUSB0", DevPath: "/dev/ttyUSB0",
				ByIdPaths:  []string{alias},
				KindReason: secretShaped,
			}},
		}}}, nil
	})
	var out struct {
		Radios []struct {
			ID         string   `json:"id"`
			KindReason string   `json:"kindReason"`
			ByIDPaths  []string `json:"byIdPaths"`
		} `json:"radios"`
	}
	if resp := h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/nodes/n1/probes/radios", nil, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("radios = %d", resp.StatusCode)
	}
	if len(out.Radios) != 1 {
		t.Fatalf("radios = %+v", out)
	}
	r := out.Radios[0]
	if r.ID != id || len(r.ByIDPaths) != 1 || r.ByIDPaths[0] != alias {
		t.Errorf("by-id identity lost: id=%q byIdPaths=%v", r.ID, r.ByIDPaths)
	}
	if r.KindReason != redact.Placeholder {
		t.Errorf("secret-shaped value must still redact, got %q", r.KindReason)
	}
}

func TestHomeAutomationKeepsSerialByIDIdentity(t *testing.T) {
	f := newHAFixture(t)
	alias := "/dev/serial/by-id/" + sonoffByIDName()
	id := "serial-by-id:" + sonoffByIDName()
	f.radios[0].Id = id
	f.radios[0].ByIdPaths = []string{alias}
	f.radios[0].KindReason = secretShaped

	v := f.get(t, "?topology=managed&select=zigbee")
	inv := v.Nodes[0].RadioProbe
	if inv == nil || len(inv.Radios) != 3 {
		t.Fatalf("radio inventory = %+v", inv)
	}
	r := inv.Radios[0]
	if r.ID != id || len(r.ByIDPaths) != 1 || r.ByIDPaths[0] != alias {
		t.Errorf("by-id identity lost: id=%q byIdPaths=%v", r.ID, r.ByIDPaths)
	}
	if r.KindReason != redact.Placeholder {
		t.Errorf("secret-shaped value must still redact, got %q", r.KindReason)
	}
	if z := v.comp(t, haZigbee); len(z.RadioCandidates) != 1 || z.RadioCandidates[0] != id {
		t.Errorf("zigbee candidates = %v, want [%s]", z.RadioCandidates, id)
	}
}
