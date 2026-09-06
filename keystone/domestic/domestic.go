// Package domestic is the household action layer (ADR-W024 A5): the
// gateway every Automation's action passes through, and the typed
// executors behind it.
//
// The rule it exists to enforce is that a household rule acts through a
// narrow, declared, permissioned operation and nothing else. An
// automation names "<capability>.<tool>"; the gateway resolves that to a
// registered Executor, checks the tier is one a rule may carry, checks
// the owning package actually declares the tool, bounds the call with a
// timeout, and audits it. There is no path here that runs an arbitrary
// command, touches the Statefile, or reaches a node agent — those are
// plans, and stay plans.
//
// The tier check is deliberately a second copy of the policy rule
// (keystone/policy RuleAutomationActionTier). Policy refuses such a rule
// at proposal; this refuses the call at dispatch. A rule that reached
// the engine some other way — an exemption, a hand-edited Statefile, a
// future path nobody has thought of — still cannot run an
// infrastructure-tier action.
package domestic

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// DefaultTimeout bounds one action. A household action is a notification
// or a small API call; anything slower has failed.
const DefaultTimeout = 15 * time.Second

// allowedTiers is the closed set of tiers a household rule's action may
// carry — the same set keystone/policy enforces at proposal.
var allowedTiers = map[string]bool{"read": true, "domestic": true}

// Call is one action the gateway is asked to perform, with the context
// that explains why. Params are already rendered: the engine resolves a
// rule's templates against its trigger before dispatch, so an executor
// receives values, never templates.
type Call struct {
	// Tool is "<capability>.<tool>", e.g. notifications.notify.
	Tool string
	// Tier is the tier the rule declares for this action.
	Tier string
	// Params are the executor's rendered inputs.
	Params map[string]string

	// Rule is the Automation that fired.
	Rule string
	// Subject is the household member/device the trigger was about.
	Subject string
	// CorrelationID ties this action, its trigger, and any answer
	// together on the household stream.
	CorrelationID string
	// TriggerType is the household event type that fired the rule.
	TriggerType string
}

// Capability and Tool split Call.Tool into its two halves.
func (c Call) Capability() string { cap, _, _ := strings.Cut(c.Tool, "."); return cap }
func (c Call) ToolName() string   { _, tool, _ := strings.Cut(c.Tool, "."); return tool }

// Result is what an executor did, in terms a person reading the
// household stream can follow.
type Result struct {
	// Summary is one line: "notified topic household-mika".
	Summary string
	// Detail is small structured context for the audit record.
	Detail map[string]string
}

// Executor performs one typed operation. An executor is registered under
// its full "<capability>.<tool>" name and never sees a call for another.
type Executor interface {
	// Name is the "<capability>.<tool>" this executor answers to.
	Name() string
	// Execute performs the operation. It must respect ctx's deadline and
	// must not retry on its own — the gateway owns the bound, and a
	// household action that needs retrying is one a later trigger will
	// raise again.
	Execute(ctx context.Context, c Call) (Result, error)
}

// Gateway dispatches calls to registered executors under the checks
// above. The zero value refuses everything (no executors registered),
// which is the right posture for a control plane with no capabilities
// installed.
type Gateway struct {
	// Timeout bounds one call; zero means DefaultTimeout.
	Timeout time.Duration
	// Declared, when set, reports the tier a capability's package
	// declares for a tool, and whether it declares it at all
	// (ADR-W005 A1 agentTools). cmd/keystone wires it from the catalog.
	//
	// nil skips ONLY the declaration check — a deployment with no
	// catalog configured can still run the executors it has registered,
	// and the tier check below still runs. It never widens what a rule
	// may carry.
	Declared func(capability, tool string) (tier string, ok bool)
	// Audit, when set, is called after every dispatch, successful or
	// not. cmd/keystone wires it to the operational event stream so a
	// household action is visible where every other Keystone action is.
	Audit func(c Call, r Result, err error)
	Log   *slog.Logger

	executors map[string]Executor
}

