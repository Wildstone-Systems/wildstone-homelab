package agentapi

// The resource-family types (ADR-W039 §3.2), one section per family.
//
// Every string enum here is closed: a value not in its constant set
// fails Valid, and a Wildstone handler refuses it rather than mapping
// it to a default. JSON tags are lowerCamel, matching the ADR's YAML,
// so the wire form of a request is the document the ADR shows.
//
// Two rules shape every struct. A runtime asserts nothing about
// authority: there is no person, connection, audience or permission
// label on any request, because Wildstone derives all four from the
// Handle at the moment of use (§3.1 principle 4). And every result a
// runtime receives is data, not instruction (§4 item 4): content
// arrives already redacted and enveloped, and a Delivery confers
// nothing.

import (
	"encoding/json"
	"time"
)

// ---- Capabilities ---------------------------------------------------

// AgentClass names which Agent a manifest describes (ADR-W033 design
// §5.2). It is decided by the gateway from the Agent record, never by a
// request field; it mirrors the same two strings in keystone/butler and
// keystone/runtimedb, which this package cannot import.
type AgentClass string

// The closed set of Agent classes.
const (
	AgentClassOwner  AgentClass = "owner"
	AgentClassMember AgentClass = "member"
)

// Valid reports whether c is a known class.
func (c AgentClass) Valid() bool {
	switch c {
	case AgentClassOwner, AgentClassMember:
		return true
	}
	return false
}

// ToolSpec is one capability as the manifest offers it: the same three
// things a provider-neutral tool declaration carries today (name,
// description, JSON-schema input), plus the read-only hint a runtime
// needs to know which calls are safe to repeat.
type ToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// InputSchema is the JSON-schema-shaped parameter spec, carried
	// verbatim. It is opaque here: a runtime hands it to its model, and
	// Wildstone validates arguments against it at invoke.
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	// ReadOnlyHint marks a tool that observes and never effects. It is
	// a hint for the runtime's own retry policy, not an authority: a
	// read is still authorised per call.
	ReadOnlyHint bool `json:"readOnlyHint,omitempty"`
}

// ActionSpec is one domestic executor this deployment wired — a
// household action a session can actually ask for, with its policy
// tier. It mirrors the manifest's action inventory.
type ActionSpec struct {
	Tool string `json:"tool"`
	Tier string `json:"tier"`
}

// Manifest is the capability surface Wildstone offers one task,
// narrowed by the Agent's effective authority at the time of the fetch
// (§4.1 "negotiate"). It carries the system prompt as well as the
// tools, as the Toolset does, so a runtime cannot be handed one
// surface's tools and another's instructions.
//
// A runtime treats it as the complete surface for Wildstone actions
// (§4 item 3). Anything not listed here does not exist for this task.
type Manifest struct {
	AgentClass AgentClass `json:"agentClass"`
	// SystemPrompt is the surface's instruction hierarchy.
	SystemPrompt string     `json:"systemPrompt"`
	Tools        []ToolSpec `json:"tools"`
	// ReadOnly is the QA-only constraint in force: the offered set is
	// already narrowed to reads, and dispatch refuses a mutating tool
	// however it is named.
	ReadOnly bool `json:"readOnly,omitempty"`
	// ProposableKinds is the resource-kind registry the propose path
	// accepts; empty on a surface that has no propose path.
	ProposableKinds []string `json:"proposableKinds,omitempty"`
	// Actions is what the deployment actually wired. Empty means no
	// executor is reachable, not "unknown".
	Actions []ActionSpec `json:"actions,omitempty"`
	// Unavailable names tools this deployment lists but cannot serve,
	// each with its reason, so a runtime can tell "not set up here"
	// from "not allowed".
	Unavailable []string `json:"unavailable,omitempty"`
	// Fingerprint is the manifest's version stamp: a stable digest of
	// the enforceable facts, recorded on the work trace so a later
	// comparison can tell "the model behaved differently" from "it was
	// offered different tools".
	Fingerprint string `json:"fingerprint,omitempty"`
}

