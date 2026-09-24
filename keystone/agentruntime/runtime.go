// Package agentruntime is Wildstone's agent runtime contract (issue #357).
//
// Wildstone is an agent platform, not an agent framework. This package
// is the line between the two. Wildstone owns everything on this side of
// it — job lifecycle, identity, tool authorization, audit, timeouts,
// cancellation and runtime selection — and a runtime owns only the
// model's reasoning loop on the other side.
//
// A runtime is an adapter that implements AgentRuntime. The native
// adapter (keystone/agentruntime/native) delegates the loop to the
// OpenAI Agents SDK; Hermes and OpenClaw adapters implement the same
// interface. Nothing in this package names a model provider or an SDK
// type, and nothing may: an adapter that needs one keeps it behind its
// own boundary.
package agentruntime

import (
	"context"
	"errors"
	"time"
)

// State is a job's lifecycle state. It is Wildstone's vocabulary, not a
// runtime's: every adapter maps whatever its engine reports onto these.
type State string

const (
	// Queued: accepted, not yet running.
	StateQueued State = "QUEUED"
	// Running: the runtime is reasoning (a model turn is in flight).
	StateRunning State = "RUNNING"
	// WaitingForTool: Wildstone is authorizing or executing a tool call.
	StateWaitingForTool State = "WAITING_FOR_TOOL"
	// WaitingForUser: the agent asked a question. Resume with a reply.
	StateWaitingForUser State = "WAITING_FOR_USER"
	// Blocked: the agent cannot make progress on its own — it said so,
	// it ran out of budget, it kept repeating itself, or it produced no
	// next action. Resume with guidance, or cancel.
	StateBlocked State = "BLOCKED"
	// Completed: finished with a final answer.
	StateCompleted State = "COMPLETED"
	// Failed: stopped by an error, a timeout or a guardrail.
	StateFailed State = "FAILED"
	// Cancelled: stopped on request.
	StateCancelled State = "CANCELLED"
)

// Terminal reports whether no further transition is possible.
func (s State) Terminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCancelled
}

// Settled reports whether the job has stopped moving on its own: it is
// terminal, or it is parked waiting for a person. A settled job is never
// "still thinking" — that is the guarantee this contract exists to give.
func (s State) Settled() bool {
	return s.Terminal() || s == StateWaitingForUser || s == StateBlocked
}

// Resumable reports whether Resume may be called in this state.
func (s State) Resumable() bool {
	return s == StateWaitingForUser || s == StateBlocked
}

// Job is one unit of agent work handed to a runtime.
type Job struct {
	// ID is unique per runtime. Required.
	ID string
	// Instructions is the system prompt: the surface's instruction
	// hierarchy, already chosen by the caller.
	Instructions string
	// Input is the person's request.
	Input string
	// Actor is who asked, for the audit trail.
	Actor string
	// Tools is the Wildstone tool surface. Every call the agent makes is
	// routed through it, and it is where authorization happens. Nil
	// means a tool-free job.
	Tools ToolHost
	// Recorder receives the audit trail. Required: a job that cannot be
	// audited does not run.
	Recorder Recorder
	// Limits are the guardrails. Zero fields take DefaultLimits.
	Limits Limits
}

// EventKind names what Resume is being told.
type EventKind string

const (
	// EventUserReply answers a WAITING_FOR_USER question, or gives a
	// BLOCKED job the guidance it asked for.
	EventUserReply EventKind = "user_reply"
)

// Event is input to a parked job.
type Event struct {
	Kind EventKind
	Text string
}

// Status is a point-in-time view of a job.
type Status struct {
	JobID   string
	Runtime string
	State   State
	// Reason explains BLOCKED and FAILED, and why a job was cancelled.
	Reason string
	// Question is what the agent asked, in WAITING_FOR_USER.
	Question string
	// Output is the final answer, in COMPLETED.
	Output string
	// Turns counts model turns in the current run; ToolCalls counts
	// every tool call across the job's whole life, including resumes.
	Turns     int
	ToolCalls int
	CreatedAt time.Time
	UpdatedAt time.Time
	// LastHeartbeat is the last sign of life from the runtime. It is the
	// health signal for a long-running job.
	LastHeartbeat time.Time
	FinishedAt    time.Time
}

// Capabilities describes what a runtime supports.
type Capabilities struct {
	Runtime string
	// Engine names what runs the loop ("openai-agents-sdk"), and
	// EngineVersion its version when known. Informational only.
	Engine        string
	EngineVersion string
	ToolCalling   bool
	Resume        bool
	Cancel        bool
}

// Health is a runtime's readiness.
type Health struct {
	OK        bool
	Detail    string
	CheckedAt time.Time
}

// AgentRuntime is the contract every runtime adapter implements.
//
// Start returns as soon as the job is accepted; the work runs in the
// background. Wait blocks until the job is Settled. Every method is safe
// for concurrent use.
type AgentRuntime interface {
	Name() string
	Start(ctx context.Context, job Job) (Status, error)
	Resume(ctx context.Context, jobID string, ev Event) (Status, error)
	Cancel(ctx context.Context, jobID string) (Status, error)
	Status(ctx context.Context, jobID string) (Status, error)
	Wait(ctx context.Context, jobID string) (Status, error)
	Capabilities(ctx context.Context) (Capabilities, error)
	Health(ctx context.Context) Health
}

var (
	ErrUnknownJob   = errors.New("agentruntime: unknown job")
	ErrDuplicateJob = errors.New("agentruntime: job id already in use")
	ErrInvalidJob   = errors.New("agentruntime: invalid job")
	ErrNotResumable = errors.New("agentruntime: job is not waiting for input")
)
