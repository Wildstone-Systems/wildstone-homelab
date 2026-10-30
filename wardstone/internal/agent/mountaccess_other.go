//go:build !linux

package agent

import (
	"context"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

func runMountAccessCheck(context.Context, string, uint32, uint32) wsv1.MountAccessFailureReason {
	return wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSUPPORTED_BACKEND
}
