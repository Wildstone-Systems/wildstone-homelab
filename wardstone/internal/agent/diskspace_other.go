//go:build !linux

package agent

import (
	"context"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// Wardstone targets Linux; statfs and mountinfo have no portable form.
func (a *Agent) diskSpace(ctx context.Context, op *wsv1.DiskSpace) (*wsv1.OpResult_DiskSpace, *wsv1.OpError) {
	return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "unsupported platform")
}
