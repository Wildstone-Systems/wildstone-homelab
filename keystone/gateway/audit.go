package gateway

// Keystone-side audit trail (ADR-W003 W7): Dispatch appends one JSONL record
// per dispatched operation to <Config.DataDir>/audit.log, independent of the
// agent's own structured-log audit trail (proposal §4.2's "two independent
// audit trails"). No-op when DataDir is unset, matching every other
// DataDir-gated persistence path added in W2.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// auditMaxBytes is the rotation threshold: before appending, if audit.log is
// already at or above this size, it is renamed to audit.log.1 (replacing any
// existing generation) and a fresh audit.log is started. Exactly one
// rotated generation is kept. A package var so tests can lower it instead of
// writing 64 MiB of fixtures.
var auditMaxBytes int64 = 64 << 20

// auditRecord is one line of <DataDir>/audit.log.
type auditRecord struct {
	Ts        string `json:"ts"`
	Node      string `json:"node"`
	OpID      string `json:"op_id"`
	Arm       string `json:"arm"`
	Outcome   string `json:"outcome"` // "ok" | "error"
	ErrorCode string `json:"error_code"`
}

// auditDispatch records the outcome of one Dispatch call. Called from
// Dispatch only (not DispatchRaw, which exists for tests that deliberately
// send hand-corrupted wire messages outside the normal op flow).
func (g *Gateway) auditDispatch(nodeID, opID string, op *wsv1.Operation, res *wsv1.OpResult, dispatchErr error) {
	if g.cfg.DataDir == "" {
		return
	}
	arm := opArmFromOperation(op)
	outcome, errCode := "ok", ""
	switch {
	case dispatchErr != nil:
		// Transport-level failure (node disconnected, context cancelled,
		// oversized message) — never reached the agent's OpResult oneof at
		// all, so there is no ErrorCode to report; record what we know.
		outcome = "error"
		errCode = "DISPATCH_FAILED"
	case res.GetError() != nil:
		outcome = "error"
		errCode = res.GetError().GetCode().String()
	default:
		// A successful result's oneof variant is a more specific source of
		// truth for the arm than the Operation we sent (e.g. protocol skew
		// edge cases); fall back to the request arm if it's not resolvable.
		if a := opArmFromResult(res); a != "" {
			arm = a
		}
	}
	g.appendAudit(auditRecord{
		Ts:        time.Now().UTC().Format(time.RFC3339),
		Node:      nodeID,
		OpID:      opID,
		Arm:       arm,
		Outcome:   outcome,
		ErrorCode: errCode,
	})
}

// auditIdentity records an identity-issuing RPC (Join/Renew) — the two
// paths that mint a node's cryptographic identity and therefore belong in
// the audit trail as much as any dispatched operation. reason is empty on
// success and a short failure cause otherwise. No-op when DataDir is unset,
// like every other persistence path.
func (g *Gateway) auditIdentity(nodeID, arm, reason string) {
	if g.cfg.DataDir == "" {
		return
	}
	outcome, errCode := "ok", ""
	if reason != "" {
		outcome, errCode = "error", reason
	}
	g.appendAudit(auditRecord{
		Ts:        time.Now().UTC().Format(time.RFC3339),
		Node:      nodeID,
		Arm:       arm,
		Outcome:   outcome,
		ErrorCode: errCode,
	})
}

