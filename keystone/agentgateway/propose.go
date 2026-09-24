package agentgateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/proposals"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// Propose implements agentapi.Client (ADR-W039 §3.2 Proposals, §7.2,
// Phase 3, #339): resolve the handle, validate the document exactly as
// the propose_change tool's own diffFromInput/Validate path does, and
// file it through the ONE proposal pipeline under the runtime principal
// acting for the Person on the handle — never applied here, and never
// approvable by the runtime that filed it (§3.2's own "who may approve
// is never a runtime").
//
// This is the standalone Propose call a runtime makes directly, naming
// a resource document rather than typing a propose_change tool call.
// Invoke's own propose_change branch files through the identical
// fileProposal helper the moment that tool call succeeds (invoke.go),
// so a runtime gets the same outcome whichever way it proposes.
func (g *Gateway) Propose(ctx context.Context, req agentapi.ProposeRequest) (agentapi.ProposalRef, error) {
	s, err := g.open(ctx, req.Handle)
	if err != nil {
		return agentapi.ProposalRef{}, err
	}
	if g.Proposals.DB == nil {
		return agentapi.ProposalRef{}, fmt.Errorf("%w: proposal filing is not wired on this gateway", ErrRefused)
	}
	if len(req.Document) == 0 {
		return agentapi.ProposalRef{}, fmt.Errorf("%w: propose needs a document", ErrRefused)
	}
	res, err := resource.Unmarshal(req.Document)
	if err != nil {
		return agentapi.ProposalRef{}, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	if req.Kind != "" && string(res.Kind) != req.Kind {
		return agentapi.ProposalRef{}, fmt.Errorf("%w: kind %q does not match the document's own kind %q", ErrRefused, req.Kind, res.Kind)
	}
	if err := res.Validate(); err != nil {
		return agentapi.ProposalRef{}, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	doc, err := resource.Marshal(res)
	if err != nil {
		return agentapi.ProposalRef{}, fmt.Errorf("agentgateway: encoding the proposed document: %w", err)
	}
	id, err := g.runtimeIdentity(ctx, s)
	if err != nil {
		return agentapi.ProposalRef{}, err
	}
	summary := strings.TrimSpace(req.Rationale)
	if summary == "" {
		summary = fmt.Sprintf("%s proposes %s %s", s.hc.Butler.Name, res.Kind, res.Metadata.Name)
	}
	view, err := g.fileProposal(id, proposals.Request{Summary: summary, Put: []string{string(doc)}})
	if err != nil {
		return agentapi.ProposalRef{}, err
	}
	return agentapi.ProposalRef{ID: view.ID}, nil
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
