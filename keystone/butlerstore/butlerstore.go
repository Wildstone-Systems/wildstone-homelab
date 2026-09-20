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
	"strings"
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
	// have work done on their behalf. AdmissionCheck wires it to the
	// Butler admission records (P6); it stays a seam so a deployment
	// mid-migration, or a test, can supply its own without this package
	// deciding what admission means.
	OwnerActive func(owner string) error

	// SessionActive, ConnectionActive and GrantActive are the other
	// three legs of design §10.1's derived Task authority: "If the
	// Person, session, Connection or grant behind it later loses
	// authority, the Task loses it too at its next boundary."
	//
	// OwnerActive above is the Person leg and predates them.
	//
	// THEY ARE NOT SEPARATE PERMISSIONS. Nothing is issued, held or
	// revoked here — each one re-reads something that already existed
	// and can only ever turn a task off. A Task is a narrowing of the
	// request that created it, never a capability of its own.
	//
	// WHETHER A LEG APPLIES IS DECIDED BY THE TASK, NOT BY THE WIRING.
	// A task that recorded no originating session has no session leg to
	// re-evaluate; a routine has neither session nor Connection and is
	// its own principal (§10.2). But a task that DID record one and
	// finds the check unwired is refused, because an origin nobody can
	// confirm is an origin §10.1 answers with no. That is the shape
	// AdmissionCheck's note argues for — the wiring is the act — with
	// the data rather than the deployment saying which acts are needed,
	// so turning the rule on cannot retroactively refuse work that
	// never had the origin in the first place.
	SessionActive    func(sessionID string) error
	ConnectionActive func(connection string) error
	// GrantActive is given the task id rather than a grant id: the
	// question §10.1 asks is whether the authority this work was
	// admitted under still stands, and the grants that answer it are
	// exactly the ones scoped to this task.
	GrantActive func(taskID string) error

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
	return l.derivedAuthority(task)
}

// derivedAuthority re-evaluates the rest of §10.1's originating
// authority for one task, at one boundary.
//
// Every branch is a refusal. There is deliberately nothing here that
// can turn a no into a yes, because a Task's authority is a NARROWING
// of the request that created it — "there is no separate Task grant
// object to issue, hold or revoke" — and a function that could widen
// one would be that object by another name.
//
// The order is Person, session, Connection, grant: outermost first, so
// the reason a person is given names the largest thing that went away
// rather than an inner consequence of it.
func (l *Ledger) derivedAuthority(task runtimedb.ButlerTask) error {
	// The session the request was had in. A routine has none.
	if task.SessionID != "" {
		if l.SessionActive == nil {
			return fmt.Errorf(
				"%w: it was started in session %s and nothing here can say whether that session still stands",
				ErrAuthorityWithdrawn, task.SessionID)
		}
		if err := l.SessionActive(task.SessionID); err != nil {
			return fmt.Errorf("%w: %v", ErrAuthorityWithdrawn, err)
		}
	}
	// The Connection the request arrived over. A routine has none, and
	// a task created before 0062 has none recorded — in both cases the
	// leg does not apply rather than silently passing.
	if task.OriginConnection != "" {
		if l.ConnectionActive == nil {
			return fmt.Errorf(
				"%w: it arrived over %s and nothing here can say whether that connection still admits it",
				ErrAuthorityWithdrawn, task.OriginConnection)
		}
		if err := l.ConnectionActive(task.OriginConnection); err != nil {
			return fmt.Errorf("%w: %v", ErrAuthorityWithdrawn, err)
		}
	}
	// The grants this work was admitted under. Unlike the three above,
	// there is no field saying whether the leg applies: a task either
	// has grants scoped to it or it does not, and the check answers
	// both cases without being told which it is.
	if l.GrantActive != nil {
		if err := l.GrantActive(task.ID); err != nil {
			return fmt.Errorf("%w: %v", ErrAuthorityWithdrawn, err)
		}
	}
	return nil
}

// AdmissionCheck wires OwnerActive to Butler admission (ADR-W030 A2.1).
//
// It is a constructor rather than the default because "no admission
// records exist yet" and "this person is not admitted" must not be the
// same answer: a deployment that has not run P6's admission flow would
// otherwise find every task refused. Wiring it is the act that turns
// admission on.
func (l *Ledger) AdmissionCheck() func(string) error {
	return func(owner string) error {
		m, err := l.db.GetButlerMember(owner)
		if errors.Is(err, runtimedb.ErrNotFound) {
			return fmt.Errorf("%s has no Butler admission record", owner)
		}
		if err != nil {
			return err
		}
		if !m.Active() {
			return fmt.Errorf("%s is no longer admitted to the Butler", owner)
		}
		return nil
	}
}

