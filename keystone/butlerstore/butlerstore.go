// Package butlerstore binds the Butler's durable checkpoint seam to the
// runtime database (ADR-W030 P1/P3).
//
// keystone/butler defines TaskLedger and holds no database handle; this
// package imports both sides and implements the interface. The direction
// matters: the Butler must not be able to reach the store's full API by
// following a field, and the store must not have to know what a tool
// call is. One adapter in the middle is the whole cost of keeping both
// true.
//
// The Ledger also carries the coordinator identity. Every checkpoint is
// fenced against it, so a coordinator that has been explicitly promoted
// away stops at its next checkpoint rather than finishing work the new
// one now owns (ADR-W030 A11).
package butlerstore

import (
	"errors"
	"fmt"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// Ledger implements butler.TaskLedger over a runtime database.
type Ledger struct {
	db *runtimedb.DB
	// holder and epoch are the coordinator lease this ledger writes
	// under. Fence compares them against the stored lease before every
	// dispatch.
	holder string
	epoch  int64
	// OwnerActive, when set, reports whether a task's owner may still
	// have work done on their behalf. It is a seam because Butler
	// admission and identity links are P6's subject; until then a
	// deployment wires whatever it has, and nil means "no admission
	// check is configured", which is the current behaviour rather than a
	// silent grant.
	OwnerActive func(owner string) error
	// Now is the ledger clock; nil means time.Now.
	Now func() time.Time
}

// New builds a ledger for the coordinator identified by lease.
func New(db *runtimedb.DB, lease runtimedb.CoordinatorLease) *Ledger {
	return &Ledger{db: db, holder: lease.Holder, epoch: lease.Epoch}
}

func (l *Ledger) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// ErrAuthorityWithdrawn reports that a task may no longer proceed. It is
// deliberately one error for four situations — cancelled, offboarded,
// expired, owner disabled — because the runner's response to all four is
// the same: stop now, change nothing.
var ErrAuthorityWithdrawn = errors.New("butlerstore: this task may no longer continue")

// Authorize implements butler.TaskLedger. It is the recheck that runs
// before every slice and every model round (ADR-W030 A2.1: "check
// current membership and link generation at every retrieval and
// dispatch").
func (l *Ledger) Authorize(taskID string) error {
	task, err := l.db.GetButlerTask(taskID)
	if err != nil {
		return err
	}
	switch task.State {
	case runtimedb.ButlerTaskCancelled:
		return fmt.Errorf("%w: it was cancelled", ErrAuthorityWithdrawn)
	case runtimedb.ButlerTaskCompleted, runtimedb.ButlerTaskFailed:
		return fmt.Errorf("%w: it already finished as %s", ErrAuthorityWithdrawn, task.State)
	case runtimedb.ButlerTaskPausedOffboarded:
		// Offboarded work performs no retries, notices, or model calls
		// until an administrator reviews it (A4). Resuming it is a
		// deliberate act elsewhere, not something a slice does.
		return fmt.Errorf("%w: its owner was offboarded and it is paused for review", ErrAuthorityWithdrawn)
	}
	// The absolute deadline expires even while waiting: waiting does not
	// consume active-work budget, and it does not extend the clock (A4).
	if !task.DeadlineAt.IsZero() && l.now().After(task.DeadlineAt) {
		return fmt.Errorf("%w: its deadline passed at %s", ErrAuthorityWithdrawn,
			task.DeadlineAt.Format(time.RFC3339))
	}
	if l.OwnerActive != nil {
		if err := l.OwnerActive(task.Owner); err != nil {
			return fmt.Errorf("%w: %v", ErrAuthorityWithdrawn, err)
		}
	}
	return nil
}

// FenceAttempts implements butler.TaskLedger.
func (l *Ledger) FenceAttempts(taskID string) (int64, error) {
	return l.db.FenceButlerTaskAttempts(taskID)
}

// Fence reports whether this coordinator still holds the database.
func (l *Ledger) Fence() error { return l.db.CheckCoordinator(l.holder, l.epoch) }

// Steps returns the task's ledger with each verified step carrying the
// recorded result of its last successful attempt.
//
// The join happens here rather than in the store because "the result a
// resume may reuse" is a runner concept: the store records every
// attempt, including the failed and fenced ones, and deciding which of
// them counts is exactly what this adapter is for.
func (l *Ledger) Steps(taskID string) ([]butler.LedgerStep, error) {
	steps, err := l.db.ListButlerTaskSteps(taskID)
	if err != nil {
		return nil, err
	}
	attempts, err := l.db.ListButlerTaskAttempts(taskID)
	if err != nil {
		return nil, err
	}
	// Last success wins: attempts come back oldest first, so a later
	// successful retry overwrites an earlier one.
	results := map[int64]string{}
	verifiedAt := map[int64]time.Time{}
	for _, a := range attempts {
		if a.State == runtimedb.ButlerAttemptSucceeded {
			results[a.Step] = a.ResultJSON
			// EndedAt is when the reading was actually taken, which is
			// what freshness is measured from — not when the step row
			// was created, which can predate a long call.
			verifiedAt[a.Step] = a.EndedAt
		}
	}
	out := make([]butler.LedgerStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, butler.LedgerStep{
			Seq:        s.Seq,
			Ordinal:    s.Ordinal,
			Kind:       string(s.Kind),
			Name:       s.Name,
			State:      string(s.State),
			Params:     s.ParamsJSON,
			Result:     results[s.Seq],
			VerifiedAt: verifiedAt[s.Seq],
		})
	}
	return out, nil
}

