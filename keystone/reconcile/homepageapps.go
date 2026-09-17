package reconcile

// The launcher, as data rather than as Homepage's YAML.
//
// The dashboard capability has two profiles. profile "homepage" renders
// services.yaml and a container reads it; profile "wildstone" serves the
// page from Keystone and needs the same answer as a Go value. This file
// is the second exit from one renderer, and it exists so that there is
// not a second renderer.
//
// That matters more than it sounds. Titles, descriptions, icon slugs and
// grouping are a household's idea of what its own apps are called, and
// two implementations of "what apps does this house have" would disagree
// the first time somebody installed something neither had a row for. A
// family switching profiles should see the same twelve cards with the
// same names, because they are the same twelve cards.

import "github.com/chickenrat17/wildstone-homelab/keystone/resource"

// App is one launcher card for a Wildstone-served dashboard.
//
// Deliberately without a status field. Reachability is runtime state and
// this package renders desired state; joining the two here would make a
// pure function of the Statefile depend on the runtime database. The
// caller holds both and does the join (see keystone/api's home summary),
// which is also the layer that can tell "not answering" from "nobody has
// checked" — a distinction §7 insists on and a bool could not carry.
type App struct {
	// Group is "Household", "Admin", or the household-surfaces group.
	// Presentation order is the caller's business; Groups below states
	// the order the renderer means.
	Group string `json:"group"`
	Title string `json:"title"`
	// Desc is one line about the app. It is what a card shows UNTIL
	// something is wrong with the app, at which point §7 replaces it
	// with the reason — a red dot on its own tells a family nothing
	// they can act on.
	Desc string `json:"desc,omitempty"`
	// Href is the address a BROWSER must use, which under the tailnet
	// posture is the node's certified name and this Route's port rather
	// than the host the Route declares.
	Href string `json:"href"`
	// Icon is a dashboard-icons slug or an mdi- name, exactly as the
	// Homepage profile would write it, so one icon set serves both.
	Icon string `json:"icon,omitempty"`
	// Container and Node identify the thing to look up an observation
	// for. Container is empty for a Route that names an address, and a
	// caller must render that as unknown rather than as down.
	Container string `json:"container,omitempty"`
	Node      string `json:"node,omitempty"`
	// Route is the Route resource's own name, which is what makes two
	// cards that resolved to the same title distinguishable.
	Route string `json:"route,omitempty"`
}

// Groups is the group order a dashboard should present, Household first
// because it is what the family opens the page for.
//
// Exported from here rather than restated in the page for the same
// reason the cards are: the group names are strings two renderers would
// otherwise have to agree on by hand, and a layout naming a group the
// service list does not have is the specific bug homepageEntries'
// comment warns about.
func Groups() []string { return append([]string{portalGroupName}, homepageGroups...) }

// Apps resolves desired state into the launcher's cards.
//
// The empty case is passed through rather than special-cased: a
// household with no routed services gets homepageEmptyState, which is
// one card naming the command that installs the first app. Zero services
// is the normal state of a correct fresh install, and a page that
// rendered it as a blank grid would read as a broken dashboard.
func Apps(desired []resource.Resource) []App {
	entries := homepageEntries(desired)
	apps := make([]App, 0, len(entries))
	for _, e := range entries {
		apps = append(apps, App{
			Group:     e.group,
			Title:     e.title,
			Desc:      e.desc,
			Href:      e.href,
			Icon:      e.icon,
			Container: e.container,
			Node:      e.node,
			Route:     e.name,
		})
	}
	return apps
}
