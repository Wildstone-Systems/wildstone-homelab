package butler

// The host.* tool declarations (ADR-W039 §3.4, §5; H7, #353) — the
// Owner-class operator surface's half of Tier 3 "host administration".
//
// This file declares exactly the seven toolEntry values, each OptIn:
// true so no service bundle and no Agent's default set ever picks one up
// (ADR-W033 design §6.2); reaching one still needs an explicit, named
// assignment (runtimedb butler_tool_assignments), which is the "opt-in
// grant" H7 asks for. They are registered ONLY on ToolGateway
// (AgentClassOwner) — HouseholdGateway never registers them, so a
// Member-class Agent's manifest cannot list host.* at all; it is not a
// tool that gateway refuses, it is a tool that gateway does not have.
//
// Every handler here is a thin adapter: build the domestic.Call, dispatch
// it through gw.HostActions, and hand back what came back. All the real
// decisions — the tier check, the confirm-tier approval flow, the audit
// line — happen inside keystone/domestic's Gateway, exactly as they do
// for a household rule's action. This file adds no second copy of any of
// that.
import (
	"context"
	"fmt"
	"strconv"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/domestic"
)

// registerHostOpsTools adds the host.* toolEntry values to a freshly
// built ToolGateway. Split out of NewToolGateway so this family's
// declarations live beside everything else about it.
func registerHostOpsTools(gw *ToolGateway) {
	gw.tools["host.container_logs"] = toolEntry{
		Spec:      hostContainerLogsSpec,
		Untrusted: true,
		OptIn:     true,
		Handler:   handleHostContainerLogs,
	}
	gw.tools["host.container_stats"] = toolEntry{
		Spec:    hostContainerStatsSpec,
		OptIn:   true,
		Handler: handleHostContainerStats,
	}
	gw.tools["host.disk_usage"] = toolEntry{
		Spec:    hostDiskUsageSpec,
		OptIn:   true,
		Handler: handleHostDiskUsage,
	}
	gw.tools["host.unit_status"] = toolEntry{
		Spec:    hostUnitStatusSpec,
		OptIn:   true,
		Handler: handleHostUnitStatus,
	}
	gw.tools["host.container_restart"] = toolEntry{
		Spec:     hostContainerRestartSpec,
		Mutating: true,
		OptIn:    true,
		Handler:  handleHostContainerRestart,
	}
	gw.tools["host.prune_images"] = toolEntry{
		Spec:     hostPruneImagesSpec,
		Mutating: true,
		OptIn:    true,
		Handler:  handleHostPruneImages,
	}
	gw.tools["host.reconcile_poke"] = toolEntry{
		Spec:     hostReconcilePokeSpec,
		Mutating: true,
		OptIn:    true,
		Handler:  handleHostReconcilePoke,
	}
}

// hostCall builds the domestic.Call every host.* handler dispatches,
// carrying who is actually asking (this gateway's own person, never a
// model-supplied field) and whether a person is present to be asked to
// confirm (ADR-W024 A5.1) — the operator session is attended exactly
// when a person, not a routine or an administrative check, is behind it.
func (gw *ToolGateway) hostCall(tool, tier string, params map[string]string) domestic.Call {
	return domestic.Call{
		Tool:        tool,
		Tier:        tier,
		Params:      params,
		Rule:        "operator-session",
		Subject:     gw.person,
		TriggerType: "butler.operator_session",
		Attended:    gw.caller == CallerPersonFacing,
	}
}

// dispatchHostAction runs one host.* call through the domestic Gateway
// and turns its answer into a ToolOutcome, the same shape
// handleDoHouseholdAction produces for a family session's action.
func (gw *ToolGateway) dispatchHostAction(ctx context.Context, tool, tier string, params map[string]string) (ToolOutcome, error) {
	if gw.HostActions == nil {
		// Not Rejected: this mirrors get_container_logs's own "no node
		// read surface is configured" answer — a deployment gap, not a
		// refusal of something the model asked for.
		return ToolOutcome{Content: "host administration is not enabled on this deployment."}, nil
	}
	res, err := gw.HostActions.Dispatch(ctx, gw.hostCall(tool, tier, params))
	if err != nil {
		return ToolOutcome{Content: fmt.Sprintf("%s did not run: %v", tool, err), Rejected: true}, nil
	}
	return ToolOutcome{
		Content:           res.Summary,
		EffectPending:     res.EffectPending,
		EffectConfirmed:   res.EffectConfirmed,
		ProposalConfirmed: res.ProposalConfirmed,
	}, nil
}

var hostContainerLogsSpec = provider.Tool{
	Name: "host.container_logs",
	Description: "Read the tail of a Wildstone-owned container's logs on a node (Tier 3 host " +
		"administration, ADR-W039). Read-only. Requires an explicit opt-in grant even for an Owner " +
		"Agent — it is never on by default.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"container": map[string]any{"type": "string", "description": "Container name as Wildstone manages it."},
			"node":      map[string]any{"type": "string", "description": "Wardstone node id. Omit for the default node."},
			"lines":     map[string]any{"type": "integer", "description": fmt.Sprintf("How many trailing lines. 1..%d.", domestic.MaxHostLogLines)},
		},
		"required": []string{"container"},
	},
}

func handleHostContainerLogs(ctx context.Context, gw *ToolGateway, input map[string]any) (ToolOutcome, error) {
	container := stringInput(input, "container")
	if container == "" {
		return ToolOutcome{Content: "name the container to read logs for."}, nil
	}
	params := map[string]string{"container": container}
	if node := stringInput(input, "node"); node != "" {
		params["node"] = node
	}
	if lines := intInput(input, "lines"); lines > 0 {
		params["lines"] = strconv.Itoa(lines)
	}
	return gw.dispatchHostAction(ctx, "host.container_logs", domestic.TierRead, params)
}

