package agentruntime

import (
	"context"
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
