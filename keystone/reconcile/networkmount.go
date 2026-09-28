package reconcile

// NetworkMount applier and observer (ADR-W045 A3.2): an NFS share the
// node mounts under its mount root and records in /etc/fstab.
//
// Observe asks the node, not the managed cache: a share is present when
// DiskSpace says the target IS a mount point of the declared type. A
// share that dropped (the NAS rebooted, the node booted while the NAS was
// down) therefore reads as absent, and the Low-risk create re-mounts it
// unattended. An agent below protocol minor 19 cannot answer DiskSpace;
// for it the managed cache stands in, the way declared-only kinds work.
//
// There is no unmount op, so a removed declaration leaves the share
// mounted; it is never observed once undesired and so never planned as a
// delete.

import (
	"context"
	"fmt"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

func (r *Reconciler) applyNetworkMountStep(ctx context.Context, nodeID string, step plan.PlanStep, desiredByKey map[resKey]resource.Resource) StepResult {
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}
	if step.Action == plan.ActionDelete {
		sr.Applied, sr.HealthOK = true, true
		r.markManaged(nodeID, key, false)
		return sr
	}
	want, ok := desiredByKey[key]
	if !ok || want.NetworkMount == nil {
		sr.Error = "internal: no desired NetworkMount spec for " + step.Name
		return sr
	}
	m := want.NetworkMount
	res, err := r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_EnsureMount{
		EnsureMount: &wsv1.EnsureMount{Source: m.Source, Target: m.Target, Fstype: m.Type, Options: m.Options},
	}})
	if err != nil {
		sr.Error = err.Error()
		return sr
	}
	if oe := res.GetError(); oe != nil {
		sr.Error = fmt.Sprintf("mount %s at %s: %s: %s", m.Source, m.Target, oe.GetCode(), oe.GetMessage())
		return sr
	}
	sr.Applied = true
	r.markManaged(nodeID, key, true)

	// Health: the target must now BE the share. EnsureMount answers
	// "already mounted" for anything mounted at the target, so this is
	// also what catches a different filesystem sitting there.
	mounted, known, err := r.shareMounted(ctx, nodeID, m)
	switch {
	case err != nil:
		sr.Error = err.Error()
	case known && !mounted:
		sr.Error = fmt.Sprintf("%s is not mounted from %s after EnsureMount", m.Target, m.Source)
	default:
		sr.HealthOK = true
	}
	return sr
}

func (r *Reconciler) observeNetworkMount(ctx context.Context, nodeID string, k resKey, want resource.Resource, isDesired bool) (resource.Resource, bool, error) {
	if !isDesired || want.NetworkMount == nil {
		return resource.Resource{}, false, nil
	}
	mounted, known, err := r.shareMounted(ctx, nodeID, want.NetworkMount)
	if err != nil {
		return resource.Resource{}, false, fmt.Errorf("observe network mount %s: %w", k.Name, err)
	}
	if !known {
		mounted = r.isManaged(nodeID, k)
	}
	if !mounted {
		return resource.Resource{}, false, nil
	}
	return want, true, nil
}

// shareMounted reports whether m.Target is currently a mount point of
// m.Type. known is false when the agent is too old to answer (protocol
// minor < 19), which is not an error: the caller falls back.
func (r *Reconciler) shareMounted(ctx context.Context, nodeID string, m *resource.NetworkMountSpec) (mounted, known bool, err error) {
	res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_DiskSpace{
		DiskSpace: &wsv1.DiskSpace{Path: m.Target},
	}})
	if err != nil {
		return false, false, err
	}
	if oe := res.GetError(); oe != nil {
		switch oe.GetCode() {
		case wsv1.ErrorCode_ERROR_CODE_INVALID:
			return false, false, nil // agent skew: cannot tell
		case wsv1.ErrorCode_ERROR_CODE_NOT_FOUND:
			return false, true, nil
		default:
			return false, false, fmt.Errorf("disk space %s: %s: %s", m.Target, oe.GetCode(), oe.GetMessage())
		}
	}
	d := res.GetDiskSpace()
	// "nfs" and "nfs4" are what the kernel reports for either request
	// depending on the negotiated version, so both count as the share.
	isNFS := d.GetFstype() == "nfs" || d.GetFstype() == "nfs4"
	return d.GetExists() && d.GetMountPoint() == m.Target && isNFS, true, nil
}
