package butler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

// defaultMaxContentBytes bounds how much text any single tool result
// contributes to the conversation before truncation (ADR-W006 A5.3, B3:
// "bound content sizes").
const defaultMaxContentBytes = 4000

// ToolOutcome is what a tool handler produces before the centralized
// Dispatch pipeline (redact -> envelope -> truncate) runs on it.
type ToolOutcome struct {
	// Content is the human/model-readable text result.
	Content string
	// Plan is non-nil for get_plan and propose_change: the plan.Plan that
	// was produced (for audit purposes), never applied.
	Plan *plan.Plan
	// ResourceYAML is the canonical YAML of the validated resource an
	// ACCEPTED propose_change described (ADR-W017 A3): what the host
	// submits through the shared proposal pipeline as principal "butler"
	// acting-for the caller. Empty for every other tool and for rejected
	// proposals. Carrying the document changes nothing about this
	// package's structural ceiling — the YAML only ever becomes a
	// PROPOSED plan record a human must approve.
	ResourceYAML string
	// Rejected is true when propose_change's policy check blocked the
	// proposal outright.
	Rejected bool
}

type toolHandler func(ctx context.Context, gw *ToolGateway, input map[string]any) (ToolOutcome, error)

type toolEntry struct {
	Spec provider.Tool
	// Untrusted marks a tool whose Content may contain attacker-
	// influenceable text (e.g. an agent-reported health detail) and must be
	// wrapped in the untrusted-content envelope (ADR-W006 A5.2).
	Untrusted bool
	// Mutating marks a tool that produces a Plan (propose_change). Such
	// tools are withheld from a ReadOnly/QAOnly session's offered set and,
	// as a structural backstop, refused at dispatch when the session did not
	// permit mutating tools — so a confused or injected model that names one
	// anyway cannot reach the handler (ADR-W006 A4).
	Mutating bool
	Handler  toolHandler
}

// ToolGateway is the Butler's tool gateway (ADR-W006 A3, B1): the read
// tools over real Keystone state plus the single plan-mediated write tool,
// propose_change. See doc.go for the structural no-apply guarantee.
type ToolGateway struct {
	// Store is the Statefile every read/propose tool consults.
	Store *statefile.Store
	// Reader is the Butler's entire live-node read surface (reader.go).
	// nil is valid for tests that only exercise Statefile-backed tools.
	Reader *AgentReader
	// DefaultNode is used by get_health when the model's tool call omits a
	// node.
	DefaultNode string
	// MaxContentBytes bounds a single tool result's size before it's
	// handed back to the model. <= 0 uses defaultMaxContentBytes.
	MaxContentBytes int

	tools map[string]toolEntry
}

// NewToolGateway builds a ToolGateway with the full ADR-W006 A3 tool set
// registered.
func NewToolGateway(store *statefile.Store, reader *AgentReader) *ToolGateway {
	gw := &ToolGateway{Store: store, Reader: reader}
	gw.tools = map[string]toolEntry{
		"get_system_overview":   {Spec: systemOverviewSpec, Handler: handleGetSystemOverview},
		"get_capability_status": {Spec: capabilityStatusSpec, Handler: handleGetCapabilityStatus},
		"get_health":            {Spec: healthSpec, Untrusted: true, Handler: handleGetHealth},
		"get_plan":              {Spec: getPlanSpec, Handler: handleGetPlan},
		"explain_resource":      {Spec: explainResourceSpec, Handler: handleExplainResource},
		"propose_change":        {Spec: proposeChangeSpec, Mutating: true, Handler: handleProposeChange},
	}
	return gw
}

