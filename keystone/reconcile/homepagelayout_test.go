package reconcile

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// TestHomepageLayoutIsAGridNotAList is the F87 headline. Homepage's
// default group style is a list — one service per line, one column —
// which is exactly what the alpha VM showed. `style: row` plus a column
// count is the whole difference between that and a grid of cards.
func TestHomepageLayoutIsAGridNotAList(t *testing.T) {
	got := string(renderHomepageSettings([]resource.Resource{
		ownedRouteFull("jellyfin", "media", "media.example.com", "container/jellyfin:8096", "family_public"),
	}))
	for _, want := range []string{
		"layout:\n",
		"  Household:\n    style: row\n    columns: 3\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// TestHomepageLayoutNamesOnlyGroupsThatExist. The layout and the service
// list are two files that must agree about group names; both are
// derived from homepageEntries so they cannot drift, and a layout entry
// for a group with no services draws an empty heading.
func TestHomepageLayoutNamesOnlyGroupsThatExist(t *testing.T) {
	family := []resource.Resource{
		ownedRouteFull("jellyfin", "media", "media.example.com", "container/jellyfin:8096", "family_public"),
	}
	if got := string(renderHomepageSettings(family)); strings.Contains(got, "Admin:") {
		t.Errorf("laid out a group with no services:\n%s", got)
	}
	withAdmin := append(family,
		ownedRouteFull("uptime", "monitoring", "uptime.example.com", "container/uptime-kuma:3001", "admin_tailscale"))
	got := string(renderHomepageSettings(withAdmin))
	for _, want := range []string{"  Household:\n", "  Admin:\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	// Same order the service list renders them in.
	if strings.Index(got, "  Household:") > strings.Index(got, "  Admin:") {
		t.Errorf("layout order disagrees with the service list:\n%s", got)
	}
}

// TestHomepageSettingsWithNoServicesOmitsTheLayout: an empty `layout:`
// key is not the same as an absent one.
func TestHomepageSettingsWithNoServicesOmitsTheLayout(t *testing.T) {
	got := string(renderHomepageSettings(nil))
	if strings.Contains(got, "layout:") {
		t.Errorf("emitted an empty layout block:\n%s", got)
	}
	if !strings.Contains(got, "title: Household") {
		t.Errorf("the rest of the settings must still render:\n%s", got)
	}
}

// TestHomepageSettingsAreStable: every managed file is a pure function
// of desired state, or the prereq rewrite replaces the container on
// every tick.
func TestHomepageSettingsAreStable(t *testing.T) {
	res := []resource.Resource{
		ownedRouteFull("uptime", "monitoring", "uptime.example.com", "container/uptime-kuma:3001", "admin_tailscale"),
		ownedRouteFull("jellyfin", "media", "media.example.com", "container/jellyfin:8096", "family_public"),
	}
	rev := []resource.Resource{res[1], res[0]}
	if string(renderHomepageSettings(res)) != string(renderHomepageSettings(rev)) {
		t.Error("settings render is order-dependent")
	}
}

// TestHomepageStatusIsADot: the siteMonitor result is only visible
// because settings asks for it, so the two halves of the status feature
// live or die together.
func TestHomepageStatusIsADot(t *testing.T) {
	got := string(renderHomepageSettings(nil))
	if !strings.Contains(got, "statusStyle: dot") {
		t.Errorf("no status style, so a rendered siteMonitor shows nothing:\n%s", got)
	}
}

// TestHomepageBookmarksAreAnEmptyList, for the F55 reason: an unmanaged
// Homepage config file gets Homepage's own sample content, and on the
// alpha VM "My First Group / Github" sat under the household's real
// services.
func TestHomepageBookmarksAreAnEmptyList(t *testing.T) {
	got := string(renderHomepageBookmarks())
	if !strings.Contains(got, "[]") {
		t.Errorf("want an empty YAML list:\n%s", got)
	}
}

// TestHomepageWidgetsNeedNoSecrets. Every widget with household content
// needs either a location Keystone does not hold or an API key, and a
// key is a write-only secret (ADR-W012 W2) that this file has no way to
// materialize conditionally. Emitting a reference here would write the
// literal string "secret/..." into a running dashboard.
func TestHomepageWidgetsNeedNoSecrets(t *testing.T) {
	got := string(renderHomepageWidgets())
	if strings.Contains(got, "secret/") || strings.Contains(got, "key:") {
		t.Errorf("a widget here would need a secret this file cannot resolve:\n%s", got)
	}
	for _, want := range []string{"- greeting:", "- datetime:", "- resources:"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// TestHomepageChromeIsAPrereqAndInTheDigest. Both halves matter and
// each was a separate outage: not a prereq means the bind mount finds
// no file and Docker creates a DIRECTORY in its place; not in the digest
// means the file is corrected on disk and the container reading it is
// never restarted (F73).
func TestHomepageChromeIsAPrereqAndInTheDigest(t *testing.T) {
	desired := []resource.Resource{
		ownedRouteFull("jellyfin", "media", "media.example.com", "container/jellyfin:8096", "family_public"),
	}
	want := []string{
		homepageServicesPath(testManagedPrefix),
		homepageSettingsPath(testManagedPrefix),
		homepageWidgetsPath(testManagedPrefix),
		homepageBookmarksPath(testManagedPrefix),
	}

	prereqs := make(map[string]bool)
	for _, f := range nodePrereqFiles(testManagedPrefix, desired) {
		prereqs[f.path] = true
	}
	byKey := make(map[resKey]resource.Resource, len(desired))
	for _, r := range desired {
		byKey[resKey{Kind: r.Kind, Name: r.Metadata.Name}] = r
	}
	digest := managedConfigContents(testManagedPrefix, byKey)

	for _, p := range want {
		if !prereqs[p] {
			t.Errorf("%s is not a node prereq — the bind mount would create a directory", p)
		}
		if _, ok := digest[p]; !ok {
			t.Errorf("%s is not in the config digest — Homepage would keep serving the old one", p)
		}
	}
}
