package agentruntime

import (
	"encoding/json"
	"time"
)

// Limits are the guardrails Wildstone enforces OUTSIDE a runtime's loop.
// They do not trust the runtime or the model to guarantee forward
// progress: each one turns "might hang" into a definite state.
type Limits struct {
	// JobTimeout is the hard wall-clock limit for one run (a Start or a
	// Resume). Reaching it: FAILED.
	JobTimeout time.Duration
	// TurnTimeout bounds one model turn: the longest the runtime may go
	// without making progress (a turn, a tool call, an answer).
	// Heartbeats do not count as progress. Reaching it: FAILED.
	TurnTimeout time.Duration
	// ToolTimeout bounds one tool call. Reaching it hands the model a
	// timeout error and counts as a tool failure.
	ToolTimeout time.Duration
	// StallTimeout is the longest the runtime may go with no message at
	// all, heartbeats included. Reaching it means the process is frozen:
	// FAILED.
	StallTimeout time.Duration
	// HeartbeatInterval is how often the runtime is asked to report in.
	HeartbeatInterval time.Duration
	// MaxTurns bounds model turns per run. Reaching it: BLOCKED.
	MaxTurns int
	// MaxToolCalls bounds tool calls across the job's whole life. It
	// does not reset on Resume, because a budget that resets is not a
	// budget. Reaching it: BLOCKED.
	MaxToolCalls int
	// MaxRepeatedCalls is how many times the same tool may be called
	// with the same input in one job. The next one: BLOCKED.
	MaxRepeatedCalls int
	// MaxToolFailures is how many tool calls in a row may fail or be
	// denied before the job stops replanning. Reaching it: BLOCKED.
	MaxToolFailures int
	// MaxInvalidActions is how many malformed actions (bad arguments,
	// unknown tools) the model may produce in one run before it is
	// judged unable to drive the tools. Exceeding it: FAILED.
	MaxInvalidActions int
}

// ReasonTurnBudget is the BLOCKED reason when a run spends its turn
// budget. It is a pause rather than a problem — the agent was still
// working — so callers may treat it as "say continue".
const ReasonTurnBudget = "turn budget exhausted"

// DefaultLimits are conservative values for a conversational job.
var DefaultLimits = Limits{
	JobTimeout:        10 * time.Minute,
	TurnTimeout:       3 * time.Minute,
	ToolTimeout:       60 * time.Second,
	StallTimeout:      45 * time.Second,
	HeartbeatInterval: 5 * time.Second,
	MaxTurns:          12,
	MaxToolCalls:      64,
	MaxRepeatedCalls:  3,
	MaxToolFailures:   3,
	MaxInvalidActions: 3,
}

// WithDefaults fills every zero or negative field from DefaultLimits.
func (l Limits) WithDefaults() Limits {
	d := DefaultLimits
	dur := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	num := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	dur(&l.JobTimeout, d.JobTimeout)
	dur(&l.TurnTimeout, d.TurnTimeout)
	dur(&l.ToolTimeout, d.ToolTimeout)
	dur(&l.StallTimeout, d.StallTimeout)
	dur(&l.HeartbeatInterval, d.HeartbeatInterval)
	num(&l.MaxTurns, d.MaxTurns)
	num(&l.MaxToolCalls, d.MaxToolCalls)
	num(&l.MaxRepeatedCalls, d.MaxRepeatedCalls)
	num(&l.MaxToolFailures, d.MaxToolFailures)
	num(&l.MaxInvalidActions, d.MaxInvalidActions)
	return l
}

// Guard tracks one job's budgets and failure patterns. It is not safe
// for concurrent use; an adapter holds one per job and consults it from
// a single goroutine.
//
// It is here, not in an adapter, so every runtime stops for the same
// reasons.
type Guard struct {
	limits     Limits
	toolCalls  int
	turns      int
	invalid    int
	failStreak int
	seenInputs map[string]int
}

// NewGuard returns a Guard over limits (defaults applied).
func NewGuard(l Limits) *Guard {
	return &Guard{limits: l.WithDefaults(), seenInputs: map[string]int{}}
}

// Limits returns the effective limits.
func (g *Guard) Limits() Limits { return g.limits }

// ToolCalls and Turns report the counters.
func (g *Guard) ToolCalls() int { return g.toolCalls }
func (g *Guard) Turns() int     { return g.turns }

// NewRun resets the per-run counters (turns and invalid actions) for a
// Resume. The tool budget, the repeat memory and the failure streak
// carry over.
func (g *Guard) NewRun() {
	g.turns = 0
	g.invalid = 0
}

// Turn records a model turn and reports a BLOCKED reason when the turn
// budget is spent.
func (g *Guard) Turn() (blocked string) {
	g.turns++
	if g.turns > g.limits.MaxTurns {
		return ReasonTurnBudget
	}
	return ""
}

// Invalid records a malformed action and reports a FAILED reason when
// the model has produced too many.
func (g *Guard) Invalid() (failed string) {
	g.invalid++
	if g.invalid > g.limits.MaxInvalidActions {
		return "the model kept producing invalid actions"
	}
	return ""
}

// BeforeTool records an intended tool call and reports a BLOCKED reason
// when it must not run: the budget is spent, or the same call has
// already been made too often.
func (g *Guard) BeforeTool(name string, input map[string]any) (blocked string) {
	if g.toolCalls >= g.limits.MaxToolCalls {
		return "tool call budget exhausted"
	}
	key := name + "\x00" + canonical(input)
	g.seenInputs[key]++
	if g.seenInputs[key] > g.limits.MaxRepeatedCalls {
		return "the agent kept repeating the same action (" + name + ")"
	}
	g.toolCalls++
	return ""
}

// AfterTool records a tool call's outcome and reports a BLOCKED reason
// when too many in a row have failed.
func (g *Guard) AfterTool(failed bool) (blocked string) {
	if !failed {
		g.failStreak = 0
		return ""
	}
	g.failStreak++
	if g.failStreak >= g.limits.MaxToolFailures {
		return "repeated tool failures"
	}
	return ""
}

// canonical renders input with sorted keys, so two calls that differ
// only in key order are the same call.
func canonical(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
