package api

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func res(kind resource.Kind, name, owner, pkg string) resource.Resource {
	return resource.Resource{Kind: kind, Metadata: resource.Metadata{
		Name: name, Owner: owner, Package: pkg, Node: "n1"}}
}

func names(ds []deletionRef) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Kind+"/"+d.Name)
	}
	return out
}

// Disabling one profile touches only its own resources.
func TestDisableRemovesOnlyTheNamedPackage(t *testing.T) {
	current := []resource.Resource{
		res(resource.KindContainer, "kavita", "capability/books", "books-kavita"),
		res(resource.KindRoute, "kavita", "capability/books", "books-kavita"),
		res(resource.KindContainer, "calibre-web", "capability/books", "books-calibre-web"),
		res(resource.KindRoute, "books", "capability/books", "books-calibre-web"),
	}
	got := names(deletesForDisable("books-kavita", "capability/books", current, nil))
	if len(got) != 2 {
		t.Fatalf("want only kavita's two resources, got %v", got)
	}
	for _, g := range got {
		if g == "Container/calibre-web" || g == "Route/books" {
			t.Errorf("a sibling profile's resource was selected: %s", g)
		}
	}
}

// The first client-side attempt applied the legacy rule per capability
// to EVERY capability with no stamped resources, so disabling one book
// app selected Authelia, LLDAP, Caddy and the dashboard — 34 resources
// from a command naming one app.
func TestDisableNeverLeavesItsOwnCapability(t *testing.T) {
	current := []resource.Resource{
		res(resource.KindContainer, "kavita", "capability/books", "books-kavita"),
		// Other capabilities, all provenance-free the way a deployment
		// installed before the Package field is.
		res(resource.KindContainer, "authelia", "capability/identity", ""),
		res(resource.KindSecret, "lldap-jwt-secret", "capability/identity", ""),
		res(resource.KindContainer, "caddy-tailnet-ts", "capability/ingress", ""),
		res(resource.KindContainer, "homepage", "capability/dashboard", ""),
	}
	for _, d := range deletesForDisable("books-kavita", "capability/books", current, nil) {
		if d.Name != "kavita" {
			t.Errorf("selected %s/%s, which belongs to another capability", d.Kind, d.Name)
		}
	}
}

// A resource the SURVIVING sibling still renders is never deleted. Both
// book packages declare Secret/restic-books — the restic repository
// password — and whichever installed last owns the stamp on it.
func TestDisableKeepsWhatASurvivingSiblingNeeds(t *testing.T) {
	current := []resource.Resource{
		res(resource.KindContainer, "kavita", "capability/books", "books-kavita"),
		res(resource.KindSecret, "restic-books", "capability/books", "books-kavita"),
		res(resource.KindContainer, "calibre-web", "capability/books", "books-calibre-web"),
	}
	survives := map[[2]string]bool{
		{"Secret", "restic-books"}:   true,
		{"Container", "calibre-web"}: true,
	}
	for _, d := range deletesForDisable("books-kavita", "capability/books", current, survives) {
		if d.Name == "restic-books" {
			t.Fatal("deleted the repository password the surviving book app still uses")
		}
	}
}

// Data belongs to the household from the moment it lands (ADR-W004 A4),
// and the reconciler refuses a Volume Delete outright — so proposing one
// never removed data, it only guaranteed a step that failed every tick.
func TestDisableNeverProposesDeletingData(t *testing.T) {
	current := []resource.Resource{
		res(resource.KindVolume, "kavita-config", "capability/books", "books-kavita"),
		res(resource.KindSeedFile, "kavita-seed", "capability/books", "books-kavita"),
		res(resource.KindContainer, "kavita", "capability/books", "books-kavita"),
	}
	got := names(deletesForDisable("books-kavita", "capability/books", current, nil))
	if len(got) != 1 || got[0] != "Container/kavita" {
		t.Fatalf("want only the container, got %v", got)
	}
}

// A deployment installed before provenance existed still disables: with
// nothing else claiming the capability, its unstamped resources are this
// package's.
func TestDisableStillWorksWithoutProvenance(t *testing.T) {
	current := []resource.Resource{
		res(resource.KindContainer, "kavita", "capability/books", ""),
		res(resource.KindRoute, "kavita", "capability/books", ""),
	}
	if got := names(deletesForDisable("books-kavita", "capability/books", current, nil)); len(got) != 2 {
		t.Fatalf("a legacy install must still be removable, got %v", got)
	}
}