// Register adds an executor. A second registration for the same name
// replaces the first — wiring is a startup act, and a silent duplicate
// would be two implementations of one tool name.
func (g *Gateway) Register(e Executor) {
	if g.executors == nil {
		g.executors = map[string]Executor{}
	}
	g.executors[e.Name()] = e
}

// Tools lists the registered tool names, sorted.
func (g *Gateway) Tools() []string {
	out := make([]string, 0, len(g.executors))
	for name := range g.executors {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// RefusalError is a call the gateway would not make. It is separated
// from an executor's own failure because the two mean different things
// to whoever reads the household stream: a refusal is a rule asking for
// something it may not have, and a failure is a thing that went wrong.
type RefusalError struct {
	Tool   string
	Reason string
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("domestic: refused %s: %s", e.Tool, e.Reason)
}

// Dispatch runs one call under the gateway's checks and bound.
func (g *Gateway) Dispatch(ctx context.Context, c Call) (Result, error) {
	res, err := g.dispatch(ctx, c)
	if g.Audit != nil {
		g.Audit(c, res, err)
	}
	return res, err
}

func (g *Gateway) dispatch(ctx context.Context, c Call) (Result, error) {
	if !allowedTiers[c.Tier] {
		return Result{}, &RefusalError{Tool: c.Tool, Reason: fmt.Sprintf(
			"tier %q: a household rule may only run read or domestic actions; infrastructure changes are plans (ADR-W024 A5)", c.Tier)}
	}
	capability, tool := c.Capability(), c.ToolName()
	if capability == "" || tool == "" {
		return Result{}, &RefusalError{Tool: c.Tool, Reason: "tool must be <capability>.<tool>"}
	}
	if g.Declared != nil {
		declaredTier, ok := g.Declared(capability, tool)
		if !ok {
			return Result{}, &RefusalError{Tool: c.Tool, Reason: fmt.Sprintf(
				"capability %q declares no agentTool %q: an action must be one its package offers (ADR-W005 A1)", capability, tool)}
		}
		if declaredTier != c.Tier {
			return Result{}, &RefusalError{Tool: c.Tool, Reason: fmt.Sprintf(
				"the rule carries tier %q but %s declares the tool at %q", c.Tier, capability, declaredTier)}
		}
	}
	e, ok := g.executors[c.Tool]
	if !ok {
		return Result{}, &RefusalError{Tool: c.Tool, Reason: "no executor is registered for this tool on this control plane"}
	}

	timeout := g.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := e.Execute(ctx, c)
	if err != nil {
		return res, fmt.Errorf("domestic: %s: %w", c.Tool, err)
	}
	if g.Log != nil {
		g.Log.Info("domestic action", "tool", c.Tool, "rule", c.Rule, "summary", res.Summary)
	}
	return res, nil
}

// DeclaredFromPackages builds a Declared function over a set of parsed
// packages: capability → tool → tier, from each package's agentTools
// block. Kept here rather than in cmd/keystone so the mapping from a
// package's declaration to the gateway's check has one definition.
func DeclaredFromPackages(pkgs []AgentToolSource) func(capability, tool string) (string, bool) {
	byCapability := map[string]map[string]string{}
	for _, p := range pkgs {
		tools := byCapability[p.Capability]
		if tools == nil {
			tools = map[string]string{}
			byCapability[p.Capability] = tools
		}
		for name, tier := range p.Tools {
			tools[name] = tier
		}
	}
	return func(capability, tool string) (string, bool) {
		tier, ok := byCapability[capability][tool]
		return tier, ok
	}
}

// AgentToolSource is one package's agentTools declaration, flattened.
// The caller (cmd/keystone) reads it off keystone/pkgspec; this package
// stays free of the package format.
type AgentToolSource struct {
	Capability string
	// Tools maps a tool name to the risk tier its package declares.
	Tools map[string]string
}
