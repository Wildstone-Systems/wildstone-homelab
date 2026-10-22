package reconcile

// NetworkMount applier and observer (ADR-W045 A3.2): an NFS share the
// node mounts under its mount root and records in /etc/fstab.
//
// Observe asks the node, not the managed cache: a share is present when
// DiskSpace says the target IS a mount point of the declared share —
// same source, every declared option live. A different share or changed
// options there reads as the live spec, so an approved change is planned
// as an Update rather than taken as already applied. A
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
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
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

	// Health: the target must now BE this share, with these options.
	// EnsureMount answers "already mounted" for anything mounted at the
	// target and only rewrites fstab, so after a change of source or
	// options the OLD mount can still be the live one — there is no
	// unmount op. Say so, rather than report the change as done.
	st, err := r.shareState(ctx, nodeID, m)
	switch {
	case err != nil:
		sr.Error = err.Error()
	case st.known && st.other:
		sr.Error = fmt.Sprintf("%s is still mounted from %s with the old settings; /etc/fstab now names %s and takes effect at the next reboot or remount (there is no unmount op)",
			m.Target, st.liveSource, m.Source)
	case st.known && !st.ours:
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
	st, err := r.shareState(ctx, nodeID, want.NetworkMount)
	if err != nil {
		return resource.Resource{}, false, fmt.Errorf("observe network mount %s: %w", k.Name, err)
	}
	switch {
	case !st.known:
		if !r.isManaged(nodeID, k) {
			return resource.Resource{}, false, nil
		}
		return want, true, nil
	case st.ours:
		return want, true, nil
	case st.other:
		// A share IS mounted there, but not the declared one, or not with
		// the declared options: report what is live, so the diff plans an
		// Update instead of believing the approved change already applied.
		live := *want.NetworkMount
		live.Source, live.Options = st.liveSource, nil
		obs := want
		obs.NetworkMount = &live
		return obs, true, nil
	default:
		return resource.Resource{}, false, nil
	}
}

// shareState is what the node says is mounted at a share's target.
type shareState struct {
	known      bool // false: the agent is too old to answer (minor < 19)
	ours       bool // the declared share, with every declared option
	other      bool // an NFS share, but a different source or options
	liveSource string
}

func (r *Reconciler) shareState(ctx context.Context, nodeID string, m *resource.NetworkMountSpec) (shareState, error) {
	res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_DiskSpace{
		DiskSpace: &wsv1.DiskSpace{Path: m.Target},
	}})
	if err != nil {
		return shareState{}, err
	}
	if oe := res.GetError(); oe != nil {
		switch oe.GetCode() {
		case wsv1.ErrorCode_ERROR_CODE_INVALID:
			return shareState{}, nil // agent skew: cannot tell
		case wsv1.ErrorCode_ERROR_CODE_NOT_FOUND:
			return shareState{known: true}, nil
		default:
			return shareState{}, fmt.Errorf("disk space %s: %s: %s", m.Target, oe.GetCode(), oe.GetMessage())
		}
	}
	d := res.GetDiskSpace()
	// "nfs" and "nfs4" are what the kernel reports for either request
	// depending on the negotiated version, so both count as a share.
	isNFS := d.GetFstype() == "nfs" || d.GetFstype() == "nfs4"
	if !d.GetExists() || d.GetMountPoint() != m.Target || !isNFS {
		return shareState{known: true}, nil
	}
	if d.GetSource() == m.Source && hasOptions(d.GetOptions(), m.Options) {
		return shareState{known: true, ours: true, liveSource: d.GetSource()}, nil
	}
	return shareState{known: true, other: true, liveSource: d.GetSource()}, nil
}

// hasOptions reports whether every declared mount option is live. A flag
// ("ro", "soft") must appear as is; a key=value option matches a live
// value it prefixes, because the kernel reports what it negotiated —
// "vers=4" is live as "vers=4.2".
func hasOptions(live, declared []string) bool {
	for _, want := range declared {
		found := false
		k, v, isKV := strings.Cut(want, "=")
		for _, have := range live {
			if !isKV {
				found = have == want
			} else if hk, hv, ok := strings.Cut(have, "="); ok && hk == k && strings.HasPrefix(hv, v) {
				found = true
			}
			if found {
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
