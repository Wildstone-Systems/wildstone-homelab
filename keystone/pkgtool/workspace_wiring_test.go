package pkgtool

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace"
)

func TestWorkspaceCapabilityIsContainerFreeAndGrantsNothing(t *testing.T) {
	if portal.Requestable(workspace.Capability) {
		t.Fatal("workspace storage configuration offered to household members")
	}
	sp, ok := walkShipped(t)[workspace.Package]
	if !ok {
		t.Fatal("workspace package missing")
	}
	if len(sp.pkg.AgentTools) != 0 {
		t.Fatal("workspace config must not grant domestic tools")
	}
	for _, params := range []map[string]string{
		{"node": "n1"},
		{"node": "n1", "workspace_path": "/mnt/private workspace", "required_mount": "/mnt", "quota_bytes": "2147483648", "max_file_bytes": "1024", "scratch_retention_days": "14", "trash_retention_days": "60"},
	} {
		rendered, err := capability.NewEngine().Expand(sp.pkg, params, capability.NewCatalog(sp.pkg))
		if err != nil {
			t.Fatal(err)
		}
		if len(rendered) != 1 || rendered[0].Kind != resource.KindConfigFile {
			t.Fatalf("workspace owns resources beyond settings: %+v", rendered)
		}
		settings, err := workspace.Read(rendered, "n1")
		if err != nil {
			t.Fatal(err)
		}
		if params["workspace_path"] == "" {
			if settings.Root != workspace.DefaultRoot || settings.QuotaBytes != 1<<30 || settings.MaxFileBytes != workspace.MaxFileBytes || settings.ScratchRetentionDays != 7 || settings.TrashRetentionDays != 30 {
				t.Fatalf("defaults: %+v", settings)
			}
		} else if settings.Root != params["workspace_path"] || settings.RequiredMount != "/mnt" || settings.QuotaBytes != 2<<30 || settings.MaxFileBytes != 1024 || settings.ScratchRetentionDays != 14 || settings.TrashRetentionDays != 60 {
			t.Fatalf("custom settings: %+v", settings)
		}
	}
	// No package, including runtime sidecars, receives the default root.
	for name, resources := range renderShippedPackages(t) {
		for _, r := range resources {
			if r.Container != nil {
				for _, mount := range r.Container.Mounts {
					if mount.Source == workspace.DefaultRoot {
						t.Errorf("%s mounts workspace into %s", name, r.Metadata.Name)
					}
				}
			}
		}
	}
}
