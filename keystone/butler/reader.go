package butler

import (
	"context"
	"fmt"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/gateway"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
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

// MaxLogLines bounds one get_container_logs call. A Butler reading logs
// is looking for the line that explains a failure, not archiving them:
// the whole result is redacted, enveloped and size-bounded before it
// reaches a model, and asking for a thousand lines only means the
// interesting one gets truncated away.
const MaxLogLines = 200

// ContainerLogs calls the agent's existing read-only ContainerLogs op
// for a managed container on nodeID and returns the tail as text
// (ADR-W030 P2).
//
// It adds no new operation to the Wardstone protocol and no new
// authority: ContainerLogs is already in the closed op surface, already
// restricted to containers Wardstone manages, and already read-only.
// What was missing was any way for the operator's Butler to ask, which
// meant "why did it fall over?" always ended in a human going to the
// node.
//
// Log content is written by whatever is running in the container, so it
// is attacker-influenceable in exactly the way a health detail is and
// the calling tool must envelope it. A NOT_FOUND/FORBIDDEN result is a
// normal "no such container of ours", not a tool failure.
func (a *AgentReader) ContainerLogs(ctx context.Context, nodeID, name string, tail int) (present bool, logs string, err error) {
	if a == nil || a.Gateway == nil {
		return false, "", fmt.Errorf("agent reader: not configured for node %q", nodeID)
	}
	if tail <= 0 || tail > MaxLogLines {
		tail = MaxLogLines
	}
	res, err := a.Gateway.Dispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ContainerLogs{
		ContainerLogs: &wsv1.ContainerLogs{Name: name, Tail: uint32(tail)},
	}})
	if err != nil {
		return false, "", fmt.Errorf("container logs %s/%s: %w", nodeID, name, err)
	}
	if oe := res.GetError(); oe != nil {
		switch oe.GetCode() {
		case wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, wsv1.ErrorCode_ERROR_CODE_FORBIDDEN:
			return false, "", nil
		default:
			return false, "", fmt.Errorf("container logs %s/%s: %s: %s", nodeID, name, oe.GetCode(), oe.GetMessage())
		}
	}
	return true, string(res.GetContainerLogs().GetLogs()), nil
}
