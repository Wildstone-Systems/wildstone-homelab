// Package butlerlive serves a real conversation as a named Butler
// (ADR-W031), over the durable runner and the coordinator-fenced ledger
// ADR-W030 built (P3).
//
// It exists because every piece this needs was already written and
// none of them were joined up. keystone/runtimedb knows which Butler
// answers at an address and what model it was assigned; keystone/butler
// has a Runner that checkpoints every dispatch; keystone/butlerstore
// implements the seam between them. What was missing was the thing that
// takes an inbound message and walks it through all three, and until it
// existed the guarantees were true of the store and not of a
// conversation.
//
// Three decisions from ADR-W031 Part A shape everything here, and each
// one is a thing this package deliberately does NOT do.
//
// A2 — the model is assigned, never chosen. Nothing in this package
// looks at the message before deciding which model runs it. The Butler
// comes from the ADDRESS the message arrived at, and the model comes
// from that Butler's record. There is no scoring, no locality check and
// no capacity count, because anything that decided would first have to
// read, and the reading is the disclosure the decision was meant to
// prevent.
//
// A6 — a Butler whose model is unreachable WAITS. There is no fallback
// path in this file. Not to another Butler, not to another route, and
// not to the legacy -butler-provider: "the local one was down, so the
// cloud one took it" is the exact sentence the design exists to make
// impossible, and the only way to be sure it is never said is to have
// nowhere to say it from.
//
// A4 — one Butler's material does not reach another Butler's model.
// Every session is built with an authority resolver and a memory scope
// bound to ONE Butler, at construction. A running session has no call
// shape that could name a different one.
package butlerlive

