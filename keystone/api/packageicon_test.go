package api

import (
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
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

// Flow syntax is ordinary YAML and a package is free to use it. The
// first cut regexed the raw template text for a line beginning
// `upstream:`, so this silently fell back to the generic icon.
func TestFlowStyleRouteStillResolvesItsIcon(t *testing.T) {
	pkg := &pkgspec.Package{Templates: []string{
		"apiVersion: wildstone.dev/v1\nkind: Route\nmetadata: {name: media}\nspec: {host: a, upstream: \"container/jellyfin:8096\"}\n",
	}}
	if got := packageIcon(pkg); got != "jellyfin" {
		t.Fatalf("packageIcon = %q, want %q — flow syntax is still a Route", got, "jellyfin")
	}
}

// An upstream built from a param resolves through the param's default,
// which is what pkgspec.Routes already does for every other reader.
func TestTemplatedUpstreamResolvesThroughDefaults(t *testing.T) {
	pkg := &pkgspec.Package{
		Params: []pkgspec.ParamDecl{{Name: "app", Default: "jellyfin"}},
		Templates: []string{
			"apiVersion: wildstone.dev/v1\nkind: Route\nmetadata:\n  name: media\nspec:\n  upstream: \"container/{{ .Params.app }}:8096\"\n",
		},
	}
	if got := packageIcon(pkg); got != "jellyfin" {
		t.Fatalf("packageIcon = %q, want %q", got, "jellyfin")
	}
}

// A package serving several routes takes the one whose container is an
// app somebody would recognise, not simply the first.
func TestTheRecognisableRouteWins(t *testing.T) {
	pkg := &pkgspec.Package{Templates: []string{
		"apiVersion: wildstone.dev/v1\nkind: Route\nmetadata: {name: api}\nspec: {upstream: \"container/photos-sidecar:3001\"}\n",
		"apiVersion: wildstone.dev/v1\nkind: Route\nmetadata: {name: web}\nspec: {upstream: \"container/immich-server:2283\"}\n",
	}}
	if got := packageIcon(pkg); got != "immich" {
		t.Fatalf("packageIcon = %q, want %q", got, "immich")
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
