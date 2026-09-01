package reconcile

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

const cfgPrefix = "/etc/wildstone"

func cfgFile(name, capability, filename, content string) resource.Resource {
	return resource.Resource{
		ApiVersion: "wildstone.dev/v1",
		Kind:       resource.KindConfigFile,
		Metadata:   resource.Metadata{Name: name, Node: "n1", Owner: "capability/" + capability},
		ConfigFile: &resource.ConfigFileSpec{Filename: filename, Content: content},
	}
}

func byKey(rs ...resource.Resource) map[resKey]resource.Resource {
	m := make(map[resKey]resource.Resource, len(rs))
	for _, r := range rs {
		m[resKey{r.Kind, r.Metadata.Name}] = r
	}
	return m
}

// autheliaLike is the shape that matters: a container mounting its
// capability's subtree of the managed prefix.
func autheliaLike() *resource.ContainerSpec {
	return &resource.ContainerSpec{
		Image: "authelia/authelia@sha256:abc",
		Mounts: []resource.Mount{
			{Source: "authelia-config", Target: "/config"},
			{Source: "/etc/wildstone/identity", Target: "/etc/wildstone/identity", ReadOnly: true},
			{Source: "/etc/wildstone/authelia", Target: "/etc/wildstone/authelia", ReadOnly: true},
		},
	}
}

// TestConfigChangeChangesTheSpecHash is the whole point: editing a file
// a container mounts has to look like a spec change, or EnsureContainer
// is a no-op and the container keeps serving what it read at start.
func TestConfigChangeChangesTheSpecHash(t *testing.T) {
	spec := autheliaLike()

	before := managedConfigContents(cfgPrefix, byKey(cfgFile("authelia-configuration", "identity", "configuration.yml", "theme: light\n")))
	after := managedConfigContents(cfgPrefix, byKey(cfgFile("authelia-configuration", "identity", "configuration.yml", "theme: dark\n")))

	h1 := specHash(spec, ContainerNetwork, configDigest(spec, before))
	h2 := specHash(spec, ContainerNetwork, configDigest(spec, after))
	if h1 == h2 {
		t.Fatal("editing a mounted config left the spec hash unchanged: the agent will treat the " +
			"container as converged and never restart it, so the new config is never read")
	}
}

// TestNewOidcClientChangesAutheliaSpecHash covers the reconciler-rendered
// half. Installing an app registers an OidcClient, which lands in the
// merged clients file — and until the provider re-reads it, the new app's
// login is broken while every test is green.
func TestNewOidcClientChangesAutheliaSpecHash(t *testing.T) {
	spec := autheliaLike()

	none := managedConfigContents(cfgPrefix, byKey())
	one := managedConfigContents(cfgPrefix, byKey(resource.Resource{
		Kind:       resource.KindOidcClient,
		Metadata:   resource.Metadata{Name: "immich", Node: "n1", Owner: "capability/photos"},
		OidcClient: &resource.OidcClientSpec{ClientId: "immich", SecretRef: "secret/immich-oidc"},
	}))

	if specHash(spec, ContainerNetwork, configDigest(spec, none)) ==
		specHash(spec, ContainerNetwork, configDigest(spec, one)) {
		t.Fatal("registering an OidcClient left Authelia's spec hash unchanged: the client is written " +
			"to clients.yml and the running provider never learns about it")
	}
}

// TestContainerWithNoManagedConfigIsUnaffected is the no-churn property,
// pinned against a value computed BEFORE the config dimension existed. A
// container that reads no managed config must hash exactly as it did, so
// landing this change replaces only the containers it actually applies
// to instead of the whole fleet.
func TestContainerWithNoManagedConfigIsUnaffected(t *testing.T) {
	spec := &resource.ContainerSpec{Image: "nginx@sha256:abc", Restart: "unless-stopped"}

	contents := managedConfigContents(cfgPrefix, byKey(cfgFile("a", "identity", "configuration.yml", "theme: light\n")))
	if d := configDigest(spec, contents); d != "" {
		t.Errorf("a container with no bind mount under the managed prefix got digest %q, want empty", d)
	}

	const beforeThisChange = "e2d3456039e9a85a"
	if got := specHash(spec, ContainerNetwork, configDigest(spec, contents)); got != beforeThisChange {
		t.Errorf("spec hash for a config-less container changed to %q (was %q): every such container "+
			"in the fleet would be replaced once for no reason", got, beforeThisChange)
	}
}

// TestRouteFragmentsAreNotInTheDigest guards the deliberate exclusion.
// Both shipped ingress profiles run Caddy with --watch, so a fragment
// write IS the reload; folding routes in here would trade a free,
// zero-downtime reload for a container replacement on every Route change.
func TestRouteFragmentsAreNotInTheDigest(t *testing.T) {
	ingress := &resource.ContainerSpec{
		Image:  "caddy@sha256:abc",
		Mounts: []resource.Mount{{Source: cfgPrefix, Target: cfgPrefix, ReadOnly: true}},
	}
	route := resource.Resource{
		Kind:     resource.KindRoute,
		Metadata: resource.Metadata{Name: "photos", Node: "n1", Owner: "capability/photos"},
		Route:    &resource.RouteSpec{Host: "photos.example.internal", Upstream: "container/immich-server:2283"},
	}
	before := configDigest(ingress, managedConfigContents(cfgPrefix, byKey()))
	after := configDigest(ingress, managedConfigContents(cfgPrefix, byKey(route)))
	if before != after {
		t.Errorf("adding a Route changed the ingress container's config digest (%q -> %q): it would be "+
			"replaced on every Route change instead of letting Caddy --watch reload it", before, after)
	}

	for p := range managedConfigContents(cfgPrefix, byKey()) {
		if p == baseCaddyfilePath(cfgPrefix) || p == placeholderFragmentPath(cfgPrefix) {
			t.Errorf("%s is in the config digest, for the same reason", p)
		}
	}
}

// TestDigestIgnoresAnAdjacentDirectory: a mount of <prefix>/identity must
// not pick up <prefix>/identity-old. Segment comparison, not string
// prefix.
func TestDigestIgnoresAnAdjacentDirectory(t *testing.T) {
	spec := &resource.ContainerSpec{
		Image:  "x@sha256:abc",
		Mounts: []resource.Mount{{Source: "/etc/wildstone/identity", Target: "/etc/wildstone/identity"}},
	}
	contents := map[string]string{"/etc/wildstone/identity-old/configuration.yml": "stale"}
	if d := configDigest(spec, contents); d != "" {
		t.Errorf("a mount of /etc/wildstone/identity picked up /etc/wildstone/identity-old (digest %q)", d)
	}
}

// TestDigestIsOrderIndependent: managedConfigContents builds from a map,
// so an unstable digest would make every tick see drift it just fixed.
func TestDigestIsOrderIndependent(t *testing.T) {
	spec := autheliaLike()
	a := cfgFile("one", "identity", "configuration.yml", "a\n")
	b := cfgFile("two", "identity", "extra.yml", "b\n")

	first := configDigest(spec, managedConfigContents(cfgPrefix, byKey(a, b)))
	for i := 0; i < 20; i++ {
		if got := configDigest(spec, managedConfigContents(cfgPrefix, byKey(b, a))); got != first {
			t.Fatalf("digest is not stable across map iteration order: %q vs %q", got, first)
		}
	}
}
