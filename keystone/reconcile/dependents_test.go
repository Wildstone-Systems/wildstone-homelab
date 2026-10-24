package reconcile

import (
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

func depContainer(name, owner, mount string) resource.Resource {
	spec := &resource.ContainerSpec{Image: "reg.example/" + name + "@sha256:" + strings.Repeat("a", 64)}
	if mount != "" {
		spec.Mounts = []resource.Mount{{Source: mount, Target: mount, ReadOnly: true}}
	}
	return resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindContainer,
		Metadata:  resource.Metadata{Name: name, Owner: owner, Node: "n1"},
		Container: spec,
	}
}

func depConfig(content string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindConfigFile,
		Metadata:   resource.Metadata{Name: "photos-config", Owner: "capability/photos", Node: "n1"},
		ConfigFile: &resource.ConfigFileSpec{Filename: "app.json", Content: content},
	}
}

// A container whose own resource is unchanged but which mounts a file the
// change rewrites is listed as a restart, at its unchanged digest; one
// that mounts nothing affected is not.
func TestDependentRestartsListsContainersWhoseMountedConfigChanges(t *testing.T) {
	app := depContainer("photos", "capability/photos", "/etc/wildstone/photos")
	other := depContainer("notes", "capability/notes", "/etc/wildstone/notes")
	before := []resource.Resource{app, other, depConfig(`{"v":1}`)}
	after := []resource.Resource{app, other, depConfig(`{"v":2}`)}

	got := DependentRestarts("/etc/wildstone", before, after)
	if len(got) != 1 || got[0].Name != "photos" || got[0].Action != plan.ActionUpdate {
		t.Fatalf("restarts = %+v; want only photos", got)
	}
	if got[0].Digest != plan.Digest(app) || got[0].Risk != plan.RiskMedium {
		t.Fatalf("restart step = %+v; want Medium at the unchanged digest", got[0])
	}
}

// An edited container already has its own step from plan.Diff; it is not
// listed twice. An unchanged tree restarts nothing.
func TestDependentRestartsSkipsEditedAndUnchanged(t *testing.T) {
	app := depContainer("photos", "capability/photos", "/etc/wildstone/photos")
	edited := app
	spec := *app.Container
	spec.Env = map[string]string{"X": "1"}
	edited.Container = &spec
	before := []resource.Resource{app, depConfig(`{"v":1}`)}

	if got := DependentRestarts("/etc/wildstone", before, []resource.Resource{edited, depConfig(`{"v":2}`)}); len(got) != 0 {
		t.Fatalf("edited container listed as a restart: %+v", got)
	}
	if got := DependentRestarts("/etc/wildstone", before, before); len(got) != 0 {
		t.Fatalf("unchanged tree restarts %+v", got)
	}
}
