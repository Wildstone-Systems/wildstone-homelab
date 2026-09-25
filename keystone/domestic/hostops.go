package domestic

// The host.* executor family (ADR-W039 §3.4; Hermes design doc
// docs/design/hermes-capability-package-and-execution-profiles.md §5.4,
// §9 H7; issue #353).
//
// This is Tier 3 "host administration" done the way §5.3 insists it be
// done: NOT a privileged container, not a shell, not a socket — an
// ordinary Wildstone executor, exactly like Notifier or Calendar, whose
// Execute happens to call a Wardstone op instead of an HTTP API. The
// package-level doc comment's "no path here ... reaches a node agent" is
// true of every OTHER executor in this file's siblings; this file is the
// one deliberate exception, and it is deliberate in a narrow, closed,
// reviewed way: a fixed set of named methods on HostNode, each one
// mapped 1:1 onto a Wardstone op that already exists. There is no
// Exec(string) anywhere in this file and there never will be — see
// hostops_test.go's structural pin.
//
// The menu is closed by construction: HostOps.Executors returns exactly
// the Executor values below, nothing computed from a string, nothing a
// caller can extend. Read operations (container_logs, container_stats,
// disk_usage, unit_status) carry TierRead — allow, and audited. Ones
// that change something on a node (container_restart, prune_images,
// reconcile_poke) carry TierConfirm, which routes through the SAME
// approval flow every other confirm-tier domestic action uses today
// (Gateway.confirmed, ADR-W024 A5.1): a Person answers a Wildstone
// notice, and the runtime sees `waiting`/`denied` as an ordinary refusal
// — never a new approval mechanism. "Package upgrade" and "snapshot a
// directory" are deliberately NOT in this file: see the package's
// household.go-style commentary in cmd/keystone/main.go's wiring, and
// the implementation report for #353, for why — mapping either onto a
// human-approved Plan needs a Person→principal identity and a resource
// kind neither exists today, and inventing one is out of this issue's
// scope (ADR-W003 A1's "no new protocol op" extends to "no new Plan kind
// invented ad hoc").
//
// Authority is layered ABOVE this file, not inside it. host.* tools are
// registered only on the operator surface (keystone/butler's ToolGateway,
// the Owner-class one) with OptIn: true (keystone/butler/tools_hostops.go),
// which is what keeps a new or backfilled Owner Agent from getting them
// by default (ADR-W033 design §6.2, §6.4) and keeps a Member-class Agent
// (keystone/butler's HouseholdGateway) from ever seeing them at all —
// that gateway never registers this family, so it is structurally
// absent from a Member manifest, not merely refused. Reaching one still
// needs an explicit, named assignment to that Agent (runtimedb
// butler_tool_assignments), which is the "opt-in grant" H7 asks for,
// reusing the existing machinery rather than adding a new one.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	wsgateway "github.com/chickenrat17/wildstone-homelab/keystone/gateway"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// HostOpsCapability is the agentTools capability the host.* family is
// declared under (BuiltinAgentTools-style: this is Keystone's own, not a
// package's — see HostOpsAgentTools).
const HostOpsCapability = "host"

// MaxHostLogLines bounds one host.container_logs call, mirroring
// butler.MaxLogLines: a diagnostic tail, not an export.
const MaxHostLogLines = 200

// HostOpsAgentTools declares the host.* family's tiers and opt-in status
// for the domestic Gateway's declaration check (ADR-W024 A5) and for
// anything that reads AgentToolSource.OptIn to decide a default set. It
// is Keystone's own capability, like HouseholdCapability's
// propose_timezone, because no installed package owns host administration
// — Wildstone's own executors do.
var HostOpsAgentTools = AgentToolSource{
	Capability: HostOpsCapability,
	Tools: map[string]string{
		"container_logs":    TierRead,
		"container_stats":   TierRead,
		"disk_usage":        TierRead,
		"unit_status":       TierRead,
		"container_restart": TierConfirm,
		"prune_images":      TierConfirm,
		"reconcile_poke":    TierConfirm,
	},
	OptIn: map[string]bool{
		"container_logs":    true,
		"container_stats":   true,
		"disk_usage":        true,
		"unit_status":       true,
		"container_restart": true,
		"prune_images":      true,
		"reconcile_poke":    true,
	},
}

