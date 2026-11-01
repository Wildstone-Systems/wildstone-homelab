package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

var mountAccessTimeout = 5 * time.Second

// preflightMountAccess checks selected operations in the read-write mounts of this
// already approved EnsureContainer. Source resolution remains inside
// Wardstone; failures return the container-local target and a closed reason,
// never a backing path, directory entry, file content, or raw syscall error.
func (a *Agent) preflightMountAccess(ctx context.Context, op *wsv1.EnsureContainer) *wsv1.OpError {
	ctx, cancel := context.WithTimeout(ctx, mountAccessTimeout)
	defer cancel()
	id := op.GetRuntimeIdentity()
	// Numeric host IDs cannot model Docker user-namespace remapping or a
	// rootless daemon. Refuse unknown runtimes rather than guess the mapping.
	if firstWritableMountTarget(op) != "" {
		supported, err := a.runtime.mountAccessIdentitySupported(ctx)
		if err != nil || !supported {
			reason := wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSUPPORTED_BACKEND
			if ctx.Err() != nil {
				reason = wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_TIMEOUT
			}
			return mountAccessError(firstWritableMountTarget(op), reason)
		}
	}
	for _, mount := range op.GetMounts() {
		var root string
		var oerr *wsv1.OpError
		if strings.Contains(mount.GetSource(), "/") {
			root = filepath.Clean(mount.GetSource())
			if !hostSystemMountAllowed(root) {
				reason := runMountAccessAdmission(ctx, root, a.cfg)
				if reason != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSPECIFIED {
					return mountAccessError(mount.GetTarget(), reason)
				}
			}
		}
		if mount.GetReadOnly() {
			continue
		}
		if root == "" {
			root, oerr = a.volumeMountpoint(ctx, mount.GetSource())
		}

		if oerr != nil {
			reason := wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNAVAILABLE
			if ctx.Err() != nil {
				reason = wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_TIMEOUT
			}
			return mountAccessError(mount.GetTarget(), reason)
		}

		reason := runMountAccessCheck(ctx, root, uint32(id.GetUid()), uint32(id.GetGid()))
		if reason != wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_UNSPECIFIED {
			return mountAccessError(mount.GetTarget(), reason)
		}
	}
	if ctx.Err() != nil {
		return mountAccessError(firstWritableMountTarget(op), wsv1.MountAccessFailureReason_MOUNT_ACCESS_FAILURE_REASON_TIMEOUT)
	}
	return nil
}

func firstWritableMountTarget(op *wsv1.EnsureContainer) string {
	for _, mount := range op.GetMounts() {
		if !mount.GetReadOnly() {
			return mount.GetTarget()
		}
	}
	return ""
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