// appendAudit rotates (if needed) and appends one record to
// <DataDir>/audit.log. Guarded by auditMu since Dispatch can run
// concurrently for different nodes.
//
// Durability tradeoff: writes are NOT fsync'd per line. This is a homelab
// product running on a single operator's hardware, not a multi-tenant
// compliance system — losing the last handful of audit lines in the
// (rare) event of a hard crash between write and the OS flushing its page
// cache is an acceptable tradeoff against adding an fsync (and its latency)
// to every single dispatched operation.
func (g *Gateway) appendAudit(rec auditRecord) {
	line, err := json.Marshal(rec)
	if err != nil {
		g.log.Warn("audit: marshal record failed", "error", err)
		return
	}
	line = append(line, '\n')

	g.auditMu.Lock()
	defer g.auditMu.Unlock()

	path := filepath.Join(g.cfg.DataDir, "audit.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() >= auditMaxBytes {
		rotated := path + ".1"
		os.Remove(rotated) // best-effort: keep exactly one generation
		if err := os.Rename(path, rotated); err != nil {
			g.log.Warn("audit: rotate failed", "error", err)
			// Fall through and keep appending to the oversized file rather
			// than dropping the record.
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		g.log.Warn("audit: open failed", "error", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		g.log.Warn("audit: write failed", "error", err)
	}
}

// opArmFromOperation names the oneof arm of a dispatched Operation. Kept as
// an explicit switch (mirroring wardstone/internal/agent's ops.go dispatch
// switch and audit.go's auditFields) rather than reflection, so the two
// audit trails describe arms the same way.
func opArmFromOperation(op *wsv1.Operation) string {
	switch op.GetOp().(type) {
	case *wsv1.Operation_EnsureContainer:
		return "EnsureContainer"
	case *wsv1.Operation_StopContainer:
		return "StopContainer"
	case *wsv1.Operation_ContainerLogs:
		return "ContainerLogs"
	case *wsv1.Operation_PullImage:
		return "PullImage"
	case *wsv1.Operation_WriteManagedFile:
		return "WriteManagedFile"
	case *wsv1.Operation_ReadManagedFile:
		return "ReadManagedFile"
	case *wsv1.Operation_HostFacts:
		return "HostFacts"
	case *wsv1.Operation_Health:
		return "Health"
	case *wsv1.Operation_SelfUpdate:
		return "SelfUpdate"
	case *wsv1.Operation_RemoveContainer:
		return "RemoveContainer"
	case *wsv1.Operation_ContainerStats:
		return "ContainerStats"
	case *wsv1.Operation_PruneImages:
		return "PruneImages"
	case *wsv1.Operation_EnsureVolume:
		return "EnsureVolume"
	case *wsv1.Operation_SnapshotDir:
		return "SnapshotDir"
	case *wsv1.Operation_MeasureUsage:
		return "MeasureUsage"
	case *wsv1.Operation_DiskSpace:
		return "DiskSpace"
	case *wsv1.Operation_ListBlockDevices:
		return "ListBlockDevices"
	case *wsv1.Operation_EnsureMount:
		return "EnsureMount"
	case *wsv1.Operation_TestNfs:
		return "TestNfs"
	case *wsv1.Operation_TestSmb:
		return "TestSmb"
	case *wsv1.Operation_ListPorts:
		return "ListPorts"
	case *wsv1.Operation_ProbeTcp:
		return "ProbeTcp"
	case *wsv1.Operation_ProbeHttp:
		return "ProbeHttp"
	case *wsv1.Operation_EnsureDockerNetwork:
		return "EnsureDockerNetwork"
	case *wsv1.Operation_SystemdUnitStatus:
		return "SystemdUnitStatus"
	case *wsv1.Operation_TimeSyncStatus:
		return "TimeSyncStatus"
	default:
		return "unknown"
	}
}

// opArmFromResult names the oneof arm of a successful OpResult; returns ""
// for OpResult_Error (which carries no arm information — the caller must
// fall back to opArmFromOperation) or an unrecognized/nil result.
func opArmFromResult(res *wsv1.OpResult) string {
	switch res.GetResult().(type) {
	case *wsv1.OpResult_EnsureContainer:
		return "EnsureContainer"
	case *wsv1.OpResult_StopContainer:
		return "StopContainer"
	case *wsv1.OpResult_ContainerLogs:
		return "ContainerLogs"
	case *wsv1.OpResult_PullImage:
		return "PullImage"
	case *wsv1.OpResult_WriteManagedFile:
		return "WriteManagedFile"
	case *wsv1.OpResult_ReadManagedFile:
		return "ReadManagedFile"
	case *wsv1.OpResult_HostFacts:
		return "HostFacts"
	case *wsv1.OpResult_Health:
		return "Health"
	case *wsv1.OpResult_SelfUpdate:
		return "SelfUpdate"
	case *wsv1.OpResult_RemoveContainer:
		return "RemoveContainer"
	case *wsv1.OpResult_ContainerStats:
		return "ContainerStats"
	case *wsv1.OpResult_PruneImages:
		return "PruneImages"
	case *wsv1.OpResult_EnsureVolume:
		return "EnsureVolume"
	case *wsv1.OpResult_SnapshotDir:
		return "SnapshotDir"
	case *wsv1.OpResult_MeasureUsage:
		return "MeasureUsage"
	case *wsv1.OpResult_DiskSpace:
		return "DiskSpace"
	case *wsv1.OpResult_ListBlockDevices:
		return "ListBlockDevices"
	case *wsv1.OpResult_EnsureMount:
		return "EnsureMount"
	case *wsv1.OpResult_TestNfs:
		return "TestNfs"
	case *wsv1.OpResult_TestSmb:
		return "TestSmb"
	case *wsv1.OpResult_ListPorts:
		return "ListPorts"
	case *wsv1.OpResult_ProbeTcp:
		return "ProbeTcp"
	case *wsv1.OpResult_ProbeHttp:
		return "ProbeHttp"
	case *wsv1.OpResult_EnsureDockerNetwork:
		return "EnsureDockerNetwork"
	case *wsv1.OpResult_SystemdUnitStatus:
		return "SystemdUnitStatus"
	case *wsv1.OpResult_TimeSyncStatus:
		return "TimeSyncStatus"
	default:
		return ""
	}
}
