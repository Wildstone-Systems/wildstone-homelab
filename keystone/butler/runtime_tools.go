package butler

import (
	"context"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime"
)

// RuntimeTools lived in keystone/agentruntime as ButlerTools. It moved
// here when that package became a leaf (ADR-W039): agentruntime is
// imported by butler, so it cannot import butler back.
//
// RuntimeTools serves an existing Butler surface (the operator
// ToolGateway or the HouseholdGateway) as a ToolHost. Authorization,
// redaction, enveloping of untrusted content and size bounds all stay in
// the gateway, where every other Butler path already goes through them.
type RuntimeTools struct {
	Toolset Toolset
	// ReadOnly is the QA-only verdict (ADR-W017 A4). It is applied at
	// the offered set AND at dispatch, as Session applies it.
	ReadOnly bool
}

func (b RuntimeTools) Specs() []agentruntime.ToolSpec {
	offered := b.Toolset.OfferedSpecs(b.ReadOnly)
	out := make([]agentruntime.ToolSpec, 0, len(offered))
	for _, t := range offered {
		params := t.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, agentruntime.ToolSpec{Name: t.Name, Description: t.Description, Parameters: params})
	}
	return out
}

func (b RuntimeTools) Call(ctx context.Context, name string, input map[string]any) (agentruntime.ToolResult, error) {
	// Asked first so a refusal is recorded as a refusal rather than as
	// ordinary tool output. The gateway asks the same question again at
	// dispatch, so a grant withdrawn between the two is still refused.
	if ok, why := b.Toolset.Permits(name, b.ReadOnly); !ok {
		return agentruntime.ToolResult{Content: why, Denied: true}, nil
	}
	out, err := b.Toolset.Execute(ctx, name, input, b.ReadOnly)
	if err != nil {
		// The error text is not handed to the model: it can echo input
		// the gateway has not redacted.
		return agentruntime.ToolResult{Content: "error: " + name + " could not be run", Failed: true}, nil
	}
	// A policy-rejected proposal is an answer, not a failure: the model
	// is expected to read why and change course.
	return agentruntime.ToolResult{Content: out.Content}, nil
}
