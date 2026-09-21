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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// DefaultTimeout bounds one action. A household action is a notification
// or a small API call; anything slower has failed.
const DefaultTimeout = 15 * time.Second

// Tiers a household action may carry.
//
// ADR-W024 A5 originally had two, and two turned out to be one axis
// where the world has two questions. "May this run" depends on what the
// action does AND on how it was invoked: a rule that fires at 3am and
// deletes a calendar event is a different situation from a person in
// chat saying "cancel Friday's dentist", and until `confirm` existed
// both were `domestic` and the system could not tell them apart. See
// ADR-W024 A5.1.
const (
	// TierRead reads something and changes nothing.
	TierRead = "read"
	// TierDomestic acts, and may do so unattended.
	TierDomestic = "domestic"
	// TierConfirm acts only with a person in the loop AND only after
	// that person has said yes to this specific call.
	TierConfirm = "confirm"
)

// tierAllowed reports whether a call may carry a tier, given how it
// arrived.
//
// Attendance is the second axis. An unattended rule may not carry
// `confirm` at all — not "may carry it and will be asked", because there
// is nobody to ask and a question nobody answers is a rule that silently
// stops working.
// TierAllowed is exported because three places need this answer and
// three copies of it is how they come to disagree: this gateway at
// dispatch, the Butler's family surface when it lists what the household
// can do, and the catalog test that checks a package declares nothing
// uncallable. One definition, three callers.
func TierAllowed(tier string, attended bool) bool { return tierAllowed(tier, attended) }

func tierAllowed(tier string, attended bool) bool {
	switch tier {
	case TierRead, TierDomestic:
		return true
	case TierConfirm:
		return attended
	}
	return false
}

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

	// Attended is true when a PERSON is waiting for this — a family
	// session, where somebody asked and is reading the answer — and
	// false when a rule fired on its own.
	//
	// Set by the CALLER rather than derived here from Rule or
	// TriggerType, because deriving it would mean this package deciding
	// whether a person is present by matching a string, and every future
	// surface would have to know the magic value to be believed.
	Attended bool
}

// Capability and Tool split Call.Tool into its two halves.
func (c Call) Capability() string { cap, _, _ := strings.Cut(c.Tool, "."); return cap }
func (c Call) ToolName() string   { _, tool, _ := strings.Cut(c.Tool, "."); return tool }