// InvokeRequest is one capability call (§3.4).
type InvokeRequest struct {
	Handle Handle `json:"handle"`
	Tool   string `json:"tool"`
	// Args is the tool input, validated by Wildstone against the
	// ToolSpec's InputSchema. Nil means no arguments.
	Args json.RawMessage `json:"args,omitempty"`
	// IdempotencyKey lets a runtime re-send an invoke after a lost
	// response without the effect running twice (§3.8). Empty means
	// the call is not protected, which a read never needs.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}

// Outcome is how an invoke ended. It is closed so a runtime can switch
// on it without a default branch that guesses (§4 item 5).
type Outcome string

// The closed set of invoke outcomes.
const (
	// OutcomeOK means the call ran and Content is its result.
	OutcomeOK Outcome = "ok"
	// OutcomeDenied means authorization or policy refused the call.
	// Reason says why; nothing ran.
	OutcomeDenied Outcome = "denied"
	// OutcomeWaiting means the call is parked behind a person, a
	// budget or an offline runtime. WaitReason says which. The runtime
	// stops rather than retrying a consequential effect blindly.
	OutcomeWaiting Outcome = "waiting"
	// OutcomeRejected means a proposal was refused outright by the
	// propose path's policy check: filed nowhere, applied nowhere.
	OutcomeRejected Outcome = "rejected"
	// OutcomeError means the call was admitted but its executor
	// failed. Reason carries the redacted, bounded detail.
	OutcomeError Outcome = "error"
)

// Valid reports whether o is a known outcome.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeOK, OutcomeDenied, OutcomeWaiting, OutcomeRejected, OutcomeError:
		return true
	}
	return false
}

// WaitReason is why an invoke is waiting (§4 item 5). Each one names
// something outside the runtime's control, which is why the right
// response to all of them is the same: stop, and resume when told.
type WaitReason string

// The closed set of wait reasons an invoke can return.
const (
	// WaitApprovalPending: the call became a proposal an Owner must
	// approve before anything is applied.
	WaitApprovalPending WaitReason = "approval_pending"
	// WaitConfirmPending: a confirm-tier action has asked the person
	// and is waiting for their answer.
	WaitConfirmPending WaitReason = "confirm_pending"
	// WaitRuntimeOffline: the step needs a runtime that is not
	// connected (§3.7). Deterministic work continues around it.
	WaitRuntimeOffline WaitReason = "runtime_offline"
	// WaitBudgetExhausted: the task's aggregate budget is spent and a
	// person must review before it continues.
	WaitBudgetExhausted WaitReason = "budget_exhausted"
)

// Valid reports whether r is a known wait reason.
func (r WaitReason) Valid() bool {
	switch r {
	case WaitApprovalPending, WaitConfirmPending, WaitRuntimeOffline, WaitBudgetExhausted:
		return true
	}
	return false
}

// ProposalRef names a filed proposal. The id is opaque to a runtime:
// it can be reported, and its outcome arrives later as a Delivery, but
// nothing a runtime does with the id moves the proposal (§3.2: who may
// approve is never a runtime).
type ProposalRef struct {
	ID string `json:"id"`
}

// InvokeResult is what comes back from an invoke: a result, a wait or a
// refusal (§3.4's last line). Content has already been through
// Wildstone's redact → bound → envelope pipeline, and a runtime treats
// it as untrusted text however it reads.
type InvokeResult struct {
	Outcome Outcome `json:"outcome"`
	Content string  `json:"content,omitempty"`
	// Reason is the human-readable detail for a denied, rejected or
	// errored call. Empty on ok.
	Reason string `json:"reason,omitempty"`
	// WaitReason is set exactly when Outcome is waiting.
	WaitReason WaitReason `json:"waitReason,omitempty"`
	// Proposal is set when the call filed a proposal (propose_change
	// accepted, or a mutating action escalated to a Plan).
	Proposal *ProposalRef `json:"proposal,omitempty"`
	// ServedFromLedger is true when the result is a verified earlier
	// result the ledger served on resume instead of dispatching again.
	// Wildstone still checks the authority behind it at that moment;
	// the flag tells the runtime the reading is not fresh.
	ServedFromLedger bool `json:"servedFromLedger,omitempty"`
}

