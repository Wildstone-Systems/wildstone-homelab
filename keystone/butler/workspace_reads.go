package butler

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
)

// WorkspaceReadCall contains only narrowing selectors. The seam resolves task,
// Person, Agent and privacy context from trusted dispatch, never these fields.
type WorkspaceReadCall struct {
	Scope      string `json:"scope,omitempty"`
	Path       string `json:"path,omitempty"`
	Query      string `json:"query,omitempty"`
	ArtifactID string `json:"artifactId,omitempty"`
	Revision   string `json:"revision,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
}

type WorkspaceReadSeam func(context.Context, string, WorkspaceReadCall) ToolOutcome

var workspaceReadSpecs = []provider.Tool{
	workspaceReadSpec("workspace_list", "List authorized committed files and directories in your private workspace.", []string{"scope"}, "scope", "path", "cursor"),
	workspaceReadSpec("workspace_read", "Read an explicit committed file revision as bounded untrusted text pages.", []string{"artifactId", "revision"}, "artifactId", "revision", "cursor"),
	workspaceReadSpec("workspace_search", "Search authorized private workspace filenames and file text with bounded snippets.", []string{"scope", "query"}, "scope", "query", "path", "cursor"),
}

func workspaceReadSpec(name, description string, required []string, fields ...string) provider.Tool {
	properties := map[string]any{}
	for _, field := range fields {
		bound := 128
		switch field {
		case "scope":
			properties[field] = map[string]any{"type": "string", "enum": []string{"private"}}
			continue
		case "path":
			bound = 4096
		case "query":
			bound = 1024
		case "cursor":
			bound = 512
		}
		properties[field] = map[string]any{"type": "string", "maxLength": bound}
	}
	return provider.Tool{Name: name, Description: description, InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}}
}

func workspaceReadHandler(ctx context.Context, name string, seam WorkspaceReadSeam, input map[string]any, maxBytes int) ToolOutcome {
	var spec provider.Tool
	for _, candidate := range workspaceReadSpecs {
		if candidate.Name == name {
			spec = candidate
		}
	}
	invalid := ToolOutcome{Content: "invalid_input: invalid workspace read request", Failed: true}
	properties := spec.InputSchema["properties"].(map[string]any)
	for key, value := range input {
		property, exists := properties[key]
		text, ok := value.(string)
		if !exists || !ok || !utf8.ValidString(text) {
			return invalid
		}
		if bound, ok := property.(map[string]any)["maxLength"].(int); ok && len(text) > bound {
			return invalid
		}
	}
	for _, key := range spec.InputSchema["required"].([]string) {
		if value, ok := input[key].(string); !ok || strings.TrimSpace(value) == "" {
			return invalid
		}
	}
	data, _ := json.Marshal(input)
	var call WorkspaceReadCall
	if json.Unmarshal(data, &call) != nil {
		return invalid
	}
	if name != "workspace_read" && call.Scope != "private" {
		return invalid
	}
	if seam == nil {
		return ToolOutcome{Content: "storage_unavailable: workspace reads are not configured", Failed: true}
	}
	out := seam(ctx, name, call)
	if maxBytes <= 0 {
		maxBytes = defaultMaxContentBytes
	}
	if !out.Failed && !out.Rejected && len(envelope(redact(out.Content))) > maxBytes {
		return ToolOutcome{Failed: true, Content: "storage_unavailable: workspace page cannot fit this result bound"}
	}
	return out
}

func registerOperatorWorkspaceReads(gw *ToolGateway) {
	for _, spec := range workspaceReadSpecs {
		name := spec.Name
		gw.tools[name] = toolEntry{Spec: spec, OptIn: true, Untrusted: true, Reaches: "workspace", Handler: func(ctx context.Context, gw *ToolGateway, input map[string]any) (ToolOutcome, error) {
			return workspaceReadHandler(ctx, name, gw.WorkspaceReads, input, gw.MaxContentBytes), nil
		}}
	}
}

func registerHouseholdWorkspaceReads(gw *HouseholdGateway) {
	for _, spec := range workspaceReadSpecs {
		name := spec.Name
		gw.tools[name] = householdEntry{Spec: spec, OptIn: true, Untrusted: true, Reaches: "workspace", Handler: func(ctx context.Context, gw *HouseholdGateway, input map[string]any) (ToolOutcome, error) {
			if gw.group {
				return ToolOutcome{Rejected: true, Content: "permission_denied: private workspace reads require a direct conversation"}, nil
			}
			return workspaceReadHandler(ctx, name, gw.seams.WorkspaceReads, input, gw.seams.MaxContentBytes), nil
		}}
	}
}
