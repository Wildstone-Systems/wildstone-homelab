package api

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// Removing the Matter server keeps its fabric (#442): matter-data holds
// the fabric's root CA and every commissioned device's credentials, and
// a server that starts on an empty volume makes a new fabric no device
// belongs to. Judged on what the SHIPPED package renders, so renaming
// the volume cannot quietly make removal destructive.
func TestRemovingMatterKeepsItsFabric(t *testing.T) {
	m, err := pkgspec.Load("../../packages/matter-matterjs/package.yaml")
	if err != nil {
		t.Fatal(err)
	}
	current, err := capability.NewEngine().Expand(m, map[string]string{"node": "n1"}, capability.NewCatalog(m))
	if err != nil {
		t.Fatal(err)
	}

	deleted := map[string]bool{}
	for _, d := range deletesForDisable(m.Metadata.Name, "capability/"+m.Metadata.Capability, current, nil) {
		deleted[d.Kind+"/"+d.Name] = true
	}
	var fabric bool
	for _, r := range current {
		key := string(r.Kind) + "/" + r.Metadata.Name
		switch r.Kind {
		case resource.KindVolume:
			if r.Metadata.Name == "matter-data" {
				fabric = true
			}
			if deleted[key] {
				t.Errorf("removing the package deletes %s", key)
			}
		case resource.KindContainer:
			if !deleted[key] {
				t.Errorf("removing the package leaves %s running", key)
			}
		}
	}
	if !fabric {
		t.Fatal("the package no longer renders matter-data: this test checks nothing")
	}
}
