package agent

import (
	"context"
	"errors"
)

// runtimeBackend is everything Wardstone asks of the container runtime on
// its own node (ADR-W050 §3 step 1). Every container, image, volume and
// network operation goes through it; the Docker Engine client is the only
// implementation today.
//
// The seam is per node, and internal: the Wardstone protocol (ADR-W003)
// does not change, and host work (disks, devices, mounts, systemd) stays
// outside it so a host provider can be split out separately (ADR-W050
// §2.2). The cluster-wide seam is Keystone's (W050 slice 2).
//
// The result types still carry Docker's field names. A second backend
// fills them in; renaming them is not needed to add one.
type runtimeBackend interface {
	inspect(ctx context.Context, name string) (*containerInspect, error)
	create(ctx context.Context, name string, body createBody) (string, error)
	start(ctx context.Context, id string) error
	stop(ctx context.Context, name string, timeoutSec uint32) error
	logs(ctx context.Context, name string, tail uint32, sinceUnix int64) ([]byte, error)
	pull(ctx context.Context, ref, auth string) error
	remove(ctx context.Context, id string, force bool) error
	stats(ctx context.Context, id string) (statsResult, error)
	volumeInspect(ctx context.Context, name string) (*volumeInspectResult, error)
	volumeCreate(ctx context.Context, name string, labels map[string]string) error
	networkInspect(ctx context.Context, name string) (*networkInspectResult, error)
	networkCreate(ctx context.Context, name string, labels map[string]string) error
	// pruneImages removes only images Wildstone pulled: the label filter
	// lives in the implementation and must never be bypassed.
	pruneImages(ctx context.Context) (pruneResult, error)
}

// isNotFound reports whether a runtime backend error means the named
// object does not exist.
func isNotFound(err error) bool {
	var nf interface{ notFound() bool }
	return errors.As(err, &nf) && nf.notFound()
}