// ---- Memory ---------------------------------------------------------

// MemoryScope is one of the three durable scopes Wildstone holds
// (§3.5). Values mirror the store's own.
type MemoryScope string

// The closed set of memory scopes.
const (
	MemoryScopePersonal   MemoryScope = "personal"
	MemoryScopeHousehold  MemoryScope = "household"
	MemoryScopeAgentLocal MemoryScope = "agent_local"
)

// Valid reports whether s is a known scope.
func (s MemoryScope) Valid() bool {
	switch s {
	case MemoryScopePersonal, MemoryScopeHousehold, MemoryScopeAgentLocal:
		return true
	}
	return false
}

// MemoryKind distinguishes what was stated from what was inferred. A
// runtime may remember the two a person can state — fact and
// preference — and never an inference; the store's other kinds appear
// on hits only.
type MemoryKind string

// The closed set of memory kinds.
const (
	MemoryFact       MemoryKind = "fact"
	MemoryPreference MemoryKind = "preference"
	MemoryInference  MemoryKind = "inference"
	MemoryProcedure  MemoryKind = "procedure"
)

// Valid reports whether k is a known kind.
func (k MemoryKind) Valid() bool {
	switch k {
	case MemoryFact, MemoryPreference, MemoryInference, MemoryProcedure:
		return true
	}
	return false
}

// Audience is a disclosure rule's id. The two seeded audiences are
// named here; the set is OPEN, because a household can define its own
// sets, so there is no Valid. An Audience only ever appears on what
// Wildstone returns — no request carries one, because a runtime cannot
// choose or widen an audience.
type Audience string

// The two audiences every record has before a household adds its own.
const (
	AudienceOwner     Audience = "aud_owner"
	AudienceHousehold Audience = "aud_household"
)

// RecallRequest searches memory. The reader, the Agent and the
// destination are bound from the Handle — the same MemoryReadContext
// the store binds today — and the declared processing class of the
// runtime filters what comes back (§3.5 rule 3). A runtime can narrow
// with Kind and Limit; it cannot widen anything.
type RecallRequest struct {
	Handle Handle `json:"handle"`
	Text   string `json:"text"`
	// Kind restricts the search to one kind. Empty means all.
	Kind MemoryKind `json:"kind,omitempty"`
	// Limit bounds the hits. <= 0 lets Wildstone pick its default.
	Limit int `json:"limit,omitempty"`
}

// MemoryHit is one remembered thing as a runtime may see it. It mirrors
// the Butler's own hit — content, kind, subject, source, age, the
// verify-live flag — with the scope and audience the record carries,
// which are reported so the runtime knows what it is holding and are
// not something it can act on.
type MemoryHit struct {
	Content string      `json:"content"`
	Kind    MemoryKind  `json:"kind"`
	Scope   MemoryScope `json:"scope"`
	// Subject is who or what it is about, when it is about one thing.
	Subject string `json:"subject,omitempty"`
	// Source says where it came from, in the store's provenance
	// vocabulary.
	Source   string   `json:"source,omitempty"`
	Audience Audience `json:"audience,omitempty"`
	// Confidence is the store's own confidence label.
	Confidence string `json:"confidence,omitempty"`
	// ObservedAt is when the observation was made. The Butler's hit
	// carries an age instead; age is a reading taken at a moment, and
	// a timestamp travels better.
	ObservedAt time.Time `json:"observedAt,omitzero"`
	// MustVerifyLive marks a volatile source: this is what was true
	// when it was recorded, and the current state has to be read from
	// the application before answering with it (ADR-W030 A6).
	MustVerifyLive bool `json:"mustVerifyLive,omitempty"`
}

