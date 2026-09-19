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
	// GrantRequired marks a tool that needs an explicit, work-bound
	// grant even when it is registered here and not disabled (ADR-W030
	// A8: tool access and action approval are separate). No v1 tool sets
	// it; the field exists so the resolver has something to read when
	// P12's and P13's write paths arrive, rather than being retrofitted
	// through every call site then.
	GrantRequired bool
	// OptIn marks an unusually consequential tool (ADR-W033 design
	// §6.2). It is a place to put such a tool without inventing a third
	// Agent class: its authority class stays whatever it is, and what
	// changes is that no broad grant picks it up — not a new Agent's
	// defaults, not a service bundle. Somebody names it, or it is not
	// there. No v1 tool sets it.
	OptIn bool
	// Describe is the variant for the self-capability tool, which is the
	// one tool whose answer depends on the session's own constraints
	// (ADR-W030 P2). Giving it a separate shape keeps readOnly out of
	// the other handlers' signatures, where it would be an ignored
	// parameter that a future handler might start trusting.
	Describe func(gw *ToolGateway, readOnly bool) ToolOutcome
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
	// Commitments answers where the household's commitments stand. An
	// interface so this package depends on neither the pass's timer nor
	// runtimedb; nil makes get_commitments say nothing is tracked.
	Commitments CommitmentStandings
	// Observed is the injected read-only observed-state seam the diagnose
	// tool walks (ADR-W017 A5) — cmd/keystone wires it over the runtime
	// DB's observed rows; this package imports neither reconcile nor
	// runtimedb. nil makes diagnose answer that no source is configured.
	Observed func() ([]ObservedResource, error)
	// AutomationVocabulary is the injected description of what a
	// household rule may say on THIS deployment (ADR-W024 H7): the event
	// types publishers declare and the actions the domestic gateway can
	// run. cmd/keystone wires it; this package imports neither the
	// catalog nor the gateway. nil makes get_automation_schema say the
	// deployment-specific half is unavailable rather than invent one.
	AutomationVocabulary func() (HouseholdVocabulary, error)
	// DryRunAutomation replays a draft rule document over the last n days
	// and returns a rendered summary — the same call the API's replay
	// endpoint makes, so the Butler and the operator cannot get different
	// verdicts on one rule. nil makes dry_run_automation say so.
	DryRunAutomation func(ctx context.Context, doc string, days int) (string, error)
	// MaxContentBytes bounds a single tool result's size before it's
	// handed back to the model. <= 0 uses defaultMaxContentBytes.
	MaxContentBytes int
	// Authorizer is the effective-authority seam (ADR-W030 P7). nil
	// means no administrative controls are configured and structural
	// presence decides, which is what this gateway did before P7.
	Authorizer ToolAuthorizer
	// SessionTask scopes work-bound grants to the task this session is
	// driving. Empty means no grant can apply, which is right for a
	// one-shot question that is not durable work.
	SessionTask string

	tools map[string]toolEntry
}

// SetAuthorizer binds the effective-authority resolver for one session,
// and SetSessionTask the task its work-bound grants are scoped to.
//
// They are methods rather than only fields so a caller holding the
// Toolset interface can bind them. That caller is the thing that serves
// a conversation as a named Butler: it cannot know which gateway it was
// handed, and it must be able to scope whichever one it was — a surface
// that could be served UNSCOPED because the binding did not typecheck
// is a surface that would eventually be served unscoped.
func (gw *ToolGateway) SetAuthorizer(a ToolAuthorizer) { gw.Authorizer = a }

// SetSessionTask scopes work-bound grants to one task.
func (gw *ToolGateway) SetSessionTask(id string) { gw.SessionTask = id }

// Scoped returns a copy of this gateway that resolves authority through
// a, leaving the original untouched and carrying no session's grants.
//
// It exists so something can ASK what one Butler may do without being
// that Butler's session. The host's gateway is shared by every request
// in flight, so binding an authorizer to it would be both a data race
// and a live session answering under a different Butler's narrowings.
// The copy is shallow on purpose: the seams are what this deployment
// wired and must be reported as they are, while the registry map is
// written once at construction and only read after.
//
// SessionTask is deliberately dropped rather than copied. A work-bound
// grant belongs to the task that was issued it (ADR-W030 A8), and a
// reader inheriting one would be shown a tool as available on the
// strength of a permission that expires with somebody else's work.
func (gw *ToolGateway) Scoped(a ToolAuthorizer) *ToolGateway {
	if gw == nil {
		return nil
	}
	out := *gw
	out.Authorizer = a
	out.SessionTask = ""
	return &out
}

