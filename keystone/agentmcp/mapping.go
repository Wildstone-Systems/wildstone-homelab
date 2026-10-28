// Package agentmcp contains the authority-free MCP presentation mapping shared
// by Keystone's OAuth resource and the static agent-bridge profile. It does not
// authenticate callers, resolve tasks, or dispatch tools.
package agentmcp

import (
	"encoding/json"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
)

const ProtocolVersion = "2025-06-18"

type ToolAnnotations struct {
	ReadOnlyHint bool `json:"readOnlyHint,omitempty"`
}

type Tool struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	InputSchema json.RawMessage  `json:"inputSchema"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

var defaultInputSchema = json.RawMessage(`{"type":"object"}`)

func ToolsFor(specs []agentapi.ToolSpec) []Tool {
	out := make([]Tool, 0, len(specs))
	for _, spec := range specs {
		schema := spec.InputSchema
		if len(schema) == 0 {
			schema = defaultInputSchema
		}
		out = append(out, Tool{
			Name: spec.Name, Description: spec.Description, InputSchema: schema,
			Annotations: &ToolAnnotations{ReadOnlyHint: spec.ReadOnlyHint},
		})
	}
	return out
}

type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ToolCallResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

func ToolResultFor(res agentapi.InvokeResult) ToolCallResult {
	switch res.Outcome {
	case agentapi.OutcomeOK:
		return ToolCallResult{Content: []Content{{Type: "text", Text: res.Content}}}
	case agentapi.OutcomeWaiting:
		text := "waiting: " + string(res.WaitReason)
		if res.Reason != "" {
			text += " (" + res.Reason + ")"
		}
		return ToolCallResult{Content: []Content{{Type: "text", Text: text}}}
	default:
		text := string(res.Outcome)
		if res.Reason != "" {
			text += ": " + res.Reason
		}
		return ToolCallResult{Content: []Content{{Type: "text", Text: text}}, IsError: true}
	}
}
