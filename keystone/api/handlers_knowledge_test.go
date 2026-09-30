package api

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func knowledgeConfig(owner, filename, content string) resource.Resource {
	return resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindConfigFile,
		Metadata:   resource.Metadata{Name: "knowledge-settings", Node: "n1", Owner: owner},
		ConfigFile: &resource.ConfigFileSpec{Filename: filename, Content: content}}
}

func TestTheKnowledgeFolderComesOnlyFromItsOwnCapability(t *testing.T) {
	root, err := knowledgeRootIn([]resource.Resource{
		// Another capability rendering the same filename is not the
		// knowledge capability saying where its folder is.
		knowledgeConfig("capability/notes", "knowledge.yaml", "root: /srv/wildstone/elsewhere\n"),
		knowledgeConfig("capability/knowledge", "other.yaml", "root: /srv/wildstone/wrong\n"),
		knowledgeConfig("capability/knowledge", "knowledge.yaml", "# managed\nroot: /srv/wildstone/knowledge\n"),
	})
	if err != nil || root != "/srv/wildstone/knowledge" {
		t.Fatalf("root = %q, %v", root, err)
	}
	if root, err := knowledgeRootIn(nil); err != nil || root != "" {
		t.Fatalf("nothing installed gave %q, %v", root, err)
	}
	if _, err := knowledgeRootIn([]resource.Resource{
		knowledgeConfig("capability/knowledge", "knowledge.yaml", "root: [unclosed"),
	}); err == nil {
		t.Fatal("a malformed settings file fell back to something")
	}
}