// HostNode is the CLOSED set of node operations the host.* family may
// perform, each one a single existing Wardstone op (ADR-W003 A1: the
// operation surface is a closed oneof, and this interface is narrower
// still — it is not "any Operation", it is exactly these six calls).
// There is no method here that takes a raw command string, a shell
// fragment, or an arbitrary Operation, and there must never be one
// (ground rule 2; hostops_test.go pins the method set).
//
// It exists as an interface, rather than HostOps holding a
// *wsgateway.Gateway directly, for the reason keystone/butler's AgentReader
// is one: tests substitute a fake that never touches a network, and the
// production implementation (GatewayHostNode, below) is the only thing
// in this file that imports the Wardstone protocol.
type HostNode interface {
	// ContainerLogs reads the tail of a Wildstone-owned container's logs.
	// present=false, err=nil means "no such container of ours" (the agent
	// already scopes this to owned containers — ADR-W003 §A4.3).
	ContainerLogs(ctx context.Context, node, name string, tail int) (present bool, logs string, err error)
	// ContainerStats reads live cpu/memory usage for an owned container.
	ContainerStats(ctx context.Context, node, name string) (present bool, cpuUsageNs, memUsageBytes, memLimitBytes uint64, err error)
	// DiskUsage sums regular-file bytes under a managed prefix (the agent
	// itself refuses anything else — MeasureUsage's own contract).
	DiskUsage(ctx context.Context, node, path string) (totalBytes, fileCount uint64, err error)
	// UnitStatus reads one systemd unit's state. The agent's own
	// allowlist (Config.SystemdUnitAllowlist) is the scope limit; this
	// method adds none of its own because it does not need to.
	UnitStatus(ctx context.Context, node, unit string) (activeState, subState, loadState string, err error)
	// StopContainer stops an owned container. It is exactly half of
	// "restart" — the executor below calls this and then pokes the
	// reconciler, which is the reconciler's OWN re-create path (ADR-W029
	// A10) rather than this file constructing an EnsureContainer itself.
	StopContainer(ctx context.Context, node, name string) (present, wasRunning bool, err error)
	// PruneImages removes every unreferenced Wildstone-owned image on one
	// node (the agent's own label filter — ADR-W003 §A4.3 — is the only
	// scope this ever has).
	PruneImages(ctx context.Context, node string) (reclaimedBytes uint64, deletedCount uint32, err error)
}

// GatewayHostNode is the production HostNode: a thin wrapper around
// *wsgateway.Gateway, in the same shape as keystone/butler's AgentReader —
// each method constructs exactly its own read-only or narrowly-scoped
// wsv1.Operation variant and nothing else.
type GatewayHostNode struct {
	Gateway *wsgateway.Gateway
}

func (h *GatewayHostNode) dispatch(ctx context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	if h == nil || h.Gateway == nil {
		return nil, fmt.Errorf("host node: not configured for node %q", node)
	}
	return h.Gateway.Dispatch(ctx, node, op)
}

// notFoundOrForbidden reports whether an OpResult's error is the normal
// "no such container of ours" answer rather than a real failure —
// exactly AgentReader.ContainerStats's own reading of the same two
// codes.
func notFoundOrForbidden(oe *wsv1.OpError) bool {
	switch oe.GetCode() {
	case wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, wsv1.ErrorCode_ERROR_CODE_FORBIDDEN:
		return true
	default:
		return false
	}
}

func (h *GatewayHostNode) ContainerLogs(ctx context.Context, node, name string, tail int) (bool, string, error) {
	if tail <= 0 || tail > MaxHostLogLines {
		tail = MaxHostLogLines
	}
	res, err := h.dispatch(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_ContainerLogs{
		ContainerLogs: &wsv1.ContainerLogs{Name: name, Tail: uint32(tail)},
	}})
	if err != nil {
		return false, "", fmt.Errorf("host container logs %s/%s: %w", node, name, err)
	}
	if oe := res.GetError(); oe != nil {
		if notFoundOrForbidden(oe) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("host container logs %s/%s: %s: %s", node, name, oe.GetCode(), oe.GetMessage())
	}
	return true, string(res.GetContainerLogs().GetLogs()), nil
}

