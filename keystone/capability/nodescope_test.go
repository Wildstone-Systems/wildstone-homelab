package capability

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// identityNodePkg mirrors the shape every app package now uses for the
// resources that land on the identity provider's node rather than its
// own: metadata.node is `or identity_node node`.
func identityNodePkg() *pkgspec.Package {
	return &pkgspec.Package{
		Metadata: pkgspec.Metadata{Name: "photos-immich", Version: "1.0.0", Capability: "photos"},
		Params: []pkgspec.ParamDecl{
			{Name: "node", Required: true},
			{Name: "identity_node"},
		},
		Templates: []string{
			`apiVersion: wildstone.dev/v1
kind: LdapGroup
metadata: { name: photos-users, node: "{{ or .Params.identity_node .Params.node }}" }
spec: { name: photos_users }
`,
		},
	}
}

// TestIdentityNodeDefaultsToTheOwnNode is the single-node homelab, which
// is the overwhelmingly common case and the one an operator should not
// have to answer a question about.
func TestIdentityNodeDefaultsToTheOwnNode(t *testing.T) {
	res, err := (&Engine{}).Expand(identityNodePkg(), map[string]string{"node": "node-a"}, NewCatalog())
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got := res[0].Metadata.Node; got != "node-a" {
		t.Errorf("Node = %q, want node-a", got)
	}
}

// And the multi-node case: identity elsewhere, which is the reason the
// resource cannot simply inherit the app's own node. The OidcClient
// fragment has to be written where Authelia reads it.
func TestIdentityNodeOverridesTheOwnNode(t *testing.T) {
	res, err := (&Engine{}).Expand(identityNodePkg(),
		map[string]string{"node": "node-a", "identity_node": "node-id"}, NewCatalog())
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got := res[0].Metadata.Node; got != "node-id" {
		t.Errorf("Node = %q, want node-id", got)
	}
	if res[0].Kind != resource.KindLdapGroup {
		t.Errorf("Kind = %s", res[0].Kind)
	}
}
