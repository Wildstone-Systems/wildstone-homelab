package agentgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/proposals"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// The conversation limits, owned here from Phase 2a. They are the
// values butlerlive/serve.go declares as conversationSliceRounds,
// conversationToolCalls and observationTTL, duplicated-until-2b: when
// Serve builds its runner through this package, its constants go and
// these stay.

// ConversationSliceRounds bounds one TURN's model rounds. It is the
// slice budget, not the task's: reaching it checkpoints and waits, and
// the person can say continue.
const ConversationSliceRounds = 8

// ConversationToolCalls bounds a conversation's tool calls across every
// turn and every restart. Unlike the slice budget it does not reset,
// because a budget that resets on retry is not a budget (ADR-W030 A4).
const ConversationToolCalls = 64

// ObservationTTL is how long a verified read may be reused on a resume
// before it is taken again. A conversation resumed a minute after a
// restart should not re-read the whole house; one resumed tomorrow
// should not answer today's question from yesterday's readings.
const ObservationTTL = 15 * time.Minute

// ErrRefused wraps every refusal this gateway makes of its own — a
// runtime principal bound to another Agent, a surface that cannot be
// scoped. A handle refusal is butlerstore.ErrHandleRefused and is
// passed through unchanged.
var ErrRefused = errors.New("agentgateway: refused")

// Gateway is the in-process Agent Gateway. Its fields are what
// butlerlive.Butlers holds for the same job, minus the provider: the
// gateway never talks to a model, which is the whole line ADR-W039 §0 draws.
type Gateway struct {
	DB     *runtimedb.DB
	Ledger *butlerstore.Ledger
	// Gateways builds the tool surface for one session, exactly the
	// seam Serve uses; cmd/keystone wires one closure for both.
	Gateways butlerlive.Gateways
	Log      *slog.Logger
	// Clock renders the household clock line for the system prompt.
	// nil means no clock line — the gateway does not own the
	// household's timezone, and whoever serves a conversation supplies
	// it (butlerlive.Butlers.clockLine, 2b).
	Clock func() string
	// Proposals is the ONE proposal pipeline (ADR-W039 Phase 3, #339):
	// what Propose and a successful propose_change file a Plan
	// through. A zero value (Proposals.DB == nil) is a working,
	// pre-Phase-3 configuration: propose_change still returns a Plan
	// for the caller to file after the turn, exactly as it did before
	// this field existed — a gateway wired without it is not broken,
	// it simply has not been given anywhere to file immediately.
	Proposals proposals.Deps
	// Now is the gateway clock for observation freshness; nil means
	// time.Now.
	Now func() time.Time
	// Replies is where an external runtime's answer goes (#364): a
	// Complete carrying a Reply, and a Fail, are handed to it so the
	// turn waiting on the task hears. nil keeps Complete and Fail as
	// they were, with no answer path.
	Replies Replies

	// SliceRounds, ToolCalls and ObservationTTL override the package
	// constants when > 0. Tests set them; production leaves them.
	SliceRounds    int
	ToolCalls      int
	ObservationTTL time.Duration

	// reserve makes Invoke's budget check and the AppendStep that spends
	// it one step. Calls can arrive together: the OpenAI Agents SDK runs
	// a model's tool calls concurrently, and so may any external runtime.
	// A conversation's budget is also guarded in AppendStep's own UPDATE
	// (tool_calls < tool_limit), so a call that lost that race used to
	// fail with a checkpoint error instead of waiting on the budget; a
	// task with no conversation work has no such guard at all. Held only
	// across ledger reads and one insert, never across the tool's own
	// execution, so a tool that hangs holds up nobody else's call.
	reserve sync.Mutex

	mu sync.Mutex
	// records keeps, per task, the audit record of every call this
	// gateway served — what the runner's Transcript.Audit carried when
	// it ran the calls itself. InvokeResult cannot carry a Plan or a
	// resource document (the wire has no such field, by design), and
	// until Phase 3 files proposals inside the gateway, the handler
	// that files them after a turn still needs to read them from
	// somewhere. Records is that somewhere; Forget releases a task's.
	records map[string][]butler.ToolCallRecord
}

