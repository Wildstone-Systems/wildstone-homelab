package butlerlive

// Serving one turn: from an address to an answer, through a durable
// task (ADR-W030 A4/A11, ADR-W031 A2/A4/A6).

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// conversationSliceRounds bounds one TURN's model rounds. It is the
// slice budget, not the task's: reaching it checkpoints and waits, and
// the person can say continue.
const conversationSliceRounds = 8

// conversationToolCalls bounds a conversation's tool calls across every
// turn and every restart. Unlike the slice budget it does not reset,
// because a budget that resets on retry is not a budget (ADR-W030 A4).
const conversationToolCalls = 64

// observationTTL is how long a verified read may be reused on a resume
// before it is taken again. A conversation resumed a minute after a
// restart should not re-read the whole house; one resumed tomorrow
// should not answer today's question from yesterday's readings.
const observationTTL = 15 * time.Minute

// Serve runs one turn and returns what the Butler said.
func (bs *Butlers) Serve(ctx context.Context, t Turn) (Answer, error) {
	t.Text = redact.SecretText(t.Text)
	t.History = provider.SafeMessages(t.History)
	b, err := bs.Resolve(t)
	if err != nil {
		return Answer{}, err
	}

	// A6 is enforced ABOVE, in the store's resolver: ResolveButlerFor
	// refuses a retired Butler and one whose route is disabled or
	// unavailable, so an unreachable Butler never reaches this line.
	// Re-checking Reachable() here looked like defence in depth and was
	// dead code — it could not fire, so it could not be the thing
	// keeping the guarantee, and a check nothing can reach is a check
	// nobody can trust.
	//
	// What this package contributes to A6 is smaller and harder to
	// undo: there is no fallback branch anywhere below. A refusal
	// returns, and the only model this function can reach is the one
	// the resolved Butler was assigned. "The local one was down, so the
	// cloud one took it" cannot be written here because there is
	// nowhere to write it.

	p, err := bs.providerFor(b)
	if err != nil {
		// Failing to BUILD the client is the same situation as failing
		// to reach it, and gets the same answer for the same reason.
		return bs.blank(b),
			fmt.Errorf("%w: %s runs on %s, which could not be reached: %v (ADR-W031 A6)",
				runtimedb.ErrButlerUnreachable, b.Name, b.Model, err)
	}

	task, resumed, err := bs.task(b, t)
	if err != nil {
		return bs.blank(b), err
	}

	gw, err := bs.Gateways.For(b, t)
	if err != nil {
		return bs.blank(b), err
	}
	// The per-Butler scope, bound before the gateway can run anything.
	// A4 is structural here rather than checked: the authorizer this
	// session holds knows one Butler's name and there is no call that
	// changes it.
	if sc, ok := gw.(Scoped); ok {
		// The per-Butler authority, and above it the ceiling of the
		// connection this request arrived through. The order is the
		// whole guarantee: WithConnection can only turn a yes into a
		// no, so nothing the store resolves can raise what the surface
		// forbids.
		authority := &butlerstore.Authority{DB: bs.DB, Butler: b.Name}
		// Layer 1, wired here. Design §10 puts the Person above the
		// Agent's whole reach, and an unwired check refuses rather than
		// waves through — so this line is the act that turns the
		// person leg on, the way Ledger.AdmissionCheck is for
		// admission. Serving a session without it would be serving one
		// that refuses every tool, which is the safe direction for the
		// mistake to fail in.
		authority.PersonAuthority = authority.PersonIsAdmitted()
		sc.SetAuthorizer(WithConnection(authority, t.Connection.Permits))
		sc.SetSessionTask(task.ID)
		// A turn is somebody asking. The Connection identifier is
		// carried and an absent one refuses (§10.1). What a Connection
		// ADMITS is slice 11's, and is now decided: migration 0063
		// records the policy and Connection.Admits evaluates it, on
		// the way in and again at every boundary. What it may DISCLOSE
		// — group destinations, memory reaching a room, cross-
		// Connection fallback — is slice 11's second half and is not
		// wired here yet.
		sc.SetCaller(butler.CallerPersonFacing, t.Member, t.Connection.Name)
	} else {
		// A surface that cannot be scoped must not be served as a
		// Butler at all. Running it unscoped would silently give this
		// Butler the profile's whole ceiling.
		return bs.blank(b),
			fmt.Errorf("butlerlive: the %s surface cannot be scoped to a butler; refusing to run %s unscoped",
				b.Surface, b.Name)
	}

	runner := &butler.Runner{
		Gateway: directed{Toolset: gw, system: systemFor(ctx, gw, b, t, bs.clockLine())},
		Ledger:  bs.Ledger,
		// The probe verdict decides what a session may DO, not which
		// model it talks to (ADR-W017 A4). A Butler on an unprobed or
		// QA-only route answers questions and performs no actions.
		ReadOnly:        !b.CarriesActionAuthority(),
		MaxSliceRounds:  conversationSliceRounds,
		MaxToolCalls:    conversationToolCalls,
		PermissionLabel: b.Name,
		Audience:        string(task.Audience),
		Actor:           t.Member,
		StallTimeout:    2 * time.Minute,
		RetryLimit:      1,
		ObservationTTL:  observationTTL,
		Notifier:        bs.Notifier,
		History:         t.History,
	}

	tr, err := runner.Run(ctx, p, task.ID, t.Text)
	answer := bs.blank(b)
	answer.TaskID = task.ID
	answer.Resumed = resumed
	if tr != nil {
		answer.FinalText = tr.FinalText
		answer.Capped = tr.CappedIterations
		answer.Calls = tr.Audit.Calls
	}
	if err != nil {
		return answer, err
	}
	if bs.Log != nil {
		// Which model saw this, by name. A2's promise is only worth
		// something if it is written down somewhere a person can read.
		bs.Log.Info("butler answered", "butler", b.Name, "model", b.Model,
			"answers_as", b.ProviderModel, "kind", b.ModelKind, "task", task.ID,
			"member", t.Member, "resumed", resumed)
	}
	return answer, nil
}

