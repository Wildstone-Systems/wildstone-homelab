package principal

import "github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"

// Verb is a public-plane permission (ADR-W007 A2). The set is closed:
// adding a verb is a code change reviewed against the role table below.
type Verb string

const (
	// VerbRead covers every read endpoint: resources, observed status,
	// events, plans, whoami.
	VerbRead Verb = "read"
	// VerbPropose covers plan proposal — the ONLY way any principal
	// initiates a write (ground rule 4).
	VerbPropose Verb = "propose"
	// VerbApprove covers plan approval/rejection.
	VerbApprove Verb = "approve"
	// VerbManage covers principal/token administration and audit reads.
	VerbManage Verb = "manage"
	// VerbEmit covers publishing household events (ADR-W024 A3) — the
	// one verb a publisher holds, and the only thing it can do.
	VerbEmit Verb = "emit"
	// VerbConverse covers speaking to a Butler on behalf of a bound chat
	// account (ADR-W026 A3): redeeming a claim code, relaying a message
	// from a bound account, and collecting the replies queued for that
	// surface. It is the only verb a concierge holds. It carries no read
	// of Keystone state: an adapter relays a conversation, it does not
	// have one.
	VerbConverse Verb = "converse"
)

// roleVerbs is the closed RBAC table (ADR-W007 A2). The butler row is the
// structural safety claim from S4 carried into the API: it can never hold
// approve, so a fully-obeyed prompt injection still ends at a proposed
// plan awaiting a human.
var roleVerbs = map[runtimedb.Role]map[Verb]bool{
	runtimedb.RoleAdmin: {
		VerbRead: true, VerbPropose: true, VerbApprove: true, VerbManage: true,
		VerbEmit: true, VerbConverse: true,
	},
	runtimedb.RoleOperator: {
		VerbRead: true, VerbPropose: true, VerbApprove: true, VerbConverse: true,
	},
	// Unchanged by the chat work, deliberately. ADR-W024 A5 makes every
	// external agent — Hermes, Codex, anything else — a butler-role
	// principal, so a chat verb here would appear on credentials minted
	// long before chat existed and hand them the operator's private
	// queue. The adapters get their own roles below.
	runtimedb.RoleButler: {
		VerbRead: true, VerbPropose: true,
	},
	// The two chat adapters relay and nothing else: not read, not
	// propose, not emit. A compromised adapter can carry messages for
	// accounts a human already bound, and cannot look at the house.
	//
	// Neither needs more. A session's proposals are submitted under an
	// internally constructed butler identity acting-for the bound
	// person, never under the adapter's own credential — so even the
	// admin-surface adapter holds no propose verb of its own, and
	// certainly no approve (ADR-W026 A5).
	runtimedb.RoleConcierge: {
		VerbConverse: true,
	},
	runtimedb.RoleValet: {
		VerbConverse: true,
	},
	// A publisher emits and nothing else: not read, not propose. A shim
	// that is compromised can lie about the house; it cannot see or
	// touch the Statefile.
	runtimedb.RolePublisher: {
		VerbEmit: true,
	},
}

// Allowed reports whether role holds verb. Unknown roles hold nothing.
func Allowed(role runtimedb.Role, verb Verb) bool {
	return roleVerbs[role][verb]
}
