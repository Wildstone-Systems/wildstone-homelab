package butler

// The ledger-envelope and objective-classification vocabulary that used
// to live in runner.go, ledger_envelope.go and objective.go before
// ADR-W039 Phase 2b (#337) moved the Runner itself to
// keystone/agentruntime/legacy.
//
// These names are aliased rather than dropped: keystone/butlerstore's
// Ledger implements TaskLedger/ObjectiveLedger structurally and is
// documented against these exact names, keystone/agentgateway writes
// the same trace and step vocabulary on the runner's behalf, and
// keystone/butlerlive's StallNotifier field and
// keystone/agentruntime/legacy's own SliceLedger both need the same
// LedgerStep/LedgerTraceEvent/ObjectiveEffect shapes. Moving the
// definitions to keystone/agentruntime — a leaf package neither this
// package nor keystone/agentruntime/legacy's strict import ban needs to
// worry about — and aliasing them back here changed no call site.

import (
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentruntime"
)

const (
	LedgerStepModel = agentruntime.LedgerStepModel
	LedgerStepTool  = agentruntime.LedgerStepTool

	LedgerStepVerified = agentruntime.LedgerStepVerified
	LedgerStepFailed   = agentruntime.LedgerStepFailed
)

const (
	TraceSliceStarted   = agentruntime.TraceSliceStarted
	TraceModelCall      = agentruntime.TraceModelCall
	TraceModelAnswer    = agentruntime.TraceModelAnswer
	TraceToolDispatched = agentruntime.TraceToolDispatched
	TraceToolResult     = agentruntime.TraceToolResult
	TraceDenial         = agentruntime.TraceDenial
	TraceWaitOpened     = agentruntime.TraceWaitOpened
	TraceAttemptFenced  = agentruntime.TraceAttemptFenced
	TraceOutcome        = agentruntime.TraceOutcome
)

const (
	LedgerWaitReply            = agentruntime.LedgerWaitReply
	LedgerWaitBudgetExhausted  = agentruntime.LedgerWaitBudgetExhausted
	LedgerWaitModelUnavailable = agentruntime.LedgerWaitModelUnavailable
	LedgerWaitUncertainOutcome = agentruntime.LedgerWaitUncertainOutcome
)

type (
	LedgerStep       = agentruntime.LedgerStep
	LedgerAttempt    = agentruntime.LedgerAttempt
	LedgerEvidence   = agentruntime.LedgerEvidence
	LedgerTraceEvent = agentruntime.LedgerTraceEvent
	TaskLedger       = agentruntime.TaskLedger
	StallNotifier    = agentruntime.StallNotifier
	ToolStepResult   = agentruntime.ToolStepResult
	ObjectiveEffect  = agentruntime.ObjectiveEffect
	ObjectiveLedger  = agentruntime.ObjectiveLedger
)

// CallFingerprint identifies a tool call by name and arguments. Alias
// of the shared implementation (keystone/agentruntime), which
// keystone/agentgateway also calls directly by that name.
func CallFingerprint(name string, input map[string]any) string {
	return agentruntime.CallFingerprint(name, input)
}

// VerifiedResults reads a task's ledger and returns, per fingerprint,
// the recorded result of every tool step that reached verified within
// ttl of now. Alias of the shared implementation.
func VerifiedResults(steps []LedgerStep, now time.Time, ttl time.Duration) map[string]string {
	return agentruntime.VerifiedResults(steps, now, ttl)
}