func (h *GatewayHostNode) ContainerStats(ctx context.Context, node, name string) (bool, uint64, uint64, uint64, error) {
	res, err := h.dispatch(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_ContainerStats{
		ContainerStats: &wsv1.ContainerStats{Name: name},
	}})
	if err != nil {
		return false, 0, 0, 0, fmt.Errorf("host container stats %s/%s: %w", node, name, err)
	}
	if oe := res.GetError(); oe != nil {
		if notFoundOrForbidden(oe) {
			return false, 0, 0, 0, nil
		}
		return false, 0, 0, 0, fmt.Errorf("host container stats %s/%s: %s: %s", node, name, oe.GetCode(), oe.GetMessage())
	}
	cs := res.GetContainerStats()
	return true, cs.GetCpuUsageNs(), cs.GetMemUsageBytes(), cs.GetMemLimitBytes(), nil
}

func (h *GatewayHostNode) DiskUsage(ctx context.Context, node, path string) (uint64, uint64, error) {
	res, err := h.dispatch(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_MeasureUsage{
		MeasureUsage: &wsv1.MeasureUsage{Path: path},
	}})
	if err != nil {
		return 0, 0, fmt.Errorf("host disk usage %s/%s: %w", node, path, err)
	}
	if oe := res.GetError(); oe != nil {
		return 0, 0, fmt.Errorf("host disk usage %s/%s: %s: %s", node, path, oe.GetCode(), oe.GetMessage())
	}
	mu := res.GetMeasureUsage()
	return mu.GetTotalBytes(), mu.GetFileCount(), nil
}

func (h *GatewayHostNode) UnitStatus(ctx context.Context, node, unit string) (string, string, string, error) {
	res, err := h.dispatch(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_SystemdUnitStatus{
		SystemdUnitStatus: &wsv1.SystemdUnitStatus{Unit: unit},
	}})
	if err != nil {
		return "", "", "", fmt.Errorf("host unit status %s/%s: %w", node, unit, err)
	}
	if oe := res.GetError(); oe != nil {
		return "", "", "", fmt.Errorf("host unit status %s/%s: %s: %s", node, unit, oe.GetCode(), oe.GetMessage())
	}
	su := res.GetSystemdUnitStatus()
	return su.GetActiveState(), su.GetSubState(), su.GetLoadState(), nil
}

func (h *GatewayHostNode) StopContainer(ctx context.Context, node, name string) (bool, bool, error) {
	res, err := h.dispatch(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_StopContainer{
		StopContainer: &wsv1.StopContainer{Name: name},
	}})
	if err != nil {
		return false, false, fmt.Errorf("host container restart %s/%s: %w", node, name, err)
	}
	if oe := res.GetError(); oe != nil {
		if notFoundOrForbidden(oe) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("host container restart %s/%s: %s: %s", node, name, oe.GetCode(), oe.GetMessage())
	}
	return true, res.GetStopContainer().GetWasRunning(), nil
}

func (h *GatewayHostNode) PruneImages(ctx context.Context, node string) (uint64, uint32, error) {
	res, err := h.dispatch(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_PruneImages{
		PruneImages: &wsv1.PruneImages{},
	}})
	if err != nil {
		return 0, 0, fmt.Errorf("host prune images %s: %w", node, err)
	}
	if oe := res.GetError(); oe != nil {
		return 0, 0, fmt.Errorf("host prune images %s: %s: %s", node, oe.GetCode(), oe.GetMessage())
	}
	pi := res.GetPruneImages()
	return pi.GetReclaimedBytes(), pi.GetDeletedCount(), nil
}

// HostOps builds the host.* executor family. One value, shared by every
// registered executor, holds the seams; there is no per-executor state.
type HostOps struct {
	// Authority is the same household-execution lease check every other
	// executor in this package makes.
	Authority HouseholdExecutionAuthority
	// Node is where a call actually goes. nil means the family is
	// unavailable (Available reports false) — a deployment that has not
	// turned Tier 3 on wires no Node, and Executors() still returns the
	// closed menu so the declaration/manifest shape is stable either way.
	Node HostNode
	// DefaultNode is used when a call names none. Empty means a call
	// must name one.
	DefaultNode string
	// ReconcilePoke triggers the reconcile loop's convergence pass
	// (keystone/reconcile/loop.Loop.Poke) — the same hook
	// apiSrv.OnApproved uses after a Plan is approved. host.reconcile_poke
	// IS this call; host.container_restart calls it after StopContainer
	// so the reconciler's own re-create path brings the container back
	// (ADR-W029 A10) rather than this package ever building an
	// EnsureContainer itself. nil is a working configuration: the two
	// executors that use it report they cannot rather than panicking.
	ReconcilePoke func()
}

