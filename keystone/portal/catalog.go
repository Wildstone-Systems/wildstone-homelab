package portal

// The Catalog's rules: what a household member may ask for, and what is
// not theirs to ask for at all.
//
// "REQUEST, DON'T INSTALL" is the whole shape of this surface. A member
// drafts; the owner approves and picks the machine. So nothing here
// installs anything, and nothing here lets a member name a node, a path
// or a secret — there is no field in which to say it, which is the same
// property the Files API has and for the same reason.
//
// THE OMISSION IS PART OF THE DESIGN, not a filter over a longer list.
// §12.5's closing note says so outright: sign-in, the network, backups,
// monitoring and the machines are absent, and saying so once beats a
// member finding a control that refuses them. operatorOnly below is
// that sentence as code.

import (
	"sort"
	"strings"
)

// operatorOnly are the capabilities a household surface does not offer.
//
// Named by CAPABILITY rather than by package, because that is the level
// at which the answer is the same for every profile: a household does
// not request "ingress via Caddy" any more than it requests "ingress
// via something else". Both are the operator's.
//
// Deliberately generous. A capability that appears here and should not
// have costs a member one conversation with the owner; one that is
// missing puts a control for the household's DNS on the family's
// dashboard.
var operatorOnly = map[string]bool{
	// Sign-in, the directory, and anything that decides who anybody is.
	"identity": true,
	"console":  true,
	// The network: ingress, DNS, the VPN, the captcha solver that exists
	// only to get past someone else's bot check.
	"ingress":        true,
	"dns":            true,
	"vpn":            true,
	"captcha-solver": true,
	// Watching the machines. Beszel is host-monitoring: a family reading
	// a graph of CPU sees a problem they cannot fix, which is the same
	// reason the machines card is at the foot of the front page.
	"metrics":         true,
	"monitoring":      true,
	"host-monitoring": true,
	"timeseries":      true,
	// The plumbing the house runs on rather than uses.
	"mqtt":          true,
	"zigbee":        true,
	"notifications": true,
	// Fetching things off the internet on the household's behalf. An
	// indexer is a legal and a bandwidth decision, not a preference, and
	// the *arr automation exists to drive one.
	"indexers":         true,
	"downloads":        true,
	"subtitles":        true,
	"requests":         true,
	"movie-automation": true,
	"tv-automation":    true,
	"music-automation": true,
	// Chat bots. A Connection joins one Person to one Agent, and only an
	// Owner makes one, in the Console, with the platform's credential
	// in hand (ADR-W033 B7.1) — there is nothing here a member could
	// usefully ask for.
	"chat-telegram": true,
	"chat-matrix":   true,
	"chat-discord":  true,
	"chat-slack":    true,
	// An Agent's runtime. Bound to one Agent by the Console's Agent ->
	// Runtime screen, exactly like a chat bot's Connection — there is
	// nothing here a member could usefully ask for either (ADR-W039
	// Phase 6/H5, #351).
	"agent-hermes": true,
	// Wildstone's own surfaces. A member cannot request the dashboard
	// they are standing on, or the drop box behind its Files card.
	"dashboard": true,
	"filedrop":  true,
	"assistant": true,
	"homarr":    true,
	// The household lists behind the dashboard's Lists card, installed
	// with the dashboard like the drop box is.
	"lists": true,
}

// Requestable reports whether a capability may be asked for from a
// household surface.
func Requestable(capability string) bool {
	return !operatorOnly[strings.TrimSpace(strings.ToLower(capability))]
}

// Category is how the Catalog groups what a household could add — by
// what it is FOR, not by what it is built from.
//
// A member looking for somewhere to put photos is not looking for "a
// capability of class media"; the grouping has to be the question they
// actually arrived with.
type Category struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// Categories is the closed set, in the order the page shows them.
var Categories = []Category{
	{Key: "watching", Title: "Watching"},
	{Key: "reading", Title: "Reading"},
	{Key: "house", Title: "Running the house"},
	{Key: "files", Title: "Files and photos"},
	{Key: "talk", Title: "Talking"},
	{Key: "other", Title: "Everything else"},
}

