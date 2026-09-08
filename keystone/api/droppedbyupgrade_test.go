package api

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func owned(kind resource.Kind, name, owner string) resource.Resource {
	return resource.Resource{Kind: kind, Metadata: resource.Metadata{Name: name, Owner: owner, Node: "n1"}}
}

// byPkg is `owned` plus the provenance that tells sibling profiles apart.
func byPkg(kind resource.Kind, name, owner, pkg string) resource.Resource {
	r := owned(kind, name, owner)
	r.Metadata.Package = pkg
	return r
}

// testPkg declares no templates, so ContainerNames is empty and
// packageIsTheInstalledOne answers true — which is what makes the
// legacy, provenance-free cases below behave exactly as they used to.
func testPkg(name, capability string) *pkgspec.Package {
	return &pkgspec.Package{Metadata: pkgspec.Metadata{Name: name, Capability: capability}}
}

func exclusivePkg(name, capability string) *pkgspec.Package {
	p := testPkg(name, capability)
	p.Metadata.Exclusive = true
	return p
}

// The live case. home-assistant 1.1.0 replaced a mounted ConfigFile with
// seeded files. Installing over the old version only ever added, so the
// statefile kept configfile/home-assistant-wildstone — reconciled
// forever, written to disk forever, mounted by nothing.
func TestUpgradeDropsWhatTheNewVersionNoLongerDeclares(t *testing.T) {
	current := []resource.Resource{
		owned(resource.KindContainer, "home-assistant", "capability/home-assistant"),
		owned(resource.KindConfigFile, "home-assistant-wildstone", "capability/home-assistant"),
	}
	rendered := []resource.Resource{
		owned(resource.KindContainer, "home-assistant", "capability/home-assistant"),
		owned(resource.KindSeedFile, "home-assistant-configuration", "capability/home-assistant"),
	}
	got := droppedByUpgrade(testPkg("home-assistant", "home-assistant"), current, rendered)
	if len(got) != 1 {
		t.Fatalf("want the dropped ConfigFile, got %v", got)
	}
	if got[0].Kind != "ConfigFile" || got[0].Name != "home-assistant-wildstone" {
		t.Errorf("wrong resource: %+v", got[0])
	}
}

// Data is never proposed for deletion. A Volume delete cannot apply at
// all (ADR-W004 A4), so proposing one would fail the whole plan every
// tick until F60 set it aside — taking the rest of the upgrade with it.
func TestUpgradeNeverProposesDeletingData(t *testing.T) {
	current := []resource.Resource{
		owned(resource.KindVolume, "ha-config", "capability/home-assistant"),
		owned(resource.KindSeedFile, "ha-seed", "capability/home-assistant"),
		owned(resource.KindRoute, "hass", "capability/home-assistant"),
	}
	// The new version declares none of them.
	got := droppedByUpgrade(testPkg("home-assistant", "home-assistant"), current, nil)
	for _, d := range got {
		if d.Kind == "Volume" || d.Kind == "SeedFile" {
			t.Errorf("data must never be proposed for deletion, got %+v", d)
		}
	}
	if len(got) != 1 || got[0].Kind != "Route" {
		t.Fatalf("want only the Route, got %v", got)
	}
}

// Another capability's resources are not this upgrade's business.
func TestUpgradeLeavesOtherCapabilitiesAlone(t *testing.T) {
	current := []resource.Resource{
		owned(resource.KindRoute, "photos", "capability/photos-immich"),
		owned(resource.KindRoute, "hass", "capability/home-assistant"),
	}
	got := droppedByUpgrade(testPkg("home-assistant", "home-assistant"), current, nil)
	if len(got) != 1 || got[0].Name != "hass" {
		t.Fatalf("want only this capability's Route, got %v", got)
	}
}

// A reinstall of the same version drops nothing, so an upgrade that
// changes no resources proposes no deletions.
func TestReinstallingTheSameVersionDropsNothing(t *testing.T) {
	same := []resource.Resource{
		owned(resource.KindContainer, "home-assistant", "capability/home-assistant"),
		owned(resource.KindRoute, "hass", "capability/home-assistant"),
	}
	if got := droppedByUpgrade(testPkg("home-assistant", "home-assistant"), same, same); len(got) != 0 {
		t.Fatalf("want none, got %v", got)
	}
}

// The same upgrade must propose the same plan twice: a set iterated in
// map order would produce a different document each time, and approval
// compares against what was proposed.
func TestDroppedResourcesAreOrdered(t *testing.T) {
	current := []resource.Resource{
		owned(resource.KindRoute, "b", "capability/x"),
		owned(resource.KindRoute, "a", "capability/x"),
		owned(resource.KindConfigFile, "c", "capability/x"),
	}
	first := droppedByUpgrade(testPkg("x", "x"), current, nil)
	for i := 0; i < 20; i++ {
		again := droppedByUpgrade(testPkg("x", "x"), current, nil)
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("order is not stable: %v vs %v", first, again)
			}
		}
	}
	if first[0].Kind != "ConfigFile" || first[1].Name != "a" {
		t.Errorf("want kind then name order, got %v", first)
	}
}