// Available reports whether this family can actually reach a node.
func (h *HostOps) Available() bool { return h != nil && h.Node != nil }

func (h *HostOps) node(c Call) string {
	if n := strings.TrimSpace(c.Params["node"]); n != "" {
		return n
	}
	return h.DefaultNode
}

// Executors returns the seven domestic.Executor values that make up the
// host.* family — the CLOSED menu H7 asks for. main.go registers each of
// these on the domestic Gateway exactly as it registers Notifier or
// Calendar; nothing here is built from a string a caller supplies.
func (h *HostOps) Executors() []Executor {
	return []Executor{
		&hostContainerLogs{h},
		&hostContainerStats{h},
		&hostDiskUsage{h},
		&hostUnitStatus{h},
		&hostContainerRestart{h},
		&hostPruneImages{h},
		&hostReconcilePoke{h},
	}
}

// unavailable is the shared refusal every host.* executor gives when
// Tier 3 has not been wired on this deployment (Node == nil). It is a
// RefusalError, not a plain error, for the same reason every other
// executor's configuration gap is: it is a household telling somebody
// "that is not turned on here", not a bug.
func (h *HostOps) unavailable(tool string) error {
	return &RefusalError{Tool: tool, Reason: "host administration is not enabled on this deployment"}
}

// --- read tier ---

type hostContainerLogs struct{ h *HostOps }

func (e *hostContainerLogs) Name() string { return HostOpsCapability + ".container_logs" }

func (e *hostContainerLogs) Usage() string {
	return "Read the tail of a Wildstone-owned container's logs on a node. params: container " +
		"(required), node (optional, default node if configured), lines (optional, 1.." +
		strconv.Itoa(MaxHostLogLines) + ")."
}

func (e *hostContainerLogs) Execute(ctx context.Context, c Call) (Result, error) {
	if !e.h.Available() {
		return Result{}, e.h.unavailable(e.Name())
	}
	if err := checkExecution(ctx, e.h.Authority); err != nil {
		return Result{}, err
	}
	name := strings.TrimSpace(c.Params["container"])
	if name == "" {
		return Result{}, fmt.Errorf("param %q is required", "container")
	}
	node := e.h.node(c)
	if node == "" {
		return Result{}, fmt.Errorf("no node was named and this deployment has no default node")
	}
	tail, _ := strconv.Atoi(strings.TrimSpace(c.Params["lines"]))
	present, logs, err := e.h.Node.ContainerLogs(ctx, node, name, tail)
	if err != nil {
		return Result{}, err
	}
	if !present {
		return Result{Summary: fmt.Sprintf("no container named %q is managed by Wildstone on node %s", name, node)}, nil
	}
	if strings.TrimSpace(logs) == "" {
		return Result{Summary: fmt.Sprintf("%s on %s is running and has written nothing to its logs", name, node)}, nil
	}
	// Summary, not Detail: this is the one host.* result that is not one
	// line, because the whole point of the call is the log text. It
	// carries attacker-influenceable content (whatever the container
	// wrote), which is why the toolEntry above this executor marks it
	// Untrusted and every caller must envelope it before a model sees it
	// (ADR-W006 A5.2) — exactly as butler.AgentReader.ContainerLogs's
	// existing caller already does for the operator's get_container_logs.
	return Result{
		Summary: fmt.Sprintf("logs for %s on %s (newest last):\n%s", name, node, logs),
		Detail:  map[string]string{"node": node, "container": name},
	}, nil
}

type hostContainerStats struct{ h *HostOps }

func (e *hostContainerStats) Name() string { return HostOpsCapability + ".container_stats" }

func (e *hostContainerStats) Usage() string {
	return "Read live cpu/memory usage for a Wildstone-owned container. params: container (required), " +
		"node (optional, default node if configured)."
}