// Replies accepts what an external runtime reports about a turn it was
// handed. butlerlive.Butlers implements it.
type Replies interface {
	RuntimeReplied(ctx context.Context, h agentapi.Handle, reply string) error
	RuntimeFailed(taskID, reason string)
}

var _ Replies = (*butlerlive.Butlers)(nil)

// InProcess returns g as an agentapi.Client. The native runner will
// hold this (2b); an HTTP transport (2c) calls the same methods with
// the runtime principal in the context.
func InProcess(g *Gateway) agentapi.Client { return g }

var _ agentapi.Client = (*Gateway)(nil)

func (g *Gateway) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *Gateway) sliceRounds() int {
	if g.SliceRounds > 0 {
		return g.SliceRounds
	}
	return ConversationSliceRounds
}

func (g *Gateway) toolCalls() int {
	if g.ToolCalls > 0 {
		return g.ToolCalls
	}
	return ConversationToolCalls
}

func (g *Gateway) observationTTL() time.Duration {
	if g.ObservationTTL > 0 {
		return g.ObservationTTL
	}
	return ObservationTTL
}

// principalKey carries the runtime principal a call authenticated as.
type principalKey struct{}

// WithPrincipal marks ctx as a call authenticated as the named runtime
// principal. The HTTP transport sets it from the bearer; the native
// path sets it to the Butler's own credential. A ctx without one is an
// in-process call under the process's own trust, and the binding check
// is skipped — there is no principal to bind.
func WithPrincipal(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, principalKey{}, name)
}

func principalFrom(ctx context.Context) (string, bool) {
	name, ok := ctx.Value(principalKey{}).(string)
	return name, ok && name != ""
}

// session is one resolved handle with the Toolset built for it: what
// Serve assembles between resolving the Butler and constructing the
// Runner (serve.go:87-128), rebuilt per call so every Console change
// is visible on the next one (§4.1 "assign permissions").
type session struct {
	hc       butlerstore.HandleContext
	turn     butlerlive.Turn
	toolset  butler.Toolset
	readOnly bool
}

// open resolves the handle, checks the runtime principal's binding and
// builds the scoped Toolset.
//
// Duplicated-until-2b: the scoping block is serve.go:95-128 verbatim in
// intent — the per-Butler Authority with the person leg wired, the
// Connection ceiling over it, the task and the caller bound — so that
// when Serve hands its runner a handle instead of a Toolset, this is
// the one copy left.
func (g *Gateway) open(ctx context.Context, h agentapi.Handle) (*session, error) {
	if g.Ledger == nil || g.DB == nil || g.Gateways == nil {
		return nil, fmt.Errorf("%w: the gateway is not wired", ErrRefused)
	}
	hc, err := g.Ledger.ResolveHandle(h)
	if err != nil {
		return nil, err
	}
	if principal, ok := principalFrom(ctx); ok {
		// §4.1 identity binding: the credential names one Agent for
		// its life, and a handle for another Agent's task is refused
		// however valid the handle is. Phase 1b's issue rule, applied
		// at every use rather than only at issue.
		seq, bound, err := g.DB.RuntimePrincipalButler(principal)
		if err != nil {
			return nil, err
		}
		if !bound {
			return nil, fmt.Errorf("%w: %s is not a runtime principal", ErrRefused, principal)
		}
		if seq != hc.Butler.Seq {
			return nil, fmt.Errorf("%w: %s is bound to another agent and cannot act for %s (ADR-W039 §4.1)",
				ErrRefused, principal, hc.Butler.Name)
		}
	}
	turn := turnFor(hc)
	gw, err := g.Gateways.For(hc.Butler, turn)
	if err != nil {
		return nil, err
	}
	sc, ok := gw.(butlerlive.Scoped)
	if !ok {
		// A surface that cannot be scoped must not be served as a
		// Butler at all (serve.go:121-128).
		return nil, fmt.Errorf("%w: the %s surface cannot be scoped to a butler; refusing to serve %s unscoped",
			ErrRefused, hc.Butler.Surface, hc.Butler.Name)
	}
	// The per-Butler authority, and above it the ceiling of the
	// Connection the task was admitted over. The order is the whole
	// guarantee: WithConnection can only turn a yes into a no.
	authority := &butlerstore.Authority{DB: g.DB, Butler: hc.Butler.Name}
	authority.PersonAuthority = authority.PersonIsAdmitted()
	sc.SetAuthorizer(butlerlive.WithConnection(authority, turn.Connection.Permits))
	sc.SetSessionTask(hc.Task.ID)
	sc.SetCaller(butler.CallerPersonFacing, hc.Person, hc.OriginConnection)
	return &session{
		hc:      hc,
		turn:    turn,
		toolset: gw,
		// The probe verdict decides what a session may DO, not which
		// model it talks to (ADR-W017 A4).
		readOnly: !hc.Butler.CarriesActionAuthority(),
	}, nil
}

