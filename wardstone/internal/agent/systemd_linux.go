//go:build linux

package agent

// SystemdUnitStatus (ADR-W003 W6d). The requested unit must be in
// Config.SystemdUnitAllowlist — this is agent-side defense in depth: even a
// compromised Keystone can only observe the allowlisted units. Talks to
// systemd over D-Bus via github.com/coreos/go-systemd/v22 (pre-approved
// exception to §A4.5), no shell-out.

import (
	"context"
	"slices"

	"github.com/coreos/go-systemd/v22/dbus"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// systemdClient is the seam over the D-Bus connection: tests substitute a
// fake returning canned property maps, since D-Bus is unavailable in CI
// containers and no test may require a real system bus.
type systemdClient interface {
	GetUnitPropertiesContext(ctx context.Context, unit string) (map[string]any, error)
	Close()
}

// systemdConn acquires a real systemd D-Bus connection. Tests replace this
// var; production code leaves it as-is.
var systemdConn = func(ctx context.Context) (systemdClient, error) {
	return dbus.NewSystemConnectionContext(ctx)
}

func (a *Agent) systemdUnitStatus(ctx context.Context, op *wsv1.SystemdUnitStatus) (*wsv1.OpResult_SystemdUnitStatus, *wsv1.OpError) {
	unit := op.GetUnit()
	if unit == "" {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "unit is required")
	}
	if !slices.Contains(a.cfg.SystemdUnitAllowlist, unit) {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"unit %q is not in the systemd unit allowlist %v", unit, a.cfg.SystemdUnitAllowlist)
	}

	conn, err := systemdConn(ctx)
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "connect to systemd: %v", err)
	}
	defer conn.Close()

	props, err := conn.GetUnitPropertiesContext(ctx, unit)
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "get unit properties %q: %v", unit, err)
	}

	activeSinceSec := uint64(0)
	if usec, ok := props["ActiveEnterTimestamp"].(uint64); ok && usec > 0 {
		activeSinceSec = usec / 1_000_000
	}

	return &wsv1.OpResult_SystemdUnitStatus{SystemdUnitStatus: &wsv1.SystemdUnitStatusResult{
		ActiveState:        stringProp(props, "ActiveState"),
		SubState:           stringProp(props, "SubState"),
		LoadState:          stringProp(props, "LoadState"),
		ActiveSinceUnixSec: activeSinceSec,
	}}, nil
}

func stringProp(props map[string]any, key string) string {
	s, _ := props[key].(string)
	return s
}
