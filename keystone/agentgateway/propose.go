package agentgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/proposals"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// Propose implements agentapi.Client (ADR-W039 §3.2 Proposals,
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
	if req.Kind == agentapi.ProposableAgentSubscription {
		return g.proposeSubscription(ctx, s, req)
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

// proposeSubscription is Propose's branch for
// agentapi.ProposableAgentSubscription (ADR-W039 §3.6, Phase 4, #341):
// an Owner-approved standing watch, filed the same way any other
// proposal is — same principal, same audit trail — but through
// proposals.FileSubscription rather than the resource-document path,
// because a subscription is neither desired state nor a Statefile
// document.
//
// The ONE thing this gateway enforces that FileSubscription cannot: the
// capability set may only narrow. A tool this Agent's session is not
// currently offered — read-only or not, held or not — is refused right
// here, however the proposal names it, before a Plan is even filed for
// an Owner to look at. Re-checked again, live, at every use a matched
// Task makes of it (agentgateway.checkSubscriptionScope) — this is the
// proposal-time half of that same guarantee.
func (g *Gateway) proposeSubscription(ctx context.Context, s *session, req agentapi.ProposeRequest) (agentapi.ProposalRef, error) {
	var sub agentapi.SubscriptionProposal
	if err := json.Unmarshal(req.Document, &sub); err != nil {
		return agentapi.ProposalRef{}, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	if sub.MatchKind == "" {
		return agentapi.ProposalRef{}, fmt.Errorf("%w: a subscription needs a match kind", ErrRefused)
	}
	if len(sub.Capabilities) == 0 {
		return agentapi.ProposalRef{}, fmt.Errorf("%w: a subscription needs at least one capability", ErrRefused)
	}
	offered := map[string]bool{}
	for _, spec := range s.toolset.OfferedSpecs(s.readOnly) {
		offered[spec.Name] = true
	}
	for _, c := range sub.Capabilities {
		if !offered[c] {
			return agentapi.ProposalRef{}, fmt.Errorf(
				"%w: %q is not a tool %s currently holds; a subscription can only narrow, never widen, what its Agent already has",
				ErrRefused, c, s.hc.Butler.Name)
		}
	}
	id, err := g.runtimeIdentity(ctx, s)
	if err != nil {
		return agentapi.ProposalRef{}, err
	}
	summary := strings.TrimSpace(req.Rationale)
	if summary == "" {
		summary = fmt.Sprintf("%s subscribes to %s events", s.hc.Butler.Name, sub.MatchKind)
	}
	view, err := proposals.FileSubscription(g.Proposals, id, proposals.SubscriptionRequest{
		Butler: s.hc.Butler.Name, MatchKind: sub.MatchKind, MatchJSON: string(sub.Match),
		Audience: sub.Audience, CapabilitySet: sub.Capabilities, BudgetJSON: string(sub.Budget),
		Summary: summary, Trigger: sub.Trigger, Cadence: sub.Cadence, Source: sub.Source,
	})
	if err != nil {
		return agentapi.ProposalRef{}, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	return agentapi.ProposalRef{ID: view.ID}, nil
}
