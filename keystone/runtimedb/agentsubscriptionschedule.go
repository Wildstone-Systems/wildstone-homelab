package runtimedb

// Scheduled subscription runs, schedule state and external-source
// checkpoints (migration 0099, epic #326 S03, #456).
//
// The evaluator (keystone/automation/scheduled.go) decides WHEN a
// scheduled subscription runs and WHETHER it may; this file is what
// makes the answer durable and race-free:
//
//   - ClaimSubscriptionRun opens at most one running run per
//     subscription (a partial unique index, not the loop's good
//     behaviour) and moves next_due_at to claim-time + cadence, so any
//     amount of downtime collapses into at most one catch-up run.
//   - CompleteSubscriptionRun is the ONLY writer of a checkpoint. It
//     commits by compare-and-swap on the checkpoint version the run was
//     handed, and only after re-checking every fence the run was
//     dispatched under. A run that crashed, was fenced, or lost the race
//     commits nothing; the next run resumes from the last COMMITTED
//     checkpoint, so committed work is never skipped and uncommitted
//     work is re-read (at-least-once; the consumer dedupes by message
//     fingerprint).
//   - AbandonExpiredSubscriptionRuns turns a run whose lease ran out into
//     a failure with bounded backoff, so a runner that dies mid-run
//     neither wedges the subscription nor causes a replay storm.
//
// Every time here comes from the caller's clock, never now(): the
// scheduler's cadence, catch-up and backoff are proved against a
// deterministic clock.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Schedule holds. A hold survives restarts and is lifted only by an
// explicit ResumeAgentSubscription.
const (
	SubscriptionHoldNone         = ""
	SubscriptionHoldPaused       = "paused"
	SubscriptionHoldAuthFailed   = "auth_failed"
	SubscriptionHoldConfigFailed = "config_failed"
)

// Run states.
const (
	SubscriptionRunRunning   = "running"
	SubscriptionRunSucceeded = "succeeded"
	SubscriptionRunFailed    = "failed"
	SubscriptionRunAbandoned = "abandoned"
	SubscriptionRunFenced    = "fenced"
)

// Run outcomes, as a run reports them. The distinctions are the contract
// (ADR-W030 A5, epic #326): an empty mailbox is not an unreachable one,
// and a rejected password is not a transient outage.
const (
	// RunOutcomeOK: the source answered and the run's results are durable.
	RunOutcomeOK = "ok"
	// RunOutcomeEmpty: the source answered with nothing new. A valid
	// answer; it may still advance the checkpoint (a new UIDVALIDITY).
	RunOutcomeEmpty = "empty"
	// RunOutcomeUnavailable: the source could not be reached. Retry with
	// backoff; nothing concluded.
	RunOutcomeUnavailable = "unavailable"
	// RunOutcomeStale: only an old observation was available. Nothing is
	// committed from it; retry with backoff.
	RunOutcomeStale = "stale"
	// RunOutcomeAuthFailed: the source refused the credential. Retrying
	// would only lock the account; the schedule holds until the Person
	// fixes it and resumes.
	RunOutcomeAuthFailed = "auth_failed"
	// RunOutcomeConfigFailed: permanent misconfiguration (a folder that
	// does not exist, a malformed filter). Holds like auth_failed.
	RunOutcomeConfigFailed = "config_failed"
	// RunOutcomeError: Keystone's own failure to dispatch or process.
	// Transient; retry with backoff.
	RunOutcomeError = "error"
	// RunOutcomeAbandoned: the run's lease expired with no report (the
	// runner crashed). Recorded by the sweep, never reported.
	RunOutcomeAbandoned = "abandoned"
)

// Retry backoff bounds: the first retry after RetryBaseDelay, doubling,
// never longer than the cadence itself or SubscriptionRetryMaxDelay.
const (
	SubscriptionRetryBaseDelay = time.Minute
	SubscriptionRetryMaxDelay  = time.Hour
)

// DefaultSubscriptionRunLease is how long a run may stay in flight
// before the sweep abandons it.
const DefaultSubscriptionRunLease = 10 * time.Minute

var (
	// ErrSubscriptionRunInFlight: the subscription already has a running
	// run. Not an error to report; the tick simply does not overlap.
	ErrSubscriptionRunInFlight = errors.New("runtimedb: this subscription already has a run in flight")
	// ErrSubscriptionNotDue: not due yet, or held.
	ErrSubscriptionNotDue = errors.New("runtimedb: this subscription is not due")
	// ErrSubscriptionNotDispatchable: disabled, not a scheduled
	// subscription, or not at the approved version the claim named.
	ErrSubscriptionNotDispatchable = errors.New("runtimedb: this subscription may not be dispatched")
	// ErrSubscriptionRunFenced: the run is no longer the one allowed to
	// report — it finished, was abandoned, or a fence it was dispatched
	// under moved. Nothing was committed.
	ErrSubscriptionRunFenced = errors.New("runtimedb: this subscription run has been fenced")
	// ErrCheckpointConflict: the checkpoint moved since the run was
	// handed it. Nothing was committed and the run is fenced.
	ErrCheckpointConflict = errors.New("runtimedb: the subscription checkpoint moved under this run")
)

