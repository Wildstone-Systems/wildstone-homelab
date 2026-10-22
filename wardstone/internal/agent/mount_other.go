//go:build !linux

package agent

import wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"

// Wardstone targets Linux; EnsureMount performs a real syscall.Mount which
// has no portable equivalent, so non-Linux builds refuse outright rather
// than attempting something platform-specific and untested.
func (a *Agent) ensureMount(op *wsv1.EnsureMount) (*wsv1.OpResult_EnsureMount, *wsv1.OpError) {
	return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "unsupported platform")
}
