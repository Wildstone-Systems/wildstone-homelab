package runtimedb

// The single active coordinator lease (ADR-W030 A11, P3).
//
// Wildstone runs one Keystone. That is a decision, not a limitation
// waiting to be lifted: a household with two schedulers racing over one
// task ledger has failures nobody can reproduce, and the recovery story
// in ADR-W018 assumes one authoritative runtime database.
//
// So the lease here is deliberately unclever. There is no election, no
// timeout-based takeover, and no automatic failback. A second process
// that finds another holder refuses to dispatch and says so. Taking over
// is an explicit administrative act — PromoteCoordinator — which fences
// the previous holder by bumping the epoch that every in-flight dispatch
// carries.

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrCoordinatorHeld is returned when another process holds the lease.
// It is not a retryable condition: the caller must stop, not wait.
var ErrCoordinatorHeld = errors.New("runtimedb: another coordinator holds the lease")

// ErrCoordinatorFenced is returned when the caller's epoch has been
// superseded by an explicit promotion. Work in flight must stop at its
// next checkpoint rather than finish into state it no longer owns.
var ErrCoordinatorFenced = errors.New("runtimedb: coordinator fenced by a newer lease epoch")

// ErrCoordinatorRestoreFenced is returned while a database has a restore
// generation. Quarantine release removes the data-access hold only; it never
// restores execution authority. #286 owns the explicit post-restore promotion
// record that will permit a later restart to acquire the promoted epoch.
var ErrCoordinatorRestoreFenced = fmt.Errorf("%w: restored database pending explicit coordinator promotion", ErrCoordinatorFenced)

// CoordinatorLease is the current holder of the runtime database.
type CoordinatorLease struct {
	Holder       string    `json:"holder"`
	Epoch        int64     `json:"epoch"`
	AcquiredAt   time.Time `json:"acquiredAt"`
	HeartbeatAt  time.Time `json:"heartbeatAt"`
	PromotedFrom string    `json:"promotedFrom,omitempty"`
}

// RestoredCoordinatorPromotion is the immutable authorization decision that
// advanced a restored database's preserved lease. It records both sides of
// the fence; the returned lease alone cannot explain who authorized recovery
// or which restore evidence it relied on.
type RestoredCoordinatorPromotion struct {
	RestoreGeneration string           `json:"restoreGeneration"`
	EvidenceID        string           `json:"evidenceId"`
	Previous          CoordinatorLease `json:"previous"`
	Promoted          CoordinatorLease `json:"promoted"`
	PromotedAt        time.Time        `json:"promotedAt"`
	PromotedBy        string           `json:"promotedBy"`
}

