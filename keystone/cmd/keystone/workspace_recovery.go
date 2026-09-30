package main

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// Startup must settle its conservative rollback before wiring dispatch. The
// explicit operational gates are different: administrators need the API to
// reconcile restore state or release a stop, and those gates still refuse work.
func recoverWorkspaceAtStartup(recover func() (int64, error), log *slog.Logger) error {
	n, err := recover()
	if err != nil {
		if errors.Is(err, runtimedb.ErrQuarantined) || errors.Is(err, runtimedb.ErrHouseholdStopped) {
			log.Warn("workspace publication recovery deferred by execution gate")
			return nil
		}
		return fmt.Errorf("workspace publication recovery failed before dispatch: %w", err)
	}
	if n > 0 {
		log.Info("unsettled workspace publications rolled back; recovery bytes retained", "count", n)
	}
	return nil
}
