package api

import (
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// Removing the Z-Wave server keeps its store (#444): the volume holds
// the network's security keys and node table, and the seeded
// settings.json is the household's from the moment it landed. Judged on
// what the SHIPPED package renders, so renaming the volume or turning
// the seed into a managed file cannot quietly make removal destructive.
func TestRemovingZwaveKeepsItsStore(t *testing.T) {
	zw, err := pkgspec.Load("../../packages/zwave-zwave-js-ui/package.yaml")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := pkgspec.Load("../../packages/identity/package.yaml")
	if err != nil {
		t.Fatal(err)
	}
	current, err := capability.NewEngine().Expand(zw, map[string]string{
		"node":         "n1",
		"admin_domain": "admin.example.internal",
		"usb_device":   "/dev/serial/by-id/usb-Zooz_800_Z-Wave_Stick_0001-if00",
		"listen_addr":  "192.168.1.20",
	}, capability.NewCatalog(zw, identity))
	if err != nil {
		t.Fatal(err)
	}

	deleted := map[string]bool{}
	for _, d := range deletesForDisable(zw.Metadata.Name, "capability/"+zw.Metadata.Capability, current, nil) {
		deleted[d.Kind+"/"+d.Name] = true
	}
	var store, seed bool
	for _, r := range current {
		key := string(r.Kind) + "/" + r.Metadata.Name
		switch r.Kind {
		case resource.KindVolume, resource.KindSeedFile:
			if r.Kind == resource.KindVolume && r.Metadata.Name == "zwave-js-ui-store" {
				store = true
			}
			if r.Kind == resource.KindSeedFile {
				seed = true
			}
			if deleted[key] {
				t.Errorf("removing the package deletes %s", key)
			}
		case resource.KindContainer, resource.KindRoute:
			if !deleted[key] {
				t.Errorf("removing the package leaves %s running", key)
			}
		}
	}
	if !store || !seed {
		t.Fatalf("the package no longer renders its store volume (%v) or its seed (%v): this test checks nothing", store, seed)
	}
}