// turnFor is the Turn a resolved handle stands for: every field the
// gateway factory and the prompt read, from the task's admission and
// nothing else. Text and History are the runtime's business and stay
// empty; AsOwner is a fact about the request that opened the task and
// is not recorded on it, so it is false here (see the 2b notes).
func turnFor(hc butlerstore.HandleContext) butlerlive.Turn {
	return butlerlive.Turn{
		Butler:    hc.Butler.Name,
		Member:    hc.Person,
		SessionID: hc.Task.SessionID,
		Group:     hc.Audience == runtimedb.ButlerAudienceShared,
		Connection: butlerlive.ConnectionScope{
			Name:    hc.OriginConnection,
			Ref:     hc.OriginConnectionRef,
			Permits: permitsFor(hc.OriginConnection),
		},
		// AsOwner and SystemExtra are recorded on the task at open
		// (migration 0083, ADR-W039 Phase 2b, #337) precisely because
		// neither is derivable from the handle otherwise: AsOwner
		// shapes MemoryDestination.ConsoleOwner and SystemExtra reaches
		// the prompt through systemFor, and a gateway-served turn must
		// see both exactly as the closure path did.
		AsOwner:     hc.AsOwner,
		SystemExtra: hc.SystemExtra,
	}
}

// permitsFor is the Connection ceiling a named Connection carries. The
// portal is the one Connection with a ceiling of its own; the Console
// and the chat adapters carry none (butlerlive.ConsoleConnection).
// Derived from the handle's Connection, never supplied by the caller:
// a ceiling the runtime could omit is not a ceiling.
func permitsFor(connection string) func(string) (bool, string) {
	if connection == portal.Connection {
		return portal.Permits
	}
	return nil
}

// Manifest implements agentapi.Client: the surface for the task on the
// handle, narrowed by the Butler's effective authority right now.
// toolSpecs is a manifest's tool list: the offered specs, which are
// what the model is handed (Runner.Run's specs) — the list narrows to
// the same set for the same reason, a name is itself a disclosure —
// each marked read-only unless its capability mutates.
func toolSpecs(ts butler.Toolset, caps butler.CapabilityManifest, readOnly bool) ([]agentapi.ToolSpec, error) {
	mutating := make(map[string]bool, len(caps.Tools))
	for _, t := range caps.Tools {
		mutating[t.Name] = t.Mutating
	}
	offered := ts.OfferedSpecs(readOnly)
	tools := make([]agentapi.ToolSpec, 0, len(offered))
	for _, spec := range offered {
		var schema json.RawMessage
		if spec.InputSchema != nil {
			body, err := json.Marshal(spec.InputSchema)
			if err != nil {
				return nil, fmt.Errorf("agentgateway: encoding the input schema of %s: %w", spec.Name, err)
			}
			schema = body
		}
		tools = append(tools, agentapi.ToolSpec{
			Name:         spec.Name,
			Description:  spec.Description,
			InputSchema:  schema,
			ReadOnlyHint: !mutating[spec.Name],
		})
	}
	return tools, nil
}

var _ agentapi.ToolsClient = (*Gateway)(nil)