// RememberRequest promotes a runtime-local fact into Wildstone memory
// (§3.5 rule 1). Provenance — which conversation, which room — comes
// from the Handle, not from the request, exactly as the native
// remember tool takes it from the session today. Content is stored
// after Wildstone's redaction, whatever the runtime sent.
type RememberRequest struct {
	Handle Handle `json:"handle"`
	// Scope may be personal (the Person on the handle) or agent_local
	// (this Agent). Empty defaults to personal. household is refused:
	// promotion to Household stays a Person-approved control.
	Scope MemoryScope `json:"scope,omitempty"`
	// Kind is fact or preference; an inference is refused.
	Kind MemoryKind `json:"kind"`
	// Subject is who or what it is about; "me" resolves to the Person
	// on the handle.
	Subject string `json:"subject,omitempty"`
	Content string `json:"content"`
}

// ---- Work -----------------------------------------------------------

// StepKind is what a ledger step records. Values mirror the store's.
type StepKind string

// The closed set of step kinds.
const (
	StepModel    StepKind = "model"
	StepTool     StepKind = "tool"
	StepWait     StepKind = "wait"
	StepDelivery StepKind = "delivery"
)

// Valid reports whether k is a known step kind.
func (k StepKind) Valid() bool {
	switch k {
	case StepModel, StepTool, StepWait, StepDelivery:
		return true
	}
	return false
}

// StepState is where a ledger step is. Values mirror the store's.
type StepState string

// The closed set of step states.
const (
	StepProposed StepState = "proposed"
	StepRunning  StepState = "running"
	StepVerified StepState = "verified"
	StepFailed   StepState = "failed"
	StepSkipped  StepState = "skipped"
)

// Valid reports whether s is a known step state.
func (s StepState) Valid() bool {
	switch s {
	case StepProposed, StepRunning, StepVerified, StepFailed, StepSkipped:
		return true
	}
	return false
}