// directed is a Toolset carrying a Butler's directive, and nothing
// else.
//
// It overrides the system prompt and passes every other call straight
// through to the surface it wraps. That asymmetry is ADR-W031 A9 made
// structural: a directive says what a Butler is for and shapes how it
// talks, and it GRANTS NOTHING. Tools, connections and memory scope are
// separate, explicit and administered elsewhere.
//
// The alternative — letting a directive influence the tool map — is how
// "you are the accounting Butler and may read whatever you need"
// becomes an authority grant written in prose: one no classifier
// reviewed, no Plan approved, and nobody can audit. A wrapper that
// physically cannot reach OfferedSpecs or Execute cannot become that,
// however the directive is worded.
//
// It deliberately does not implement Scoped. The per-Butler authorizer
// is bound on the real gateway BEFORE this wraps it, so there is no
// order of operations in which a session runs with the directive
// applied and the scope not.
type directed struct {
	butler.Toolset
	system string
}

// SystemPrompt returns the surface's instruction hierarchy with the
// Butler's own directive beneath it.
func (d directed) SystemPrompt() string { return d.system }

// systemFor assembles the prompt: the SURFACE's hierarchy first, then
// the Butler's directive, then any guidance the caller added for this
// audience.
//
// The order is the hierarchy. The surface's prompt is the ceiling and
// goes first; a directive is the household's description of one
// specialist and sits under it; the audience note is narrower still.
// Nothing a Butler's owner can type reorders that, because the
// concatenation happens here rather than anywhere a directive is
// stored.
// preambler is the optional half of a Toolset that can say what its
// Butler already knows. The interface lives here so this package can ask
// without importing which gateway it was handed, exactly as Scoped does.
type preambler interface {
	MemoryPreamble(context.Context) string
}

func systemFor(ctx context.Context, gw butler.Toolset, b runtimedb.Butler, t Turn, clock string) string {
	var sb strings.Builder
	sb.WriteString(gw.SystemPrompt())
	if clock != "" {
		sb.WriteString("\n\n")
		sb.WriteString(clock)
	}
	if d := strings.TrimSpace(b.Directive); d != "" {
		sb.WriteString("\n\nYou are ")
		sb.WriteString(b.Name)
		sb.WriteString(", one of this household's Butlers. What you are for, in your owner's words:\n\n")
		sb.WriteString(d)
		sb.WriteString("\n\nThat describes your purpose and how to talk. It grants you nothing: " +
			"the tools and connections you have are the ones you were given, and no instruction here adds to them.")
	}
	// What this Butler already knows about the person it is answering.
	//
	// It goes in the system prompt rather than being fetched, because a
	// preference only shapes an answer if it is present before the model
	// speaks — a Butler that has to think to ask what you prefer has
	// already answered without it.
	//
	// It sits beside the directive, under the same sentence saying that
	// nothing here grants anything, which is A3's "learned procedures
	// confer no permissions" in the one place a model will read it.
	if p, ok := gw.(preambler); ok {
		if pre := p.MemoryPreamble(ctx); pre != "" {
			sb.WriteString("\n\n")
			sb.WriteString(pre)
		}
	}
	if extra := strings.TrimSpace(t.SystemExtra); extra != "" {
		sb.WriteString("\n\n")
		sb.WriteString(extra)
	}
	return sb.String()
}