import (
	"errors"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/householdtz"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// Turn is one inbound message, already bounded and authenticated by the
// caller.
type Turn struct {
	// Transport and Address are WHERE the message arrived. Address is
	// the receiving bot's own identity — on the chat path, the adapter
	// capability recovered from its Keystone credential (ADR-W031 A7:
	// "the bot token IS the address").
	//
	// It deliberately does not come from the request body. A field an
	// adapter fills in is a field an adapter can fill in wrongly, and
	// one compromised adapter could then address any Butler in the
	// house.
	Transport string
	Address   string

	// Butler names one directly. It is for the Console, where the
	// holder picks a Butler from a list and there is no bot in the
	// middle; A10 gives them every Butler, so naming one is not an
	// escalation. On the chat path this is empty and the address
	// decides.
	Butler string

	// Member is the person talking, already resolved from a binding.
	Member string

	// Text is what they said.
	Text string

	// SessionID continues an existing conversation; empty opens one.
	SessionID string
	// History is the earlier turns of that conversation, user and
	// assistant text only.
	History []provider.Message

	// AsOwner marks the Console holder acting on a Butler they own
	// (ADR-W031 A10).
	//
	// A10 gives them every Butler: they create them, assign the models,
	// and decide which household members may talk to which. The access
	// list is that decision — it is the answer to "who may talk to
	// Alfred", asked about other people. Holding the owner to it makes
	// them add themselves to a list they alone maintain, which protects
	// nothing: the same credential grants that access in one call. A8
	// already names this shape and rejects it — a ceremony nobody can
	// refuse teaches people that approvals are paperwork.
	//
	// It removes the ACCESS and ADMISSION checks and nothing else. The
	// surface ceiling still holds, a retired Butler is still retired,
	// and an unreachable model still waits: those are not permissions
	// the owner has, they are facts about the Butler.
	//
	// The caller sets this only for a principal that holds manage. A
	// chat adapter never can — a concierge or valet credential has no
	// spelling for it — so the path a household member's message takes
	// is untouched.
	AsOwner bool

	// Connection names the CONNECTION the request arrived through, and
	// it carries a ceiling of its own: a set of tools refused on this
	// surface whatever the Butler, the grants or the administrative
	// controls would otherwise allow (connection.go).
	//
	// It is not the same as RequireSurface below, and the difference is
	// the point. The surface test is about the BUTLER — is this one
	// entitled to answer a family caller. This is about the REQUEST —
	// whatever that Butler may do elsewhere, an operator-class tool is
	// refused when the question came from the household's dashboard.
	// The first catches a Butler built with too much; the second
	// catches an existing operator Butler exposed on the wrong surface
	// by mistake, and only the second is still true when somebody
	// hand-crafts the request.
	//
	// Empty means no connection ceiling, which is right for the Console
	// and for the chat adapters: each already has its own, and a name
	// here that nothing set would be a ceiling nobody chose.
	Connection ConnectionScope

	// RequireSurface is the structural ceiling the caller arrived
	// through, and a Butler on any other surface is refused.
	//
	// A3 says a Butler NARROWS a surface and can never widen one. A
	// family adapter's credential reaching an admin-surface Butler
	// would widen it — the bot holds a concierge token that cannot
	// approve anything, but the session behind it would be running the
	// operator's tool map, and "create a Butler" would have become the
	// way to assemble a capability the surface was built to withhold.
	//
	// Empty means no constraint, which is right for the Console holder:
	// they hold both surfaces already (A10), and there is no credential
	// pinning them to one.
	RequireSurface runtimedb.ButlerSurface

	// Group marks a shared audience (ADR-W026 A7).
	Group bool
	// SystemExtra is surface guidance the caller adds, e.g. the
	// group-chat note. It instructs and authorises nothing (A9).
	SystemExtra string
}

// ConsoleConnection is the name the Console is known by in an authority
// check.
//
// The Console is a CONNECTION, not the absence of one (design §8.2, B8:
// it is a first-class trusted Connection). Naming it matters because
// §10.1 fails closed on a missing Connection, and a Console turn that
// left the field empty would be indistinguishable from a caller that
// forgot to fill it in — which is exactly the inference this slice
// removes everywhere else.
//
// It carries no ceiling of its own: ConnectionScope.Permits stays nil
// on this path, because the Console holder already holds both surfaces
// (ADR-W031 A10) and a ceiling nobody chose is not one.
const ConsoleConnection = "console"

// Answer is what one turn produced.
type Answer struct {
	// Butler and Model name what actually answered. They are returned
	// rather than logged because "which model saw this" is the question
	// A2 exists to answer, and an answer only the process can see is not
	// one a person can rely on.
	Butler    string
	Model     string
	ModelKind string
	// Provider names the client kind the route resolved to, for the
	// session's audit row.
	Provider string
	// Surface is the Butler's own, snapshotted onto the session so an
	// audit reading "which surface was this" does not depend on the
	// Butler still existing.
	Surface runtimedb.ButlerSurface
	// ReadOnly is the probe verdict applied to THIS Butler's route
	// (ADR-W017 A4): a session that answers questions and performs no
	// actions. It is not a statement about the model's quality, and it
	// never changes which model ran.
	ReadOnly bool

	TaskID    string
	FinalText string
	Capped    bool
	Resumed   bool
	Calls     []butler.ToolCallRecord
}

// Gateways builds the tool surface for one session.
//
// It is a seam rather than a field because the two surfaces are built
// from things this package must not import — a Statefile, an agent
// reader, the domestic action dispatcher. cmd/keystone owns those and
// hands back a Toolset already narrowed to the surface.
//
// The returned Toolset must be safe to mutate: butlerlive sets the
// per-Butler authorizer and the session's task on it, so a shared
// instance would leak one Butler's scope into another's session. Each
// call returns a fresh one.
type Gateways interface {
	For(b runtimedb.Butler, t Turn) (butler.Toolset, error)
}

// GatewayFunc adapts a function to Gateways.
type GatewayFunc func(b runtimedb.Butler, t Turn) (butler.Toolset, error)

// For implements Gateways.
func (f GatewayFunc) For(b runtimedb.Butler, t Turn) (butler.Toolset, error) { return f(b, t) }

// Scoped is the pair of per-session controls a Toolset accepts. Both
// gateway types satisfy it; the interface is here so this package can
// bind them without importing which one it was given.
type Scoped interface {
	SetAuthorizer(a butler.ToolAuthorizer)
	SetSessionTask(id string)
	// SetCaller states who the session answers for and on which
	// authority path (design §10.1). It is on this interface for the
	// same reason SetAuthorizer is: a surface that could be served
	// WITHOUT being told who is asking is a surface that would
	// eventually be served without it, and this package is the one
	// place that knows.
	SetCaller(kind butler.CallerKind, person, connection string)
}

// Butlers serves conversations as named Butlers.
type Butlers struct {
	DB     *runtimedb.DB
	Ledger *butlerstore.Ledger
	// Providers builds the client for a Butler's ASSIGNED route. It is
	// handed the route's own provider, model and endpoint and must use
	// them; it is not a place to choose.
	Providers func(b runtimedb.Butler) (provider.Provider, error)
	Gateways  Gateways
	// Native, when set, is the agent runtime conversations run on
	// (ADR-W039) for every Butler whose route it can speak. nil keeps
	// every conversation on butler.Runner.
	Native agentruntime.AgentRuntime
	// Notifier delivers the one stalled-task notice. nil records the
	// stall and messages nobody.
	Notifier butler.StallNotifier
	Log      *slog.Logger
	// Location is the household's timezone, and Now its clock (nil is
	// time.Now). Every Butler is told the date and time in this zone:
	// without it a model has no "today" but a tool's say-so, and no way
	// at all to turn "this weekend" into dates.
	Location *time.Location
	// Zone, when set, is the household's live timezone and wins over
	// Location, so an approved change applies without a restart.
	Zone *householdtz.Zone
	Now  func() time.Time

	// mu guards clients.
	mu sync.Mutex
	// clients caches one provider per ROUTE, keyed by the route's seq
	// and the backend it resolved to. Building an HTTP client per
	// message is waste; caching across a reconfiguration would be worse
	// than waste, so the key carries what the route said rather than
	// only which route it was.
	clients map[string]provider.Provider
}

// ErrNoButlers reports that this deployment has created no Butlers at
// all.
//
// It is separate from every other failure because it is the only one
// with a legitimate fallback. A deployment upgrading into ADR-W031 has
// no Butlers on its first boot, and refusing to answer would mean the
// upgrade silently went mute. Everything else here — no Butler at this
// address, this person may not talk to that Butler, that Butler's model
// is down — is a real answer about a real Butler, and substituting a
// different model for any of them is the disclosure A6 forbids.
var ErrNoButlers = errors.New("butlerlive: this deployment has no Butlers")

// Any reports whether any Butler exists. It is the ONLY question whose
// answer may send a caller down the legacy single-provider path, and it
// is a fact about the deployment rather than about the message: nothing
// reads what was said in order to answer it.
func (bs *Butlers) Any() (bool, error) { return bs.DB.AnyButlers() }

// Resolve answers which Butler a turn is for, and refuses if that
// person may not talk to it.
//
// The order is the point. The address decides the Butler; the member is
// then checked against that Butler's access list. Resolving the other
// way round — find the Butlers this person may use, then pick one —
// would be a selection step, and a selection step needs a reason to
// prefer one, and the only available reason is the message.
func (bs *Butlers) Resolve(t Turn) (runtimedb.Butler, error) {
	if t.Member == "" {
		return runtimedb.Butler{}, fmt.Errorf("butlerlive: a turn needs the member it came from")
	}
	var (
		b   runtimedb.Butler
		err error
	)
	switch {
	case t.Butler != "" && t.AsOwner:
		b, err = bs.ownedButler(t.Butler)
	case t.Butler != "":
		b, err = bs.DB.ResolveButlerFor(t.Butler, t.Member)
	case t.Transport != "" && t.Address != "":
		b, err = bs.DB.ResolveButlerAtAddress(t.Transport, t.Address, t.Member)
	default:
		return runtimedb.Butler{}, fmt.Errorf(
			"butlerlive: a turn needs either a named butler or the address it arrived at")
	}
	if err != nil {
		return runtimedb.Butler{}, err
	}
	// A3's ceiling, checked after the Butler is known and before
	// anything is built from it.
	//
	// The refusal is deliberately shaped like "no such Butler" rather
	// than "wrong surface": a family bot learning that an admin Butler
	// exists at an address it just tried is the same disclosure
	// ErrNoButlerAccess exists to avoid.
	if t.RequireSurface != "" && b.Surface != t.RequireSurface {
		return runtimedb.Butler{}, fmt.Errorf("%w: %s is not on the %s surface (ADR-W031 A3)",
			runtimedb.ErrNoButlerAccess, b.Name, t.RequireSurface)
	}
	return b, nil
}

// errorsIsNotFound keeps the store's sentinel in one place, so the
// "this conversation has no task yet" branch cannot drift into
// swallowing a real read failure.
func errorsIsNotFound(err error) bool { return errors.Is(err, runtimedb.ErrNotFound) }

// ownedButler resolves a Butler for its owner, skipping the access list
// and the admission record and nothing else.
//
// The refusals it keeps are the ones that are not about permission. A
// retired Butler does not answer anybody, its owner included; a Butler
// whose model is unreachable waits (A6) rather than being answered by
// something else. Reproducing those here rather than reusing
// ResolveButlerFor is deliberate: that function's job is to decide
// whether a PERSON may talk to a Butler, and an owner path that called
// it with a flag would put the exemption inside the check everyone else
// depends on.
func (bs *Butlers) ownedButler(name string) (runtimedb.Butler, error) {
	b, err := bs.DB.GetButler(name)
	if err != nil {
		return runtimedb.Butler{}, err
	}
	if !b.Live() {
		return runtimedb.Butler{}, fmt.Errorf("%w: %s has been retired or disabled",
			runtimedb.ErrButlerUnreachable, b.Name)
	}
	if !b.Reachable() {
		why := "unavailable"
		if !b.ModelEnabled {
			why = "turned off"
		}
		return runtimedb.Butler{}, fmt.Errorf(
			"%w: %s runs on %s, which is %s — this waits rather than moving to another model (ADR-W031 A6)",
			runtimedb.ErrButlerUnreachable, b.Name, b.Model, why)
	}
	return b, nil
}

// ErrAmbiguousButler reports that a caller with no address must name
// which Butler they mean.
var ErrAmbiguousButler = errors.New("butlerlive: name which butler you mean")

// Sole returns the one Butler on surface that member may talk to.
//
// This is NOT the router A2 withdrew, and the difference is worth being
// precise about because the shapes look similar from a distance.
//
// A router decided which of several models should receive a message,
// and to decide it had to read the message — so the material reached a
// model before the step meant to protect it. Sole decides nothing of
// the kind. It reads the access list, which is settled before the
// message exists and does not change when it arrives; with exactly one
// candidate there is no choice to make, and with more than one it
// REFUSES and asks the person to say. At no point is anything about the
// message an input.
//
// It exists for the Console, where the holder is talking to a Butler
// without a bot in between. A household with one Butler should not have
// to name it on every question; a household with four must, because
// picking one for them is picking which model sees what they typed.
func (bs *Butlers) Sole(surface runtimedb.ButlerSurface, member string, asOwner bool) (runtimedb.Butler, error) {
	all, err := bs.DB.ListButlers()
	if err != nil {
		return runtimedb.Butler{}, err
	}
	var eligible []runtimedb.Butler
	for _, b := range all {
		if surface != "" && b.Surface != surface {
			continue
		}
		if !b.Live() {
			continue
		}
		if asOwner {
			// The owner has every Butler on this surface (A10), so the
			// question here is only "is there exactly one".
			eligible = append(eligible, b)
			continue
		}
		// Access is the store's answer, not a field read here: a
		// membership check reimplemented at the caller is a membership
		// check that drifts.
		resolved, rerr := bs.DB.ResolveButlerFor(b.Name, member)
		if errors.Is(rerr, runtimedb.ErrNoButlerAccess) || errors.Is(rerr, runtimedb.ErrNotAdmitted) {
			continue
		}
		if rerr != nil && !errors.Is(rerr, runtimedb.ErrButlerUnreachable) {
			return runtimedb.Butler{}, rerr
		}
		// An unreachable Butler still counts as a candidate. Skipping it
		// would silently move the conversation to a different model the
		// moment one went down — A6's forbidden substitution, arrived at
		// by filtering rather than by falling back.
		if rerr != nil {
			resolved = b
		}
		eligible = append(eligible, resolved)
	}
	switch len(eligible) {
	case 0:
		return runtimedb.Butler{}, fmt.Errorf("%w: no butler answers to you on the %s surface",
			runtimedb.ErrNoButlerAccess, surface)
	case 1:
		return eligible[0], nil
	default:
		names := make([]string, 0, len(eligible))
		for _, b := range eligible {
			names = append(names, b.Name)
		}
		return runtimedb.Butler{}, fmt.Errorf("%w: %s", ErrAmbiguousButler, strings.Join(names, ", "))
	}
}

// ConnectionScope is one connection's rule, as the narrowest thing a
// caller can hand over: a name for refusals to cite, and a predicate.
//
// An interface-free shape on purpose. butlerlive should not import the
// surfaces that call it — the portal knows what the portal denies — and
// a function value is the whole of what this package needs to enforce
// it.
type ConnectionScope struct {
	// Name is what a refusal cites, e.g. "household_portal".
	Name string
	// Ref is the Connection ROW this arrived over, for the legs that
	// must not be fooled by address reuse (design §10.1; migration
	// 0062). Zero for the built-in surfaces, which are Connections a
	// deployment has rather than rows somebody added.
	Ref int64
	// Permits answers for one tool. A nil Permits is no ceiling.
	Permits func(tool string) (bool, string)
}
