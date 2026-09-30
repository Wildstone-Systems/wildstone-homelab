package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

type startupWorkspaceFixture struct {
	recoveryErr, restoreErr, stopErr error
	restore                          runtimedb.RestoreState
	stop                             runtimedb.HouseholdExecutionStop
}

func (f startupWorkspaceFixture) RecoverWorkspaceWrites() (int64, error) { return 3, f.recoveryErr }
func (f startupWorkspaceFixture) GetRestoreState() (runtimedb.RestoreState, error) {
	return f.restore, f.restoreErr
}
func (f startupWorkspaceFixture) GetHouseholdExecutionStop() (runtimedb.HouseholdExecutionStop, error) {
	return f.stop, f.stopErr
}
func TestWorkspaceStartupRecoveryRefusesUnexpectedFailure(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	disk := errors.New("injected database I/O failure")
	for _, failure := range []error{disk, runtimedb.ErrCoordinatorFenced} {
		ready, err := recoverWorkspaceAtStartup(startupWorkspaceFixture{recoveryErr: failure}, log)
		if ready || !errors.Is(err, failure) {
			t.Fatalf("startup allowed dispatch after %v: %v", failure, err)
		}
	}
	for _, gate := range []error{runtimedb.ErrQuarantined, runtimedb.ErrHouseholdStopped} {
		// Existing execution gates deliberately wrap read errors with the same
		// sentinels as an active gate. A sentinel alone is not positive evidence.
		f := startupWorkspaceFixture{recoveryErr: fmt.Errorf("%w: injected gate lookup failure", gate)}
		if ready, err := recoverWorkspaceAtStartup(f, log); ready || err == nil {
			t.Fatalf("unconfirmed %v allowed dispatch", gate)
		}
		f.restore.Quarantined = true
		f.restoreErr = disk
		if ready, err := recoverWorkspaceAtStartup(f, log); ready || !errors.Is(err, disk) {
			t.Fatalf("failed restore read ignored: %v", err)
		}
		f.restoreErr = nil
		f.stopErr = disk
		if ready, err := recoverWorkspaceAtStartup(f, log); ready || !errors.Is(err, disk) {
			t.Fatalf("failed stop read ignored: %v", err)
		}
	}
	// Confirmed gates retain the administrative surface, but never enable
	// coordination in this process even if the Owner later releases the gate.
	for _, f := range []startupWorkspaceFixture{
		{recoveryErr: runtimedb.ErrQuarantined, restore: runtimedb.RestoreState{Quarantined: true}},
		{recoveryErr: runtimedb.ErrHouseholdStopped, stop: runtimedb.HouseholdExecutionStop{Active: true}},
	} {
		if ready, err := recoverWorkspaceAtStartup(f, log); ready || err != nil {
			t.Fatalf("confirmed gate must retain API without coordination: ready=%v err=%v", ready, err)
		}
	}
	if ready, err := recoverWorkspaceAtStartup(startupWorkspaceFixture{}, log); !ready || err != nil {
		t.Fatalf("successful recovery must enable coordination: ready=%v err=%v", ready, err)
	}
}