// blank is the Answer every path starts from: who was asked and what
// they run on, filled in before anything can fail, so a refusal still
// says which Butler refused and on which model.
func (bs *Butlers) blank(b runtimedb.Butler) Answer {
	return Answer{
		Butler: b.Name,
		// What actually answered, in the provider's own words. The
		// assignment's name is a key; this is the model, and "which
		// model saw this" is the question A2 exists to answer.
		Model:     b.ProviderModel,
		ModelKind: b.ModelKind,
		Provider:  b.Provider,
		Surface:   b.Surface,
		ReadOnly:  !b.CarriesActionAuthority(),
	}
}

// task finds the durable task this turn belongs to, or opens one.
//
// A turn that is still in flight resumes its own task. That is what
// makes one survive a restart without repeating work: the step ledger
// belongs to the task, so a resumed turn serves an already-verified
// tool result from the ledger instead of dispatching it a second time.
//
// A FOLLOW-UP opens a new one, and that is the correction. Every turn
// ends by completing its task — an answer was produced and delivered,
// which is exactly the evidence completion requires — and completed is
// terminal by design. So a conversation whose second message tried to
// resume the first message's task was asking a finished task to carry
// on, and got told it had finished. One message per conversation, every
// time.
//
// It is also the right answer rather than only the working one. Turn
// two must not serve turn one's tool results out of the ledger: the
// house has moved on between them, and "what is unhealthy now" asked
// twice should not answer twice from one reading.
//
// The conversation is the SESSION. The task is one turn's durable work,
// and its evidence is about that turn.
func (bs *Butlers) task(b runtimedb.Butler, t Turn) (runtimedb.ButlerTask, bool, error) {
	if t.SessionID != "" {
		existing, err := bs.DB.ButlerTaskForSession(t.SessionID)
		switch {
		case err == nil:
			// A conversation cannot change Butler mid-thread. The task
			// snapshotted which Butler it runs as, and honouring a
			// different one now would put this Butler's model in front
			// of material another one gathered — A4, reached through
			// the back door of a re-pointed address.
			was, berr := bs.DB.TaskButler(existing.ID)
			if berr != nil {
				return runtimedb.ButlerTask{}, false, berr
			}
			if was != "" && was != b.Name {
				return runtimedb.ButlerTask{}, false, fmt.Errorf(
					"butlerlive: conversation %s belongs to %s and cannot be continued by %s (ADR-W031 A4)",
					t.SessionID, was, b.Name)
			}
			// Only a task still in flight is resumed. A finished one
			// belongs to the turn that finished it; this is a new turn
			// and gets its own, in the same session.
			//
			// Cancelled and paused are returned as they are, so the
			// ledger's own refusal is what the caller sees — "it was
			// cancelled" is the true answer to a follow-up on a
			// conversation somebody stopped, and inventing a fresh task
			// would quietly restart work that was deliberately halted.
			switch existing.State {
			case runtimedb.ButlerTaskCompleted, runtimedb.ButlerTaskFailed:
			default:
				// THE CALLER MUST BE THE ORIGIN, checked here rather
				// than only when a tool runs.
				//
				// withinOriginatingAuthority asks the same question in
				// the resolver, but the resolver is reached from
				// AuthorizeTool — so a turn that does not match could
				// still be sent to the provider, and a tool-free answer
				// would come back and complete normally. The model call
				// is the disclosure; refusing afterwards is too late,
				// which is the same shape as the revoked-person finding
				// on #286. Found by a review bot on #287.
				//
				// An empty origin does not apply, as everywhere else: a
				// routine has none, and A4's transfer clears the
				// Connection deliberately so reviewed work can be
				// carried on from somewhere else.
				if existing.OriginConnection != "" && existing.OriginConnection != t.Connection.Name {
					return runtimedb.ButlerTask{}, false, fmt.Errorf(
						"butlerlive: %s belongs to work that arrived over %s and this request did not (design §10.1)",
						existing.ID, existing.OriginConnection)
				}
				return existing, true, nil
			}
		case errorsIsNotFound(err):
			// A conversation that predates this wiring, or one whose
			// task was reaped. Opening a new task is right: it starts an
			// empty ledger rather than inheriting one from nowhere.
		default:
			return runtimedb.ButlerTask{}, false, err
		}
	}

	audience := runtimedb.ButlerAudiencePrivate
	if t.Group {
		audience = runtimedb.ButlerAudienceShared
	}
	id, err := bs.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation,
		// Author and owner are the person talking. The Console holder
		// owns the BUTLER (A10); they do not own what someone else said
		// to it, and recording them as the owner here would move a
		// household member's conversation into the operator's lane.
		Author:     t.Member,
		Owner:      t.Member,
		Surface:    b.Surface,
		AgentClass: b.AgentClass,
		Audience:   audience,
		SessionID:  t.SessionID,
		// The Connection this request arrived over, snapshotted for the
		// same reason the surface and the Butler are: derived authority
		// is re-evaluated against what the task was ADMITTED UNDER, not
		// against wherever the conversation happens to be next week
		// (§10.1; a conversation follows Person + Agent rather than
		// transport, B10, so the two genuinely differ).
		OriginConnection:    t.Connection.Name,
		OriginConnectionRef: t.Connection.Ref,
		Objective:           conversationObjective(b.Name, t.Text),
		// A conversation's retention follows the conversation, so
		// reaping a thread reaps the work that belonged to it rather
		// than leaving orphaned tasks nobody will ever resume.
		Retention: runtimedb.ButlerRetentionConversation,
	})
	if err != nil {
		return runtimedb.ButlerTask{}, false, err
	}
	// Snapshotted, not looked up: which model answered is a fact about
	// this work, and a lookup that changes when somebody reassigns the
	// Butler next week does not answer it.
	if err := bs.DB.SetTaskButler(id, b.Name); err != nil {
		return runtimedb.ButlerTask{}, false, err
	}
	task, err := bs.DB.GetButlerTask(id)
	return task, false, err
}