// Tools implements agentapi.ToolsClient: the Agent's own tool set, for
// a runtime that lists its tools before it holds any task (#364).
//
// The surface is built for the Agent the runtime principal is bound to
// (as its creator, for the seams — see below) and scoped to it exactly
// as open scopes a task's — the per-Butler authority — but checked as
// an ADMINISTRATIVE caller: the question is
// what this Agent may be offered, and there is no person or Connection
// to ask it for yet. Nothing is dispatched through this surface. Every
// Invoke opens its own task-scoped one, with the person and Connection
// legs on.
func (g *Gateway) Tools(ctx context.Context) ([]agentapi.ToolSpec, error) {
	if g.DB == nil || g.Gateways == nil {
		return nil, fmt.Errorf("%w: the gateway is not wired", ErrRefused)
	}
	principal, ok := principalFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("%w: listing tools needs the runtime principal they are for", ErrRefused)
	}
	seq, bound, err := g.DB.RuntimePrincipalButler(principal)
	if err != nil {
		return nil, err
	}
	if !bound {
		return nil, fmt.Errorf("%w: %s is not a runtime principal", ErrRefused, principal)
	}
	b, err := g.DB.GetButlerBySeq(seq)
	if err != nil {
		return nil, err
	}
	if !b.RetiredAt.IsZero() {
		return nil, fmt.Errorf("%w: %s is retired", ErrRefused, b.Name)
	}
	// Built as the Agent's creator: a surface registers a tool only when
	// its seam is wired, and some seams (memory) are bound to a member,
	// so a surface built for nobody would never list recall or remember
	// and a runtime could never call them. Only the specs are read.
	gw, err := g.Gateways.For(b, butlerlive.Turn{Butler: b.Name, Member: b.CreatedBy})
	if err != nil {
		return nil, err
	}
	sc, ok := gw.(butlerlive.Scoped)
	if !ok {
		return nil, fmt.Errorf("%w: the %s surface cannot be scoped to a butler; refusing to list %s's tools unscoped",
			ErrRefused, b.Surface, b.Name)
	}
	sc.SetAuthorizer(&butlerstore.Authority{DB: g.DB, Butler: b.Name})
	sc.SetCaller(butler.CallerAdministrative, "", "")
	readOnly := !b.CarriesActionAuthority()
	return toolSpecs(gw, gw.Capabilities(readOnly), readOnly)
}

func (g *Gateway) Manifest(ctx context.Context, h agentapi.Handle) (agentapi.Manifest, error) {
	s, err := g.open(ctx, h)
	if err != nil {
		return agentapi.Manifest{}, err
	}
	caps := s.toolset.Capabilities(s.readOnly)
	tools, err := toolSpecs(s.toolset, caps, s.readOnly)
	if err != nil {
		return agentapi.Manifest{}, err
	}
	actions := make([]agentapi.ActionSpec, 0, len(caps.Actions))
	for _, a := range caps.Actions {
		actions = append(actions, agentapi.ActionSpec{Tool: a.Tool, Tier: a.Tier})
	}
	// ProposableAgentSubscription names no Statefile document — it is a
	// platform-level proposable (ADR-W039 §3.6, Phase 4, #341), not one
	// of the Toolset's resource kinds — so every Manifest lists it,
	// whether or not this Agent holds any resource-proposing capability
	// at all. Wired only when g.Proposals is (an unwired gateway has
	// nowhere to file it either way, exactly like a resource kind).
	proposableKinds := caps.ProposableKinds
	if g.Proposals.DB != nil {
		proposableKinds = append(append([]string{}, proposableKinds...), agentapi.ProposableAgentSubscription)
	}
	clock := ""
	if g.Clock != nil {
		clock = g.Clock()
	}
	// §3.5 rule 3 (#345): the system prompt can carry
	// a memory preamble (butlerlive.systemFor's preambler branch), and a
	// preamble is disclosure exactly like a recall hit. It is checked
	// under the same call-scoped turn Invoke opens for a dispatch —
	// here scoped to assembling the prompt — and the SAME refusal
	// (runtimedb.ErrPrivacy, wrapped) is returned for the whole Manifest
	// rather than silently handing back a prompt with the restricted
	// part missing: a runtime with no valid manifest at all is the
	// gateway's version of the provider wrapper failing the whole turn
	// (privacy_test.go:32).
	class, err := g.disclosureClass(s.hc.Butler.Seq)
	if err != nil {
		return agentapi.Manifest{}, fmt.Errorf("agentgateway: resolving the runtime's processing class: %w", err)
	}
	dctx, disclosure, err := g.beginDisclosureTurn(ctx, class, s.turn.MemoryDestination(), s.hc.Task.ID)
	if err != nil {
		return agentapi.Manifest{}, fmt.Errorf("agentgateway: opening the disclosure turn: %w", err)
	}
	prompt := butlerlive.SystemPromptFor(dctx, s.toolset, s.hc.Butler, s.turn, clock)
	if derr := disclosure.Check(); derr != nil {
		return agentapi.Manifest{}, disclosureRefusalError("manifest", derr)
	}
	return agentapi.Manifest{
		AgentClass:      agentapi.AgentClass(caps.AgentClass),
		SystemPrompt:    prompt,
		Tools:           tools,
		ReadOnly:        s.readOnly,
		ProposableKinds: proposableKinds,
		Actions:         actions,
		Unavailable:     caps.Unavailable,
		Fingerprint:     caps.Fingerprint(),
	}, nil
}

