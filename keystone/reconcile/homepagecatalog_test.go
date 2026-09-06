package reconcile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
)

// TestEveryShippedRouteHasAPresentationRow is the standing contract for
// the dashboard's integration path.
//
// homepageApps is a table in Go, deliberately not a field on the Route
// (ADR-W021 A6): a package declaring its own icon and blurb restates
// what its own Route already implies. The cost of that choice is that
// nothing in the package makes an author add the row — so a new
// capability ships, appears on the family's dashboard as a dashed
// placeholder box with the internal capability slug under it, and no
// test fails. This is that test.
//
// It walks packages/ rather than restating a list, for the reason
// nodePrereqFiles is a function rather than a literal: a second copy is
// exactly how a new package gets left out of the coverage meant to bound
// it.
//
// Two exclusions, and they are the SAME two the renderer applies, read
// from the package rather than assumed: a `portal` Route is the login
// page, and the dashboard's own Route is the page being rendered.
// Neither is ever listed, so neither needs a row.
func TestEveryShippedRouteHasAPresentationRow(t *testing.T) {
	root, err := filepath.Abs("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), "package.yaml")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		pkg, err := pkgspec.Load(path)
		if err != nil {
			t.Errorf("%s: %v", e.Name(), err)
			continue
		}
		if pkg.Metadata.Capability == "dashboard" {
			continue
		}
		for _, r := range pkgspec.Routes(pkg) {
			if r.Auth == "portal" {
				continue
			}
			container := r.UpstreamContainer()
			if container == "" {
				// An "ip:port" upstream is something Wildstone does not
				// run and cannot be expected to describe.
				continue
			}
			checked++
			if _, ok := homepageApps[container]; !ok {
				t.Errorf("packages/%s declares Route %q upstream %q, and homepageApps has no row for %q.\n"+
					"Every shipped service is presented deliberately: add\n"+
					"\t%q: {\"<Title>\", \"<dashboard-icons slug>\", \"<one line for the family>\"},\n"+
					"to homepageApps in homepageservices.go. Without it the card renders as a dashed\n"+
					"placeholder with the capability slug as its description.",
					e.Name(), r.Name, r.Upstream, container, container)
			}
		}
	}
	// The walk itself is load-bearing: a broken one passes silently.
	if checked < 4 {
		t.Fatalf("checked only %d routed services under %s — the walk is broken, not the packages", checked, root)
	}
}
