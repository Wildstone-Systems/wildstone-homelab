package api

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
)

// The Console shows each app's own logo. The slug comes from the table
// the household dashboard already renders from, so the two surfaces
// cannot disagree about what an app looks like — and so there is never a
// second name-to-slug map in the Console to drift out of step.
func TestPackageIconComesFromTheDashboardTable(t *testing.T) {
	pkg := &pkgspec.Package{Templates: []string{
		"apiVersion: wildstone.dev/v1\nkind: Container\nmetadata: {name: jellyfin}\n",
		"apiVersion: wildstone.dev/v1\nkind: Route\nspec:\n  upstream: \"container/jellyfin:8096\"\n",
	}}
	if got := packageIcon(pkg); got != "jellyfin" {
		t.Fatalf("packageIcon = %q, want the dashboard's own slug %q", got, "jellyfin")
	}
}

// A package that serves no Route — an adapter, a model runner — has no
// logo to show, and must get the generic fallback rather than a guess.
// A guessed slug is right often enough to be tempting and wrong
// silently: it renders as a broken image.
func TestPackageWithNoRouteGetsTheFallback(t *testing.T) {
	pkg := &pkgspec.Package{Templates: []string{
		"apiVersion: wildstone.dev/v1\nkind: Container\nmetadata: {name: ollama}\n",
	}}
	if got := packageIcon(pkg); got != "mdi-application-outline" {
		t.Fatalf("packageIcon = %q, want the generic fallback", got)
	}
}

// An upstream that is not a container — a Route pointing at an ip:port —
// is not an app with a logo either.
func TestNonContainerUpstreamGetsTheFallback(t *testing.T) {
	pkg := &pkgspec.Package{Templates: []string{
		"apiVersion: wildstone.dev/v1\nkind: Route\nspec:\n  upstream: \"192.168.1.4:8080\"\n",
	}}
	if got := packageIcon(pkg); got != "mdi-application-outline" {
		t.Fatalf("packageIcon = %q, want the generic fallback", got)
	}
}