func (l *Ledger) AppendStep(taskID, kind, name, paramsJSON string) (int64, error) {
	return l.db.AppendButlerTaskStep(taskID, runtimedb.ButlerTaskStep{
		Kind: runtimedb.ButlerStepKind(kind), Name: name, ParamsJSON: paramsJSON,
	})
}

func (l *Ledger) StartAttempt(stepSeq int64) (butler.LedgerAttempt, error) {
	a, err := l.db.StartButlerAttempt(stepSeq)
	if err != nil {
		return butler.LedgerAttempt{}, err
	}
	return butler.LedgerAttempt{Seq: a.Seq, Attempt: a.Attempt, Epoch: a.Epoch}, nil
}

func (l *Ledger) FinishAttempt(attemptSeq int64, ok bool, resultJSON, errText string) error {
	state := runtimedb.ButlerAttemptFailed
	if ok {
		state = runtimedb.ButlerAttemptSucceeded
	}
	return l.db.FinishButlerAttempt(attemptSeq, state, resultJSON, errText)
}

func (l *Ledger) SetStepState(stepSeq int64, state string) error {
	return l.db.SetButlerStepState(stepSeq, runtimedb.ButlerStepState(state))
}

func (l *Ledger) Trace(taskID string, e butler.LedgerTraceEvent) error {
	return l.db.AppendButlerTraceEvent(taskID, runtimedb.ButlerTraceEvent{
		Type:            e.Type,
		Step:            e.Step,
		Attempt:         e.Attempt,
		Epoch:           l.epoch,
		Actor:           e.Actor,
		ModelEndpoint:   e.ModelEndpoint,
		ModelVerdict:    e.ModelVerdict,
		ManifestVersion: e.ManifestVersion,
		SourceIDs:       e.SourceIDs,
		PermissionLabel: e.PermissionLabel,
		Audience:        runtimedb.ButlerTaskAudience(e.Audience),
		// Payloads reaching here have already been through the gateway's
		// redaction pipeline or are structural counts written by the
		// runner itself. Marking them redacted is a claim this adapter
		// can actually make, because it is the only writer.
		Redacted:    true,
		PayloadJSON: e.Payload,
	})
}

func (l *Ledger) Wait(taskID, reason, detail string) error {
	return l.db.OpenButlerTaskWait(taskID, runtimedb.ButlerTaskWait{
		Reason: runtimedb.ButlerWaitReason(reason), Detail: detail,
	})
}

func (l *Ledger) Complete(taskID string, evidence []butler.LedgerEvidence, summary string) error {
	rows := make([]runtimedb.ButlerTaskEvidence, 0, len(evidence))
	for _, e := range evidence {
		rows = append(rows, runtimedb.ButlerTaskEvidence{
			Kind:            runtimedb.ButlerEvidenceKind(e.Kind),
			Description:     e.Description,
			SourceID:        e.SourceID,
			PermissionLabel: e.PermissionLabel,
			Audience:        runtimedb.ButlerTaskAudience(e.Audience),
		})
	}
	return l.db.CompleteButlerTask(taskID, rows, summary, l.holder)
}

func (l *Ledger) Fail(taskID, reason string) error {
	return l.db.TransitionButlerTask(taskID, runtimedb.ButlerTaskFailed, reason, l.holder)
}

// Running moves a task to running from wherever it legitimately can.
//
// A task already running is not an error: a slice that starts, waits and
// is continued passes through here each time, and the state machine
// refusing running → running would make the runner's first act on a
// resume a failure. An open wait is resolved first, because a task that
// is waiting has one and it is what the resume answers.
func (l *Ledger) Running(taskID, reason string) error {
	task, err := l.db.GetButlerTask(taskID)
	if err != nil {
		return err
	}
	switch task.State {
	case runtimedb.ButlerTaskRunning:
		return nil
	case runtimedb.ButlerTaskWaiting:
		return l.db.ResolveButlerTaskWait(taskID, l.holder, reason)
	case runtimedb.ButlerTaskPending, runtimedb.ButlerTaskPausedOffboarded:
		return l.db.TransitionButlerTask(taskID, runtimedb.ButlerTaskRunning, reason, l.holder)
	default:
		return fmt.Errorf("butlerstore: task %s is %s and cannot be resumed", taskID, task.State)
	}
}