// DispatchReadOnly executes the named tool but refuses any tool marked
// Mutating, returning a rejection outcome instead of running the handler.
// It is the enforcement half of ReadOnly/QAOnly mode: withholding a tool's
// spec (ReadOnlyToolSpecs) stops a well-behaved model from calling it, but a
// confused or injected model can still name it, so the dispatch path itself
// must refuse (ADR-W006 A4). Unknown tools error exactly as Dispatch does.
func (gw *ToolGateway) DispatchReadOnly(ctx context.Context, name string, input map[string]any) (ToolOutcome, error) {
	if entry, ok := gw.tools[name]; ok && entry.Mutating {
		return ToolOutcome{
			Content:  fmt.Sprintf("tool %q is not available in this read-only session", name),
			Rejected: true,
		}, nil
	}
	return gw.Dispatch(ctx, name, input)
}

// ToolSpecs returns the provider-neutral Tool declarations for every
// registered tool, sorted by name for determinism — what a Session offers
// a Provider on every Chat call.
func (gw *ToolGateway) ToolSpecs() []provider.Tool {
	names := make([]string, 0, len(gw.tools))
	for name := range gw.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	specs := make([]provider.Tool, 0, len(names))
	for _, name := range names {
		specs = append(specs, gw.tools[name].Spec)
	}
	return specs
}

// ReadOnlyToolSpecs returns only the always-allowed read tools (no
// propose_change) — for QAOnly-mode sessions (ADR-W006 A4).
func (gw *ToolGateway) ReadOnlyToolSpecs() []provider.Tool {
	all := gw.ToolSpecs()
	out := make([]provider.Tool, 0, len(all))
	for _, t := range all {
		if t.Name == "propose_change" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// maxContentBytes returns gw.MaxContentBytes, defaulted.
func (gw *ToolGateway) maxContentBytes() int {
	if gw.MaxContentBytes > 0 {
		return gw.MaxContentBytes
	}
	return defaultMaxContentBytes
}

// Dispatch executes the named tool with input and returns the fully
// processed outcome: EVERY tool's Content passes through redact() here —
// centralized so a new tool cannot forget it (ADR-W006 B3) — and
// attacker-influenceable tools additionally get the untrusted-content
// envelope, before the whole thing is size-bounded. This is the only path
// by which a tool result reaches a model; there is no way to hand a
// handler's raw output to a Provider.
func (gw *ToolGateway) Dispatch(ctx context.Context, name string, input map[string]any) (ToolOutcome, error) {
	entry, ok := gw.tools[name]
	if !ok {
		return ToolOutcome{}, fmt.Errorf("butler: unknown tool %q", name)
	}
	outcome, err := entry.Handler(ctx, gw, input)
	if err != nil {
		// Even error text is redacted/bounded — a handler error could echo
		// back attacker-influenced input.
		outcome.Content = fmt.Sprintf("error: %v", err)
	}
	outcome.Content = redact(outcome.Content)
	if entry.Untrusted {
		// Bound the inner content BEFORE enveloping so truncation can never
		// cut the closing tag and leave the envelope unterminated for this
		// and every subsequent turn (the envelope tags themselves are small
		// and fixed, so the total stays close to the budget).
		inner := truncate(outcome.Content, gw.maxContentBytes())
		outcome.Content = envelope(inner)
	} else {
		outcome.Content = truncate(outcome.Content, gw.maxContentBytes())
	}
	return outcome, nil
}

// truncate bounds s to at most n bytes, appending a marker if it cut
// content off (ADR-W006 A5.3/B3: bound content sizes fed back to the
// model). The cut is made on a UTF-8 rune boundary so it never splits a
// multi-byte rune into invalid bytes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n...[truncated, %d bytes omitted]", len(s)-cut)
}

// stringInput reads a string field from a tool-call input map, defaulting
// to "" if absent or the wrong type.
func stringInput(input map[string]any, key string) string {
	v, ok := input[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// mapInput reads a nested map field from a tool-call input map.
func mapInput(input map[string]any, key string) map[string]any {
	v, ok := input[key]
	if !ok {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

// joinNonEmpty joins non-empty strings with sep — a small formatting
// helper used by several read-tool handlers.
func joinNonEmpty(parts []string, sep string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}
