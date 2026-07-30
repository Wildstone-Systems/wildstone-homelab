//go:build !linux

package agent

import (
	"context"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// Wardstone targets Linux; systemd D-Bus has no portable equivalent, so
// non-Linux builds refuse outright (matches ensureMount's _other.go
// pattern, ADR-W003 W6c/W6d).
func (a *Agent) systemdUnitStatus(_ context.Context, _ *wsv1.SystemdUnitStatus) (*wsv1.OpResult_SystemdUnitStatus, *wsv1.OpError) {
	return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "unsupported platform")
}
