package butler

// The effective-authority seam every dispatch goes through (ADR-W030
// A7/A8, work package P7).
//
// A tool being registered on a gateway is now the FIRST of three
// questions, not the only one. The other two — has an administrator
// turned it off here, and does this piece of work hold a grant for it —
// are administrative state, and this package still holds no database.
// So the answer arrives through a seam, implemented in
// keystone/butlerstore over the runtime DB's resolver.
//
// The ordering matters and is enforced on the store side: structural
// first (a grant cannot conjure a tool the gateway does not register),
// then disabled (which wins over a cached manifest, a saved task, and a
// grant issued a minute ago), then grants, then the tool's default.
//
// A nil Authorizer means no administrative controls are configured, and
// every structurally present tool works exactly as it did before P7.
// That is deliberate: "no controls exist yet" and "everything is
// disabled" must not be the same answer.

import "fmt"

// ToolDecision is the resolver's answer for one dispatch.
type ToolDecision struct {
	Allowed bool
	// Reason is shown to the model and to the person. "You do not have
	// that tool here" and "an administrator turned that off" are
	// different situations, and reporting them identically produces a
	// household that cannot tell a missing capability from a withdrawn
	// one.
	Reason string
	// Requestable is true when the answer is no but a person could
	// change it — the case A7 calls asking for a missing capability.
	Requestable bool
	// GrantSeq names the grant that allowed it, for the work trace.
	GrantSeq int64
}

// ToolAuthorityQuery is what the resolver needs. The gateway supplies
// the structural facts because only it knows them; the store supplies
// the administrative ones.
type ToolAuthorityQuery struct {
	AgentClass string
	Tool       string
	// Structural is whether this gateway registers the tool at all.
	Structural bool
	// GrantRequired is the tool's own metadata.
	GrantRequired bool
	// TaskID and RoutineName scope a work-bound grant. Empty means no
	// grant can apply, which is the right default for a one-shot
	// question that is not durable work.
	TaskID      string
	RoutineName string
}

// ToolAuthorizer resolves effective authority for one dispatch.
type ToolAuthorizer interface {
	AuthorizeTool(q ToolAuthorityQuery) (ToolDecision, error)
}

// authorize runs the seam for one tool on one agent class, failing closed if
// the resolver itself errors: a control plane that cannot say whether
// something is permitted has not said yes.
func authorize(a ToolAuthorizer, class, tool string, structural, grantRequired bool, taskID, routine string) ToolDecision {
	if a == nil {
		// No administrative controls configured. Structural is still
		// the boundary; a tool the gateway does not register was already
		// refused before reaching here.
		return ToolDecision{Allowed: structural}
	}
	d, err := a.AuthorizeTool(ToolAuthorityQuery{
		AgentClass: class, Tool: tool, Structural: structural,
		GrantRequired: grantRequired, TaskID: taskID, RoutineName: routine,
	})
	if err != nil {
		return ToolDecision{
			Allowed: false,
			Reason:  fmt.Sprintf("could not check whether %s is permitted right now, so it was not run", tool),
		}
	}
	return d
}

// refusal turns a denied decision into the outcome a model sees. It is
// Rejected rather than an error because a refusal is a real answer to
// the call, and one the model should reason about rather than retry.
func refusal(tool string, d ToolDecision) ToolOutcome {
	reason := d.Reason
	if reason == "" {
		reason = fmt.Sprintf("%s is not available in this session", tool)
	}
	if d.Requestable {
		reason += ". You can ask for it to be enabled; you cannot use it until someone does."
	}
	return ToolOutcome{Content: reason, Rejected: true}
}
