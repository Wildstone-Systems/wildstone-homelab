package runtimedb

import "fmt"

// CheckHouseholdExecution checks this handle's acquired lease, never merely the
// lease row someone else owns. Every external household effect passes this
// store boundary; permitted reads do not. Explicit promotion invalidates the
// old process at its next boundary. Reopening the DB grants no authority.
func (db *DB) CheckHouseholdExecution() error {
	// Report the data quarantine first. It is the immediate reason every
	// Butler-facing path is closed during reconciliation. After release this
	// gate opens, but the independent coordinator check below still refuses
	// execution until post-restore promotion.
	if err := db.gate(GateDispatch); err != nil {
		return err
	}
	lease := db.executionLease.Load()
	if lease == nil {
		return fmt.Errorf("%w: this process has not acquired execution authority", ErrCoordinatorFenced)
	}
	if err := db.CheckCoordinator(lease.Holder, lease.Epoch); err != nil {
		return err
	}
	return nil
}
