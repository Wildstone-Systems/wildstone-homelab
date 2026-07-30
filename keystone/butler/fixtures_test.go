package butler

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

func testDigest() string {
	return "ghcr.io/example/app@sha256:" + strings.Repeat("b", 64)
}

// planted64HexSecret is a 64-char hex string shaped like a real secret
// value (sha256-digest length) planted directly into a resource field, to
// verify redaction catches it regardless of field name.
const planted64HexSecret = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9"

func newTestStore(t *testing.T) *statefile.Store {
	t.Helper()
	s, err := statefile.New(t.TempDir())
	if err != nil {
		t.Fatalf("statefile.New: %v", err)
	}
	return s
}

// seedResource puts+commits r into s.
func seedResource(t *testing.T, s *statefile.Store, r resource.Resource) {
	t.Helper()
	if err := s.Put(r); err != nil {
		t.Fatalf("seed Put %s/%s: %v", r.Kind, r.Metadata.Name, err)
	}
	if err := s.Commit("test", "seed", "seed fixture"); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}
}

func routeResource(name, host, upstream, auth, classification string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindRoute,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/photos"},
		Route: &resource.RouteSpec{
			Host:           host,
			Upstream:       upstream,
			Auth:           auth,
			Classification: classification,
		},
	}
}

func containerResourceWithSecret(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindContainer,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/immich", Node: "node-a"},
		Container: &resource.ContainerSpec{
			Image: testDigest(),
			Env: map[string]string{
				"DB_PASSWORD_HASH": planted64HexSecret,
			},
		},
	}
}

func oidcClientResource(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindOidcClient,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/immich"},
		OidcClient: &resource.OidcClientSpec{
			ClientId:     name,
			ClientName:   name,
			RedirectUris: []string{"https://photos.example.com/callback"},
			AuthPolicy:   "two_factor",
			SecretRef:    "secret/immich-db",
		},
	}
}
