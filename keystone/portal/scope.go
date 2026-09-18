package portal

// The household portal's connection scope: the ceiling that makes a
// misconfigured assistant harmless on the family's dashboard.
//
// WHAT THIS ADDS THAT THE SURFACE TEST DOES NOT. The Assistant door
// already refuses any Butler that is not on the family surface or is on
// the operator profile (api.portalMayReach), and it rechecks that on
// every message. That is a test about the ASSISTANT. This is a test
// about the CONNECTION: whatever the assistant is allowed elsewhere —
// in the Console, over chat, inside a routine — an operator-class tool
// is refused when the request arrived through the dashboard. The two
// are different failures. The first catches "somebody built a family
// Butler with too much"; the second catches "somebody exposed an
// existing operator Butler here by mistake", which is the case the
// implementation package asks to be proved by hand-crafting a request
// and being refused.
//
// DENY IS ENFORCED. ALLOW IS DESCRIBED. That asymmetry is deliberate
// and it is the one thing to understand about this file.
//
// The design's scope sketch lists an allow set as well, and reading it
// as "refuse everything not named here" is tempting, stricter, and
// wrong for a live household — its seven lines are an abbreviation, not
// an inventory, and this repository's actual household tools include
// several it never mentions (locks.unlock, notifications.notify,
// wiki.internal). Enforcing it as a whitelist would take working
// household function away on the first deploy and call it security.
//
// So: a denied prefix is refused here, unconditionally, before any
// grant or administrative control is consulted. Anything else falls
// through to the resolver that already decides it — the per-Butler
// authority, the disabled list, the grants. The allow set is published
// for the page that shows a member what the assistant may do, labelled
// as what the surface is FOR rather than as the mechanism.
//
// THE VOCABULARY IS THIS REPOSITORY'S, NOT THE DESIGN'S. The design
// writes infrastructure.*, packages.*, nodes.manage; the tools are
// actually called infra.restart_container, containers.restart and so
// on, and the design says in as many words to use the repository's
// final authority vocabulary where it differs. Both spellings are
// denied below. Denying a prefix nothing uses costs nothing; missing
// one that something does is the whole bug.

import "strings"

// Connection is the name this surface is known by in a refusal, and it
// is what a member is shown when something is refused because of where
// they asked from rather than because of who they are.
const Connection = "household_portal"

// deniedPrefixes is the ceiling. A tool whose name begins with one of
// these is refused on this connection whatever else permits it.
//
// Prefix matching on a dotted name, with the boundary checked: "infra."
// must not be satisfied by a tool called "infrastructure_safe.read",
// and "nodes" must not match "nodes_public.read". Each entry is either
// a whole tool name or a dotted family.
var deniedPrefixes = []string{
	// The machines and what runs on them. A family dashboard that could
	// restart a container would need authority this connection is not
	// allowed to hold — and the design's read-only machines card exists
	// precisely so this is never tempting.
	"infra",
	"infrastructure",
	"containers",
	"container",
	"nodes",
	"node",
	// Installing, upgrading and removing capabilities. A member may
	// REQUEST one — that is a plan the owner approves, and it goes
	// through the plan pipeline, not through a tool.
	"packages",
	"package",
	"capabilities",
	"capability",
	// Who people are and what they may do. The one family that must
	// never be reachable from the surface that authenticates by
	// forwarded header: a tool here could edit the group that grants
	// the tool.
	"identity",
	"principals",
	"policy",
	"grants",
	// Secrets, in any spelling. Nothing on a household surface reads a
	// credential.
	"secrets",
	"secret",
	// The control plane's own state and the ledger of what it did.
	"statefile",
	"plans",
	"audit",
	"backups",
	"backup",
	"restore",
}

// Allowed is what this surface is FOR, in the household's language, for
// the page that tells a member what the assistant may do.
//
// Presented, not enforced — see the file comment. Each entry carries the
// qualification that makes it honest: almost nothing here is an
// unconditional yes, because the member's own permissions still apply
// underneath, and saying "yes" where the truth is "yes, where you have
// it" is how a page teaches somebody to expect something they will be
// refused.
type Capability struct {
	Name string `json:"name"`
	// What it is in a household sentence.
	What string `json:"what"`
	// Qualified is the condition, or empty for an unconditional yes.
	Qualified string `json:"qualified,omitempty"`
}

// Allows returns the published scope.
func Allows() []Capability {
	return []Capability{
		{Name: "household.basic", What: "Answer questions about the house"},
		{Name: "home.*", What: "Lights, heating, doors and sensors", Qualified: "where you have it"},
		{Name: "pantry.*", What: "The shopping list and what is in the kitchen", Qualified: "where you have it"},
		{Name: "calendar:self", What: "Your own calendar and the household's", Qualified: "your audience only"},
		{Name: "files:shared", What: "The household's shared folder", Qualified: "granted"},
		{Name: "media.*", What: "What is on the media library", Qualified: "if granted"},
		{Name: "notifications.notify", What: "Send you and the household a notification"},
	}
}

// Denies returns the ceiling, in the same shape, for the same page.
//
// Worth showing rather than hiding: the design's point is that the
// intersection model becomes legible to a household. A member who can
// read "operator only — never from here" is a member who does not have
// to discover it by being refused.
func Denies() []Capability {
	return []Capability{
		{Name: "infra.*, containers.*, nodes.*", What: "Restart or change the machines", Qualified: "operator only — never from here"},
		{Name: "packages.*, capabilities.*", What: "Install or remove apps", Qualified: "you can ask; the owner installs"},
		{Name: "identity.*, policy.*, grants.*", What: "Accounts, groups and permissions", Qualified: "operator only — never from here"},
		{Name: "secrets.*", What: "Read a password or a key", Qualified: "never, on any surface"},
		{Name: "plans.*, audit.*, backups.*", What: "The control plane's own records", Qualified: "operator only — never from here"},
	}
}

// Permits reports whether this connection allows a tool to run, and the
// sentence to show when it does not.
//
// The refusal names the CONNECTION rather than an error code, because
// the difference between "you may not" and "not from here" is the whole
// thing a member needs in order to know what to do next: the first is
// about them, the second is about the surface, and only the second has
// an obvious remedy.
func Permits(tool string) (bool, string) {
	name := strings.ToLower(strings.TrimSpace(tool))
	for _, prefix := range deniedPrefixes {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// The boundary test. A denied family is "infra" plus a
		// separator; the bare name is denied too, but "infra_helper"
		// is a different tool and this list does not get to claim it
		// by accident.
		rest := name[len(prefix):]
		if rest == "" || rest[0] == '.' || rest[0] == ':' || rest[0] == '/' {
			return false, "That needs the owner — it isn't something I can do from the dashboard."
		}
	}
	return true, ""
}