// Result is what an executor did, in terms a person reading the
// household stream can follow.
type Result struct {
	EffectPending bool
	// EffectConfirmed is set only after the executor confirms a completed mutation.
	EffectConfirmed   bool
	ProposalConfirmed bool
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
	Authority HouseholdExecutionAuthority
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
	// Confirm, when set, puts a question to a household member and
	// reports where they have got to with it. cmd/keystone binds it to
	// the notices table, which is the household's own ask-and-answer
	// channel: per member, expiring, single-use, answerable from
	// whichever surface they are actually on, and deliberately NOT tied
	// to any one notification capability.
	//
	// nil refuses every confirm-tier call rather than performing it. A
	// control plane with no way to ask is not a control plane with
	// permission.
	Confirm func(ctx context.Context, req ConfirmRequest) (Confirmation, error)
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

// UsageDescriber is an executor that can say what it takes, in general
// (Describer, below, says what one particular call would do). Optional:
// an executor without it is still dispatched, it is just offered to a
// model by name alone.
type UsageDescriber interface {
	// Usage is one line of plain text naming the params and their
	// shapes, written for a model deciding what to send.
	Usage() string
}

// Usage returns what a registered tool takes, or "" when the tool is
// not registered or does not describe itself.
//
// It exists because a family Butler is handed ONE tool that performs
// any household action, and the actions' inputs lived only in comments.
// A model that cannot see "date (today | tomorrow | YYYY-MM-DD)" guesses
// — and on the first day of the calendar it guessed the household
// timeline instead, and told a member the calendar was empty.
func (g *Gateway) Usage(tool string) string {
	if d, ok := g.executors[tool].(UsageDescriber); ok {
		return d.Usage()
	}
	return ""
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
	if c.Tier != "read" {
		if err := runtimedb.CheckPrivacyDelivery(ctx, runtimedb.MemoryDestination{}); err != nil {
			return Result{}, err
		}
	}
	if !tierAllowed(c.Tier, c.Attended) {
		if c.Tier == TierConfirm {
			return Result{}, &RefusalError{Tool: c.Tool, Reason: "this action needs somebody to confirm it, " +
				"and a rule firing on its own has nobody to ask (ADR-W024 A5.1)"}
		}
		return Result{}, &RefusalError{Tool: c.Tool, Reason: fmt.Sprintf(
			"tier %q: a household action may only be read, domestic or confirm; infrastructure changes are plans (ADR-W024 A5)", c.Tier)}
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

	if c.Tier != TierRead {
		if err := checkExecution(ctx, g.Authority); err != nil {
			return Result{}, err
		}
	}
	if c.Tier == TierConfirm {
		if err := g.confirmed(ctx, c, e); err != nil {
			return Result{}, err
		}
	}

	timeout := g.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if c.Tier != TierRead {
		if err := checkExecution(ctx, g.Authority); err != nil {
			return Result{}, err
		}
	}
	if err := CheckExecutionContext(ctx); err != nil {
		return Result{}, err
	}
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
	// OptIn names the tools this package declared as opt-in (ADR-W033
	// design §6.2). A separate map rather than a field on Tools so the
	// risk-tier lookup every household call already does keeps its
	// shape; this one is asked only when a bundle or a default is being
	// worked out.
	OptIn map[string]bool
	// Tools maps a tool name to the risk tier its package declares.
	Tools map[string]string
}

// --- confirmation (ADR-W024 A5.1) ---

// Confirmation is where a household member has got to on one question.
type Confirmation int

const (
	// ConfirmAsked: the question has just been put to them.
	ConfirmAsked Confirmation = iota
	// ConfirmPending: it was put to them earlier and they have not
	// answered yet.
	ConfirmPending
	// ConfirmApproved: they said yes, to THIS call.
	ConfirmApproved
	// ConfirmDenied: they said no.
	ConfirmDenied
)

// ConfirmRequest is one question to put to one person.
type ConfirmRequest struct {
	// Tool and Member are who is being asked about what.
	Tool, Member string
	// Key identifies THIS call — the tool and its arguments — so that a
	// yes authorizes the thing that was asked about and nothing else.
	// Approving "delete the dentist appointment on Friday" must not
	// approve "delete dinner on Saturday".
	Key string
	// Question is what the person reads, phrased by the executor when it
	// can (see Describer) so they are answering about an event rather
	// than about a tool name and a parameter list.
	Question string
	// CorrelationID ties the question, the answer and the action
	// together on the household stream.
	CorrelationID string
}

// Describer is an Executor that can say what a call WOULD do, without
// doing it.
//
// Optional, and worth implementing for anything at the confirm tier: a
// person answering "Butler wants to run delete_event with title=dentist"
// is being asked to approve a tool call, where "Delete 'Dentist for Sam'
// from Fri 18 Sep?" is a question about their week. It also moves an
// executor's own refusals EARLIER — an ambiguous match is reported
// before somebody is asked to confirm a thing the gateway could not have
// performed anyway.
type Describer interface {
	Describe(ctx context.Context, c Call) (string, error)
}

// confirmKey identifies one call for the purpose of consent.
//
// Over the tool, the subject and every rendered parameter, sorted — so
// it is stable across map iteration order, and so that changing any
// argument makes it a different question. That is the whole security
// property: consent is bound to the call, not to the tool.
func confirmKey(c Call) string {
	keys := make([]string, 0, len(c.Params))
	for k := range c.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00", c.Tool, c.Subject)
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%s\x00", k, c.Params[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// confirmed puts the question and reports whether the answer lets this
// call proceed.
//
// Three of the four outcomes are refusals, and all three are phrased for
// the person who will read them through the surface's "did not run"
// wording. The model cannot turn any of them into a success, because it
// never sees a token: what it gets back is a refusal, and what actually
// authorizes the retry is a row in the notices table written by the
// member the question was asked of (runtimedb.AnswerNotice checks the
// answerer, the expiry, and that a reply resolves to a clear yes or no).
func (g *Gateway) confirmed(ctx context.Context, c Call, e Executor) error {
	if g.Confirm == nil {
		return &RefusalError{Tool: c.Tool, Reason: "this action needs confirming and this control plane has " +
			"no way to ask anybody"}
	}
	if strings.TrimSpace(c.Subject) == "" {
		return &RefusalError{Tool: c.Tool, Reason: "this action needs confirming and this session is not bound " +
			"to a household member, so there is nobody to ask"}
	}

	question := fmt.Sprintf("Run %s with %s?", c.Tool, renderParams(c.Params))
	if d, ok := e.(Describer); ok {
		described, err := d.Describe(ctx, c)
		if err != nil {
			// The executor's own refusal, raised BEFORE anybody is asked
			// to approve something that could not have happened.
			return fmt.Errorf("domestic: %s: %w", c.Tool, err)
		}
		if strings.TrimSpace(described) != "" {
			question = described
		}
	}

	state, err := g.Confirm(ctx, ConfirmRequest{
		Tool: c.Tool, Member: c.Subject, Key: confirmKey(c),
		Question: question, CorrelationID: c.CorrelationID,
	})
	if err != nil {
		return fmt.Errorf("domestic: %s: asking %s to confirm: %w", c.Tool, c.Subject, err)
	}
	switch state {
	case ConfirmApproved:
		return nil
	case ConfirmDenied:
		return &RefusalError{Tool: c.Tool, Reason: "you said no to this"}
	case ConfirmPending:
		return &RefusalError{Tool: c.Tool, Reason: "still waiting for you to confirm: " + question}
	default:
		return &RefusalError{Tool: c.Tool, Reason: "I have asked you to confirm this — " + question +
			" — answer it and ask me again"}
	}
}

// renderParams is the fallback question's argument list, sorted so the
// same call reads the same way twice.
func renderParams(params map[string]string) string {
	if len(params) == 0 {
		return "no arguments"
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, ", ")
}
