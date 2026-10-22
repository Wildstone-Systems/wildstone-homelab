package reconcile

import (
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
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

	h1 := specHash(spec, ContainerNetwork, configDigest(cfgPrefix, spec, before), nil)
	h2 := specHash(spec, ContainerNetwork, configDigest(cfgPrefix, spec, after), nil)
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

	if specHash(spec, ContainerNetwork, configDigest(cfgPrefix, spec, none), nil) ==
		specHash(spec, ContainerNetwork, configDigest(cfgPrefix, spec, one), nil) {
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
	if d := configDigest(cfgPrefix, spec, contents); d != "" {
		t.Errorf("a container with no bind mount under the managed prefix got digest %q, want empty", d)
	}

	const beforeThisChange = "e2d3456039e9a85a"
	if got := specHash(spec, ContainerNetwork, configDigest(cfgPrefix, spec, contents), nil); got != beforeThisChange {
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
	before := configDigest(cfgPrefix, ingress, managedConfigContents(cfgPrefix, byKey()))
	after := configDigest(cfgPrefix, ingress, managedConfigContents(cfgPrefix, byKey(route)))
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
	if d := configDigest(cfgPrefix, spec, contents); d != "" {
		t.Errorf("a mount of /etc/wildstone/identity picked up /etc/wildstone/identity-old (digest %q)", d)
	}
}

// TestDigestIsOrderIndependent: managedConfigContents builds from a map,
// so an unstable digest would make every tick see drift it just fixed.
func TestDigestIsOrderIndependent(t *testing.T) {
	spec := autheliaLike()
	a := cfgFile("one", "identity", "configuration.yml", "a\n")
	b := cfgFile("two", "identity", "extra.yml", "b\n")

	first := configDigest(cfgPrefix, spec, managedConfigContents(cfgPrefix, byKey(a, b)))
	for i := 0; i < 20; i++ {
		if got := configDigest(cfgPrefix, spec, managedConfigContents(cfgPrefix, byKey(b, a))); got != first {
			t.Fatalf("digest is not stable across map iteration order: %q vs %q", got, first)
		}
	}
}

// F73: a managed config file rewritten without restarting the container
// that reads it applies and has no effect. Hit twice in one evening —
// Authelia enforced stale access rules for 49 minutes after an approved
// plan corrected them, and Homepage served a stale service list for an
// hour after a capability was installed. Both times the file on disk
// matched desired state, so nothing was drifting and everything reported
// healthy.
//
// The mechanism to prevent it already existed; these two files were
// simply not in it.

func TestAutheliaAccessRulesReachTheRunningProvider(t *testing.T) {
	authelia := &resource.ContainerSpec{
		Mounts: []resource.Mount{{Source: cfgPrefix + "/authelia", Target: cfgPrefix + "/authelia", ReadOnly: true}},
	}
	before := configDigest(cfgPrefix, authelia, managedConfigContents(cfgPrefix, keyed(
		ingressRes("internal"),
	)))
	after := configDigest(cfgPrefix, authelia, managedConfigContents(cfgPrefix, keyed(
		ingressRes("internal"),
		tailnetRoute("home", "home.example.com"), // authelia_oidc: adds a rule
	)))
	if before == after {
		t.Error("adding an authenticated Route left Authelia's digest unchanged: it would go on enforcing the old access_control")
	}
}

func TestDashboardServiceListReachesTheRunningDashboard(t *testing.T) {
	homepage := &resource.ContainerSpec{
		Mounts: []resource.Mount{{Source: cfgPrefix + "/dashboard/services.yaml", Target: "/app/config/services.yaml", ReadOnly: true}},
	}
	before := configDigest(cfgPrefix, homepage, managedConfigContents(cfgPrefix, keyed(
		ingressRes("internal"),
	)))
	after := configDigest(cfgPrefix, homepage, managedConfigContents(cfgPrefix, keyed(
		ingressRes("internal"),
		tailnetRoute("jellyfin", "jellyfin.example.com"),
	)))
	if before == after {
		t.Error("installing a service left the dashboard's digest unchanged: it would go on showing the old list")
	}
}

// The other half of the same change, and the reason the first attempt at
// it was wrong: the ingress mounts the prefix ROOT to reach routes/, and
// must not be replaced every time an unrelated capability's config
// changes. Caddy reloads what it cares about itself.
func TestPrefixRootMountIsASpectator(t *testing.T) {
	ingress := &resource.ContainerSpec{
		Mounts: []resource.Mount{{Source: cfgPrefix, Target: cfgPrefix, ReadOnly: true}},
	}
	before := configDigest(cfgPrefix, ingress, managedConfigContents(cfgPrefix, keyed(
		ingressRes("internal"),
	)))
	after := configDigest(cfgPrefix, ingress, managedConfigContents(cfgPrefix, keyed(
		ingressRes("internal"),
		tailnetRoute("home", "home.example.com"),
		tailnetRoute("jellyfin", "jellyfin.example.com"),
	)))
	if before != after {
		t.Errorf("the ingress was replaced by another capability's config change (%q -> %q)", before, after)
	}
}

// TestDeclaredDashboardSettingsAreNotOverwrittenInTheDigest pins #410: a
// package that declares its own dashboard/settings.yaml ConfigFile is the
// one owner of that path. Before the fix, managedConfigContents collected
// the declared ConfigFile and then unconditionally overwrote the same key
// with the generated Homepage settings, so the digest never reflected the
// declared owner — the container hash "converged" against content nothing
// ever wrote to disk (prereq writes and the digest disagreeing is exactly
// how the recurring drift proposal was manufactured).
func TestDeclaredDashboardSettingsAreNotOverwrittenInTheDigest(t *testing.T) {
	declared := "householdName: The Smiths\naccentHue: 210\nsearchEngine: duckduckgo\n"
	settings := cfgFile("dashboard-settings", "dashboard", "settings.yaml", declared)

	contents := managedConfigContents(cfgPrefix, byKey(settings))
	got, ok := contents[homepageSettingsPath(cfgPrefix)]
	if !ok {
		t.Fatal("declared dashboard settings missing from the digest contents entirely")
	}
	if got != declared {
		t.Errorf("dashboard/settings.yaml in the digest = %q, want the declared ConfigFile content %q", got, declared)
	}
}

// TestAnUnrelatedCapabilitysSettingsDoesNotSuppressHomepage guards the
// other direction of the same fix: the exclusion is keyed by the exact
// managed PATH a ConfigFile claims, not by filename. A different
// capability that happens to also ship a "settings.yaml" must not stop
// Homepage's own chrome from being generated, because the two files live
// in different capability subtrees and never collide on disk.
func TestAnUnrelatedCapabilitysSettingsDoesNotSuppressHomepage(t *testing.T) {
	other := cfgFile("pantryos-settings", "pantryos", "settings.yaml", "some: other-app-setting\n")

	contents := managedConfigContents(cfgPrefix, byKey(other))
	got, ok := contents[homepageSettingsPath(cfgPrefix)]
	if !ok {
		t.Fatal("Homepage's own settings.yaml was suppressed by an unrelated capability's file of the same name")
	}
	if want := string(renderHomepageSettings(nil)); got != want {
		t.Errorf("Homepage settings in the digest = %q, want the generated chrome %q", got, want)
	}
}

// keyed builds the desired-set map managedConfigContents expects.
func keyed(rs ...resource.Resource) map[resKey]resource.Resource {
	out := map[resKey]resource.Resource{}
	for _, r := range rs {
		out[resKey{r.Kind, r.Metadata.Name}] = r
	}
	return out
}
