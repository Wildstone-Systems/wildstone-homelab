package plan

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func deviceContainer(name, pkg, node string, sources ...string) resource.Resource {
	spec := &resource.ContainerSpec{Image: "x"}
	for _, s := range sources {
		spec.Devices = append(spec.Devices, resource.DeviceMapping{Source: s})
	}
	return resource.Resource{
		Kind: resource.KindContainer,
		Metadata: resource.Metadata{Name: name, Node: node, Package: pkg,
			Owner: "capability/" + strings.SplitN(pkg, "-", 2)[0]},
		Container: spec,
	}
}

const (
	stickAlias = "/dev/serial/by-id/usb-Zooz_ZST10_700-if00"
	stickTTY   = "/dev/ttyUSB0"
	stickID    = "usb:0658:0200:0001"
)

var stickInventory = []Radio{{ID: stickID, Paths: []string{stickAlias, stickTTY}}}

// TestDuplicateDeviceOwnership is the #435 table: which desired states
// the plan path refuses, and which it must leave alone.
func TestDuplicateDeviceOwnership(t *testing.T) {
	for _, tc := range []struct {
		name    string
		desired []resource.Resource
		radios  map[string][]Radio
		wantErr []string // substrings; nil means no conflict
	}{
		{
			name: "exact duplicate path, offline node",
			desired: []resource.Resource{
				deviceContainer("zwave-js-ui", "zwave-js-ui", "n1", stickAlias),
				deviceContainer("homeassistant", "home-assistant", "n1", stickAlias),
			},
			wantErr: []string{"node n1", "Container/zwave-js-ui (package zwave-js-ui)",
				"Container/homeassistant (package home-assistant)", stickAlias, "remove the device"},
		},
		{
			name: "alias and resolved tty with inventory",
			desired: []resource.Resource{
				deviceContainer("zwave-js-ui", "zwave-js-ui", "n1", stickAlias),
				deviceContainer("homeassistant", "home-assistant", "n1", stickTTY),
			},
			radios:  map[string][]Radio{"n1": stickInventory},
			wantErr: []string{"radio " + stickID, "via " + stickAlias, "via " + stickTTY},
		},
		{
			name: "alias and tty without inventory are not provably one device",
			desired: []resource.Resource{
				deviceContainer("zwave-js-ui", "zwave-js-ui", "n1", stickAlias),
				deviceContainer("homeassistant", "home-assistant", "n1", stickTTY),
			},
		},
		{
			name: "identical path on different nodes",
			desired: []resource.Resource{
				deviceContainer("zwave-js-ui", "zwave-js-ui", "n1", stickAlias),
				deviceContainer("zwave-js-ui-2", "zwave-js-ui", "n2", stickAlias),
			},
			radios: map[string][]Radio{"n1": stickInventory, "n2": stickInventory},
		},
		{
			name: "shareable GPU and usb bus paths",
			desired: []resource.Resource{
				deviceContainer("jellyfin", "media-jellyfin", "n1", "/dev/dri", "/dev/dri/renderD128", "/dev/bus/usb"),
				deviceContainer("frigate", "cameras-frigate", "n1", "/dev/dri", "/dev/dri/renderD128", "/dev/bus/usb"),
			},
			radios: map[string][]Radio{"n1": stickInventory},
		},
		{
			name: "one container naming alias and tty is one claim",
			desired: []resource.Resource{
				deviceContainer("zwave-js-ui", "zwave-js-ui", "n1", stickAlias, stickTTY),
			},
			radios: map[string][]Radio{"n1": stickInventory},
		},
		{
			name: "inventoried non-serial path is exclusive",
			desired: []resource.Resource{
				deviceContainer("a", "a-pkg", "n1", "/dev/bus/usb/001/004"),
				deviceContainer("b", "b-pkg", "n1", "/dev/bus/usb/001/004"),
			},
			radios:  map[string][]Radio{"n1": {{ID: "sysfs:/sys/devices/bt", Paths: []string{"/dev/bus/usb/001/004"}}}},
			wantErr: []string{"radio sysfs:/sys/devices/bt"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := DuplicateDeviceOwnership(tc.desired, tc.radios, nil)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("duplicate accepted")
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

// A duplicate already desired must not block a proposal that touches
// neither side of it.
func TestDuplicateDeviceOwnershipOnlyReportsTouched(t *testing.T) {
	desired := []resource.Resource{
		deviceContainer("a", "a-pkg", "n1", stickTTY),
		deviceContainer("b", "b-pkg", "n1", stickTTY),
		deviceContainer("c", "c-pkg", "n1", "/dev/ttyACM0"),
	}
	if err := DuplicateDeviceOwnership(desired, nil, map[string]bool{"c": true}); err != nil {
		t.Fatalf("unrelated proposal refused: %v", err)
	}
	if err := DuplicateDeviceOwnership(desired, nil, map[string]bool{"b": true}); err == nil {
		t.Fatal("proposal touching a duplicate accepted")
	}
}

// RadioOwners is the reusable "who owns this radio" map (#435, for the
// radio picker and the aggregate view): keyed by inventory id when the
// node answered, by exact path when it did not.
func TestRadioOwners(t *testing.T) {
	desired := []resource.Resource{
		deviceContainer("zwave-js-ui", "zwave-js-ui", "n1", stickAlias),
		deviceContainer("jellyfin", "media-jellyfin", "n1", "/dev/dri/renderD128"),
		deviceContainer("other-node", "zwave-js-ui", "n2", stickAlias),
	}
	owners := RadioOwners(desired, "n1", stickInventory)
	if len(owners) != 1 || len(owners[stickID]) != 1 {
		t.Fatalf("owners = %+v", owners)
	}
	if c := owners[stickID][0]; c.Container != "zwave-js-ui" || c.Package != "zwave-js-ui" || c.RadioID != stickID || c.Source != stickAlias {
		t.Fatalf("claim = %+v", c)
	}
	offline := RadioOwners(desired, "n1", nil)
	if len(offline["path:"+stickAlias]) != 1 {
		t.Fatalf("offline owners = %+v", offline)
	}
}
