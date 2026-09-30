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
		err := recoverWorkspaceAtStartup(startupWorkspaceFixture{recoveryErr: failure}, log)
		if !errors.Is(err, failure) {
			t.Fatalf("startup allowed dispatch after %v: %v", failure, err)
		}
	}
	for _, gate := range []error{runtimedb.ErrQuarantined, runtimedb.ErrHouseholdStopped} {
		// Existing execution gates deliberately wrap read errors with the same
		// sentinels as an active gate. A sentinel alone is not positive evidence.
		f := startupWorkspaceFixture{recoveryErr: fmt.Errorf("%w: injected gate lookup failure", gate)}
		if err := recoverWorkspaceAtStartup(f, log); err == nil {
			t.Fatalf("unconfirmed %v allowed dispatch", gate)
		}
		f.restore.Quarantined = true
		f.restoreErr = disk
		if err := recoverWorkspaceAtStartup(f, log); !errors.Is(err, disk) {
			t.Fatalf("failed restore read ignored: %v", err)
		}
		f.restoreErr = nil
		f.stopErr = disk
		if err := recoverWorkspaceAtStartup(f, log); !errors.Is(err, disk) {
			t.Fatalf("failed stop read ignored: %v", err)
		}
	}
	// Confirmed gate states retain the administrative recovery surface.
	for _, f := range []startupWorkspaceFixture{
		{recoveryErr: runtimedb.ErrQuarantined, restore: runtimedb.RestoreState{Quarantined: true}},
		{recoveryErr: runtimedb.ErrHouseholdStopped, stop: runtimedb.HouseholdExecutionStop{Active: true}},
	} {
		if err := recoverWorkspaceAtStartup(f, log); err != nil {
			t.Fatalf("confirmed gate lost recovery API: %v", err)
		}
	}
	if err := recoverWorkspaceAtStartup(startupWorkspaceFixture{}, log); err != nil {
		t.Fatal(err)
	}
}
