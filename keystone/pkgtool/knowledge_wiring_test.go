package pkgtool

// The knowledge capability's own conformance check (ADR-W046).
//
// The claim the filedrop check makes, about the same seam: the
// package's `agentTools` block is what the domestic gateway will ALLOW,
// and keystone/domestic.KnowledgeTools is what it will actually
// DISPATCH. A tool in one and not the other fails in a way nothing
// reports at install time.

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func TestKnowledgeToolsAreDeclaredByThePackage(t *testing.T) {
	sp, ok := walkShipped(t)["knowledge-wildstone"]
	if !ok {
		t.Fatal("knowledge-wildstone is not in the catalog")
	}
	if sp.pkg.Metadata.Capability != domestic.KnowledgeCapability {
		t.Fatalf("the package's capability is %q but the executors register under %q",
			sp.pkg.Metadata.Capability, domestic.KnowledgeCapability)
	}
	declared := map[string]string{}
	for _, tool := range sp.pkg.AgentTools {
		declared[tool.Name] = tool.Risk
	}
	for name, tier := range domestic.KnowledgeTools {
		got, present := declared[name]
		if !present {
			t.Errorf("keystone registers knowledge.%s, which the package does not declare", name)
			continue
		}
		if got != tier {
			t.Errorf("knowledge.%s is declared at %q and registered at %q", name, got, tier)
		}
	}
	for name, tier := range declared {
		if _, present := domestic.KnowledgeTools[name]; !present {
			t.Errorf("the package declares knowledge.%s, which nothing registers", name)
		}
		if !domestic.TierAllowed(tier, true) {
			t.Errorf("knowledge.%s is declared at tier %q, which a family session cannot carry", name, tier)
		}
	}
}

// The folder must survive the package. It is a host path the Owner
// chose, never a Volume the package owns, so no uninstall and no
// catalog change can ever be read as "delete the household's notes".
// And it must be backed up, because nothing can rebuild it.
func TestTheKnowledgeFolderOutlivesThePackageAndIsBackedUp(t *testing.T) {
	rendered, ok := renderShippedPackages(t)["knowledge-wildstone"]
	if !ok {
		t.Fatal("knowledge-wildstone did not render")
	}
	var root string
	for _, r := range rendered {
		if r.Kind == resource.KindConfigFile && r.ConfigFile != nil && r.ConfigFile.Filename == "knowledge.yaml" {
			root = r.ConfigFile.Content
		}
		if r.Kind == resource.KindContainer {
			t.Errorf("knowledge-wildstone declares a Container %q: the tools are Keystone's, and the UI is a separate package",
				r.Metadata.Name)
		}
	}
	if root == "" {
		t.Fatal("knowledge-wildstone renders no knowledge.yaml, so Keystone cannot find the folder")
	}
	backedUp := false
	for _, r := range rendered {
		if r.Kind != resource.KindBackupPlan || r.BackupPlan == nil {
			continue
		}
		for _, p := range r.BackupPlan.Paths {
			if p == "/srv/wildstone/knowledge" {
				backedUp = true
			}
		}
		if len(r.BackupPlan.Volumes) != 0 {
			t.Errorf("the knowledge BackupPlan backs up volumes %v: the notes are a host folder", r.BackupPlan.Volumes)
		}
	}
	if !backedUp {
		t.Error("no BackupPlan covers the knowledge folder at its default path")
	}
}

// The web UI is a window onto the knowledge folder, never a second copy
// of it: by default it must mount exactly the folder the knowledge
// capability keeps, own no Volume the notes could end up in, and keep
// SilverBullet's in-container shell switched off.
func TestTheNotesUIOpensTheKnowledgeFolder(t *testing.T) {
	shipped := walkShipped(t)
	defaultOf := func(pkg, param string) string {
		sp, ok := shipped[pkg]
		if !ok {
			t.Fatalf("%s is not in the catalog", pkg)
		}
		for _, p := range sp.pkg.Params {
			if p.Name == param {
				return p.Default
			}
		}
		t.Fatalf("%s has no %s param", pkg, param)
		return ""
	}
	folder := defaultOf("knowledge-wildstone", "knowledge_path")
	if got := defaultOf("notes-silverbullet", "knowledge_path"); got != folder {
		t.Fatalf("notes-silverbullet opens %q by default and the knowledge folder is %q: "+
			"a household accepting both defaults would get an empty UI", got, folder)
	}
	mounted := false
	for _, r := range shipped["notes-silverbullet"].rendered {
		switch {
		case r.Kind == resource.KindVolume:
			t.Errorf("notes-silverbullet declares Volume %q: the UI must hold nothing of its own", r.Metadata.Name)
		case r.Kind == resource.KindContainer && r.Container != nil:
			for _, m := range r.Container.Mounts {
				if m.Source == folder && m.Target == "/data" && !m.ReadOnly {
					mounted = true
				}
			}
			if r.Container.Env["SB_SHELL_BACKEND"] != "off" {
				t.Errorf("SilverBullet's shell backend is %q: it runs commands in the container from the browser",
					r.Container.Env["SB_SHELL_BACKEND"])
			}
		}
	}
	if !mounted {
		t.Errorf("notes-silverbullet does not mount %s at /data", folder)
	}
}
