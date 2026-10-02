package butler

import (
	"context"
	"encoding/json"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"strings"
	"unicode/utf8"
)

// WorkspaceWriteCall carries only model selectors/content. Trusted task and
// Invoke idempotency identity are carried separately by operation context.
type WorkspaceWriteCall struct {
	Scope            string  `json:"scope,omitempty"`
	Path             string  `json:"path,omitempty"`
	ExpectedRevision *string `json:"expectedRevision"`
	ArtifactID       string  `json:"artifactId,omitempty"`
	WriteID          string  `json:"writeId,omitempty"`
	Index            int     `json:"index,omitempty"`
	Text             string  `json:"text,omitempty"`
	Revision         string  `json:"revision,omitempty"`
	OldText          string  `json:"oldText,omitempty"`
	NewText          string  `json:"newText,omitempty"`
}
type WorkspaceWriteSeam func(context.Context, string, WorkspaceWriteCall) ToolOutcome

var workspaceWriteSpecs = []provider.Tool{
	writeSpec("workspace_write_begin", "Begin a private staged create or full edit against the current revision.", []string{"scope", "path", "expectedRevision"}, "scope", "path", "expectedRevision", "artifactId"),
	writeSpec("workspace_write_chunk", "Append a consecutive bounded UTF-8 text chunk; exact retries reuse it.", []string{"writeId", "index", "text"}, "writeId", "index", "text"),
	writeSpec("workspace_write_commit", "Atomically publish a staged file after current authority and revision checks.", []string{"writeId"}, "writeId"),
	writeSpec("workspace_patch", "Replace exactly one occurrence against an explicit committed revision.", []string{"artifactId", "revision", "oldText", "newText"}, "artifactId", "revision", "oldText", "newText"),
}

func workspaceWriteTool(name string) bool {
	for _, s := range workspaceWriteSpecs {
		if s.Name == name {
			return true
		}
	}
	return false
}
func writeSpec(name, description string, required []string, fields ...string) provider.Tool {
	props := map[string]any{}
	for _, field := range fields {
		bound := 128
		switch field {
		case "scope":
			props[field] = map[string]any{"type": "string", "enum": []string{"private"}}
			continue
		case "index":
			props[field] = map[string]any{"type": "integer", "minimum": 0, "maximum": 10239}
			continue
		case "expectedRevision":
			props[field] = map[string]any{"type": []string{"string", "null"}, "maxLength": 128}
			continue
		case "path":
			bound = 4096
		case "text", "oldText", "newText":
			bound = 1024
		}
		props[field] = map[string]any{"type": "string", "maxLength": bound}
	}
	return provider.Tool{Name: name, Description: description, InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": props, "required": required}}
}
func workspaceWriteHandler(ctx context.Context, name string, seam WorkspaceWriteSeam, input map[string]any, max int) ToolOutcome {
	invalid := ToolOutcome{Failed: true, Content: "invalid_input: invalid workspace write request"}
	var spec provider.Tool
	for _, s := range workspaceWriteSpecs {
		if s.Name == name {
			spec = s
		}
	}
	props := spec.InputSchema["properties"].(map[string]any)
	for key, value := range input {
		prop, ok := props[key]
		if !ok {
			return invalid
		}
		if key == "expectedRevision" && value == nil {
			continue
		}
		if key == "index" {
			switch v := value.(type) {
			case int:
				if v < 0 || v > 10239 {
					return invalid
				}
			case float64:
				if v < 0 || v > 10239 || v != float64(int(v)) {
					return invalid
				}
			default:
				return invalid
			}
			continue
		}
		text, ok := value.(string)
		if !ok || !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
			return invalid
		}
		if bound, ok := prop.(map[string]any)["maxLength"].(int); ok && len(text) > bound {
			return invalid
		}
	}
	for _, key := range spec.InputSchema["required"].([]string) {
		if _, ok := input[key]; !ok {
			return invalid
		}
	}
	data, _ := json.Marshal(input)
	var call WorkspaceWriteCall
	if json.Unmarshal(data, &call) != nil {
		return invalid
	}
	if name == "workspace_write_begin" && (call.Scope != "private" || call.Path == "" || (call.ArtifactID == "") != (call.ExpectedRevision == nil) || (call.ExpectedRevision != nil && *call.ExpectedRevision == "")) {
		return invalid
	}
	if name == "workspace_patch" && (call.ArtifactID == "" || call.Revision == "" || call.OldText == "" || len(call.OldText)+len(call.NewText) > 1024) {
		return invalid
	}
	if (name == "workspace_write_chunk" || name == "workspace_write_commit") && call.WriteID == "" {
		return invalid
	}
	if seam == nil {
		return ToolOutcome{Failed: true, Content: "storage_unavailable: workspace writes not configured"}
	}
	out := seam(ctx, name, call)
	if max <= 0 {
		max = defaultMaxContentBytes
	}
	if !out.Failed && !out.Rejected && len(envelope(redact(out.Content))) > max {
		return ToolOutcome{Failed: true, Content: "storage_unavailable: workspace receipt cannot fit this result bound"}
	}
	return out
}
func registerWorkspaceWrites(gw *ToolGateway) {
	for _, spec := range workspaceWriteSpecs {
		name := spec.Name
		gw.tools[name] = toolEntry{Spec: spec, Mutating: true, OptIn: true, Untrusted: true, Reaches: "workspace", Handler: func(ctx context.Context, g *ToolGateway, in map[string]any) (ToolOutcome, error) {
			return workspaceWriteHandler(ctx, name, g.WorkspaceWrites, in, g.MaxContentBytes), nil
		}}
	}
}
func registerHouseholdWorkspaceWrites(gw *HouseholdGateway) {
	for _, spec := range workspaceWriteSpecs {
		name := spec.Name
		gw.tools[name] = householdEntry{Spec: spec, Mutating: true, OptIn: true, Untrusted: true, Reaches: "workspace", Handler: func(ctx context.Context, g *HouseholdGateway, in map[string]any) (ToolOutcome, error) {
			if g.group {
				return ToolOutcome{Rejected: true, Content: "permission_denied: private writes require a direct conversation"}, nil
			}
			return workspaceWriteHandler(ctx, name, g.seams.WorkspaceWrites, in, g.seams.MaxContentBytes), nil
		}}
	}
}
