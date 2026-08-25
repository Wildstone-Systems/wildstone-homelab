package api

// Backup endpoints (ADR-W015 A2): the ledger read plus on-demand run and
// restore-alongside. Run/restore carry the approve verb — they are
// human-initiated EXECUTIONS, so the Butler's propose-only role cannot
// trigger them; scheduled runs remain the engine's standing duty
// (ADR-W011 A4). The engine is reached through function hooks
// cmd/keystone wires (the MintJoin pattern), keeping this package free
// of a backup-engine dependency.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func (s *Server) listBackups(r *http.Request, _ principal.Identity) (any, int, error) {
	runs, err := s.DB.ListBackupRuns(100)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if runs == nil {
		runs = []runtimedb.BackupRun{}
	}
	return map[string]any{"runs": runs}, http.StatusOK, nil
}

func (s *Server) runBackup(r *http.Request, id principal.Identity) (any, int, error) {
	return s.backupExec(r, id, s.BackupRun, "backup run")
}

func (s *Server) restoreBackup(r *http.Request, id principal.Identity) (any, int, error) {
	return s.backupExec(r, id, s.BackupRestore, "restore")
}

func (s *Server) backupExec(r *http.Request, id principal.Identity, hook func(context.Context, string) error, what string) (any, int, error) {
	if hook == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("backup engine not wired")
	}
	plan := r.PathValue("plan")
	if plan == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("plan name required")
	}
	// The engine records its own full provenance (plan record, ledger,
	// events); the audit middleware already records who called this
	// endpoint. Errors surface with the engine's detail — a run that
	// fails mid-way still left its failed ledger row.
	if err := hook(r.Context(), plan); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("%s %s: %w", what, plan, err)
	}
	_ = id
	return map[string]string{"plan": plan, "result": what + " completed"}, http.StatusOK, nil
}
