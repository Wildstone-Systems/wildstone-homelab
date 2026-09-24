package agentgateway

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

// The Memory family. Today memory reaches a runtime as two household
// tools, recall and remember, on the family Toolset — bound to the
// session's Person, Butler and destination by the gateway factory
// (butlerstore.MemorySeams) and authorised per call like any other
// tool. Phase 3 binds MemoryReadContext from the handle inside this
// package; until then the methods here go through Invoke so that
// authorisation, disclosure filtering, redaction and the ledger row
// are exactly the ones a model's own recall call gets.

// Recall is not implemented in this phase. The recall tool renders
// its hits as text for a model, and a []MemoryHit needs the structured
// read that Phase 3's gateway-side binding of MemoryReadContext
// supplies (ADR-W039 §3.5, §7.2 C12). A runtime that wants recall
// today invokes the "recall" tool through Invoke and receives the
// same text the native runner does.
func (g *Gateway) Recall(context.Context, agentapi.RecallRequest) ([]agentapi.MemoryHit, error) {
	return nil, &agentapi.NotImplementedError{
		Method: "Recall", Phase: "Phase 3",
		Reason: "structured hits need the gateway-side MemoryReadContext binding; invoke the recall tool for the rendered form",
	}
}

// Remember implements agentapi.Client through the remember tool. The
// tool enforces §3.5 rule 1 today — personal or agent_local, never
// household; fact or preference, never an inference; the speaker's
// own audience — and refuses through the same authority seam every
// tool does. A refusal is returned as an error wrapping ErrRefused,
// because Remember has no outcome to carry it.
func (g *Gateway) Remember(ctx context.Context, req agentapi.RememberRequest) error {
	if req.Content == "" {
		return fmt.Errorf("%w: nothing to remember", ErrRefused)
	}
	args := map[string]any{"content": req.Content}
	if req.Scope != "" {
		args["scope"] = string(req.Scope)
	}
	if req.Kind != "" {
		args["kind"] = string(req.Kind)
	}
	if req.Subject != "" {
		args["subject"] = req.Subject
	}
	body, err := json.Marshal(args)
	if err != nil {
		return err
	}
	res, err := g.Invoke(ctx, agentapi.InvokeRequest{Handle: req.Handle, Tool: "remember", Args: body})
	if err != nil {
		return err
	}
	if res.Outcome != agentapi.OutcomeOK {
		return fmt.Errorf("%w: remember %s: %s", ErrRefused, res.Outcome, res.Reason)
	}
	return nil
}