func (e *hostContainerStats) Execute(ctx context.Context, c Call) (Result, error) {
	if !e.h.Available() {
		return Result{}, e.h.unavailable(e.Name())
	}
	if err := checkExecution(ctx, e.h.Authority); err != nil {
		return Result{}, err
	}
	name := strings.TrimSpace(c.Params["container"])
	if name == "" {
		return Result{}, fmt.Errorf("param %q is required", "container")
	}
	node := e.h.node(c)
	if node == "" {
		return Result{}, fmt.Errorf("no node was named and this deployment has no default node")
	}
	present, cpuNs, memBytes, memLimit, err := e.h.Node.ContainerStats(ctx, node, name)
	if err != nil {
		return Result{}, err
	}
	if !present {
		return Result{Summary: fmt.Sprintf("no container named %q is managed by Wildstone on node %s", name, node)}, nil
	}
	return Result{
		Summary: fmt.Sprintf("%s on %s: cpu=%dns mem=%d/%d bytes", name, node, cpuNs, memBytes, memLimit),
		Detail: map[string]string{
			"node": node, "container": name,
			"cpuUsageNs": strconv.FormatUint(cpuNs, 10),
			"memUsage":   strconv.FormatUint(memBytes, 10),
			"memLimit":   strconv.FormatUint(memLimit, 10),
		},
	}, nil
}

type hostDiskUsage struct{ h *HostOps }

func (e *hostDiskUsage) Name() string { return HostOpsCapability + ".disk_usage" }

func (e *hostDiskUsage) Usage() string {
	return "Measure disk usage under a managed prefix on a node (the agent refuses anything else). " +
		"params: path (required), node (optional, default node if configured)."
}

func (e *hostDiskUsage) Execute(ctx context.Context, c Call) (Result, error) {
	if !e.h.Available() {
		return Result{}, e.h.unavailable(e.Name())
	}
	if err := checkExecution(ctx, e.h.Authority); err != nil {
		return Result{}, err
	}
	path := strings.TrimSpace(c.Params["path"])
	if path == "" {
		return Result{}, fmt.Errorf("param %q is required", "path")
	}
	node := e.h.node(c)
	if node == "" {
		return Result{}, fmt.Errorf("no node was named and this deployment has no default node")
	}
	total, files, err := e.h.Node.DiskUsage(ctx, node, path)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Summary: fmt.Sprintf("%s on %s: %d bytes across %d files", path, node, total, files),
		Detail: map[string]string{
			"node": node, "path": path,
			"totalBytes": strconv.FormatUint(total, 10),
			"fileCount":  strconv.FormatUint(files, 10),
		},
	}, nil
}

type hostUnitStatus struct{ h *HostOps }

func (e *hostUnitStatus) Name() string { return HostOpsCapability + ".unit_status" }

func (e *hostUnitStatus) Usage() string {
	return "Read one systemd unit's state on a node (the agent's own allowlist is the scope limit). " +
		"params: unit (required), node (optional, default node if configured)."
}

func (e *hostUnitStatus) Execute(ctx context.Context, c Call) (Result, error) {
	if !e.h.Available() {
		return Result{}, e.h.unavailable(e.Name())
	}
	if err := checkExecution(ctx, e.h.Authority); err != nil {
		return Result{}, err
	}
	unit := strings.TrimSpace(c.Params["unit"])
	if unit == "" {
		return Result{}, fmt.Errorf("param %q is required", "unit")
	}
	node := e.h.node(c)
	if node == "" {
		return Result{}, fmt.Errorf("no node was named and this deployment has no default node")
	}
	active, sub, load, err := e.h.Node.UnitStatus(ctx, node, unit)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Summary: fmt.Sprintf("%s on %s: %s (%s/%s)", unit, node, active, sub, load),
		Detail:  map[string]string{"node": node, "unit": unit, "activeState": active, "subState": sub, "loadState": load},
	}, nil
}

// --- confirm tier ---

type hostContainerRestart struct{ h *HostOps }

func (e *hostContainerRestart) Name() string { return HostOpsCapability + ".container_restart" }

func (e *hostContainerRestart) Usage() string {
	return "Stop a Wildstone-owned container and let the reconciler bring it back from desired state " +
		"(a container that is not part of desired state stays stopped). Asks you to confirm first. " +
		"params: container (required), node (optional, default node if configured)."
}

func (e *hostContainerRestart) Describe(ctx context.Context, c Call) (string, error) {
	name := strings.TrimSpace(c.Params["container"])
	if name == "" {
		return "", fmt.Errorf("param %q is required", "container")
	}
	node := e.h.node(c)
	if node == "" {
		return "", fmt.Errorf("no node was named and this deployment has no default node")
	}
	return fmt.Sprintf("Restart %s on %s?", name, node), nil
}

