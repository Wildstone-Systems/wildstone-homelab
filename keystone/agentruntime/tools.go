package agentruntime

import (
	"context"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
)

// ToolSpec is one tool offered to an agent. Parameters is a JSON Schema
// object.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ToolResult is what a tool call produced, already redacted and bounded
// for the model.
type ToolResult struct {
	Content string
	// Denied is true when authorization refused the call. Content then
	// explains the refusal to the model.
	Denied bool
	// Failed is true when the tool ran and errored.
	Failed bool
	// Block, when set, stops the job: BLOCKED, with Block as the reason,
	// and the model is not told anything further. A host uses it when
	// the job must not continue without a person — a spent budget, an
	// outcome it could not confirm.
	Block string
}

// ToolHost is the Wildstone tool surface a runtime drives. It is the
// ONLY way an agent reaches a tool: a runtime never holds a handler, so
// there is no path around authorization.
type ToolHost interface {
	// Specs is the set offered to the model: only tools the caller may
	// use right now.
	Specs() []ToolSpec
	// Call authorizes and runs one tool. A refusal is a result
	// (Denied), not an error. An error means the job must not continue
	// at all — the host broke, or the authority behind the whole job was
	// withdrawn — and ends it FAILED.
	Call(ctx context.Context, name string, input map[string]any) (ToolResult, error)
}

// ButlerTools serves an existing Butler surface (the operator
// ToolGateway or the HouseholdGateway) as a ToolHost. Authorization,
// redaction, enveloping of untrusted content and size bounds all stay in
// the gateway, where every other Butler path already goes through them.
type ButlerTools struct {
	Toolset butler.Toolset
	// ReadOnly is the QA-only verdict (ADR-W017 A4). It is applied at
	// the offered set AND at dispatch, as Session applies it.
	ReadOnly bool
}

func (b ButlerTools) Specs() []ToolSpec {
	offered := b.Toolset.OfferedSpecs(b.ReadOnly)
	out := make([]ToolSpec, 0, len(offered))
	for _, t := range offered {
		params := t.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, ToolSpec{Name: t.Name, Description: t.Description, Parameters: params})
	}
	return out
}

func (b ButlerTools) Call(ctx context.Context, name string, input map[string]any) (ToolResult, error) {
	// Asked first so a refusal is recorded as a refusal rather than as
	// ordinary tool output. The gateway asks the same question again at
	// dispatch, so a grant withdrawn between the two is still refused.
	if ok, why := b.Toolset.Permits(name, b.ReadOnly); !ok {
		return ToolResult{Content: why, Denied: true}, nil
	}
	out, err := b.Toolset.Execute(ctx, name, input, b.ReadOnly)
	if err != nil {
		// The error text is not handed to the model: it can echo input
		// the gateway has not redacted.
		return ToolResult{Content: "error: " + name + " could not be run", Failed: true}, nil
	}
	// A policy-rejected proposal is an answer, not a failure: the model
	// is expected to read why and change course.
	return ToolResult{Content: out.Content}, nil
}