// SessionCheck, ConnectionCheck and GrantCheck wire the other three
// legs of §10.1's derived authority to the runtime database.
//
// They are constructors rather than defaults for AdmissionCheck's
// reason, restated: wiring is the act. DerivedAuthorityChecks below
// turns all four on together, which is how a deployment should do it —
// three of four legs is not three quarters of the rule, it is a rule
// with a hole whose shape nobody wrote down.

// SessionCheck refuses a task whose originating session was withdrawn.
func (l *Ledger) SessionCheck() func(string) error {
	return func(sessionID string) error { return l.db.ButlerSessionActive(sessionID) }
}

// ConnectionCheck refuses a task whose originating Connection no longer
// admits it.
//
// WHAT IT CHECKS TODAY IS EXISTENCE, AND THAT IS THE WHOLE OF IT. Slice
// 11 (#212) is what makes a Connection an admission boundary; until it
// lands there is no rule about what a Connection admits, and inventing
// one here would be this slice answering a question it was told to
// leave open (#211: "State this split explicitly in the code, or #212
// will find a Connection leg that looks finished and is not").
//
// The built-in surfaces are not rows and cannot be withdrawn: the
// Console and the household dashboard are Connections a deployment HAS
// rather than ones somebody added (§8.2), so they answer yes here and
// their ceilings are enforced where they always were.
func (l *Ledger) ConnectionCheck() func(string) error {
	return func(connection string) error {
		if connection == "" {
			return errors.New("it arrived over no connection")
		}
		if builtInConnection(connection) {
			return nil
		}
		transport, external, ok := strings.Cut(connection, ":")
		if !ok {
			return fmt.Errorf("%q is not a connection this deployment knows", connection)
		}
		if _, err := l.db.ChatConnectionAt(transport, external); err != nil {
			if errors.Is(err, runtimedb.ErrNotFound) {
				return fmt.Errorf("the %s connection it arrived over is gone", connection)
			}
			return err
		}
		return nil
	}
}

// builtInConnection reports whether a name is one of the Connections a
// deployment has rather than one somebody added.
//
// The names are duplicated from keystone/butlerlive and keystone/portal
// rather than imported: butlerlive imports THIS package, and portal is
// an HTTP surface the store has no business knowing. Two short strings
// is what keeping that direction costs, and connectionNamesAgree in the
// butlerlive tests pins them together.
func builtInConnection(name string) bool {
	return name == "console" || name == "household_portal"
}

// GrantCheck refuses a task when ANY grant it was admitted under has
// been revoked.
//
// A task that never held a grant is unaffected: most work is done with
// tools nobody had to grant, and "no grants exist" is not "a grant was
// taken away". Those two cases are what this check distinguishes.
//
// ANY, NOT ALL. An earlier version stopped the task only once every
// grant had been revoked, and that threshold had no principle behind
// it — there is no reason a task should survive the first revocation
// and die on the last. §10.1 is singular and unconditional: "if the
// Person, session, Connection or grant behind it later loses
// authority, the Task loses it too at its next boundary". Revoking a
// grant is a deliberate act by an Owner on this specific work, and the
// per-tool resolver refusing that one tool is not the same as the work
// stopping. Found by a review bot on #287.
//
// EXPIRY IS NOT REVOCATION and is deliberately not treated as one. A
// grant that ran out is the clock doing what it was set to do, not
// somebody withdrawing permission; the tool it covered is no longer
// permitted, which the resolver already answers per tool, and killing
// the whole task for it would stop work that never needed the tool
// again.
func (l *Ledger) GrantCheck() func(string) error {
	return func(taskID string) error {
		grants, err := l.db.ListToolGrants(runtimedb.GrantScopeTask, taskID)
		if err != nil {
			return err
		}
		for _, g := range grants {
			if !g.RevokedAt.IsZero() {
				return fmt.Errorf("the %s grant it was admitted under was revoked", g.Tool)
			}
		}
		return nil
	}
}

// DerivedAuthorityChecks turns on all four legs of §10.1's derived Task
// authority. It is the one call a deployment makes.
func (l *Ledger) DerivedAuthorityChecks() {
	l.OwnerActive = l.AdmissionCheck()
	l.SessionActive = l.SessionCheck()
	l.ConnectionActive = l.ConnectionCheck()
	l.GrantActive = l.GrantCheck()
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
