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

// CoordinatorLease is the current holder of the runtime database.
type CoordinatorLease struct {
	Holder       string    `json:"holder"`
	Epoch        int64     `json:"epoch"`
	AcquiredAt   time.Time `json:"acquiredAt"`
	HeartbeatAt  time.Time `json:"heartbeatAt"`
	PromotedFrom string    `json:"promotedFrom,omitempty"`
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

	stamp := now()
	var cur string
	err = tx.QueryRow(`SELECT holder FROM coordinator_lease WHERE id = 1`).Scan(&cur)
	switch {
	case errors.Is(err, sql.ErrNoRows):
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

	var cur string
	var epoch int64
	err = tx.QueryRow(`SELECT holder, epoch FROM coordinator_lease WHERE id = 1`).Scan(&cur, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return CoordinatorLease{}, fmt.Errorf("runtimedb: no coordinator lease exists to promote from")
	}
	if err != nil {
		return CoordinatorLease{}, fmt.Errorf("runtimedb: promoting coordinator: %w", err)
	}
	if cur == holder {
		return CoordinatorLease{}, fmt.Errorf("runtimedb: %q already holds the coordinator lease", holder)
	}
	stamp := now()
	if _, err := tx.Exec(
		`UPDATE coordinator_lease SET holder = ?, epoch = ?, acquired_at = ?, heartbeat_at = ?, promoted_from = ?
		 WHERE id = 1`, holder, epoch+1, stamp, stamp, cur); err != nil {
		return CoordinatorLease{}, fmt.Errorf("runtimedb: promoting coordinator: %w", err)
	}
	lease, err := readCoordinator(tx)
	if err != nil {
		return CoordinatorLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return CoordinatorLease{}, fmt.Errorf("runtimedb: promoting coordinator: %w", err)
	}
	return lease, nil
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
