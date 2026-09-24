package agentruntime

import "github.com/chickenrat17/wildstone-homelab/keystone/plan"

// ToolCallRecord is one audited tool call: what the model asked for, and
// the (already redacted/enveloped/truncated) result it got back.
//
// Moved from keystone/butler (ADR-W039 Phase 2b, #337) so that
// keystone/agentruntime/native's Runner and keystone/butler's Toolset
// implementations can share one shape without native importing butler:
// butler.ToolCallRecord is now a type alias of this.
type ToolCallRecord struct {
	// StepSeq is assigned by the durable runner, never by model input.
	StepSeq int64
	Name    string
	Input   map[string]any
	Result  string
	Error   string
	Plan    *plan.Plan
	// ResourceYAML carries an accepted propose_change's canonical
	// resource document (ADR-W017 A3) for the host to submit as a real
	// proposed plan record after the session.
	ResourceYAML string
	Rejected     bool
}

// Audit is the session audit record ADR-W006 A5.4 requires: provider/
// model, every tool call + result, every plan produced.
type Audit struct {
	Provider string
	Calls    []ToolCallRecord
	// Plans collects every plan.Plan produced during the session (from
	// get_plan and propose_change calls), in call order.
	Plans []*plan.Plan
}

// Transcript is what a Session or a Runner returns: the model's final
// assistant text plus the full session audit.
type Transcript struct {
	FinalText string
	Audit     Audit
	// CappedIterations is true if the loop stopped because a round or
	// iteration limit was reached with the model still requesting tool
	// calls, rather than because the model produced a final answer.
	CappedIterations bool
}
