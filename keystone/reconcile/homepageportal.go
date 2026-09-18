package reconcile

// The dashboard's two ACTING surfaces (#197, #198), as Homepage cards
// that hold a live frame rather than a link.
//
// Everything else on the dashboard is somewhere to go: a name, an icon,
// a status dot, and a link to an application that does the work. The
// portal's surfaces are things to DO from the page itself — drop a
// photo, ask the assistant — and a card that merely links to them is
// the dashboard Wildstone already had.
//
// Homepage can express exactly this and no more: a service widget of
// type `iframe`, which is why #198's "iframe a Wildstone-owned compact
// surface" is the recommended shape and #197's "do not fork Homepage"
// is satisfiable. The dashboard stays a presentation adapter; it is
// handed a URL and a height, and it knows nothing about uploads,
// assistants, storage or authority.
//
// KEYED BY PACKAGE, not by capability or container. There is no
// container — these surfaces are served by Keystone, so the upstream is
// an address and the homepageApps table (which is keyed by container
// name) cannot describe them. The package name is what the Route
// carries, is unique in the catalog, and is the thing that actually
// decides which surface a Route leads to.

import "strings"

// portalCard is one household surface as a card.
//
// classes is Homepage's own height vocabulary, passed through
// unchanged: h-72 is 18rem, which is tall enough for a drop zone plus
// four recent files, or for a question and three turns of answer, and
// short enough that two of them do not push the household's
// applications off the first screen of a tablet.
type portalCard struct {
	title       string
	icon        string
	description string
	// page is the full surface a person is sent to by the card's own
	// link; widget is the compact one the frame shows.
	page   string
	widget string
	// name is what Homepage calls the frame. It is the iframe's `name`
	// attribute and nothing more, but a blank one makes the whole
	// widget fail to key correctly on a refresh.
	name    string
	classes string
}

// portalCards is the table. One row per household surface; adding a
// third surface is a row here and a package that declares its Route.
var portalCards = map[string]portalCard{
	"filedrop-wildstone": {
		title:       "Files",
		icon:        "mdi-cloud-upload-outline",
		description: "Drop a photo or a file straight onto the household's storage",
		page:        "/filedrop/",
		widget:      "/filedrop/widget",
		name:        "files",
		classes:     "h-72",
	},
	"assistant-wildstone": {
		title:       "Assistant",
		icon:        "mdi-robot-happy-outline",
		description: "Ask the household assistant, with your own permissions",
		page:        "/assistant/",
		widget:      "/assistant/widget",
		name:        "assistant",
		classes:     "h-72",
	},
}

// portalGroup is the group these cards render in, and it is FIRST in
// homepageGroups.
//
// First because of what they are: the two things on the page a person
// can do without going anywhere. Their own group rather than mixed into
// Household because a live frame beside a row of link cards looks like
// a rendering fault, and because the layout gives this group a wider
// column count — a chat box three-to-a-row is a chat box nobody can
// type in.
const portalGroupName = "Quick actions"

// portalColumns is how many of these sit across one row. Two, so each
// frame gets half the page on a laptop and the whole of it on a phone.
const portalColumns = 2

// portalCardFor returns the card a Route leads to, if it leads to one.
func portalCardFor(pkg string) (portalCard, bool) {
	card, ok := portalCards[strings.TrimSpace(pkg)]
	return card, ok
}
