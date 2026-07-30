package butler

import (
	"context"
	"fmt"

	"github.com/chickenrat17/wildstone-homelab/keystone/gateway"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// AgentReader is the Butler's entire live-node read surface (ADR-W006 A1,
// A3): a thin wrapper around *gateway.Gateway exposing exactly two
// methods — Health and ContainerStats — each of which constructs only its
// own read-only wsv1.Operation variant. There is no method here (and none
// added anywhere else in this package outside _test.go files) that can
// construct an EnsureContainer/StopContainer/RemoveContainer/EnsureVolume/
// WriteManagedFile op, and this package never imports keystone/reconcile.
// This is the "read/observe handle" ADR-W006 A1 requires the Butler be
// given instead of anything Apply-capable.
type AgentReader struct {
	Gateway *gateway.Gateway
}

// Health calls the agent's Health op for nodeID and returns whether it
// reports OK plus its free-text detail. Detail is agent/operator-composed
// and therefore attacker-influenceable — callers must envelope it before
// handing it to a model (see envelope.go and the get_health tool).
func (a *AgentReader) Health(ctx context.Context, nodeID string) (ok bool, detail string, err error) {
	if a == nil || a.Gateway == nil {
		return false, "", fmt.Errorf("agent reader: not configured for node %q", nodeID)
	}
	res, err := a.Gateway.Dispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_Health{Health: &wsv1.Health{}}})
	if err != nil {
		return false, "", fmt.Errorf("health %s: %w", nodeID, err)
	}
	if oe := res.GetError(); oe != nil {
		return false, "", fmt.Errorf("health %s: %s: %s", nodeID, oe.GetCode(), oe.GetMessage())
	}
	h := res.GetHealth()
	return h.GetOk(), h.GetDetail(), nil
}

// ContainerStatsResult is the read-only subset of a container's live stats
// the Butler can see.
type ContainerStatsResult struct {
	Present       bool
	CpuUsageNs    uint64
	MemUsageBytes uint64
	MemLimitBytes uint64
}

// ContainerStats calls the agent's ContainerStats op for a named container
// on nodeID. A NOT_FOUND/FORBIDDEN result is reported as Present=false with
// no error — that is a normal "container isn't up (or isn't ours)" answer,
// not a tool failure.
func (a *AgentReader) ContainerStats(ctx context.Context, nodeID, name string) (ContainerStatsResult, error) {
	if a == nil || a.Gateway == nil {
		return ContainerStatsResult{}, fmt.Errorf("agent reader: not configured for node %q", nodeID)
	}
	res, err := a.Gateway.Dispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ContainerStats{
		ContainerStats: &wsv1.ContainerStats{Name: name},
	}})
	if err != nil {
		return ContainerStatsResult{}, fmt.Errorf("container stats %s/%s: %w", nodeID, name, err)
	}
	if oe := res.GetError(); oe != nil {
		switch oe.GetCode() {
		case wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, wsv1.ErrorCode_ERROR_CODE_FORBIDDEN:
			return ContainerStatsResult{Present: false}, nil
		default:
			return ContainerStatsResult{}, fmt.Errorf("container stats %s/%s: %s: %s", nodeID, name, oe.GetCode(), oe.GetMessage())
		}
	}
	cs := res.GetContainerStats()
	return ContainerStatsResult{
		Present:       true,
		CpuUsageNs:    cs.GetCpuUsageNs(),
		MemUsageBytes: cs.GetMemUsageBytes(),
		MemLimitBytes: cs.GetMemLimitBytes(),
	}, nil
}
