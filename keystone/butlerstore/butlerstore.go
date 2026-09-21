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
	// have work done on their behalf. AdmissionCheck wires it to the
	// Butler admission records (P6); it stays a seam so a deployment
	// mid-migration, or a test, can supply its own without this package
	// deciding what admission means.
	OwnerActive func(owner string) error

	// SessionActive and ConnectionActive are the other legs of design
	// §10.1's derived Task authority that a TASK can be re-evaluated
	// against: "If the
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
	ConnectionActive func(connection OriginConnection) error

	// THERE IS DELIBERATELY NO GRANT LEG. §10.1 names a grant among the
	// things a Task derives from, and a revoked grant is enforced — by
	// the RESOLVER, per tool, which is where it belongs. Refusing the
	// tool is the whole of what the revocation said.
	//
	// Stopping the WORK is a different act with its own mechanism: a
	// state transition. A4 already does exactly that for transferred
	// work (offboarding.go pauses it to paused-offboarded and its
	// comment says it "waits for grant and connection review"), and an
	// Owner stopping a routine cancels it.
	//
	// A task-level check derived from grant history was tried here and
	// removed. Grant rows are immutable and accumulate, so every
	// predicate over them needs a recovery rule, and each recovery rule
	// needs another: all-revoked, then any-revoked, then
	// newest-after-newest — three review rounds on #287, each fix
	// creating the next gap. The signature of a rule that should not be
	// derived from history at all.

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
		// By ROW, with the address only for the refusal to cite. An
		// address is reusable — a revoked butler_addresses row can be
		// followed by a new one carrying the same transport and id,
		// even for a different Agent — so checking the address would
		// let a re-added bot resurrect the authority of work whose
		// Connection was withdrawn. Found by a review bot on #287.
		if err := l.ConnectionActive(OriginConnection{
			Name: task.OriginConnection, Ref: task.OriginConnectionRef, Owner: task.Owner,
		}); err != nil {
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

// SessionCheck and ConnectionCheck wire the other legs of §10.1's
// derived authority to the runtime database.
//
// They are constructors rather than defaults for AdmissionCheck's
// reason, restated: wiring is the act. DerivedAuthorityChecks below
// turns them on together, which is how a deployment should do it — a
// subset is not a fraction of the rule, it is a rule with a hole whose
// shape nobody wrote down.

// SessionCheck refuses a task whose originating session was withdrawn.
func (l *Ledger) SessionCheck() func(string) error {
	return func(sessionID string) error { return l.db.ButlerSessionActive(sessionID) }
}

// OriginConnection is the Connection a task was admitted over: the row
// it actually was, and the address it wore.
//
// Both, because they answer different questions. The REF decides — it
// is immutable, and a new row always gets a new one — while the NAME is
// what a refusal cites, because an id is not something to show a
// person.
type OriginConnection struct {
	Name string
	Ref  int64
	// Owner is the Person the task belongs to, because "does this
	// Connection still admit it" is not a question about the
	// Connection alone. A bot narrowed to Owners, or a Person demoted
	// out of the role it admits, both withdraw a task's authority over
	// a Connection that is otherwise perfectly alive.
	//
	// It is the same handle Ledger.OwnerActive is given, and it is
	// checked here for a different reason: OwnerActive asks whether
	// this Person is still admitted to the BUTLER, and this asks
	// whether they are still admitted over this CONNECTION. A Person
	// can pass one and fail the other, which is the whole of design
	// §8.3 — the Connection is its own boundary.
	Owner string
}

// ConnectionCheck refuses a task whose originating Connection no longer
// admits it.
//
// IT CHECKS EXISTENCE AND THEN ADMISSION, which is what slice 11
// (#212) added. Slice 10 left this at existence alone and said so:
// there was no rule yet about what a Connection admits, so inventing
// one here would have been answering a question that slice was told to
// leave open. Migration 0063 is that rule, and Connection.Admits is
// where it is evaluated — the same function the message path and the
// claim path call, so a task re-checked at a boundary is held to the
// policy a new message would be held to.
//
// WHY ADMISSION BELONGS AT THE BOUNDARY AND NOT ONLY AT THE DOOR.
// §10.1: a Task "is re-evaluated at every authorization boundary... If
// the Person, session, Connection or grant behind it later loses
// authority, the Task loses it too at its next boundary." A task
// admitted an hour ago over a bot that has since been narrowed to
// Owners, by a Person who has since been demoted, is exactly the case
// a door-only check cannot see: nothing about the task changed, and
// everything about its admission did.
//
// The built-in surfaces are not rows and cannot be withdrawn: the
// Console and the household dashboard are Connections a deployment HAS
// rather than ones somebody added (§8.2), so they answer yes here and
// their ceilings are enforced where they always were.
func (l *Ledger) ConnectionCheck() func(OriginConnection) error {
	return func(c OriginConnection) error {
		if c.Name == "" {
			return errors.New("it arrived over no connection")
		}
		if BuiltInConnection(c.Name) {
			return nil
		}
		if c.Ref == 0 {
			// A named Connection with no row behind it. That is a task
			// recorded before 0062 carried the reference, and it cannot
			// be confirmed — which §10.1 answers with no rather than
			// falling back to the address it happens to name.
			return fmt.Errorf(
				"the %s connection it arrived over cannot be identified", c.Name)
		}
		// ErrNoConnection, not ErrNotFound: ChatConnectionBySeq answers
		// with the connection package's own sentinel. Testing the wrong
		// one made this branch unreachable — a revoked Connection came
		// back as a raw internal error, and a real database failure was
		// indistinguishable from a revocation. The test passed anyway
		// because it asserted on the substring "connection", which both
		// messages happen to contain.
		conn, err := l.db.ChatConnectionBySeq(c.Ref)
		if err != nil {
			if errors.Is(err, runtimedb.ErrNoConnection) {
				return fmt.Errorf("the %s connection it arrived over is gone", c.Name)
			}
			return err
		}
		// Whose work this is. Without a Person there is no admission
		// question to ask, and §10.1 answers a missing leg with no
		// rather than with "not applicable" — a task recorded with no
		// owner cannot be shown to be admitted anywhere.
		if c.Owner == "" {
			return fmt.Errorf(
				"nothing records who it belongs to, so %s cannot be shown to admit it", c.Name)
		}
		m, err := l.db.GetButlerMember(c.Owner)
		if err != nil {
			if errors.Is(err, runtimedb.ErrNotFound) {
				return fmt.Errorf(
					"%s is not a person on record, so %s cannot be shown to admit them",
					c.Owner, c.Name)
			}
			return err
		}
		// The ROLE AS IT STANDS NOW. Reading it fresh is the point:
		// the whole value of a boundary check is that it sees a
		// demotion that happened after the task was admitted.
		if ok, reason := conn.Admits(m.Role); !ok {
			return fmt.Errorf("%s no longer admits it: %s", c.Name, reason)
		}
		return nil
	}
}

// BuiltInConnection reports whether a name is one of the Connections a
// deployment HAS rather than one somebody added.
//
// Exported only so the agreement between these copies and the constants
// they copy can be pinned from a package that can see both — this one
// cannot, which is the whole reason the names are copied.
//
// The names are duplicated from keystone/butlerlive and keystone/portal
// rather than imported: butlerlive imports THIS package, and portal is
// an HTTP surface the store has no business knowing. Two short strings
// is what keeping that direction costs.
//
// TestBuiltInConnectionNamesAgree, in keystone/api, pins them to the
// constants they copy,
// so renaming either one fails here rather than silently refusing every
// in-flight Console or dashboard task at its next boundary. An earlier
// version of this comment claimed such a test existed when it did not.
func BuiltInConnection(name string) bool {
	return name == "console" || name == "household_portal"
}

// DerivedAuthorityChecks turns on the legs of §10.1's derived Task
// authority that a task can be re-evaluated against. It is the one call
// a deployment makes.
//
// Three, not four: the grant leg is the resolver's, per tool. See the
// note on the fields above.
func (l *Ledger) DerivedAuthorityChecks() {
	l.OwnerActive = l.AdmissionCheck()
	l.SessionActive = l.SessionCheck()
	l.ConnectionActive = l.ConnectionCheck()
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
		receipt := e.ReceiptID
		if e.ReceiptStep != 0 {
			var err error
			receipt, err = l.db.RecordButlerEffectReceipt(taskID, runtimedb.EffectReceipt{ProposalOnly: e.ProposalOnly, Kind: "tool", SourceID: fmt.Sprintf("step:%d", e.ReceiptStep), Step: e.ReceiptStep, Outcome: "succeeded", PermissionLabel: e.PermissionLabel, Audience: runtimedb.ButlerTaskAudience(e.Audience)})
			if err != nil {
				return err
			}
		}
		rows = append(rows, runtimedb.ButlerTaskEvidence{
			ReceiptID:       receipt,
			Kind:            runtimedb.ButlerEvidenceKind(e.Kind),
			Description:     e.Description,
			SourceID:        e.SourceID,
			PermissionLabel: e.PermissionLabel,
			Audience:        runtimedb.ButlerTaskAudience(e.Audience),
		})
	}
	saved, err := l.db.PersistedEffectEvidence(taskID)
	if err != nil {
		return err
	}
	seen := map[int64]bool{}
	for _, e := range rows {
		if e.ReceiptID != 0 {
			seen[e.ReceiptID] = true
		}
	}
	for _, e := range saved {
		if !seen[e.ReceiptID] {
			rows = append(rows, e)
		}
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

func (l *Ledger) Objective(taskID string) (string, error) {
	task, err := l.db.GetButlerTask(taskID)
	return task.Objective, err
}
func (l *Ledger) ClassifyObjective(taskID, kind string, proposalOnly bool, effects []butler.ObjectiveEffect) error {
	if err := l.Fence(); err != nil {
		return err
	}
	if err := l.Authorize(taskID); err != nil {
		return err
	}
	required := make([]runtimedb.RequiredEffect, 0, len(effects))
	for _, effect := range effects {
		required = append(required, runtimedb.RequiredEffect{Description: effect.Description, SourceKey: effect.SourceKey, ProposalOnly: effect.ProposalOnly})
	}
	return l.db.ClassifyButlerObjectiveEffects(taskID, runtimedb.ObjectiveKind(kind), proposalOnly, required)
}

func (l *Ledger) ConfirmedObjectiveOperations(taskID string) (map[string]bool, error) {
	if err := l.Fence(); err != nil {
		return nil, err
	}
	if err := l.Authorize(taskID); err != nil {
		return nil, err
	}
	return l.db.ConfirmedObjectiveOperations(taskID)
}
