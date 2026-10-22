//go:build !linux

package agent

import wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"

// Wardstone targets Linux; adjtimex has no portable equivalent, so
// non-Linux builds refuse outright (matches ensureMount's _other.go
// pattern, ADR-W003 W6c/W6d).
func (a *Agent) timeSyncStatus(_ *wsv1.TimeSyncStatus) (*wsv1.OpResult_TimeSyncStatus, *wsv1.OpError) {
	return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "unsupported platform")
}
