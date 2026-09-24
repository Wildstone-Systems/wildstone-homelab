package conformance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

// Target is one agentapi.Client implementation to run the conformance
// table against — an in-process gateway, an httpclient.Client pointed
// at a real server, or a third party's own Wildstone-compatible
// implementation. Only Client and Handle are required; every other
// field is a fixture-specific probe a particular check needs, and a
// check whose field is left at its zero value skips itself with a
// reason rather than assuming a property this package cannot know every
// deployment has (ADR-W039 §4's "may" list is long, and a conformance
// suite that failed on a runtime's legitimate choice there would be
// testing itself, not the contract).
type Target struct {
	// Client is the agentapi.Client under test.
	Client agentapi.Client

	// Deliveries is the agentapi.DeliveryClient under test. Nil skips
	// every delivery check — legitimate for a runtime that only ever
	// receives pushed deliveries (agentapi.DeliveryClient's own doc
	// comment: "a runtime that only ever receives PUSHED deliveries need
	// never implement this at all").
	Deliveries agentapi.DeliveryClient

	// Context returns the context every check uses for its Client and
	// Deliveries calls. Nil means context.Background() for every call —
	// enough for an httpclient.Client, whose authority travels in its
	// Bearer, never in the context. An in-process implementation whose
	// authority is asserted through the context in this codebase
	// (agentgateway.WithPrincipal) supplies its own that does that
	// wrapping, once, here — rather than every check having to know
	// about it.
	Context func() context.Context

	// Handle returns a fresh, live handle for a runnable task, good for
	// exactly the one check that calls it. Run calls it once per check
	// that needs a handle, so a target whose handles are single-use real
	// ledger-backed tasks is exercised the way ADR-W039 §3.7 describes
	// ("issuing a new one replaces the live one") rather than one stale
	// handle being reused past what it was ever meant to authorise.
	//
	// Required: Run fails immediately if this is nil.
	Handle func(t *testing.T) agentapi.Handle

	// ReadOnlyTool is a tool name the manifest offers with ReadOnlyHint
	// true, safe to invoke with ReadOnlyArgs and expected to answer ok.
	// Empty skips the read-only-invoke check (the manifest check still
	// runs; it only skips comparing ReadOnlyTool against what the
	// manifest actually marked).
	ReadOnlyTool string
	// ReadOnlyArgs is ReadOnlyTool's input, verbatim. Nil means no
	// arguments.
	ReadOnlyArgs json.RawMessage

	// UnknownTool is a tool name the manifest does not offer. Empty uses
	// a name this package invents, which is fine for any target: the
	// check only needs a name nothing recognises.
	UnknownTool string

	// ProposeTool is a mutating tool name whose call always results in a
	// filed proposal — Wildstone's own propose_change, or whatever a
	// third party names the equivalent. Empty skips the propose check.
	ProposeTool string
	// ProposeArgs is ProposeTool's input, verbatim. Nil means no
	// arguments.
	ProposeArgs json.RawMessage
	// NeverApplied reports, right after ProposeTool was invoked, that
	// its effect was never actually applied — the target's own probe of
	// whatever ProposeTool would otherwise have changed (a container
	// spec never created, a file never written). Required for the
	// propose check to run at all: a propose_change tool this package
	// cannot verify the harmlessness of is not one it can pass judgment
	// on.
	NeverApplied func(t *testing.T) bool

	// RememberSubject and RememberContent seed one fact through Remember
	// so Recall has something to look for. Empty Content uses a value
	// this package invents (unique per run, so repeated runs against a
	// durable store never collide on content already there).
	RememberSubject string
	RememberContent string
	// RequireRecallHit, when true, fails the recall check if Recall
	// finds nothing for what was just remembered — appropriate for a
	// target whose Remember tool actually persists. A target whose
	// fixture only echoes success without writing anything durable
	// (common for a minimal conformance harness that does not wire real
	// memory) leaves this false, and the check still validates the
	// scope of whatever hits, if any, come back.
	RequireRecallHit bool

	// SeedDelivery, if set, is called once, before the ack check, to put
	// at least one unacked delivery in the target's own queue (e.g. by
	// writing one directly to whatever store backs Deliveries). Nil
	// skips the ack check with a reason, since acking needs something to
	// ack.
	SeedDelivery func(t *testing.T)

	// Skip names checks, by their exact Run subtest name (see the
	// checks table in run.go), that this target cannot support and
	// understands why — e.g. a capability surface with no side-effect-
	// free tool at all. Empty runs every check; an entry here always
	// takes precedence over a field-based skip, so a reason chosen
	// deliberately is the one that is reported.
	Skip map[string]string
}

// Ctx returns the context a check or a Script should use for one of
// Target's Client/Deliveries calls (see Context's own doc comment).
func (target Target) Ctx() context.Context {
	if target.Context != nil {
		return target.Context()
	}
	return context.Background()
}

// requireHandle is every check's own way of asking for a handle: fail
// loudly if the target has none, rather than nil-panicking three lines
// further down in a way that would look like the check itself is
// broken.
func requireHandle(t *testing.T, target Target) agentapi.Handle {
	t.Helper()
	if target.Handle == nil {
		t.Fatal("conformance: target.Handle is nil")
	}
	return target.Handle(t)
}
