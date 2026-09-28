package agent

import (
	"context"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// diskSpaceTarget resolves the one of path and volume that is set to a
// host path the agent may read sizes of: a managed path, or the backing
// directory of a wildstone-owned volume. Shared by DiskSpace and
// MeasureUsage's volume arm (protocol minor 19).
func (a *Agent) diskSpaceTarget(ctx context.Context, path, volume string) (string, *wsv1.OpError) {
	switch {
	case path != "" && volume != "":
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "set path or volume, not both")
	case path != "":
		return a.resolveManaged(path)
	case volume != "":
		return a.volumeMountpoint(ctx, volume)
	default:
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "path or volume required")
	}
}

// volumeMountpoint returns the backing directory of a wildstone-owned
// volume. Only ever one Wildstone created: sizing is read-only, but the
// ownership rule is the same one every volume op keeps.
func (a *Agent) volumeMountpoint(ctx context.Context, volume string) (string, *wsv1.OpError) {
	vol, err := a.docker.volumeInspect(ctx, volume)
	if err != nil {
		if derr, ok := err.(*dockerError); ok && derr.notFound() {
			return "", opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "volume %q does not exist", volume)
		}
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "volume inspect: %v", err)
	}
	if vol.Labels[OwnerLabel] != ownerValue {
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN, "volume %q exists but is not wildstone-owned", volume)
	}
	if vol.Mountpoint == "" {
		return "", opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "volume %q reports no mountpoint", volume)
	}
	return vol.Mountpoint, nil
}