// categoryOf places a capability in a category.
//
// By capability rather than by package, so two profiles of one thing
// never land in different groups — which is how a household ends up
// asking for a second Jellyfin because the first was filed somewhere
// they did not look.
var categoryOf = map[string]string{
	// Watching and listening.
	"media":      "watching",
	"cameras":    "watching",
	"podcasts":   "watching",
	"music":      "watching",
	"audiobooks": "watching",
	// Reading.
	"books":     "reading",
	"feeds":     "reading",
	"bookmarks": "reading",
	"documents": "reading",
	// Running the house.
	"recipes":         "house",
	"home-assistant":  "house",
	"home-automation": "house",
	"inventory":       "house",
	"budget":          "house",
	"passwords":       "house",
	// A calendar is where a household writes down where its people will
	// be. It belongs with the other things the house runs on rather than
	// with mail, which is somebody else's account.
	"calendar": "house",
	// Files and photos.
	"photos":    "files",
	"files":     "files",
	"file-sync": "files",
	"pdf-tools": "files",
	// Talking, and the assistant.
	"mail":            "talk",
	"ai-chat":         "talk",
	"language-models": "talk",
	// Everything else, named rather than defaulted, so a capability
	// landing in "other" is a gap in this table rather than a decision.
	"code":      "other",
	"utilities": "other",
}

// CategoryFor returns the category key for a capability.
func CategoryFor(capability string) string {
	if c, ok := categoryOf[strings.TrimSpace(strings.ToLower(capability))]; ok {
		return c
	}
	return "other"
}

// profileAliases fill the gaps where a package's profile is not the
// container name the launcher's description table is keyed by.
//
// Five rows, and each one is a package a household would plausibly
// request. Everything else that misses the table is operator-class and
// never reaches this surface.
var profileAliases = map[string]string{
	"immich":    "immich-server",
	"ollama":    "open-webui",
	"container": "home-assistant",
	"matrix":    "open-webui",
	"telegram":  "open-webui",
}

// DescribeKey maps a package profile onto the key its description is
// filed under, so a caller can look it up without knowing about the
// aliases.
func DescribeKey(profile string) string {
	if alias, ok := profileAliases[profile]; ok {
		return alias
	}
	return profile
}

// SensitivityNote turns a package's declared data sensitivity into the
// pill a member sees, or empty for a package that declares none.
//
// EMPTY EMITS NO PILL AT ALL. §12.5 is specific about this: "an empty
// label emits no pill at all — a grey blob beside a name reads as a
// broken glyph." Low sensitivity is also nothing to say: every app holds
// something, and a pill on all of them tells a member nothing.
func SensitivityNote(level string) string {
	switch strings.TrimSpace(strings.ToLower(level)) {
	case "high":
		return "sensitive"
	case "medium":
		return "personal"
	default:
		return ""
	}
}

// LeavesTheHouse reports whether a capability sends household content to
// something outside it, for the flag §12.5 asks the draft to carry.
//
// A short list of the ones that genuinely do. It is not derived from the
// package, because nothing in the schema says it — and a member deciding
// whether to ask for a chat bridge is owed the fact that their messages
// will leave.
func LeavesTheHouse(capability string) bool {
	switch strings.TrimSpace(strings.ToLower(capability)) {
	case "mail", "chat-telegram", "chat-matrix", "chat-discord", "chat-slack", "ai-chat":
		return true
	}
	return false
}

// deliberatelyOther are the capabilities that belong in "Everything
// else" on purpose rather than by falling through.
//
// The distinction matters because a missing map entry and a considered
// "this really is miscellaneous" are the same zero value, and the first
// is how nineteen capabilities ended up in one group without anybody
// deciding that. A test asserts every requestable capability is either
// categorised or named here.
var deliberatelyOther = map[string]bool{
	"code":      true,
	"utilities": true,
}

// DeliberatelyOther reports whether a capability is in "Everything else"
// by decision rather than by omission.
func DeliberatelyOther(capability string) bool {
	return deliberatelyOther[strings.TrimSpace(strings.ToLower(capability))]
}

// OperatorOnly returns the capabilities this surface refuses to offer,
// so a test can hold every one of them to a name a package actually
// declares. A denial that matches nothing protects nothing.
func OperatorOnly() []string {
	out := make([]string, 0, len(operatorOnly))
	for c := range operatorOnly {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
