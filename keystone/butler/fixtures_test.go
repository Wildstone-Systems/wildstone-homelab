package butler

import (
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

func testDigest() string {
	return "ghcr.io/example/app@sha256:" + strings.Repeat("b", 64)
}

// planted64HexSecret is a 64-char hex string (sha256-digest length) planted
// into a resource field to verify redaction strips it regardless of field
// name. It is DELIBERATELY a low-entropy repeating pattern, not a random
// value: redaction keys on the 32+-hex-char shape (which this still is), so
// the test stays meaningful, while the low entropy keeps the gitleaks CI
// scanner from flagging this test fixture as a real leaked secret. Do not
// "randomize" it — that reintroduces a false-positive gitleaks failure.
const planted64HexSecret = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

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
		Metadata:   resource.Metadata{Name: name, Owner: "capability/photos", Node: "node-a"},
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
		Metadata:   resource.Metadata{Name: name, Owner: "capability/immich", Node: "node-a"},
		OidcClient: &resource.OidcClientSpec{
			ClientId:     name,
			ClientName:   name,
			RedirectUris: []string{"https://photos.example.com/callback"},
			AuthPolicy:   "two_factor",
			SecretRef:    "secret/immich-db",
		},
	}
}
