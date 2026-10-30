package agent

import (
	"context"
	"fmt"
	"strings"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// preflightMountAccess proves only the read-write mounts carried by this
// already approved EnsureContainer. Source resolution remains inside
// Wardstone; failures return the container-local target and a closed reason,
// never a backing path, directory entry, file content, or raw syscall error.
func (a *Agent) preflightMountAccess(ctx context.Context, op *wsv1.EnsureContainer) *wsv1.OpError {
	id := op.GetRuntimeIdentity()
	for _, mount := range op.GetMounts() {
		if mount.GetReadOnly() {
			continue
		}

		var root string
		var oerr *wsv1.OpError
		if strings.Contains(mount.GetSource(), "/") {
			root, oerr = a.resolveManaged(mount.GetSource())
		} else {
			root, oerr = a.volumeMountpoint(ctx, mount.GetSource())
		}
		if oerr != nil {
			return mountAccessError(mount.GetTarget(), wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE)
		}

		reason := runMountAccessCheck(ctx, root, uint32(id.GetUid()), uint32(id.GetGid()))
		if reason != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSPECIFIED {
			return mountAccessError(mount.GetTarget(), reason)
		}
	}
	return nil
}

func mountAccessError(target string, reason wsv1.MountAccessFailureReason) *wsv1.OpError {
	return &wsv1.OpError{
		Code:    wsv1.ErrorCode_ERROR_CODE_RUNTIME,
		Message: fmt.Sprintf("mount target %q failed effective-identity access preflight: %s", target, mountAccessReasonName(reason)),
		MountAccessFailure: &wsv1.MountAccessFailure{
			Target: target,
			Reason: reason,
		},
	}
}

func mountAccessReasonName(reason wsv1.MountAccessFailureReason) string {
	switch reason {
	case wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE:
		return "unavailable"
	case wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_IDENTITY_DENIED:
		return "identity denied"
	case wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_READ_ONLY:
		return "read-only"
	case wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_TIMEOUT:
		return "timeout"
	case wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_CLEANUP_INCOMPLETE:
		return "cleanup incomplete"
	case wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSUPPORTED_BACKEND:
		return "unsupported backend"
	default:
		return "unspecified"
	}
}
