package agentapi

import "context"

// Client is what a runtime calls Wildstone through: every call with
// authority goes this way, and Wildstone calls back only to deliver and
// to probe (ADR-W039 §4.1 "who calls whom"). The native runner holds an
// in-process implementation; an external runtime holds one over the
// chosen transport (§3.8). Neither can tell which it has, which is the
// property Phase 2's byte-identical-manifest test pins.
//
// Every method takes a Handle, directly or inside its request, and
// every method's authority is derived from that handle on the
// Wildstone side at the moment of the call (§3.4 steps 1–2). The
// comments below say which Wildstone side enforces what, so a reader of
// this interface does not have to guess where a check lives — and so
// nobody adds a check on the runtime side thinking it is the only one.
//
// The error a method returns is a transport or protocol failure — the
// call did not happen. A refusal is not an error: it is an Outcome on
// the result, because §4 item 5 wants a runtime to handle denied and
// waiting as ordinary answers, and an error path is where retries hide.
type Client interface {
	// Manifest returns the capability surface for the task on the
	// handle, narrowed by the Agent's effective authority right now.
	//
	// Wildstone enforces: authn (credential → principal → bound Agent;
	// disabled or revoked ⇒ refused); the three-layer resolver over the
	// Agent's assignments, controls and grants; the read-only verdict;
	// the ceiling (a runtime credential carries at most the butler
	// role's verbs). Every Console change is visible on the next fetch.
	Manifest(ctx context.Context, h Handle) (Manifest, error)

	// Invoke runs one capability (§3.4).
	//
	// Wildstone enforces: authn; handle → Task → Person, Connection,
	// audience, budget; authz through the tool resolver and the
	// Connection ceiling; the policy tier (read runs, domestic goes to
	// the executor, confirm asks and waits, mutating infra becomes a
	// proposal and is never applied here); secret resolution below
	// inference, by reference; redaction, bounding and the untrusted
	// envelope on the result; ledger and audit rows. The runtime chooses
	// the tool and its arguments, and nothing else.
	Invoke(ctx context.Context, req InvokeRequest) (InvokeResult, error)

	// Recall searches Wildstone memory for the reader on the handle.
	//
	// Wildstone enforces: the read context (reader, Agent, destination)
	// bound from the handle; scope and audience in the candidate query;
	// per-Agent restrictions; the processing-class filter that withholds
	// local-only records from a runtime not declared local-only (§3.5
	// rule 3, undeclared fails closed). The runtime may narrow by kind
	// and limit.
	Recall(ctx context.Context, req RecallRequest) ([]MemoryHit, error)

	// Remember promotes one fact into Wildstone memory (§3.5 rule 1).
	//
	// Wildstone enforces: the scope may be personal or agent_local and
	// never household; the kind may be fact or preference and never an
	// inference; the audience is the speaker's and cannot be widened;
	// provenance comes from the handle's conversation; redaction before
	// storage; a forgotten record is not relearned. What a runtime
	// remembers locally confers nothing (§3.5 rule 2).
	Remember(ctx context.Context, req RememberRequest) error

	// Checkpoint writes one ledger checkpoint for the task on the
	// handle and returns what Wildstone assigned (Phase 4 surface; the
	// native runner adopts it in Phase 2 through the in-process client).
	//
	// Wildstone enforces: the task is still authorised to continue
	// (not cancelled, owner not offboarded, deadline not passed); the
	// lease epoch — a checkpoint under a fenced attempt is refused;
	// sequence assignment; the aggregate tool budget; redaction of
	// params and results before they are stored.
	Checkpoint(ctx context.Context, c Checkpoint) (CheckpointReceipt, error)

	// Start moves the task to running (Phase 4 surface).
	//
	// Wildstone enforces: the task authorisation check and the state
	// machine — only a pending or waiting task starts.
	Start(ctx context.Context, w WorkStart) error

	// Wait parks the task with a closed reason (Phase 4 surface).
	//
	// Wildstone enforces: the reason is one of the closed set; the
	// stall notice to the owner, deduplicated and held through quiet
	// hours, is Wildstone's to send, never the runtime's.
	Wait(ctx context.Context, w WorkWait) error

	// Complete writes evidence and completes the task, atomically
	// (Phase 4 surface).
	//
	// Wildstone enforces: the permission label and audience stamped on
	// every evidence row come from the task's own admission; the task
	// must be running; the ledger's verified steps, not the runtime's
	// summary, are what the record shows as done.
	Complete(ctx context.Context, w WorkComplete) error

	// Fail moves the task to failed (Phase 4 surface).
	//
	// Wildstone enforces: the state machine, and that a failed task's
	// in-flight attempts are fenced so a late result is refused.
	Fail(ctx context.Context, w WorkFail) error

	// Propose files a change for Owner approval (Phase 3 surface). The
	// outcome arrives later as a Delivery of kind proposal_outcome.
	//
	// Wildstone enforces: the kind is one the manifest offered; schema
	// validation of the document; policy classification; the Plan is
	// filed as proposed, attributed to the Agent acting for the Person
	// on the handle, and never applied here; who may approve is never a
	// runtime.
	Propose(ctx context.Context, req ProposeRequest) (ProposalRef, error)

	// Notify sends text outward through the task's Connection (Phase 4
	// surface; §4 item 8).
	//
	// Wildstone enforces: the Connection is the handle's own; admission;
	// disclosure by destination; quiet hours; dedupe; the platform
	// credential stays in the chat adapter. The runtime supplies the
	// text and nothing about where it goes.
	Notify(ctx context.Context, req NotifyRequest) error

	// Heartbeat reports liveness (§4 item 10).
	//
	// Wildstone enforces: health is its own determination from arrival
	// times — a runtime cannot declare itself connected — and a revoked
	// principal's heartbeat is refused like any other call.
	Heartbeat(ctx context.Context, hb Heartbeat) error
}