// The bug this scoping exists for. books-calibre-web and books-kavita
// are two profiles of capability "books", so both render resources
// owned by "capability/books". Keyed on owner alone, enabling one read
// the other's container and Route as leftovers of an upgrade — so
// installing one book app uninstalled the other, which nobody chose.
func TestInstallingOneProfileLeavesItsSiblingRunning(t *testing.T) {
	current := []resource.Resource{
		byPkg(resource.KindContainer, "kavita", "capability/books", "books-kavita"),
		byPkg(resource.KindRoute, "kavita", "capability/books", "books-kavita"),
	}
	rendered := []resource.Resource{
		byPkg(resource.KindContainer, "calibre-web", "capability/books", "books-calibre-web"),
		byPkg(resource.KindRoute, "books", "capability/books", "books-calibre-web"),
	}
	got := droppedByUpgrade(testPkg("books-calibre-web", "books"), current, rendered)
	if len(got) != 0 {
		t.Fatalf("a sibling profile must survive an install; proposed deleting %v", got)
	}
}

// Upgrading a profile still converges to its own new definition — the
// scoping narrows which resources are considered, not whether upgrade
// pruning happens at all.
func TestUpgradingAProfileStillDropsItsOwnLeftovers(t *testing.T) {
	current := []resource.Resource{
		byPkg(resource.KindContainer, "calibre-web", "capability/books", "books-calibre-web"),
		byPkg(resource.KindConfigFile, "calibre-old", "capability/books", "books-calibre-web"),
		byPkg(resource.KindContainer, "kavita", "capability/books", "books-kavita"),
	}
	rendered := []resource.Resource{
		byPkg(resource.KindContainer, "calibre-web", "capability/books", "books-calibre-web"),
	}
	got := droppedByUpgrade(testPkg("books-calibre-web", "books"), current, rendered)
	if len(got) != 1 || got[0].Name != "calibre-old" {
		t.Fatalf("want only this package's dropped ConfigFile, got %v", got)
	}
}

// An exclusive capability replaces its sibling, and the replacement is
// in the plan to be read rather than a side effect of the install.
func TestExclusiveCapabilityStillReplacesItsSibling(t *testing.T) {
	current := []resource.Resource{
		byPkg(resource.KindContainer, "pihole", "capability/dns", "dns-pihole"),
		byPkg(resource.KindRoute, "pihole", "capability/dns", "dns-pihole"),
	}
	rendered := []resource.Resource{
		byPkg(resource.KindContainer, "adguard", "capability/dns", "dns-adguard"),
	}
	got := droppedByUpgrade(exclusivePkg("dns-adguard", "dns"), current, rendered)
	if len(got) != 2 {
		t.Fatalf("an exclusive capability must propose replacing its sibling, got %v", got)
	}
}

// An exclusive capability replaces a LEGACY sibling too.
//
// Scoping the unstamped case to packageIsTheInstalledOne alone was a
// regression on the owner-keyed sweep it replaced: switching dns-pihole
// to dns-adguard on a deployment written before provenance existed
// skipped every unstamped container and route, so AdGuard installed
// while Pi-hole kept running and the two fought over :53.
func TestExclusiveReplacesAnUnstampedSibling(t *testing.T) {
	current := []resource.Resource{
		owned(resource.KindContainer, "pihole", "capability/dns"),
		owned(resource.KindRoute, "pihole", "capability/dns"),
	}
	rendered := []resource.Resource{
		byPkg(resource.KindContainer, "adguard", "capability/dns", "dns-adguard"),
	}
	got := droppedByUpgrade(exclusivePkg("dns-adguard", "dns"), current, rendered)
	if len(got) != 2 {
		t.Fatalf("an exclusive capability must replace an unstamped sibling, got %v", got)
	}
}

// The same case on a PLURAL capability keeps both, which is the whole
// point of the scoping — the two rules must not collapse into each other.
func TestPluralDoesNotReplaceAnUnstampedSibling(t *testing.T) {
	current := []resource.Resource{
		owned(resource.KindContainer, "kavita", "capability/books"),
	}
	rendered := []resource.Resource{
		byPkg(resource.KindContainer, "calibre-web", "capability/books", "books-calibre-web"),
	}
	// books-calibre-web declares a container that is not present, so it
	// is not "the installed one" — exactly the shape that regressed.
	pkg := testPkg("books-calibre-web", "books")
	pkg.Templates = []string{"apiVersion: wildstone.dev/v1\nkind: Container\nmetadata: {name: calibre-web}\nspec: {image: x}\n"}
	if got := droppedByUpgrade(pkg, current, rendered); len(got) != 0 {
		t.Fatalf("a plural capability must keep an unstamped sibling, got %v", got)
	}
}
