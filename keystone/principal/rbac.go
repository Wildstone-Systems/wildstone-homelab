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
	// VerbEmit covers publishing household events (ADR-W022 A3) — the
	// one verb a publisher holds, and the only thing it can do.
	VerbEmit Verb = "emit"
)

// roleVerbs is the closed RBAC table (ADR-W007 A2). The butler row is the
// structural safety claim from S4 carried into the API: it can never hold
// approve, so a fully-obeyed prompt injection still ends at a proposed
// plan awaiting a human.
var roleVerbs = map[runtimedb.Role]map[Verb]bool{
	runtimedb.RoleAdmin: {
		VerbRead: true, VerbPropose: true, VerbApprove: true, VerbManage: true, VerbEmit: true,
	},
	runtimedb.RoleOperator: {
		VerbRead: true, VerbPropose: true, VerbApprove: true,
	},
	runtimedb.RoleButler: {
		VerbRead: true, VerbPropose: true,
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