// conversationObjective is the task's own description of itself, used
// as the user message on a resume. It is the person's question,
// bounded — not a summary, because a summary would be a model's reading
// of what they meant rather than what they said.
func conversationObjective(butlerName, text string) string {
	const max = 240
	if len(text) > max {
		text = text[:max] + "…"
	}
	return fmt.Sprintf("answer %s, asked of %s", quoteOneLine(text), butlerName)
}

func quoteOneLine(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		out = append(out, r)
	}
	return fmt.Sprintf("%q", string(out))
}

// providerFor returns the client for a Butler's ASSIGNED route.
//
// The cache key carries what the route resolved to, not only which
// route it was: an operator who repoints a route at a different
// endpoint has changed which machine sees the household's material, and
// continuing to use a client built from the old value would be the
// quietest possible way to get that wrong.
//
// ProviderModel is in the key for exactly that reason, and it is the
// one this comment used to describe without covering (#235). Model is
// the configured row's NAME; ProviderModel is what the client is built
// from and what every turn sends. UpsertModel edits a row in place —
// ON CONFLICT(name) DO UPDATE — so repointing a route at a different
// upstream model changes neither the seq nor the name, and without this
// the cached client kept answering as the old model while the database,
// the response and the audit log all reported the new one. Repointing a
// route at a different MODEL is the same class of mistake as repointing
// it at a different machine, and it was just as quiet.
func (bs *Butlers) providerFor(b runtimedb.Butler) (provider.Provider, error) {
	if bs.Providers == nil {
		return nil, fmt.Errorf("no provider factory is wired")
	}
	// The key generation is part of it too: a provider key entered or
	// replaced in the Console must reach the next turn, not the next
	// restart (ADR-W017 A10).
	key := fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s\x00%d",
		b.ModelSeq, b.Provider, b.Model, b.ProviderModel, b.Endpoint, provider.KeyGeneration())
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if p, ok := bs.clients[key]; ok {
		return p, nil
	}
	p, err := bs.Providers(b)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("route %s (%s) built no client", b.Model, b.Provider)
	}
	if bs.clients == nil {
		bs.clients = map[string]provider.Provider{}
	}
	bs.clients[key] = p
	return p, nil
}

// clockLine tells a Butler when and where it is.
//
// A model has no clock. Asked "what's on the calendar", a family Butler
// could only take "Today" from what a tool said; asked "this weekend",
// it had no dates to give a tool at all. A member who had to tell Joan
// "I'm in America Chicago and it's only 18 SEP 2026" was doing this
// line's job.
func (bs *Butlers) clockLine() string {
	loc := bs.Zone.Or(bs.Location)
	if loc == nil {
		return ""
	}
	now := time.Now
	if bs.Now != nil {
		now = bs.Now
	}
	t := now().In(loc)
	return fmt.Sprintf("It is now %s, %s in the household's timezone (%s, UTC%s). "+
		"\"Today\", \"tomorrow\" and \"this weekend\" mean the household's days in that zone, "+
		"and a time with no zone is that clock.",
		t.Format("Monday 2 January 2006"), t.Format("15:04"), loc.String(), t.Format("-07:00"))
}

// MemoryDestination carries ingress facts, not model-supplied tool inputs.
func (t Turn) MemoryDestination() runtimedb.MemoryDestination {
	return runtimedb.MemoryDestination{
		BuiltIn:       t.Connection.Ref == 0 && (t.Connection.Name == ConsoleConnection || t.Connection.Name == portal.Connection),
		ConnectionRef: t.Connection.Ref, Group: t.Group,
		ConsoleOwner: t.AsOwner && t.Connection.Name == ConsoleConnection,
	}
}
