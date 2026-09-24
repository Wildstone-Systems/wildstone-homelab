package agentgateway

import (
	"context"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

// Propose is not implemented in this phase. Filing a Plan for Owner
// approval is today the API handler's post-processing of the
// propose_change tool's ToolCallRecord (C9), through
// submitProposalOpts in keystone/api — which this package cannot
// import without a cycle. Phase 3 moves that pipeline below the API
// and files the Plan here (ADR-W039 §7.2). Until then a runtime
// proposes by invoking propose_change, whose result carries a
// Proposal reference and is never applied.
func (g *Gateway) Propose(context.Context, agentapi.ProposeRequest) (agentapi.ProposalRef, error) {
	return agentapi.ProposalRef{}, &agentapi.NotImplementedError{
		Method: "Propose", Phase: "Phase 3",
		Reason: "in-gateway Plan filing needs the proposal pipeline moved out of keystone/api; invoke propose_change instead",
	}
}

// Notify is not implemented in this phase. Outbound text through the
// task's Connection — admission, disclosure by destination, quiet
// hours, dedupe, the adapter's credential — is the Events family, and
// Phase 4 supplies it (ADR-W039 §4 item 8). The one outbound notice
// the native runner sends today, the stalled-task notice, stays on
// its StallNotifier until then.
func (g *Gateway) Notify(context.Context, agentapi.NotifyRequest) error {
	return &agentapi.NotImplementedError{
		Method: "Notify", Phase: "Phase 4",
		Reason: "outbound delivery through the task's Connection is the Events family",
	}
}