// SubscriptionRetryDelay is the bounded backoff after the n-th
// consecutive failure (n >= 1): 1m, 2m, 4m, ... capped at the cadence
// and at SubscriptionRetryMaxDelay.
func SubscriptionRetryDelay(cadence time.Duration, failures int) time.Duration {
	limit := SubscriptionRetryMaxDelay
	if cadence > 0 && cadence < limit {
		limit = cadence
	}
	d := SubscriptionRetryBaseDelay
	for i := 1; i < failures && d < limit; i++ {
		d *= 2
	}
	if d > limit {
		d = limit
	}
	return d
}

// SubscriptionSchedule is a scheduled subscription's durable timing and
// health.
type SubscriptionSchedule struct {
	Subscription int64 `json:"subscription"`
	// NextDueAt zero means due at the next tick.
	NextDueAt           time.Time `json:"nextDueAt,omitzero"`
	Hold                string    `json:"hold,omitempty"`
	HoldReason          string    `json:"holdReason,omitempty"`
	HeldAt              time.Time `json:"heldAt,omitzero"`
	HeldBy              string    `json:"heldBy,omitempty"`
	ConsecutiveFailures int       `json:"consecutiveFailures"`
	LastRunAt           time.Time `json:"lastRunAt,omitzero"`
	LastSuccessAt       time.Time `json:"lastSuccessAt,omitzero"`
	LastFailureAt       time.Time `json:"lastFailureAt,omitzero"`
	LastOutcome         string    `json:"lastOutcome,omitempty"`
	LastError           string    `json:"lastError,omitempty"`
	BlockedAt           time.Time `json:"blockedAt,omitzero"`
	BlockedReason       string    `json:"blockedReason,omitempty"`
}

// Due reports whether the schedule is due at t and not held.
func (s SubscriptionSchedule) Due(t time.Time) bool {
	return s.Hold == SubscriptionHoldNone && (s.NextDueAt.IsZero() || !t.Before(s.NextDueAt))
}

// SubscriptionRun is one scheduled run.
type SubscriptionRun struct {
	ID                    int64     `json:"id"`
	Subscription          int64     `json:"subscription"`
	SubscriptionVersion   int       `json:"subscriptionVersion"`
	SourceConnection      string    `json:"sourceConnection,omitempty"`
	ConnectionEpoch       int64     `json:"connectionEpoch,omitempty"`
	PersonID              string    `json:"personId,omitempty"`
	CoordinatorEpoch      int64     `json:"coordinatorEpoch,omitempty"`
	BaseCheckpointVersion int64     `json:"baseCheckpointVersion"`
	State                 string    `json:"state"`
	Outcome               string    `json:"outcome,omitempty"`
	TaskID                string    `json:"taskId,omitempty"`
	Error                 string    `json:"error,omitempty"`
	DueAt                 time.Time `json:"dueAt,omitzero"`
	StartedAt             time.Time `json:"startedAt"`
	LeaseExpiresAt        time.Time `json:"leaseExpiresAt"`
	FinishedAt            time.Time `json:"finishedAt,omitzero"`
}

// SubscriptionCheckpoint is the last committed position of one
// subscription in one source. Version 0 with a nil Cursor means nothing
// has ever been committed: the consumer starts from its own default.
type SubscriptionCheckpoint struct {
	Subscription   int64           `json:"subscription"`
	Source         string          `json:"source"`
	Version        int64           `json:"version"`
	Cursor         json.RawMessage `json:"cursor,omitempty"`
	CommittedAt    time.Time       `json:"committedAt,omitzero"`
	CommittedByRun int64           `json:"committedByRun,omitempty"`
}

func formatAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseAt(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// ensureSubscriptionSchedule creates a schedule row for a scheduled
// subscription that has none. An event subscription gets none.
func ensureSubscriptionSchedule(tx *sql.Tx, id int64) error {
	_, err := tx.Exec(`INSERT INTO agent_subscription_schedules (subscription, updated_at)
		SELECT id, ? FROM agent_subscriptions WHERE id = ? AND trigger_kind = 'schedule'
		ON CONFLICT(subscription) DO NOTHING`, now(), id)
	if err != nil {
		return fmt.Errorf("runtimedb: creating subscription %d's schedule: %w", id, err)
	}
	return nil
}

const subscriptionScheduleColumns = `SELECT subscription, next_due_at, hold, hold_reason, held_at, held_by,
	consecutive_failures, last_run_at, last_success_at, last_failure_at, last_outcome, last_error,
	blocked_at, blocked_reason FROM agent_subscription_schedules`

func scanSubscriptionSchedule(row rowScanner) (SubscriptionSchedule, error) {
	var s SubscriptionSchedule
	var due, held, lastRun, lastOK, lastFail, blocked string
	err := row.Scan(&s.Subscription, &due, &s.Hold, &s.HoldReason, &held, &s.HeldBy,
		&s.ConsecutiveFailures, &lastRun, &lastOK, &lastFail, &s.LastOutcome, &s.LastError,
		&blocked, &s.BlockedReason)
	if errors.Is(err, sql.ErrNoRows) {
		return SubscriptionSchedule{}, ErrNotFound
	}
	if err != nil {
		return SubscriptionSchedule{}, fmt.Errorf("runtimedb: scanning subscription schedule: %w", err)
	}
	for _, f := range []struct {
		text string
		into *time.Time
	}{{due, &s.NextDueAt}, {held, &s.HeldAt}, {lastRun, &s.LastRunAt}, {lastOK, &s.LastSuccessAt},
		{lastFail, &s.LastFailureAt}, {blocked, &s.BlockedAt}} {
		if *f.into, err = parseAt(f.text); err != nil {
			return SubscriptionSchedule{}, fmt.Errorf("runtimedb: bad schedule timestamp %q: %w", f.text, err)
		}
	}
	return s, nil
}

// GetSubscriptionSchedule reads a scheduled subscription's schedule, or
// ErrNotFound for an event subscription or one never approved.
func (db *DB) GetSubscriptionSchedule(id int64) (SubscriptionSchedule, error) {
	return scanSubscriptionSchedule(db.sql.QueryRow(subscriptionScheduleColumns+` WHERE subscription = ?`, id))
}

// PauseAgentSubscription holds a scheduled subscription. A run already
// in flight may still finish and commit: pausing is "no more runs", not
// a revocation (that is the dispatch gate's and disable's job).
func (db *DB) PauseAgentSubscription(id int64, by, reason string, at time.Time) error {
	if by == "" {
		return fmt.Errorf("runtimedb: pausing a subscription needs who paused it")
	}
	return db.setSubscriptionHold(id, SubscriptionHoldPaused, reason, by, at)
}

// ResumeAgentSubscription lifts any hold — a pause, or the hold an
// auth_failed/config_failed run put on it once the Person has fixed the
// cause. next_due_at is left alone: an overdue schedule gets exactly one
// catch-up run, and the failure count restarts so it is not also
// delayed by the backoff that preceded the hold.
func (db *DB) ResumeAgentSubscription(id int64, by string, at time.Time) error {
	if err := db.gate(GateIngestion); err != nil {
		return err
	}
	if by == "" {
		return fmt.Errorf("runtimedb: resuming a subscription needs who resumed it")
	}
	res, err := db.sql.Exec(`UPDATE agent_subscription_schedules
		SET hold = '', hold_reason = '', held_at = '', held_by = '', consecutive_failures = 0, updated_at = ?
		WHERE subscription = ?`, formatAt(at), id)
	if err != nil {
		return fmt.Errorf("runtimedb: resuming subscription %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("runtimedb: subscription %d has no schedule: %w", id, ErrNotFound)
	}
	return nil
}

func (db *DB) setSubscriptionHold(id int64, hold, reason, by string, at time.Time) error {
	if err := db.gate(GateIngestion); err != nil {
		return err
	}
	res, err := db.sql.Exec(`UPDATE agent_subscription_schedules
		SET hold = ?, hold_reason = ?, held_at = ?, held_by = ?, updated_at = ?
		WHERE subscription = ?`, hold, reason, formatAt(at), by, formatAt(at), id)
	if err != nil {
		return fmt.Errorf("runtimedb: holding subscription %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("runtimedb: subscription %d has no schedule: %w", id, ErrNotFound)
	}
	return nil
}

// SetAgentSubscriptionCadence changes how often a scheduled subscription
// runs, within the same bounds a proposal is held to. Cadence is the
// Person's pacing choice, not scope: it does not bump the version. The
// next due time is pulled in if the new cadence makes it sooner, never
// pushed out past what was already promised.
func (db *DB) SetAgentSubscriptionCadence(id int64, cadence time.Duration) error {
	if err := db.gate(GateIngestion); err != nil {
		return err
	}
	_, cadence, err := NormalizeSubscriptionTrigger(SubscriptionTriggerSchedule, cadence, "")
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE agent_subscriptions SET cadence_seconds = ?, updated_at = ?
		WHERE id = ? AND trigger_kind = 'schedule'`, int64(cadence/time.Second), now(), id)
	if err != nil {
		return fmt.Errorf("runtimedb: setting subscription %d's cadence: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("runtimedb: subscription %d is not a scheduled subscription: %w", id, ErrNotFound)
	}
	s, err := scanSubscriptionSchedule(tx.QueryRow(subscriptionScheduleColumns+` WHERE subscription = ?`, id))
	if err == nil && !s.LastRunAt.IsZero() && !s.NextDueAt.IsZero() {
		sooner := s.LastRunAt.Add(cadence)
		if sooner.Before(s.NextDueAt) {
			if _, err := tx.Exec(`UPDATE agent_subscription_schedules SET next_due_at = ? WHERE subscription = ?`,
				formatAt(sooner), id); err != nil {
				return err
			}
		}
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return tx.Commit()
}

// RecordSubscriptionBlocked notes that the dispatch gate refused a due
// subscription. It is health, not failure: the failure count and the due
// time are untouched, so lifting the block yields exactly one run. The
// row is only rewritten when the reason changes, so a subscription
// blocked for a week does not churn a write every tick.
func (db *DB) RecordSubscriptionBlocked(id int64, reason string, at time.Time) error {
	_, err := db.sql.Exec(`UPDATE agent_subscription_schedules
		SET blocked_at = ?, blocked_reason = ?, updated_at = ?
		WHERE subscription = ? AND blocked_reason <> ?`, formatAt(at), reason, formatAt(at), id, reason)
	if err != nil {
		return fmt.Errorf("runtimedb: recording subscription %d blocked: %w", id, err)
	}
	return nil
}

// SubscriptionRunClaim is what the evaluator asserts when it opens a
// run: the exact approved version, source, connection epoch and Person
// it checked the gate against. The claim re-reads each inside its
// transaction and refuses on any difference.
type SubscriptionRunClaim struct {
	Subscription     int64
	Version          int
	SourceConnection string
	ConnectionEpoch  int64
	PersonID         string
	At               time.Time
	Lease            time.Duration
}

// ClaimSubscriptionRun opens a run, returning it with the checkpoint it
// resumes from. ErrSubscriptionRunInFlight means the previous run has
// not finished (no overlap); ErrSubscriptionNotDue means it is not due
// or is held; ErrSubscriptionNotDispatchable means the row moved since
// the gate looked at it.
func (db *DB) ClaimSubscriptionRun(c SubscriptionRunClaim) (SubscriptionRun, SubscriptionCheckpoint, error) {
	if err := db.gate(GateDispatch); err != nil {
		return SubscriptionRun{}, SubscriptionCheckpoint{}, err
	}
	if c.At.IsZero() {
		return SubscriptionRun{}, SubscriptionCheckpoint{}, fmt.Errorf("runtimedb: a run claim needs the scheduler's time")
	}
	if c.Lease <= 0 {
		c.Lease = DefaultSubscriptionRunLease
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return SubscriptionRun{}, SubscriptionCheckpoint{}, err
	}
	defer func() { _ = tx.Rollback() }()
	sub, err := scanAgentSubscription(tx.QueryRow(agentSubscriptionColumns+` WHERE id = ?`, c.Subscription))
	if errors.Is(err, sql.ErrNoRows) {
		return SubscriptionRun{}, SubscriptionCheckpoint{}, ErrSubscriptionNotDispatchable
	}
	if err != nil {
		return SubscriptionRun{}, SubscriptionCheckpoint{}, err
	}
	if sub.TriggerKind != SubscriptionTriggerSchedule || !sub.Dispatchable() || sub.Version != c.Version ||
		sub.SourceConnection != c.SourceConnection || sub.PersonID != c.PersonID {
		return SubscriptionRun{}, SubscriptionCheckpoint{}, ErrSubscriptionNotDispatchable
	}
	if c.SourceConnection != "" {
		var epoch int64
		if err := tx.QueryRow(`SELECT lease_epoch FROM integration_connections WHERE name = ?`,
			c.SourceConnection).Scan(&epoch); err != nil || epoch != c.ConnectionEpoch {
			return SubscriptionRun{}, SubscriptionCheckpoint{}, ErrSubscriptionNotDispatchable
		}
	}
	sched, err := scanSubscriptionSchedule(tx.QueryRow(subscriptionScheduleColumns+` WHERE subscription = ?`, c.Subscription))
	if err != nil {
		return SubscriptionRun{}, SubscriptionCheckpoint{}, err
	}
	if !sched.Due(c.At) {
		return SubscriptionRun{}, SubscriptionCheckpoint{}, ErrSubscriptionNotDue
	}
	cp, err := readSubscriptionCheckpoint(tx, c.Subscription, c.SourceConnection)
	if err != nil {
		return SubscriptionRun{}, SubscriptionCheckpoint{}, err
	}
	var coordEpoch int64
	if lease := db.executionLease.Load(); lease != nil {
		coordEpoch = lease.Epoch
	}
	var person any
	if c.PersonID != "" {
		person = c.PersonID
	}
	res, err := tx.Exec(`INSERT INTO agent_subscription_runs
		(subscription, subscription_version, source_connection, connection_epoch, person_id, coordinator_epoch,
		 base_checkpoint_version, state, due_at, started_at, lease_expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'running', ?, ?, ?)`,
		c.Subscription, c.Version, c.SourceConnection, c.ConnectionEpoch, person, coordEpoch,
		cp.Version, formatAt(sched.NextDueAt), formatAt(c.At), formatAt(c.At.Add(c.Lease)))
	if err != nil {
		if isUniqueViolation(err) {
			return SubscriptionRun{}, SubscriptionCheckpoint{}, ErrSubscriptionRunInFlight
		}
		return SubscriptionRun{}, SubscriptionCheckpoint{}, fmt.Errorf("runtimedb: claiming a subscription run: %w", err)
	}
	runID, err := res.LastInsertId()
	if err != nil {
		return SubscriptionRun{}, SubscriptionCheckpoint{}, err
	}
	// The next run is due one cadence from THIS claim, not from the
	// missed slot: however long the scheduler was down, what it owes is
	// one run, not a backlog.
	if _, err := tx.Exec(`UPDATE agent_subscription_schedules
		SET next_due_at = ?, last_run_at = ?, blocked_at = '', blocked_reason = '', updated_at = ?
		WHERE subscription = ?`, formatAt(c.At.Add(sub.Cadence)), formatAt(c.At), formatAt(c.At), c.Subscription); err != nil {
		return SubscriptionRun{}, SubscriptionCheckpoint{}, err
	}
	run, err := scanSubscriptionRun(tx.QueryRow(subscriptionRunColumns+` WHERE id = ?`, runID))
	if err != nil {
		return SubscriptionRun{}, SubscriptionCheckpoint{}, err
	}
	if err := tx.Commit(); err != nil {
		return SubscriptionRun{}, SubscriptionCheckpoint{}, err
	}
	return run, cp, nil
}

// SetSubscriptionRunTask records the Task a dispatched run opened.
func (db *DB) SetSubscriptionRunTask(runID int64, taskID string) error {
	_, err := db.sql.Exec(`UPDATE agent_subscription_runs SET task_id = ? WHERE id = ?`, taskID, runID)
	return err
}

// SubscriptionRunReport is what a run reports when it finishes.
type SubscriptionRunReport struct {
	Outcome string
	// Checkpoint is the new opaque cursor, or nil to leave it. Only an
	// ok or empty outcome may carry one: a run that could not read the
	// source has nothing to commit a position for.
	Checkpoint json.RawMessage
	Error      string
	At         time.Time
}

// CompleteSubscriptionRun records a run's outcome and, for ok/empty,
// commits its checkpoint. Call it only after the run's results are
// themselves durable: that ordering is the crash boundary. A crash
// before this call leaves the checkpoint where it was, and the next run
// re-reads from there.
//
// Every fence is re-checked in the same transaction as the commit: the
// run must still be running, the subscription still enabled at the
// same approved version and source, the connection at the same lease
// epoch, the coordinator at the same epoch, and the checkpoint at the
// version the run was handed. On any difference the run is fenced,
// nothing is committed, and ErrSubscriptionRunFenced or
// ErrCheckpointConflict is returned.
func (db *DB) CompleteSubscriptionRun(runID int64, rep SubscriptionRunReport) (SubscriptionRun, error) {
	if err := db.gate(GateIngestion); err != nil {
		return SubscriptionRun{}, err
	}
	if rep.At.IsZero() {
		return SubscriptionRun{}, fmt.Errorf("runtimedb: a run report needs the time it finished")
	}
	switch rep.Outcome {
	case RunOutcomeOK, RunOutcomeEmpty:
		if rep.Checkpoint != nil && !json.Valid(rep.Checkpoint) {
			return SubscriptionRun{}, fmt.Errorf("runtimedb: a checkpoint must be JSON")
		}
	case RunOutcomeUnavailable, RunOutcomeStale, RunOutcomeAuthFailed, RunOutcomeConfigFailed, RunOutcomeError:
		if rep.Checkpoint != nil {
			return SubscriptionRun{}, fmt.Errorf("runtimedb: a %s run has no position to commit", rep.Outcome)
		}
	default:
		return SubscriptionRun{}, fmt.Errorf("runtimedb: run outcome must be ok|empty|unavailable|stale|auth_failed|config_failed|error, got %q", rep.Outcome)
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return SubscriptionRun{}, err
	}
	defer func() { _ = tx.Rollback() }()
	run, err := scanSubscriptionRun(tx.QueryRow(subscriptionRunColumns+` WHERE id = ?`, runID))
	if err != nil {
		return SubscriptionRun{}, err
	}
	if run.State != SubscriptionRunRunning {
		return run, fmt.Errorf("%w: run %d is already %s", ErrSubscriptionRunFenced, runID, run.State)
	}
	if reason := runFenceMoved(tx, run); reason != "" {
		return db.fenceRunTx(tx, run, reason, rep.At, ErrSubscriptionRunFenced)
	}
	sub, err := scanAgentSubscription(tx.QueryRow(agentSubscriptionColumns+` WHERE id = ?`, run.Subscription))
	if err != nil {
		return SubscriptionRun{}, err
	}
	at := formatAt(rep.At)
	success := rep.Outcome == RunOutcomeOK || rep.Outcome == RunOutcomeEmpty
	if success && rep.Checkpoint != nil {
		var res sql.Result
		if run.BaseCheckpointVersion == 0 {
			res, err = tx.Exec(`INSERT INTO agent_subscription_checkpoints
				(subscription, source, version, cursor_json, committed_at, committed_by_run)
				VALUES (?, ?, 1, ?, ?, ?) ON CONFLICT(subscription, source) DO NOTHING`,
				run.Subscription, run.SourceConnection, string(rep.Checkpoint), at, run.ID)
		} else {
			res, err = tx.Exec(`UPDATE agent_subscription_checkpoints
				SET version = version + 1, cursor_json = ?, committed_at = ?, committed_by_run = ?
				WHERE subscription = ? AND source = ? AND version = ?`,
				string(rep.Checkpoint), at, run.ID, run.Subscription, run.SourceConnection, run.BaseCheckpointVersion)
		}
		if err != nil {
			return SubscriptionRun{}, fmt.Errorf("runtimedb: committing subscription checkpoint: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return db.fenceRunTx(tx, run, "the checkpoint moved since this run was dispatched", rep.At, ErrCheckpointConflict)
		}
	}
	state := SubscriptionRunSucceeded
	if !success {
		state = SubscriptionRunFailed
	}
	if _, err := tx.Exec(`UPDATE agent_subscription_runs SET state = ?, outcome = ?, error = ?, finished_at = ?
		WHERE id = ? AND state = 'running'`, state, rep.Outcome, rep.Error, at, run.ID); err != nil {
		return SubscriptionRun{}, err
	}
	if success {
		_, err = tx.Exec(`UPDATE agent_subscription_schedules
			SET consecutive_failures = 0, last_success_at = ?, last_outcome = ?, last_error = '', updated_at = ?
			WHERE subscription = ?`, at, rep.Outcome, at, run.Subscription)
	} else {
		err = recordRunFailure(tx, run.Subscription, sub.Cadence, rep.Outcome, rep.Error, rep.At)
	}
	if err != nil {
		return SubscriptionRun{}, err
	}
	run, err = scanSubscriptionRun(tx.QueryRow(subscriptionRunColumns+` WHERE id = ?`, runID))
	if err != nil {
		return SubscriptionRun{}, err
	}
	return run, tx.Commit()
}

// FenceSubscriptionRun ends a running run without committing anything —
// the evaluator's own fence, when a check only it can make (the live
// background-use authority) refuses the run's report.
func (db *DB) FenceSubscriptionRun(runID int64, reason string, at time.Time) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	run, err := scanSubscriptionRun(tx.QueryRow(subscriptionRunColumns+` WHERE id = ?`, runID))
	if err != nil {
		return err
	}
	if run.State != SubscriptionRunRunning {
		return nil
	}
	_, err = db.fenceRunTx(tx, run, reason, at, nil)
	return err
}

// fenceRunTx marks run fenced and commits tx. It returns want (wrapped
// with the reason) so a caller can return it directly.
func (db *DB) fenceRunTx(tx *sql.Tx, run SubscriptionRun, reason string, at time.Time, want error) (SubscriptionRun, error) {
	if _, err := tx.Exec(`UPDATE agent_subscription_runs SET state = 'fenced', error = ?, finished_at = ?
		WHERE id = ? AND state = 'running'`, reason, formatAt(at), run.ID); err != nil {
		return SubscriptionRun{}, err
	}
	if _, err := tx.Exec(`UPDATE agent_subscription_schedules SET last_outcome = 'fenced', last_error = ?, updated_at = ?
		WHERE subscription = ?`, reason, formatAt(at), run.Subscription); err != nil {
		return SubscriptionRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return SubscriptionRun{}, err
	}
	run.State, run.Error, run.FinishedAt = SubscriptionRunFenced, reason, at
	if want == nil {
		return run, nil
	}
	return run, fmt.Errorf("%w: %s", want, reason)
}

// runFenceMoved re-reads every fence a run was dispatched under and
// names the first that moved, or "" when none did.
func runFenceMoved(tx *sql.Tx, run SubscriptionRun) string {
	sub, err := scanAgentSubscription(tx.QueryRow(agentSubscriptionColumns+` WHERE id = ?`, run.Subscription))
	if err != nil {
		return "the subscription can no longer be read"
	}
	switch {
	case !sub.Enabled:
		return "the subscription was disabled"
	case sub.Version != run.SubscriptionVersion || sub.ApprovedVersion != run.SubscriptionVersion:
		return "the subscription was edited since this run was dispatched"
	case sub.SourceConnection != run.SourceConnection:
		return "the subscription's source changed"
	}
	if run.SourceConnection != "" {
		var epoch int64
		if err := tx.QueryRow(`SELECT lease_epoch FROM integration_connections WHERE name = ?`,
			run.SourceConnection).Scan(&epoch); err != nil {
			return "the source connection is gone"
		}
		if epoch != run.ConnectionEpoch {
			return "the source connection was fenced by a newer lease epoch"
		}
	}
	if run.CoordinatorEpoch != 0 {
		var epoch int64
		if err := tx.QueryRow(`SELECT epoch FROM coordinator_lease WHERE id = 1`).Scan(&epoch); err != nil ||
			epoch != run.CoordinatorEpoch {
			return "the coordinator that dispatched this run has been superseded"
		}
	}
	return ""
}

// recordRunFailure counts a failure and sets the bounded backoff, or the
// hold an auth/config failure calls for.
func recordRunFailure(tx *sql.Tx, subscription int64, cadence time.Duration, outcome, errText string, at time.Time) error {
	var failures int
	if err := tx.QueryRow(`SELECT consecutive_failures FROM agent_subscription_schedules WHERE subscription = ?`,
		subscription).Scan(&failures); err != nil {
		return fmt.Errorf("runtimedb: reading subscription %d's schedule: %w", subscription, err)
	}
	failures++
	hold := SubscriptionHoldNone
	switch outcome {
	case RunOutcomeAuthFailed:
		hold = SubscriptionHoldAuthFailed
	case RunOutcomeConfigFailed:
		hold = SubscriptionHoldConfigFailed
	}
	stamp := formatAt(at)
	next := formatAt(at.Add(SubscriptionRetryDelay(cadence, failures)))
	if hold != SubscriptionHoldNone {
		_, err := tx.Exec(`UPDATE agent_subscription_schedules
			SET consecutive_failures = ?, last_failure_at = ?, last_outcome = ?, last_error = ?,
			    hold = ?, hold_reason = ?, held_at = ?, held_by = 'scheduler', next_due_at = ?, updated_at = ?
			WHERE subscription = ?`, failures, stamp, outcome, errText, hold, errText, stamp, next, stamp, subscription)
		return err
	}
	_, err := tx.Exec(`UPDATE agent_subscription_schedules
		SET consecutive_failures = ?, last_failure_at = ?, last_outcome = ?, last_error = ?, next_due_at = ?, updated_at = ?
		WHERE subscription = ?`, failures, stamp, outcome, errText, next, stamp, subscription)
	return err
}

// FailSubscriptionRun records a run the evaluator could not dispatch at
// all (a Keystone-side error): a transient failure with backoff.
func (db *DB) FailSubscriptionRun(runID int64, errText string, at time.Time) error {
	_, err := db.CompleteSubscriptionRun(runID, SubscriptionRunReport{Outcome: RunOutcomeError, Error: errText, At: at})
	return err
}

// AbandonExpiredSubscriptionRuns ends every running run whose lease has
// expired at `at`, counting each as a failure with backoff. The
// checkpoint is untouched: whatever that run read and did not commit is
// read again by the next run, after the backoff — neither skipped nor
// hammered.
func (db *DB) AbandonExpiredSubscriptionRuns(at time.Time) (int, error) {
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(subscriptionRunColumns + ` WHERE state = 'running'`)
	if err != nil {
		return 0, err
	}
	var expired []SubscriptionRun
	for rows.Next() {
		run, err := scanSubscriptionRun(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		if !at.Before(run.LeaseExpiresAt) {
			expired = append(expired, run)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, run := range expired {
		if _, err := tx.Exec(`UPDATE agent_subscription_runs SET state = 'abandoned', outcome = 'abandoned',
			error = 'the run did not report before its lease expired', finished_at = ? WHERE id = ?`,
			formatAt(at), run.ID); err != nil {
			return 0, err
		}
		var cadence int64
		if err := tx.QueryRow(`SELECT cadence_seconds FROM agent_subscriptions WHERE id = ?`, run.Subscription).
			Scan(&cadence); err != nil {
			return 0, err
		}
		if err := recordRunFailure(tx, run.Subscription, time.Duration(cadence)*time.Second,
			RunOutcomeAbandoned, "the run did not report before its lease expired", at); err != nil {
			return 0, err
		}
	}
	return len(expired), tx.Commit()
}

// fenceRunningSubscriptionRuns fences whatever run is in flight for a
// subscription, inside the caller's transaction.
func fenceRunningSubscriptionRuns(tx *sql.Tx, id int64, reason string) error {
	_, err := tx.Exec(`UPDATE agent_subscription_runs SET state = 'fenced', error = ?, finished_at = ?
		WHERE subscription = ? AND state = 'running'`, reason, now(), id)
	if err != nil {
		return fmt.Errorf("runtimedb: fencing subscription %d's run: %w", id, err)
	}
	return nil
}

// abandonRunsOnRestore is RestoreBackup's own step: a run recorded as
// running in a snapshot belonged to the machine that was lost. It can
// never report, and leaving it would hold the subscription for a lease
// that means nothing on the new host.
func abandonRunsOnRestore(db *DB) error {
	_, err := db.sql.Exec(`UPDATE agent_subscription_runs SET state = 'abandoned', outcome = 'abandoned',
		error = 'restored from backup while in flight', finished_at = ? WHERE state = 'running'`, now())
	return err
}

const subscriptionRunColumns = `SELECT id, subscription, subscription_version, source_connection, connection_epoch,
	person_id, coordinator_epoch, base_checkpoint_version, state, outcome, task_id, error, due_at, started_at,
	lease_expires_at, finished_at FROM agent_subscription_runs`

func scanSubscriptionRun(row rowScanner) (SubscriptionRun, error) {
	var r SubscriptionRun
	var person sql.NullString
	var due, started, lease, finished string
	err := row.Scan(&r.ID, &r.Subscription, &r.SubscriptionVersion, &r.SourceConnection, &r.ConnectionEpoch,
		&person, &r.CoordinatorEpoch, &r.BaseCheckpointVersion, &r.State, &r.Outcome, &r.TaskID, &r.Error,
		&due, &started, &lease, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return SubscriptionRun{}, ErrNotFound
	}
	if err != nil {
		return SubscriptionRun{}, fmt.Errorf("runtimedb: scanning subscription run: %w", err)
	}
	r.PersonID = person.String
	for _, f := range []struct {
		text string
		into *time.Time
	}{{due, &r.DueAt}, {started, &r.StartedAt}, {lease, &r.LeaseExpiresAt}, {finished, &r.FinishedAt}} {
		if *f.into, err = parseAt(f.text); err != nil {
			return SubscriptionRun{}, fmt.Errorf("runtimedb: bad run timestamp %q: %w", f.text, err)
		}
	}
	return r, nil
}

// GetSubscriptionRun reads one run.
func (db *DB) GetSubscriptionRun(id int64) (SubscriptionRun, error) {
	return scanSubscriptionRun(db.sql.QueryRow(subscriptionRunColumns+` WHERE id = ?`, id))
}

// ListSubscriptionRuns returns a subscription's most recent runs, newest
// first.
func (db *DB) ListSubscriptionRuns(subscription int64, limit int) ([]SubscriptionRun, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.sql.Query(subscriptionRunColumns+` WHERE subscription = ? ORDER BY id DESC LIMIT ?`,
		subscription, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SubscriptionRun
	for rows.Next() {
		r, err := scanSubscriptionRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunningSubscriptionRun returns the subscription's run in flight, if
// any.
func (db *DB) RunningSubscriptionRun(subscription int64) (SubscriptionRun, bool, error) {
	r, err := scanSubscriptionRun(db.sql.QueryRow(subscriptionRunColumns+` WHERE subscription = ? AND state = 'running'`,
		subscription))
	if errors.Is(err, ErrNotFound) {
		return SubscriptionRun{}, false, nil
	}
	return r, err == nil, err
}

func readSubscriptionCheckpoint(q personRower, subscription int64, source string) (SubscriptionCheckpoint, error) {
	cp := SubscriptionCheckpoint{Subscription: subscription, Source: source}
	var cursor, committed string
	err := q.QueryRow(`SELECT version, cursor_json, committed_at, committed_by_run FROM agent_subscription_checkpoints
		WHERE subscription = ? AND source = ?`, subscription, source).Scan(&cp.Version, &cursor, &committed, &cp.CommittedByRun)
	if errors.Is(err, sql.ErrNoRows) {
		return cp, nil
	}
	if err != nil {
		return SubscriptionCheckpoint{}, fmt.Errorf("runtimedb: reading subscription checkpoint: %w", err)
	}
	cp.Cursor = json.RawMessage(cursor)
	if cp.CommittedAt, err = parseAt(committed); err != nil {
		return SubscriptionCheckpoint{}, err
	}
	return cp, nil
}

// GetSubscriptionCheckpoint reads the committed checkpoint for one
// subscription and source. Version 0 means nothing was ever committed.
func (db *DB) GetSubscriptionCheckpoint(subscription int64, source string) (SubscriptionCheckpoint, error) {
	return readSubscriptionCheckpoint(db.sql, subscription, source)
}
