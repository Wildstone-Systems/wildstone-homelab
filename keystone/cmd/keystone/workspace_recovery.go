package main

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// Startup must settle its conservative rollback before wiring dispatch. The
// explicit operational gates are different: administrators need the API to
// reconcile restore state or release a stop. Deferred recovery disables durable
// coordination for this process lifetime; gate release requires a restart.
type workspaceStartupStore interface {
	RecoverWorkspaceWrites() (int64, error)
	GetRestoreState() (runtimedb.RestoreState, error)
	GetHouseholdExecutionStop() (runtimedb.HouseholdExecutionStop, error)
}

func recoverWorkspaceAtStartup(db workspaceStartupStore, log *slog.Logger) (bool, error) {
	n, err := db.RecoverWorkspaceWrites()
	if err != nil {
		if errors.Is(err, runtimedb.ErrQuarantined) || errors.Is(err, runtimedb.ErrHouseholdStopped) {
			// The dispatch gate uses these sentinels for both an active gate and
			// failure to read it. Only positive, fresh state evidence may preserve
			// the administrative surface; unreadable gate state fails startup.
			restore, readErr := db.GetRestoreState()
			if readErr != nil {
				return false, fmt.Errorf("reading workspace recovery restore gate: %w", readErr)
			}
			stop, readErr := db.GetHouseholdExecutionStop()
			if readErr != nil {
				return false, fmt.Errorf("reading workspace recovery execution gate: %w", readErr)
			}
			if restore.Quarantined || stop.Active {
				log.Warn("workspace publication recovery deferred; durable coordination disabled until restart after execution gate release")
				return false, nil
			}
		}
		return false, fmt.Errorf("workspace publication recovery failed before dispatch: %w", err)
	}
	if n > 0 {
		log.Info("unsettled workspace publications rolled back; recovery bytes retained", "count", n)
	}
	return true, nil
}