// ...but once a sibling HAS claimed the capability, an unstamped
// resource is ambiguous, and deleting it on a guess is the wrong way to
// be wrong.
func TestDisableLeavesAmbiguousLegacyResourcesAlone(t *testing.T) {
	current := []resource.Resource{
		res(resource.KindContainer, "calibre-web", "capability/books", "books-calibre-web"),
		res(resource.KindSecret, "restic-books", "capability/books", ""),
	}
	for _, d := range deletesForDisable("books-kavita", "capability/books", current, nil) {
		if d.Name == "restic-books" {
			t.Fatal("an unstamped resource was claimed while a sibling also claims the capability")
		}
	}
}

// A secret can be read from a managed FILE rather than from a
// container's environment, and photos-immich is why: Immich does not
// read OAuth from the environment, so its client secret lives only
// inside immich.json. Scanning env found no dependent, so rotating it
// rewrote nothing and reported success while Immich went on presenting
// the previous client secret.
func TestSecretReadersFindConfigFilesAndWhatMountsThem(t *testing.T) {
	s := &Server{ManagedPrefix: "/etc/wildstone"}
	desired := []resource.Resource{
		{Kind: resource.KindConfigFile, Metadata: resource.Metadata{Name: "immich-config", Owner: "capability/photos", Node: "n1"},
			ConfigFile: &resource.ConfigFileSpec{Filename: "immich.json",
				Content: `{"oauth":{"clientSecret":"secret/photos-oidc"}}`}},
		{Kind: resource.KindContainer, Metadata: resource.Metadata{Name: "immich-server", Owner: "capability/photos", Node: "n1"},
			Container: &resource.ContainerSpec{Image: "x", Mounts: []resource.Mount{{Source: "/etc/wildstone/photos", Target: "/etc/wildstone/photos"}}}},
		// Same node, reads neither.
		{Kind: resource.KindContainer, Metadata: resource.Metadata{Name: "immich-redis", Owner: "capability/photos", Node: "n1"},
			Container: &resource.ContainerSpec{Image: "x"}},
	}
	containers, files, err := s.secretReaders(desired, "n1", "photos-oidc")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Metadata.Name != "immich-config" {
		t.Fatalf("the ConfigFile holding the secret was not found: %v", files)
	}
	if len(containers) != 1 || containers[0] != "immich-server" {
		t.Fatalf("containers = %v, want exactly [immich-server]", containers)
	}
}

// Env references still work, and a container is never listed twice when
// it both names the secret and mounts a file holding it.
func TestSecretReadersDoNotDoubleCount(t *testing.T) {
	s := &Server{ManagedPrefix: "/etc/wildstone"}
	desired := []resource.Resource{
		{Kind: resource.KindConfigFile, Metadata: resource.Metadata{Name: "cfg", Owner: "capability/photos", Node: "n1"},
			ConfigFile: &resource.ConfigFileSpec{Filename: "a.json", Content: "secret/photos-oidc"}},
		{Kind: resource.KindContainer, Metadata: resource.Metadata{Name: "immich-server", Owner: "capability/photos", Node: "n1"},
			Container: &resource.ContainerSpec{Image: "x",
				Env:    map[string]string{"OAUTH": "secret/photos-oidc"},
				Mounts: []resource.Mount{{Source: "/etc/wildstone/photos", Target: "/x"}}}},
	}
	containers, _, err := s.secretReaders(desired, "n1", "photos-oidc")
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 {
		t.Fatalf("containers = %v, want one entry", containers)
	}
}

// Another node's readers are not this node's.
func TestSecretReadersAreNodeScoped(t *testing.T) {
	s := &Server{ManagedPrefix: "/etc/wildstone"}
	desired := []resource.Resource{
		{Kind: resource.KindContainer, Metadata: resource.Metadata{Name: "elsewhere", Owner: "capability/photos", Node: "n2"},
			Container: &resource.ContainerSpec{Image: "x", Env: map[string]string{"OAUTH": "secret/photos-oidc"}}},
	}
	containers, files, err := s.secretReaders(desired, "n1", "photos-oidc")
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 0 || len(files) != 0 {
		t.Fatalf("another node's readers leaked in: %v %v", containers, files)
	}
}