// Records returns the audit records of every call this gateway served
// for a task, in call order, and Forget releases them. See the field.
func (g *Gateway) Records(taskID string) []butler.ToolCallRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]butler.ToolCallRecord, len(g.records[taskID]))
	copy(out, g.records[taskID])
	return out
}

// Forget drops a task's records.
func (g *Gateway) Forget(taskID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.records, taskID)
}

func (g *Gateway) record(taskID string, rec butler.ToolCallRecord) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.records == nil {
		g.records = map[string][]butler.ToolCallRecord{}
	}
	g.records[taskID] = append(g.records[taskID], rec)
}

// runtimeIdentity is the principal a Plan this gateway files is
// attributed to (ADR-W039 §3.2 Proposals, Phase 3, #339): the runtime
// principal the caller authenticated as, from ctx — WithPrincipal's own
// value, which the HTTP transport sets from the bearer — or, for an
// in-process call with no principal in ctx (the native runner talking
// to its own in-process Gateway), the Agent's own current runtime
// principal, exactly as agentContext's touchRuntimeArrival already
// resolves it. Role is always butler — a runtime credential carries at
// most that role's verbs (§3.1 principle 3) — and ActingFor is the
// Person on the handle, never anything the request could name.
func (g *Gateway) runtimeIdentity(ctx context.Context, s *session) (principal.Identity, error) {
	name, ok := principalFrom(ctx)
	if !ok {
		if g.DB == nil {
			return principal.Identity{}, fmt.Errorf("%w: the gateway is not wired", ErrRefused)
		}
		rt, err := g.DB.GetButlerRuntime(s.hc.Butler.Seq)
		if err != nil {
			return principal.Identity{}, err
		}
		name = rt.Principal
	}
	if name == "" {
		return principal.Identity{}, fmt.Errorf("%w: %s has no runtime principal to file a proposal under", ErrRefused, s.hc.Butler.Name)
	}
	return principal.Identity{Name: name, Role: runtimedb.RoleButler, ActingFor: s.hc.Person}, nil
}

// fileProposal runs req through the ONE proposal pipeline under id, and
// returns the filed Plan. A validation or policy refusal (400/409/422 —
// the pipeline's own "no" answers) comes back wrapped in ErrRefused, the
// same convention every other gateway refusal uses; a genuine failure to
// file (a database or Statefile error, the pipeline's 5xx) comes back as
// a plain error, because the call did not happen at all.
func (g *Gateway) fileProposal(id principal.Identity, req proposals.Request) (proposals.PlanView, error) {
	body, status, err := proposals.File(g.Proposals, id, req, proposals.Options{PrivateOrigin: true})
	if err != nil {
		if status >= 500 || status == 0 {
			return proposals.PlanView{}, fmt.Errorf("agentgateway: filing a proposal: %w", err)
		}
		return proposals.PlanView{}, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	view, ok := body.(proposals.PlanView)
	if !ok {
		return proposals.PlanView{}, fmt.Errorf("agentgateway: filing a proposal returned no plan (status %d)", status)
	}
	return view, nil
}

// refusalOutcome turns a gateway or handle refusal into the denied
// outcome an invoke answers with. A refusal is an answer, not a
// transport failure (§4 item 5); anything else is the error it was.
func refusalOutcome(err error) (agentapi.InvokeResult, error) {
	if errors.Is(err, butlerstore.ErrHandleRefused) || errors.Is(err, ErrRefused) {
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: err.Error()}, nil
	}
	return agentapi.InvokeResult{}, err
}