// AcquireCoordinator claims the lease for holder and returns it.
//
// Re-acquiring your own lease is a normal restart and keeps the epoch:
// the same coordinator coming back up owns the same work. Finding a
// DIFFERENT holder returns ErrCoordinatorHeld and changes nothing —
// this call never steals a lease, because a caller that could steal one
// on startup is an automatic leader election wearing a different name.
func (db *DB) AcquireCoordinator(holder string) (CoordinatorLease, error) {
	db.coordinatorMu.Lock()
	defer db.coordinatorMu.Unlock()
	if holder == "" {
		return CoordinatorLease{}, fmt.Errorf("runtimedb: coordinator lease requires a holder id")
	}
	lock := db.coordinatorLock
	if lock == nil || db.coordinatorLockHolder != holder {
		var err error
		lock, err = db.acquireCoordinatorProcessLock(holder)
		if err != nil {
			return CoordinatorLease{}, err
		}
	}
	committed := false
	defer func() {
		if !committed && lock != nil && lock != db.coordinatorLock {
			_ = lock.Close()
		}
	}()
	tx, err := db.sql.Begin()
	if err != nil {
		return CoordinatorLease{}, fmt.Errorf("runtimedb: acquiring coordinator lease: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	restore, err := getRestoreState(tx)
	if err != nil {
		return CoordinatorLease{}, fmt.Errorf("runtimedb: checking restore fence before coordinator acquisition: %w", err)
	}
	if restore.RestoreGeneration != "" && (!db.restoreCoordinatorAcquisitionAllowed.Load() || restore.CoordinatorPromotedAt.IsZero() ||
		restore.CoordinatorHolder != holder || restore.CoordinatorEpoch <= 0) {
		return CoordinatorLease{}, ErrCoordinatorRestoreFenced
	}

	stamp := now()
	var cur string
	var epoch int64
	err = tx.QueryRow(`SELECT holder, epoch FROM coordinator_lease WHERE id = 1`).Scan(&cur, &epoch)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if restore.RestoreGeneration != "" {
			return CoordinatorLease{}, ErrCoordinatorRestoreFenced
		}
		if _, err := tx.Exec(
			`INSERT INTO coordinator_lease (id, holder, epoch, acquired_at, heartbeat_at)
			 VALUES (1, ?, 1, ?, ?)`, holder, stamp, stamp); err != nil {
			return CoordinatorLease{}, fmt.Errorf("runtimedb: acquiring coordinator lease: %w", err)
		}
	case err != nil:
		return CoordinatorLease{}, fmt.Errorf("runtimedb: acquiring coordinator lease: %w", err)
	case cur != holder:
		return CoordinatorLease{}, fmt.Errorf("%w: held by %q", ErrCoordinatorHeld, cur)
	default:
		if restore.RestoreGeneration != "" && epoch != restore.CoordinatorEpoch {
			return CoordinatorLease{}, ErrCoordinatorRestoreFenced
		}
		if _, err := tx.Exec(
			`UPDATE coordinator_lease SET acquired_at = ?, heartbeat_at = ? WHERE id = 1`,
			stamp, stamp); err != nil {
			return CoordinatorLease{}, fmt.Errorf("runtimedb: acquiring coordinator lease: %w", err)
		}
	}
	lease, err := readCoordinator(tx)
	if err != nil {
		return CoordinatorLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return CoordinatorLease{}, fmt.Errorf("runtimedb: acquiring coordinator lease: %w", err)
	}
	if db.coordinatorLock != nil && db.coordinatorLock != lock {
		_ = db.coordinatorLock.Close()
	}
	db.coordinatorLock, db.coordinatorLockHolder = lock, holder
	committed = true
	db.executionLease.Store(&lease)
	return lease, nil
}

// RecoverCoordinatorStartup reconciles every durable execution checkpoint
// that must be settled before a newly acquired coordinator is allowed to wire
// task, delivery, or reviewed-routine dispatch.
//
// Acquisition and recovery are deliberately separate operations. The caller
// must first acquire the exact durable holder and epoch (including the epoch
// authorized by a restored-coordinator promotion), then call this method
// before exposing any execution surface. Each recovery operation is
// idempotent, so a crash during this sequence is safe to retry. Any error must
// fail startup; possession of the lease alone is not execution readiness.
func (db *DB) RecoverCoordinatorStartup() error {
	if err := db.RecoverInterruptedButlerAttempts(); err != nil {
		db.executionLease.Store(nil)
		return fmt.Errorf("recovering interrupted Agent work: %w", err)
	}
	if err := db.RecoverClaimedRoutineEffects(); err != nil {
		db.executionLease.Store(nil)
		return fmt.Errorf("recovering interrupted reviewed routine effects: %w", err)
	}
	return nil
}

// PromoteCoordinator is the explicit, fenced takeover an operator runs
// after establishing that the previous Keystone is gone (ADR-W030 A11).
//
// It bumps the epoch, which is what stops the old holder: every dispatch
// carries the epoch it was claimed under, and CheckCoordinator refuses
// one whose epoch has been superseded. Promoting to the current holder
// is refused rather than treated as a no-op, because a promotion is an
// assertion that somebody else has stopped, and asserting that about
// yourself means the caller has confused two situations.
func (db *DB) PromoteCoordinator(holder string) (CoordinatorLease, error) {
	if holder == "" {
		return CoordinatorLease{}, fmt.Errorf("runtimedb: coordinator promotion requires a holder id")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return CoordinatorLease{}, fmt.Errorf("runtimedb: promoting coordinator: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, lease, err := promoteCoordinatorInTx(tx, holder, now())
	if err != nil {
		return CoordinatorLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return CoordinatorLease{}, fmt.Errorf("runtimedb: promoting coordinator: %w", err)
	}
	return lease, nil
}

func promoteCoordinatorInTx(tx *sql.Tx, holder, stamp string) (CoordinatorLease, CoordinatorLease, error) {
	previous, err := readCoordinator(tx)
	if errors.Is(err, sql.ErrNoRows) {
		return CoordinatorLease{}, CoordinatorLease{}, fmt.Errorf("runtimedb: no coordinator lease exists to promote from")
	}
	if err != nil {
		return CoordinatorLease{}, CoordinatorLease{}, fmt.Errorf("runtimedb: promoting coordinator: %w", err)
	}
	if previous.Holder == holder {
		return CoordinatorLease{}, CoordinatorLease{}, fmt.Errorf("runtimedb: %q already holds the coordinator lease", holder)
	}
	update, err := tx.Exec(
		`UPDATE coordinator_lease SET holder = ?, epoch = ?, acquired_at = ?, heartbeat_at = ?, promoted_from = ?
		 WHERE id = 1 AND holder = ? AND epoch = ?`, holder, previous.Epoch+1, stamp, stamp, previous.Holder,
		previous.Holder, previous.Epoch)
	if err != nil {
		return CoordinatorLease{}, CoordinatorLease{}, fmt.Errorf("runtimedb: promoting coordinator: %w", err)
	}
	if n, rowsErr := update.RowsAffected(); rowsErr != nil || n != 1 {
		return CoordinatorLease{}, CoordinatorLease{}, fmt.Errorf("runtimedb: coordinator lease changed before promotion")
	}
	lease, err := readCoordinator(tx)
	if err != nil {
		return CoordinatorLease{}, CoordinatorLease{}, err
	}
	return previous, lease, nil
}

// PromoteRestoredCoordinator is the one transition that may advance a lease
// carried by a restored database. It revalidates the exact signed recovery
// proof, released quarantine, and independent source fence in the same write
// transaction that records the Owner decision and increments the epoch.
//
// It deliberately does not populate executionLease. The process that served
// this request remains fenced; a controlled restart must use
// AcquireCoordinator to pick up the already-authorized epoch.
func (db *DB) PromoteRestoredCoordinator(holder, by string, trustedKey ed25519.PublicKey) (RestoredCoordinatorPromotion, error) {
	var result RestoredCoordinatorPromotion
	if holder == "" {
		return result, fmt.Errorf("runtimedb: coordinator promotion requires a holder id")
	}
	if by == "" {
		return result, fmt.Errorf("runtimedb: restored coordinator promotion requires the current Owner")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return result, fmt.Errorf("runtimedb: beginning restored coordinator promotion: %w", err)
	}
	defer tx.Rollback()

	validated, err := validateRestoreEvidence(tx, trustedKey, "restored coordinator promotion")
	if err != nil {
		return result, err
	}
	state := validated.state
	if state.Quarantined || state.ReleasedAt.IsZero() || state.ReleasedBy == "" {
		return result, fmt.Errorf("runtimedb: restored coordinator promotion requires explicit quarantine release")
	}
	if !state.CoordinatorPromotedAt.IsZero() || state.CoordinatorPromotedBy != "" ||
		state.CoordinatorHolder != "" || state.CoordinatorEpoch != 0 {
		return result, fmt.Errorf("runtimedb: restore generation already has a coordinator promotion")
	}
	previous, err := readCoordinator(tx)
	if errors.Is(err, sql.ErrNoRows) {
		return result, fmt.Errorf("runtimedb: no coordinator lease exists to promote from")
	}
	if err != nil {
		return result, err
	}
	if previous.Holder == holder {
		return result, fmt.Errorf("runtimedb: %q already holds the coordinator lease", holder)
	}
	stamp := now()
	if _, err := tx.Exec(`INSERT INTO recovery_coordinator_promotions
		(restore_generation,journal_evidence_id,previous_holder,previous_epoch,promoted_holder,promoted_epoch,promoted_at,promoted_by)
		VALUES(?,?,?,?,?,?,?,?)`, state.RestoreGeneration, validated.evidenceID, previous.Holder, previous.Epoch,
		holder, previous.Epoch+1, stamp, by); err != nil {
		return result, fmt.Errorf("runtimedb: recording restored coordinator promotion: %w", err)
	}
	previous, promoted, err := promoteCoordinatorInTx(tx, holder, stamp)
	if err != nil {
		return result, err
	}
	update, err := tx.Exec(`UPDATE restore_state SET coordinator_promoted_at=?,coordinator_promoted_by=?,
		coordinator_promoted_holder=?,coordinator_promoted_epoch=?
		WHERE id=1 AND restore_generation=? AND journal_evidence_id=? AND coordinator_promoted_at=''`,
		stamp, by, promoted.Holder, promoted.Epoch, state.RestoreGeneration, validated.evidenceID)
	if err != nil {
		return result, fmt.Errorf("runtimedb: recording restored coordinator projection: %w", err)
	}
	if n, rowsErr := update.RowsAffected(); rowsErr != nil || n != 1 {
		return result, fmt.Errorf("runtimedb: current restore changed before coordinator promotion")
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("runtimedb: committing restored coordinator promotion: %w", err)
	}
	promotedAt, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return result, err
	}
	return RestoredCoordinatorPromotion{
		RestoreGeneration: state.RestoreGeneration, EvidenceID: validated.evidenceID,
		Previous: previous, Promoted: promoted, PromotedAt: promotedAt, PromotedBy: by,
	}, nil
}

// CheckCoordinator is the pre-dispatch fence. A coordinator calls it
// with the holder and epoch it acquired; anything else means it has been
// superseded and must stop.
func (db *DB) CheckCoordinator(holder string, epoch int64) error {
	var curHolder string
	var curEpoch int64
	err := db.sql.QueryRow(`SELECT holder, epoch FROM coordinator_lease WHERE id = 1`).
		Scan(&curHolder, &curEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no lease is held", ErrCoordinatorFenced)
	}
	if err != nil {
		return fmt.Errorf("runtimedb: checking coordinator lease: %w", err)
	}
	if curHolder != holder || curEpoch != epoch {
		return fmt.Errorf("%w: lease is now %q at epoch %d, caller holds epoch %d",
			ErrCoordinatorFenced, curHolder, curEpoch, epoch)
	}
	return nil
}

// HeartbeatCoordinator records that the holder is still alive. It is
// evidence for a human deciding whether to promote, and nothing reads it
// to make that decision automatically.
func (db *DB) HeartbeatCoordinator(holder string, epoch int64) error {
	result, err := db.sql.Exec("UPDATE coordinator_lease SET heartbeat_at=? WHERE id=1 AND holder=? AND epoch=?", now(), holder, epoch)
	if err != nil {
		return fmt.Errorf("runtimedb: coordinator heartbeat: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrCoordinatorFenced
	}
	return nil
}

// GetCoordinator returns the current lease, or ErrNotFound if none has
// ever been taken.
func (db *DB) GetCoordinator() (CoordinatorLease, error) {
	lease, err := readCoordinator(db.sql)
	if errors.Is(err, sql.ErrNoRows) {
		return CoordinatorLease{}, ErrNotFound
	}
	return lease, err
}

type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func readCoordinator(q rowQuerier) (CoordinatorLease, error) {
	var l CoordinatorLease
	var acquired, heartbeat string
	err := q.QueryRow(
		`SELECT holder, epoch, acquired_at, heartbeat_at, promoted_from FROM coordinator_lease WHERE id = 1`,
	).Scan(&l.Holder, &l.Epoch, &acquired, &heartbeat, &l.PromotedFrom)
	if err != nil {
		return CoordinatorLease{}, err
	}
	if l.AcquiredAt, err = time.Parse(time.RFC3339Nano, acquired); err != nil {
		return CoordinatorLease{}, fmt.Errorf("runtimedb: bad coordinator acquired_at %q: %w", acquired, err)
	}
	if l.HeartbeatAt, err = time.Parse(time.RFC3339Nano, heartbeat); err != nil {
		return CoordinatorLease{}, fmt.Errorf("runtimedb: bad coordinator heartbeat_at %q: %w", heartbeat, err)
	}
	return l, nil
}

// RunCoordinatorHeartbeat records liveness without refreshing this process's
// epoch or taking over automatically. Promotion stops the old writer.
func (db *DB) RunCoordinatorHeartbeat(ctx context.Context, interval time.Duration) error {
	lease := db.executionLease.Load()
	if lease == nil {
		return ErrCoordinatorFenced
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Transient storage failures do not revoke the lease. Retry at the
			// next tick; dispatch still checks the live database independently.
			if err := db.HeartbeatCoordinator(lease.Holder, lease.Epoch); errors.Is(err, ErrCoordinatorFenced) {
				return err
			}
		}
	}
}
