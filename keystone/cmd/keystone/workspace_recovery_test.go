package main

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestWorkspaceStartupRecoveryRefusesUnexpectedFailure(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	disk := errors.New("injected database I/O failure")
	for _, failure := range []error{disk, runtimedb.ErrCoordinatorFenced} {
		err := recoverWorkspaceAtStartup(func() (int64, error) { return 0, failure }, log)
		if !errors.Is(err, failure) {
			t.Fatalf("startup allowed dispatch after %v: %v", failure, err)
		}
	}
	// Expected gate states retain the administrative recovery surface rather
	// than hiding the very API needed to reconcile or release the gate.
	for _, gate := range []error{runtimedb.ErrQuarantined, runtimedb.ErrHouseholdStopped} {
		if err := recoverWorkspaceAtStartup(func() (int64, error) { return 0, gate }, log); err != nil {
			t.Fatalf("recovery API unavailable for %v: %v", gate, err)
		}
	}
	if err := recoverWorkspaceAtStartup(func() (int64, error) { return 3, nil }, log); err != nil {
		t.Fatal(err)
	}
}