// NewToolGateway builds a ToolGateway with the full ADR-W006 A3 tool set
// registered.
func NewToolGateway(store *statefile.Store, reader *AgentReader) *ToolGateway {
	gw := &ToolGateway{Store: store, Reader: reader}
	gw.tools = map[string]toolEntry{
		"get_system_overview":   {Spec: systemOverviewSpec, Handler: handleGetSystemOverview},
		"get_capability_status": {Spec: capabilityStatusSpec, Handler: handleGetCapabilityStatus},
		"get_health":            {Spec: healthSpec, Untrusted: true, Handler: handleGetHealth},
		"diagnose":              {Spec: diagnoseSpec, Untrusted: true, Handler: handleDiagnose},
		"get_plan":              {Spec: getPlanSpec, Handler: handleGetPlan},
		"explain_resource":      {Spec: explainResourceSpec, Handler: handleExplainResource},
		"get_automation_schema": {Spec: automationSchemaSpec, Handler: handleGetAutomationSchema},
		// Commitments: describe the schema, and report where things
		// stand. There is deliberately no third tool — an agent drafts
		// and reads, and a household whose agent can close its own
		// commitments has a tracker that agrees with itself and nothing
		// else (ADR-W035 A13).
		"get_commitment_schema": {Spec: commitmentSchemaSpec, Handler: handleGetCommitmentSchema},
		"get_commitments":       {Spec: commitmentStandingSpec, Handler: handleGetCommitments},
		// The dry run quotes household events back — subjects, payload
		// fields, whatever a sensor or another person's phone wrote — so
		// its result is attacker-influenceable like a health detail is.
		"dry_run_automation": {Spec: dryRunAutomationSpec, Untrusted: true, Handler: handleDryRunAutomation},
		"propose_change":     {Spec: proposeChangeSpec, Mutating: true, Handler: handleProposeChange},
		// The self-capability tool is read-only and available on both
		// surfaces; each reports only its own (ADR-W030 P2).
		"describe_capabilities": {Spec: describeCapabilitiesSpec, Describe: describeCapabilities},
		// Container logs are operator-only and go through the existing
		// read-only Wardstone operation. The family surface has no
		// AgentReader at all, so this tool cannot exist there.
		"get_container_logs": {Spec: containerLogsSpec, Untrusted: true, Handler: handleGetContainerLogs},
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
	return gw.dispatch(ctx, name, input, true)
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

// Dispatch executes the named tool with input and returns the fully
// processed outcome: EVERY tool's Content passes through redact() here —
// centralized so a new tool cannot forget it (ADR-W006 B3) — and
// attacker-influenceable tools additionally get the untrusted-content
// envelope, before the whole thing is size-bounded. This is the only path
// by which a tool result reaches a model; there is no way to hand a
// handler's raw output to a Provider.
func (gw *ToolGateway) Dispatch(ctx context.Context, name string, input map[string]any) (ToolOutcome, error) {
	return gw.dispatch(ctx, name, input, false)
}

// dispatch is Dispatch with the session's read-only constraint made
// explicit, so the self-capability tool can report the session it is
// actually in.
func (gw *ToolGateway) dispatch(ctx context.Context, name string, input map[string]any, readOnly bool) (ToolOutcome, error) {
	entry, ok := gw.tools[name]
	if !ok {
		// Structurally absent. The resolver would say the same thing,
		// and saying it here keeps an unknown tool an unknown tool
		// rather than a database round trip.
		return ToolOutcome{}, fmt.Errorf("butler: unknown tool %q", name)
	}
	// Administrative authority is checked before the handler runs, so a
	// disabled tool is refused even when a saved task or a cached
	// manifest still lists it (ADR-W030 A7).
	if d := authorize(gw.Authorizer, string(AgentClassOwner), name, true,
		entry.GrantRequired, gw.SessionTask, ""); !d.Allowed {
		return finishOutcome(refusal(name, d), false, gw.MaxContentBytes), nil
	}
	if entry.Describe != nil {
		return finishOutcome(entry.Describe(gw, readOnly), entry.Untrusted, gw.MaxContentBytes), nil
	}
	outcome, err := entry.Handler(ctx, gw, input)
	if err != nil {
		// Even error text is redacted/bounded — a handler error could echo
		// back attacker-influenced input.
		outcome.Content = fmt.Sprintf("error: %v", err)
	}
	return finishOutcome(outcome, entry.Untrusted, gw.MaxContentBytes), nil
}

// finishOutcome is the centralized post-handler pipeline BOTH surfaces
// run: redact, then envelope attacker-influenceable content, then bound
// the size. It lives here, shared, so the operator gateway and the
// family gateway cannot drift apart on the one step neither is allowed
// to skip (ADR-W006 A5.2/A5.3, ADR-W026 A6). maxBytes <= 0 means the
// package default.
func finishOutcome(outcome ToolOutcome, untrusted bool, maxBytes int) ToolOutcome {
	if maxBytes <= 0 {
		maxBytes = defaultMaxContentBytes
	}
	outcome.Content = redact(outcome.Content)
	if untrusted {
		// Bound the inner content BEFORE enveloping so truncation can never
		// cut the closing tag and leave the envelope unterminated for this
		// and every subsequent turn (the envelope tags themselves are small
		// and fixed, so the total stays close to the budget).
		outcome.Content = envelope(truncate(outcome.Content, maxBytes))
	} else {
		outcome.Content = truncate(outcome.Content, maxBytes)
	}
	return outcome
}

// OfferedSpecs implements Toolset (ADR-W026 A1): the operator surface's
// offered tool set, narrowed to reads for a QA-only session.
func (gw *ToolGateway) OfferedSpecs(readOnly bool) []provider.Tool {
	if readOnly {
		return gw.ReadOnlyToolSpecs()
	}
	return gw.ToolSpecs()
}

// Execute implements Toolset: dispatch with the session's read-only
// constraint enforced at the execution boundary.
func (gw *ToolGateway) Execute(ctx context.Context, name string, input map[string]any, readOnly bool) (ToolOutcome, error) {
	if readOnly {
		return gw.DispatchReadOnly(ctx, name, input)
	}
	return gw.Dispatch(ctx, name, input)
}

// SystemPrompt implements Toolset: the operator surface's prompt.
func (gw *ToolGateway) SystemPrompt() string { return SystemPrompt }

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
