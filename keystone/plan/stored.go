package plan

// StoredProposal is what a plan record's plan_json carries: the computed
// plan plus the exact desired-state documents approval will commit.
//
// It lives here, rather than in the API that reads it, because two
// separate components WRITE it — the API's propose path and the reconcile
// loop's drift path — and only one of them is the one that reads it back.
// The loop used to build this document as an untyped map and omitted
// BasedOnHead; it unmarshalled to "", the approval gate compares
// head != BasedOnHead, and no head ever equals "", so every drift repair
// was unapprovable and a drifted Route stayed down (F28). A shared struct
// makes that particular mistake a compile-time concern instead of a
// silent one.
type StoredProposal struct {
	// AuditOnly records a member's immediate maintenance-tracker edit. It is
	// history, not a submitted Plan or permission to execute host changes.
	AuditOnly bool           `json:"auditOnly,omitempty"`
	Routine   *RoutineChange `json:"routine,omitempty"`
	// BasedOnHead is the Statefile commit the proposal was computed
	// against. Approval refuses a proposal whose base has moved, so this
	// must always be set — including by proposers that change no desired
	// state at all.
	BasedOnHead string `json:"basedOnHead"`
	Summary     string `json:"summary"`
	// Put and Delete are the desired-state changes approval commits.
	// Both are legitimately empty for a drift proposal: desired state is
	// already correct and it is reality that moved. Such a proposal is
	// not inert — approving it makes the next reconcile tick a plan tick,
	// which is what authorizes the Medium/High repair steps.
	Put    []string      `json:"put,omitempty"`
	Delete []DeletionRef `json:"delete,omitempty"`
	Plan   *Plan         `json:"plan"`
	// Origin records what was ASKED FOR when a proposal came from
	// enabling a capability, rather than only what it rendered to
	// (ADR-W021 A2). Without it the audit trail says "fourteen documents
	// changed", which is a description of consequences and not an answer
	// to "what did we install" — and an upgrade has no previous input to
	// diff against.
	//
	// It lives here rather than in a PlanRecord column for the same
	// reason BasedOnHead does: it is part of the proposal, it travels
	// with it, and adding it here cannot silently miss a writer.
	Origin *CapabilityOrigin `json:"origin,omitempty"`
	// Drift marks a proposal raised by the reconcile loop rather than by
	// a proposer with an intent. Provenance only; nothing branches on it.
	Drift bool `json:"drift,omitempty"`
	// Setting is a household setting this proposal changes instead of
	// desired state (ADR-W033 E2): an Agent proposes, an Owner approves,
	// and approval applies it. A setting-only proposal carries no Put or
	// Delete and does not depend on the Statefile head.
	Setting *SettingChange `json:"setting,omitempty"`
}

// SettingChange is one household setting a proposal changes.
type SettingChange struct {
	// Timezone is the IANA zone the household keeps.
	Timezone string `json:"timezone,omitempty"`
	// Reason is why, in the proposer's words.
	Reason string `json:"reason,omitempty"`
}

// DeletionRef names a resource a proposal removes from desired state.
type DeletionRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// CapabilityOrigin is the input a capability proposal was expanded from.
//
// Params carry no secret material and must not (ADR-W021 A2): a value
// stored here reaches the runtime DB and the audit trail. Secrets travel
// as Secret resources — generated on the node, or declared `provided`
// and set by the operator — which is the one path that keeps material
// out of everything that records intent.
type CapabilityOrigin struct {
	Package string            `json:"package"`
	Version string            `json:"version"`
	Params  map[string]string `json:"params,omitempty"`
}

// RoutineChange is the exact routine version an Owner reviews. Output audience
// governs who may learn the result; Delivery describes where/how it is sent.
type RoutineChange struct {
	AssignedButler string `json:"assignedButler"`
	ProposingAgent string `json:"proposingAgent,omitempty"`
	Name           string `json:"name"`
	Revision       int    `json:"revision"`
	Document       string `json:"document"`
	OutputAudience string `json:"outputAudience"`
	Delivery       string `json:"delivery"`
}
