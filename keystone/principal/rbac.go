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
	// VerbApprove covers plan approval/rejection and the review-class
	// operations that share it. Deciding a Plan additionally requires a
	// human Owner (api planDecider): holding the verb is necessary, not
	// sufficient.
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
	// VerbClaimIntegrationWork covers an adapter long-polling for the
	// work assigned to the connections its credential is bound to
	// (ADR-W030 A5.1). It names no connection: Keystone decides which
	// work an adapter may see, so a compromised adapter cannot reach
	// another connection's work by asking for it.
	VerbClaimIntegrationWork Verb = "claim-integration-work"
	// VerbReportIntegrationResult covers answering work this adapter
	// claimed. It cannot answer work it did not claim, and it cannot
	// answer work whose connection lease has moved on.
	VerbReportIntegrationResult Verb = "report-integration-result"
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
	// A MACHINE credential, not an Owner and not an Owner Agent. See the
	// constant's own comment in runtimedb/principals.go for the three
	// axes this is not: nothing here says anything about what a person
	// may do, or about which gateway an Agent reaches. Its approve verb
	// covers machine operations that share the verb; it never decides a
	// Plan — only a human Owner does (api planDecider, ADR-W033 E2).
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
	// An adapter claims and answers, and that is the whole of it: not
	// read, not emit, not converse, not propose (ADR-W030 A5.1). The
	// two verbs it holds do not widen `emit` or `converse`, and there is
	// deliberately no inbound administration surface an adapter can
	// reach — Keystone needs none, and one would be the natural place
	// for a compromised adapter to start.
	runtimedb.RoleIntegrationAdapter: {
		VerbClaimIntegrationWork: true, VerbReportIntegrationResult: true,
	},
}

// Allowed reports whether role holds verb. Unknown roles hold nothing.
func Allowed(role runtimedb.Role, verb Verb) bool {
	return roleVerbs[role][verb]
}