// Checkpoint is one write to the task ledger, carrying the runtime's
// account of a step. It folds the three ledger checkpoints into one
// shape, selected by State:
//
//   - proposed with Step == 0: append a new step (AppendStep). The
//     receipt returns its sequence.
//   - running: open an attempt on Step (StartAttempt). The receipt
//     returns the attempt sequence and the lease epoch it runs under.
//   - verified or failed: close Attempt with Result or Error
//     (FinishAttempt) and record the step's state.
//
// Sequences and epochs are Wildstone's: a runtime carries back what a
// receipt gave it and never invents one. This is the runtime's report
// of its run (§4 item 7); the calls Wildstone actually served are
// recorded on its own side, and audit compares the two.
type Checkpoint struct {
	Handle Handle `json:"handle"`
	// Step is the Wildstone-assigned step sequence, 0 for a new step.
	Step  int64     `json:"step,omitempty"`
	Kind  StepKind  `json:"kind,omitempty"`
	Name  string    `json:"name,omitempty"`
	State StepState `json:"state"`
	// Params is the step's input as proposed, already redacted.
	Params json.RawMessage `json:"params,omitempty"`
	// Attempt is the attempt sequence being closed.
	Attempt int64 `json:"attempt,omitempty"`
	// Result is the verified result, kept so a resume can serve it
	// instead of dispatching again.
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// CheckpointReceipt is what the ledger assigned: the step sequence for
// an append, the attempt sequence and epoch for a start. A closed
// attempt returns the step and attempt it closed.
type CheckpointReceipt struct {
	Step    int64 `json:"step"`
	Attempt int64 `json:"attempt,omitempty"`
	// Epoch is the lease epoch an attempt was opened under. A result
	// carried back under an older epoch is refused: the attempt was
	// fenced (§3.7).
	Epoch int64 `json:"epoch,omitempty"`
}

// WorkStart moves a pending or waiting task to running. Reason is what
// the trace records for the transition — "delivery", "reply", a resume
// — and is text, not authority.
type WorkStart struct {
	Handle Handle `json:"handle"`
	Reason string `json:"reason,omitempty"`
}

// WorkWaitReason is why a task, as opposed to one invoke, is waiting.
// This is the ledger's own closed set and keeps the ledger's literal
// values, so Phase 4 records what a runtime reports without a mapping
// table between two spellings of the same thing. WaitReason, the
// invoke-level set, is the subset the ADR names for a capability
// result and uses the ADR's spelling.
type WorkWaitReason string

// The closed set of task wait reasons, mirroring the ledger.
const (
	WorkWaitReply             WorkWaitReason = "reply"
	WorkWaitApproval          WorkWaitReason = "approval"
	WorkWaitSchedule          WorkWaitReason = "schedule"
	WorkWaitModelUnavailable  WorkWaitReason = "model-unavailable"
	WorkWaitBudgetExhausted   WorkWaitReason = "budget-exhausted"
	WorkWaitClarification     WorkWaitReason = "clarification"
	WorkWaitSourceUnavailable WorkWaitReason = "source-unavailable"
	WorkWaitUncertainOutcome  WorkWaitReason = "uncertain-outcome"
	// WorkWaitRuntimeOffline: the Agent's own runtime is not connected
	// (§3.7, Phase 4, #340) — offline, revoked, or never registered —
	// so there is nothing to hand this task's next turn to. It is not
	// a failure of the task: the task resumes, unaltered, the moment
	// the runtime's health returns to connected (agentContext's own
	// arrival check re-offers it as a delivery).
	WorkWaitRuntimeOffline WorkWaitReason = "runtime-offline"
)

// Valid reports whether r is a known task wait reason.
func (r WorkWaitReason) Valid() bool {
	switch r {
	case WorkWaitReply, WorkWaitApproval, WorkWaitSchedule, WorkWaitModelUnavailable,
		WorkWaitBudgetExhausted, WorkWaitClarification, WorkWaitSourceUnavailable,
		WorkWaitUncertainOutcome, WorkWaitRuntimeOffline:
		return true
	}
	return false
}

// WorkWait moves the task to waiting with a closed reason and a
// redacted, bounded detail.
type WorkWait struct {
	Handle Handle         `json:"handle"`
	Reason WorkWaitReason `json:"reason"`
	Detail string         `json:"detail,omitempty"`
}

// Evidence is one observation justifying completion. The ledger's
// evidence row also carries a permission label and an audience; those
// are stamped by Wildstone from the task's own admission and are
// deliberately absent here, because a runtime supplying them would be a
// runtime asserting an audience.
type Evidence struct {
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
	// SourceID names what was observed, in the store's source
	// vocabulary.
	SourceID string `json:"sourceId,omitempty"`
	// ReceiptID and ReceiptStep tie the evidence to a served effect;
	// ProposalOnly marks evidence of a filed proposal rather than an
	// applied change.
	ReceiptID    int64 `json:"receiptId,omitempty"`
	ReceiptStep  int64 `json:"receiptStep,omitempty"`
	ProposalOnly bool  `json:"proposalOnly,omitempty"`
}

// WorkComplete writes evidence and completes the task, atomically.
//
// Reply is an external runtime's final answer to a conversation turn
// (#364). A Complete carrying one does not complete the task itself:
// Wildstone accepts the answer the way it accepts its own runtime's —
// the objective check, then completion with evidence drawn from the
// ledger — so Evidence is ignored and the task may instead end not done
// or waiting on review.
type WorkComplete struct {
	Handle   Handle     `json:"handle"`
	Summary  string     `json:"summary,omitempty"`
	Evidence []Evidence `json:"evidence,omitempty"`
	Reply    string     `json:"reply,omitempty"`
}

// WorkFail moves the task to failed.
type WorkFail struct {
	Handle Handle `json:"handle"`
	Reason string `json:"reason"`
}

// ---- Events & subscriptions -----------------------------------------

// DeliveryKind is what a Delivery carries: the four things Wildstone
// calls a runtime with (§4.1 "who calls whom").
type DeliveryKind string

// The closed set of delivery kinds.
const (
	// DeliveryTurn is a Connection turn: a person said something to
	// this Agent.
	DeliveryTurn DeliveryKind = "turn"
	// DeliverySubscription is an Owner-approved subscription match
	// (§3.6); the Task on the Handle is bounded to the subscription
	// version.
	DeliverySubscription DeliveryKind = "subscription"
	// DeliveryEscalation is a routine step asking for wording only: a
	// tool-free run.
	DeliveryEscalation DeliveryKind = "escalation"
	// DeliveryProposalOutcome reports how a filed proposal ended.
	DeliveryProposalOutcome DeliveryKind = "proposal_outcome"
)

// Valid reports whether k is a known delivery kind.
func (k DeliveryKind) Valid() bool {
	switch k {
	case DeliveryTurn, DeliverySubscription, DeliveryEscalation, DeliveryProposalOutcome:
		return true
	}
	return false
}

// Delivery is the envelope Wildstone hands a runtime, by push or on a
// pull (§3.6). It is untrusted input that confers nothing: the Task on
// the Handle has whatever authority Wildstone gave it when it matched,
// and the Payload is data. A runtime acknowledges each delivery by ID
// and tolerates redelivery of the same ID (§4 item 6); one that
// expires unacknowledged is recorded as expired, never dropped.
type Delivery struct {
	// ID is the idempotency key: a redelivery repeats it.
	ID   string       `json:"id"`
	Kind DeliveryKind `json:"kind"`
	// Subscription names the subscription that matched, empty for the
	// other kinds.
	Subscription string `json:"subscription,omitempty"`
	// Cursor is the per-subscription position this delivery advances
	// to. Opaque: a runtime resumes from the last one it acknowledged.
	Cursor string `json:"cursor,omitempty"`
	// Handle is the task this delivery opens or continues.
	Handle Handle `json:"handle"`
	// Payload is the enveloped event, turn or outcome.
	Payload json.RawMessage `json:"payload,omitempty"`
	// Attempt counts deliveries of this ID, from 1.
	Attempt int `json:"attempt,omitempty"`
	// ExpiresAt is when an unacknowledged delivery is recorded as
	// expired.
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
}

// ---- Identity & session ---------------------------------------------

// Heartbeat is a runtime's periodic liveness report (§4 item 10 in the
// runtime-to-Wildstone direction). It carries no health claim: health
// is Wildstone's determination from when the last one arrived.
type Heartbeat struct {
	At time.Time `json:"at"`
	// Version is the runtime's own version string, for the audit trail.
	Version string `json:"version,omitempty"`
	// Cursor is the last delivery cursor the runtime acknowledged, so a
	// gap is visible without waiting for a pull.
	Cursor string `json:"cursor,omitempty"`
}

// RuntimeType names which kind of runtime backs an Agent (§3.3).
type RuntimeType string

// The runtime types this ADR names. The set will grow; each addition
// is a reviewed change here.
const (
	RuntimeNative   RuntimeType = "native"
	RuntimeHermes   RuntimeType = "hermes"
	RuntimeOpenClaw RuntimeType = "openclaw"
)

// Valid reports whether t is a known runtime type.
func (t RuntimeType) Valid() bool {
	switch t {
	case RuntimeNative, RuntimeHermes, RuntimeOpenClaw:
		return true
	}
	return false
}

// RuntimeHealth is the Agent's runtime health state (§3.3, §3.7). It is
// a state on the Agent, not a failure of anything else: deterministic
// work continues around an offline runtime.
type RuntimeHealth string

// The closed set of health states, in the ADR's spelling.
const (
	HealthUnregistered RuntimeHealth = "unregistered"
	HealthConnected    RuntimeHealth = "connected"
	HealthDegraded     RuntimeHealth = "degraded"
	HealthOffline      RuntimeHealth = "offline"
	// HealthRevoked: the runtime principal is disabled and every call
	// fails closed from the next request; no runtime cooperation is
	// required.
	HealthRevoked RuntimeHealth = "revoked"
)

// Valid reports whether h is a known health state.
func (h RuntimeHealth) Valid() bool {
	switch h {
	case HealthUnregistered, HealthConnected, HealthDegraded, HealthOffline, HealthRevoked:
		return true
	}
	return false
}

// DeliveryMode is how a runtime takes deliveries: pushed to an
// endpoint, or pulled by long-polling its cursor (§4.1 "pull
// fallback").
type DeliveryMode string

// The closed set of delivery modes.
const (
	DeliveryPush DeliveryMode = "push"
	DeliveryPull DeliveryMode = "pull"
)

// Valid reports whether m is a known delivery mode.
func (m DeliveryMode) Valid() bool {
	switch m {
	case DeliveryPush, DeliveryPull:
		return true
	}
	return false
}

// ProcessingClass is where a runtime's model runs, as declared by the
// Owner at registration and never as claimed by the runtime (§3.5 rule
// 3). Wildstone cannot verify it; it can refuse to hand local-only
// content to a runtime that was not declared local-only. Undeclared
// fails closed.
type ProcessingClass string

// The closed set of processing classes.
const (
	ProcessingLocalOnly ProcessingClass = "local_only"
	ProcessingCloud     ProcessingClass = "cloud"
)

// Valid reports whether c is a known processing class.
func (c ProcessingClass) Valid() bool {
	switch c {
	case ProcessingLocalOnly, ProcessingCloud:
		return true
	}
	return false
}

// ExecutionProfileRef names #334's execution profile: what an Agent's
// runtime is trusted to do in its own workspace, a setting independent
// of RuntimeType (design doc §5 — choosing Hermes never changes the
// profile, and choosing a profile never changes what the runtime is).
//
// RuntimeRecord.ExecutionProfileRef stays a plain string (that
// vocabulary belonged to #334, not this ADR, per the comment there);
// this type exists only to name the closed set §334-H4 gives it and to
// validate a request against it. A Wildstone handler refuses an unknown
// value rather than mapping it to a default.
type ExecutionProfileRef string

// The closed set of execution profiles (design doc §5). Each tier is
// strictly wider than the last: sandboxed adds a shell and files inside
// the runtime's own container, and host-admin adds nothing to the
// container — it adds the `host` capability family to the Agent's
// Wildstone manifest instead (§5.3, §5.4).
const (
	ExecutionProfilePlatformOnly ExecutionProfileRef = "platform-only"
	ExecutionProfileSandboxed    ExecutionProfileRef = "sandboxed"
	ExecutionProfileHostAdmin    ExecutionProfileRef = "host-admin"
)

// Valid reports whether p is a known execution profile.
func (p ExecutionProfileRef) Valid() bool {
	switch p {
	case ExecutionProfilePlatformOnly, ExecutionProfileSandboxed, ExecutionProfileHostAdmin:
		return true
	}
	return false
}

// Negotiated is what registration settled (§4.1 "negotiate"). The
// capability list is the manifest as narrowed at that moment and is
// informational: every invoke still runs the full resolver.
type Negotiated struct {
	Capabilities    []string        `json:"capabilities,omitempty"`
	Delivery        DeliveryMode    `json:"delivery,omitempty"`
	ProcessingClass ProcessingClass `json:"processingClass,omitempty"`
}

// RuntimeRecord is the `runtime` field of an Agent (§3.3). Replacing it
// replaces nothing else: identity, members, tools, Connections,
// routines, subscriptions and all three memory scopes stay.
type RuntimeRecord struct {
	Type RuntimeType `json:"type"`
	// Endpoint is absent for native; a Route host or
	// container/<name>:<port> for a packaged runtime.
	Endpoint string `json:"endpoint,omitempty"`
	// CredentialRef names the Keystone principal the runtime
	// authenticates as. It is a reference: the credential itself is
	// shown once at minting and never stored on the Agent.
	CredentialRef string        `json:"credentialRef"`
	Health        RuntimeHealth `json:"health"`
	LastHeartbeat time.Time     `json:"lastHeartbeat,omitzero"`
	Negotiated    Negotiated    `json:"negotiated,omitzero"`
	// ExecutionProfileRef names the execution profile (#334) the
	// runtime's own workspace runs under. Its vocabulary belongs to
	// that issue, so it is a reference here, not an enum.
	ExecutionProfileRef string `json:"executionProfileRef,omitempty"`
}

// ---- Proposals & Notifications --------------------------------------

// ProposeRequest files a change for Owner approval (§3.2 Proposals).
// Wildstone validates the document, runs policy, files the Plan and
// reports the outcome later as a Delivery. Nothing here is applied.
type ProposeRequest struct {
	Handle Handle `json:"handle"`
	// Kind is the resource kind, from the manifest's ProposableKinds —
	// or ProposableAgentSubscription, which every Manifest lists
	// because it names no Statefile document.
	Kind string `json:"kind"`
	// Document is the proposed resource, validated by Wildstone. For
	// ProposableAgentSubscription it is a SubscriptionProposal instead
	// of a resource document.
	Document json.RawMessage `json:"document"`
	// Rationale is what the runtime tells the reviewer.
	Rationale string `json:"rationale,omitempty"`
}

// ProposableAgentSubscription is the ProposeRequest.Kind for an
// Owner-approved event subscription (§3.6, Phase 4, #341). Unlike a
// resource kind it is a platform-level proposable, not the Toolset's,
// so every Manifest lists it whether or not the Agent holds any
// resource-proposing capability at all.
const ProposableAgentSubscription = "agent-subscription"

// SubscriptionProposal is the Document body of a ProposeRequest whose
// Kind is ProposableAgentSubscription: what household event to watch
// for, and what a match may do. Wildstone validates it, runs the same
// Owner-approval Plan every other proposal does, and never enables it
// before that approval (§3.6 "Owner-approved lifecycle").
type SubscriptionProposal struct {
	// MatchKind names the matcher the automation evaluator runs
	// (a deployment-defined, closed vocabulary; "event_type" is the
	// first one Wildstone ships).
	MatchKind string `json:"matchKind"`
	// Match is the matcher's own parameters, opaque here.
	Match json.RawMessage `json:"match,omitempty"`
	// Audience is who a matched Task's output may be disclosed to.
	Audience string `json:"audience,omitempty"`
	// Capabilities is the tool-name allow-list a matched Task may use.
	// It can only ever narrow what this Agent already holds — never
	// widen it, however it is named: Wildstone refuses a name outside
	// the Agent's own offered set at proposal time, and re-checks the
	// live set again at every use a matched Task makes of it.
	Capabilities []string `json:"capabilities"`
	// Budget bounds what a matched Task may spend, in the ledger's own
	// budget shape.
	Budget json.RawMessage `json:"budget,omitempty"`
	// Trigger is "event" (the default) or "schedule": a scheduled
	// subscription is due every Cadence (a duration like "15m"; default
	// 15m, 5m..24h) and reads Source, an integration connection. A
	// Personal source additionally needs its owning Person's own
	// authorization before anything runs; Owner approval never
	// substitutes for it (epic #326 S03).
	Trigger string `json:"trigger,omitempty"`
	Cadence string `json:"cadence,omitempty"`
	Source  string `json:"source,omitempty"`
}

// NotifyRequest sends text outward through a Wildstone Connection
// (§3.2 Notifications; §4 item 8). The Connection is the task's own,
// resolved from the Handle; admission, disclosure, quiet hours and
// dedupe are evaluated there. A runtime composes the text and nothing
// else.
type NotifyRequest struct {
	Handle Handle `json:"handle"`
	Text   string `json:"text"`
	// DedupeKey collapses repeats of the same notice, as the stall
	// notifier's does today. Empty means no dedupe.
	DedupeKey string `json:"dedupeKey,omitempty"`
}
