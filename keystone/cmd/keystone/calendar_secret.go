package main

import (
	"context"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentgateway"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/domestic"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/operation"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// calendarSecretUse is the actual domestic call-site boundary. An unattended
// call must carry a reviewed routine principal; a Person binding is never
// consulted for it, even if the same Agent also has interactive access.
//
// The attended (Person) case resolves its own secret-use context from the
// call's own operation.Identity (ADR-W039 §3.1 principle 4, Phase 4, C11):
// TaskID names the task a runtime — native or external — is actually
// running, and agentgateway.MemoryReadContextForTask reads that task's own
// admission back rather than trusting a context value whoever built the
// session happened to stuff in. A call with no task identity at all (a
// direct, non-task-bound dispatch, if one ever reaches here) is refused
// rather than guessed.
func calendarSecretUse(db *runtimedb.DB, vault *runtimedb.SecretVault) func(context.Context, domestic.Call, func(string) error) (bool, error) {
	return func(ctx context.Context, c domestic.Call, consume func(string) error) (bool, error) {
		if routine := runtimedb.RoutineSecretUseContext(ctx); routine != nil {
			id, err := vault.CalendarBindingRoutine(routine)
			if err != nil {
				return true, err
			}
			if id == "" {
				return true, runtimedb.ErrSecretDenied
			}
			if c.Tool != "calendar.get_agenda" || c.Attended {
				return true, runtimedb.ErrSecretDenied
			}
			return true, vault.UseRoutine(id, routine, c.Tool, func(raw []byte) error { return consume(string(raw)) })
		}
		taskID := operation.From(ctx).TaskID
		if taskID == "" {
			return true, runtimedb.ErrSecretDenied
		}
		memCtx, err := agentgateway.MemoryReadContextForTask(db, taskID)
		if err != nil {
			return true, err
		}
		id, err := vault.CalendarBinding(memCtx)
		if err != nil {
			return true, err
		}
		if id == "" {
			return false, nil
		}
		if c.Tool != "calendar.get_agenda" || !c.Attended {
			return true, runtimedb.ErrSecretDenied
		}
		return true, vault.Use(id, memCtx, c.Tool, func(raw []byte) error { return consume(string(raw)) })
	}
}