func (e *hostContainerRestart) Execute(ctx context.Context, c Call) (Result, error) {
	if !e.h.Available() {
		return Result{}, e.h.unavailable(e.Name())
	}
	if err := checkExecution(ctx, e.h.Authority); err != nil {
		return Result{}, err
	}
	name := strings.TrimSpace(c.Params["container"])
	if name == "" {
		return Result{}, fmt.Errorf("param %q is required", "container")
	}
	node := e.h.node(c)
	if node == "" {
		return Result{}, fmt.Errorf("no node was named and this deployment has no default node")
	}
	present, wasRunning, err := e.h.Node.StopContainer(ctx, node, name)
	if err != nil {
		return Result{}, err
	}
	if !present {
		return Result{Summary: fmt.Sprintf("no container named %q is managed by Wildstone on node %s", name, node)}, nil
	}
	if e.h.ReconcilePoke != nil {
		e.h.ReconcilePoke()
	}
	return Result{
		EffectConfirmed: true,
		Summary:         fmt.Sprintf("stopped %s on %s (was running: %t) and told the reconciler to bring it back", name, node, wasRunning),
		Detail:          map[string]string{"node": node, "container": name, "wasRunning": strconv.FormatBool(wasRunning)},
	}, nil
}

type hostPruneImages struct{ h *HostOps }

func (e *hostPruneImages) Name() string { return HostOpsCapability + ".prune_images" }

func (e *hostPruneImages) Usage() string {
	return "Remove every unreferenced Wildstone-owned image on a node (never a foreign image). Asks " +
		"you to confirm first. params: node (optional, default node if configured)."
}

func (e *hostPruneImages) Describe(ctx context.Context, c Call) (string, error) {
	node := e.h.node(c)
	if node == "" {
		return "", fmt.Errorf("no node was named and this deployment has no default node")
	}
	return fmt.Sprintf("Prune unreferenced Wildstone-owned images on %s?", node), nil
}

func (e *hostPruneImages) Execute(ctx context.Context, c Call) (Result, error) {
	if !e.h.Available() {
		return Result{}, e.h.unavailable(e.Name())
	}
	if err := checkExecution(ctx, e.h.Authority); err != nil {
		return Result{}, err
	}
	node := e.h.node(c)
	if node == "" {
		return Result{}, fmt.Errorf("no node was named and this deployment has no default node")
	}
	reclaimed, deleted, err := e.h.Node.PruneImages(ctx, node)
	if err != nil {
		return Result{}, err
	}
	return Result{
		EffectConfirmed: true,
		Summary:         fmt.Sprintf("pruned %d image(s) on %s, reclaiming %d bytes", deleted, node, reclaimed),
		Detail: map[string]string{
			"node":           node,
			"reclaimedBytes": strconv.FormatUint(reclaimed, 10),
			"deletedCount":   strconv.FormatUint(uint64(deleted), 10),
		},
	}, nil
}

type hostReconcilePoke struct{ h *HostOps }

func (e *hostReconcilePoke) Name() string { return HostOpsCapability + ".reconcile_poke" }

func (e *hostReconcilePoke) Usage() string {
	return "Trigger the reconcile loop's convergence pass now instead of waiting for its next interval. " +
		"Applies nothing new — it only runs what a previously approved Plan already described. Asks you " +
		"to confirm first. Takes no params."
}

func (e *hostReconcilePoke) Describe(ctx context.Context, c Call) (string, error) {
	return "Run reconciliation now?", nil
}

func (e *hostReconcilePoke) Execute(ctx context.Context, c Call) (Result, error) {
	// Available() also gates this one, even though it never calls Node:
	// H7 is one on/off switch for the whole family, not read-ops-only.
	if !e.h.Available() {
		return Result{}, e.h.unavailable(e.Name())
	}
	if err := checkExecution(ctx, e.h.Authority); err != nil {
		return Result{}, err
	}
	if e.h.ReconcilePoke == nil {
		return Result{}, &RefusalError{Tool: e.Name(), Reason: "the reconcile loop is not wired on this control plane"}
	}
	e.h.ReconcilePoke()
	return Result{EffectConfirmed: true, Summary: "reconciliation triggered"}, nil
}
