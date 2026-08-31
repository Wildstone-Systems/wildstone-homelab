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
	// Drift marks a proposal raised by the reconcile loop rather than by
	// a proposer with an intent. Provenance only; nothing branches on it.
	Drift bool `json:"drift,omitempty"`
}

// DeletionRef names a resource a proposal removes from desired state.
type DeletionRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}
