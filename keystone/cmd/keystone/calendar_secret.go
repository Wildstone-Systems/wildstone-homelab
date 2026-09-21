package main

import (
	"context"

	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// calendarSecretUse is the actual domestic call-site boundary. An unattended
// call must carry a reviewed routine principal; a Person binding is never
// consulted for it, even if the same Agent also has interactive access.
func calendarSecretUse(vault *runtimedb.SecretVault) func(context.Context, domestic.Call, func(string) error) (bool, error) {
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
		id, err := vault.CalendarBinding(runtimedb.SecretUseContext(ctx))
		if err != nil {
			return true, err
		}
		if id == "" {
			return false, nil
		}
		if c.Tool != "calendar.get_agenda" || !c.Attended {
			return true, runtimedb.ErrSecretDenied
		}
		return true, vault.Use(id, runtimedb.SecretUseContext(ctx), c.Tool, func(raw []byte) error { return consume(string(raw)) })
	}
}
