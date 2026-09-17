package reconcile

// Homepage's chrome: settings.yaml, widgets.yaml, bookmarks.yaml.
//
// services.yaml (F55) fixed WHAT the dashboard lists. It did not fix how
// it looks, and on the alpha VM the result was a single column of bare
// links with no icons, no status and no layout — plus Homepage's own
// sample bookmarks ("My First Group") still sitting underneath, for the
// same reason the sample services did: an unmanaged file gets seeded
// (F87).
//
// Three more files, all the same category as services.yaml and rendered
// the same way — a pure function of desired state, written as a node
// prereq, folded into the dashboard container's config digest so a
// change actually reaches the running Homepage (F73).
//
// Why render them at all rather than let the operator edit the volume:
// Homepage's layout NAMES ITS GROUPS, and the groups come from Route
// classifications. A hand-edited settings.yaml would name groups that
// desired state may no longer produce, which is the two-places-must-
// agree trap this codebase rejects everywhere else. The one thing the
// layout must agree with is the service list, and now it is generated
// from the same entries.

import (
	"path"
	"strconv"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func homepageSettingsPath(managedPrefix string) string {
	return path.Join(managedPrefix, "dashboard", "settings.yaml")
}

func homepageWidgetsPath(managedPrefix string) string {
	return path.Join(managedPrefix, "dashboard", "widgets.yaml")
}

func homepageBookmarksPath(managedPrefix string) string {
	return path.Join(managedPrefix, "dashboard", "bookmarks.yaml")
}

// homepageTheme is the dashboard's palette, and it is named rather than
// written twice because a second reader appeared: the household
// surfaces are iframes, and an embedded page cannot ask Homepage what
// theme it is being drawn into.
//
// It cannot work it out for itself either. Homepage puts `scheme-light`
// on the container it frames a widget in, so an embedded page keying
// off prefers-color-scheme is told "light" while it sits on a dark
// card — which renders dark text on a dark panel, and is exactly how
// the first live dashboard came out. So the theme is passed to the
// widget in its own URL (see widgetSrc) from this constant, and the
// page obeys it instead of guessing.
const homepageTheme = "dark"

// homepageColumns is how many service cards sit across one row.
//
// This is the actual fix for "everything is in one column": Homepage's
// DEFAULT group style is a list, one service per line, which is what the
// alpha VM showed. `style: row` with a column count is what turns the
// list into a grid of cards. Three rather than four so a card keeps room
// for its description on a tablet held in portrait, which is where a
// family dashboard actually lives.
const homepageColumns = 3

// renderHomepageSettings builds Homepage's settings.yaml.
//
// Every key here is a presentation decision Wildstone is making on the
// household's behalf, and each one is a thing the bare install got
// wrong:
//
//   - layout: the one-column list becomes a grid, per group, in the
//     order homepageGroups declares. Only groups the service list
//     actually renders are named — a layout entry for a group with no
//     services draws an empty heading.
//   - statusStyle dot: the siteMonitor result as a colored dot next to
//     the name, rather than a line of HTTP status text under every card.
//   - headerStyle boxed + theme/color: a deliberate look instead of
//     Homepage's untouched default, which reads as unconfigured.
//   - hideVersion: a family dashboard should not advertise the version
//     of the software serving it, to the family or to anyone else.
//   - quicklaunch: typing filters the household's own services; the
//     internet-search and visit-URL fallbacks are off because they turn
//     a household dashboard into a browser address bar.
func renderHomepageSettings(desired []resource.Resource) []byte {
	entries := homepageEntries(desired)

	var b strings.Builder
	b.WriteString("# managed by wildstone-keystone — do not edit\n")
	b.WriteString("title: " + yamlQuote(dashboardTitle(desired)) + "\n")
	b.WriteString("description: Everything this household runs\n")
	b.WriteString("theme: " + homepageTheme + "\n")
	b.WriteString("color: slate\n")
	b.WriteString("headerStyle: boxed\n")
	b.WriteString("statusStyle: dot\n")
	b.WriteString("hideVersion: true\n")
	b.WriteString("useEqualHeights: true\n")
	b.WriteString("target: _blank\n")
	b.WriteString("quicklaunch:\n")
	b.WriteString("  searchDescriptions: true\n")
	b.WriteString("  hideInternetSearch: true\n")
	b.WriteString("  hideVisitURL: true\n")

	// No groups yet means no layout block: an empty `layout:` key is not
	// the same as an absent one, and Homepage is happier with the
	// absence than with a mapping of nothing.
	used := make([]string, 0, len(homepageGroups))
	for _, g := range homepageGroups {
		if homepageGroupUsed(entries, g) {
			used = append(used, g)
		}
	}
	if len(used) == 0 {
		return []byte(b.String())
	}
	b.WriteString("layout:\n")
	for _, g := range used {
		b.WriteString("  " + g + ":\n")
		b.WriteString("    style: row\n")
		b.WriteString("    columns: " + strconv.Itoa(columnsFor(g)) + "\n")
	}
	return []byte(b.String())
}

// columnsFor is how many cards sit across one row of a group.
//
// One exception to homepageColumns, and it is the household surfaces:
// those cards hold a live frame with a text box in it, and a third of a
// tablet's width is not something a person can type into or drop a file
// on. Two across gives each frame half the page.
func columnsFor(group string) int {
	if group == portalGroupName {
		return portalColumns
	}
	return homepageColumns
}

// renderHomepageWidgets builds Homepage's widgets.yaml — the INFO
// widgets across the top of the page, not the per-service ones.
//
// Constant, and that is a deliberate limit rather than an oversight.
// Every widget with real household content needs something Keystone
// cannot supply from desired state today:
//
//   - weather (openmeteo) needs a latitude and longitude, and no
//     resource carries the household's location.
//   - the per-service widgets — Jellyfin's now-playing, Immich's photo
//     count, Uptime Kuma's incident list — each need an API key for the
//     app they read. Those are secrets, and secrets are write-only
//     (ADR-W012 W2): the renderer can emit a `secret/<name>` reference,
//     but only a MATERIALIZING write resolves it, and this file's
//     materialize flag would then be conditional on which apps are
//     installed. That is the F81 shape and it needs a decision, not a
//     patch. See the F87 row.
//
// What is left needs nothing: search, the greeting, the clock, and the
// host's own resources. `resources` reads /proc from inside the
// container, which on a normal Docker host is the host's — so the
// numbers are the machine's, not the container's.
//
// SEARCH IS FIRST, and it is here because taking it away was a
// regression nobody chose (F90). Homepage seeds a default widgets.yaml
// containing a search bar when it finds none; writing this file
// replaced that default, so the search bar simply vanished from a
// dashboard that had always had one. Nothing declared it should go —
// it was lost as a side effect of managing the file at all, which is
// the same shape as the sample bookmarks this package already writes an
// empty file to suppress.
//
// duckduckgo because it is Homepage's own default and this is a
// restoration, not a new opinion about where a household should search.
func renderHomepageWidgets() []byte {
	return []byte(`# managed by wildstone-keystone — do not edit
- search:
    provider: duckduckgo
    target: _blank
- greeting:
    text_size: xl
    text: Welcome home
- datetime:
    text_size: xl
    format:
      dateStyle: long
      timeStyle: short
      hour12: true
- resources:
    label: This machine
    cpu: true
    memory: true
    disk: /
`)
}

// renderHomepageBookmarks builds Homepage's bookmarks.yaml, which is
// always empty.
//
// A bookmark is not desired state — Wildstone has no resource that says
// "the family uses this external site" and should not invent one. The
// file exists purely so Homepage does not seed its own sample bookmarks
// into the gap, which is the F55 mechanism exactly: on the alpha VM the
// dashboard showed the household's real services above Homepage's
// "My First Group / Github" below.
func renderHomepageBookmarks() []byte {
	return []byte(`# managed by wildstone-keystone — do not edit
# A bookmark is not desired state; this file exists so Homepage does not
# seed its sample bookmarks in place of one it cannot find.
[]
`)
}