var hostContainerStatsSpec = provider.Tool{
	Name: "host.container_stats",
	Description: "Read live cpu/memory usage for a Wildstone-owned container on a node (Tier 3 host " +
		"administration, ADR-W039). Read-only. Requires an explicit opt-in grant even for an Owner Agent.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"container": map[string]any{"type": "string", "description": "Container name as Wildstone manages it."},
			"node":      map[string]any{"type": "string", "description": "Wardstone node id. Omit for the default node."},
		},
		"required": []string{"container"},
	},
}

func handleHostContainerStats(ctx context.Context, gw *ToolGateway, input map[string]any) (ToolOutcome, error) {
	container := stringInput(input, "container")
	if container == "" {
		return ToolOutcome{Content: "name the container to read stats for."}, nil
	}
	params := map[string]string{"container": container}
	if node := stringInput(input, "node"); node != "" {
		params["node"] = node
	}
	return gw.dispatchHostAction(ctx, "host.container_stats", domestic.TierRead, params)
}

var hostDiskUsageSpec = provider.Tool{
	Name: "host.disk_usage",
	Description: "Measure disk usage under a managed prefix on a node — the agent itself refuses any " +
		"other path (Tier 3 host administration, ADR-W039). Read-only. Requires an explicit opt-in " +
		"grant even for an Owner Agent.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "A managed-prefix path to measure."},
			"node": map[string]any{"type": "string", "description": "Wardstone node id. Omit for the default node."},
		},
		"required": []string{"path"},
	},
}

func handleHostDiskUsage(ctx context.Context, gw *ToolGateway, input map[string]any) (ToolOutcome, error) {
	path := stringInput(input, "path")
	if path == "" {
		return ToolOutcome{Content: "name the managed-prefix path to measure."}, nil
	}
	params := map[string]string{"path": path}
	if node := stringInput(input, "node"); node != "" {
		params["node"] = node
	}
	return gw.dispatchHostAction(ctx, "host.disk_usage", domestic.TierRead, params)
}

var hostUnitStatusSpec = provider.Tool{
	Name: "host.unit_status",
	Description: "Read one systemd unit's state on a node — the agent's own allowlist is the scope " +
		"limit (Tier 3 host administration, ADR-W039). Read-only. Requires an explicit opt-in grant " +
		"even for an Owner Agent.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"unit": map[string]any{"type": "string", "description": "Allowlisted systemd unit name, e.g. docker.service."},
			"node": map[string]any{"type": "string", "description": "Wardstone node id. Omit for the default node."},
		},
		"required": []string{"unit"},
	},
}

func handleHostUnitStatus(ctx context.Context, gw *ToolGateway, input map[string]any) (ToolOutcome, error) {
	unit := stringInput(input, "unit")
	if unit == "" {
		return ToolOutcome{Content: "name the systemd unit to check."}, nil
	}
	params := map[string]string{"unit": unit}
	if node := stringInput(input, "node"); node != "" {
		params["node"] = node
	}
	return gw.dispatchHostAction(ctx, "host.unit_status", domestic.TierRead, params)
}

var hostContainerRestartSpec = provider.Tool{
	Name: "host.container_restart",
	Description: "Stop a Wildstone-owned container and let the reconciler bring it back from desired " +
		"state (Tier 3 host administration, ADR-W039). Changes something on a node, so a person must " +
		"confirm this specific call before it runs. Requires an explicit opt-in grant even for an " +
		"Owner Agent.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"container": map[string]any{"type": "string", "description": "Container name as Wildstone manages it."},
			"node":      map[string]any{"type": "string", "description": "Wardstone node id. Omit for the default node."},
		},
		"required": []string{"container"},
	},
}

func handleHostContainerRestart(ctx context.Context, gw *ToolGateway, input map[string]any) (ToolOutcome, error) {
	container := stringInput(input, "container")
	if container == "" {
		return ToolOutcome{Content: "name the container to restart."}, nil
	}
	params := map[string]string{"container": container}
	if node := stringInput(input, "node"); node != "" {
		params["node"] = node
	}
	return gw.dispatchHostAction(ctx, "host.container_restart", domestic.TierConfirm, params)
}

var hostPruneImagesSpec = provider.Tool{
	Name: "host.prune_images",
	Description: "Remove every unreferenced Wildstone-owned image on a node — never a foreign image " +
		"(Tier 3 host administration, ADR-W039). Changes something on a node, so a person must " +
		"confirm this specific call before it runs. Requires an explicit opt-in grant even for an " +
		"Owner Agent.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"node": map[string]any{"type": "string", "description": "Wardstone node id. Omit for the default node."},
		},
	},
}

func handleHostPruneImages(ctx context.Context, gw *ToolGateway, input map[string]any) (ToolOutcome, error) {
	params := map[string]string{}
	if node := stringInput(input, "node"); node != "" {
		params["node"] = node
	}
	return gw.dispatchHostAction(ctx, "host.prune_images", domestic.TierConfirm, params)
}

var hostReconcilePokeSpec = provider.Tool{
	Name: "host.reconcile_poke",
	Description: "Trigger the reconcile loop's convergence pass now instead of waiting for its next " +
		"interval (Tier 3 host administration, ADR-W039). Applies nothing new — only what a " +
		"previously approved Plan already described. A person must still confirm this specific call " +
		"before it runs. Requires an explicit opt-in grant even for an Owner Agent. Takes no params.",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
}

func handleHostReconcilePoke(ctx context.Context, gw *ToolGateway, _ map[string]any) (ToolOutcome, error) {
	return gw.dispatchHostAction(ctx, "host.reconcile_poke", domestic.TierConfirm, map[string]string{})
}
