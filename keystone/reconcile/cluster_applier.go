package reconcile

import (
	"context"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/gateway"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

var clusterDecimalCPU = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// GatewayClusterApplier routes only typed workload operations to the latest
// issued leader. The Wardstone term gate remains authoritative: a stale DB
// read or a lost lease is refused there and retried after fresh observation.
type GatewayClusterApplier struct {
	Gateway *gateway.Gateway
	DB      *runtimedb.DB
}

// ClusterUnavailable means the leader could not be observed reliably in
// this tick. An applying Plan stays queued and re-observes after recovery.
type ClusterUnavailable struct{ Err error }

func (e *ClusterUnavailable) Error() string { return e.Err.Error() }
func (e *ClusterUnavailable) Unwrap() error { return e.Err }

func (a *GatewayClusterApplier) Apply(ctx context.Context, req ClusterRequest) (ClusterResult, error) {
	if a == nil || a.Gateway == nil || a.DB == nil {
		return ClusterResult{}, fmt.Errorf("cluster: leader gateway is unavailable")
	}
	term, ok, err := a.DB.CurrentLeaderEpoch()
	if err != nil {
		return ClusterResult{}, err
	}
	const identityPrefix = "system:serviceaccount:wildstone-system:wardstone-"
	node, valid := strings.CutPrefix(term.Leader, identityPrefix)
	if !ok || !valid || node == "" || strings.Contains(node, "/") {
		return ClusterResult{}, &ClusterUnavailable{fmt.Errorf("cluster: no issued leader identity")}
	}
	// ObserveWorkload's desired comparison was added after the original
	// workload arm. An older leader ignores that field and reports the
	// default false, which could turn every healthy workload into drift.
	nodes, err := a.DB.ListNodes()
	if err != nil {
		return ClusterResult{}, err
	}
	compatible := false
	for _, n := range nodes {
		if n.NodeID == node && n.ProtocolMinor >= 33 {
			compatible = true
			break
		}
	}
	if !compatible {
		return ClusterResult{}, &ClusterUnavailable{fmt.Errorf("cluster: leader %s requires protocol minor 33 for approved workload recovery witnesses", node)}
	}
	if req.Op != OpWorkloadStatus && term.Epoch != req.Authority.LeaderEpoch {
		return ClusterResult{}, &ClusterUnavailable{fmt.Errorf("cluster: leadership changed before dispatch")}
	}
	var op *wsv1.Operation
	auth := &wsv1.WorkloadAuthority{AcceptedRevision: req.Authority.AcceptedRevision, PlanId: req.Authority.PlanID, StepDigest: req.Authority.StepDigest, LeaderEpoch: req.Authority.LeaderEpoch}
	switch req.Op {
	case OpApplyWorkload:
		w, err := wireWorkload(req.Workload)
		if err != nil {
			return ClusterResult{}, err
		}
		op = &wsv1.Operation{Op: &wsv1.Operation_ApplyWorkload{ApplyWorkload: &wsv1.ApplyWorkload{Authority: auth, Workload: w, ExpectedRevision: req.ExpectedRevision, ExpectedServiceRevision: req.ExpectedServiceRevision, SeparateRevisionWitnesses: true}}}
	case OpDeleteWorkload:
		op = &wsv1.Operation{Op: &wsv1.Operation_DeleteWorkload{DeleteWorkload: &wsv1.DeleteWorkload{Authority: auth, Name: req.Name, Uid: req.UID, ResourceVersion: req.ResourceVersion, ServiceUid: req.ServiceUID, ServiceResourceVersion: req.ServiceResourceVersion}}}
	case OpWorkloadStatus:
		w, err := wireWorkload(req.Workload)
		if err != nil {
			return ClusterResult{}, err
		}
		op = &wsv1.Operation{Op: &wsv1.Operation_ObserveWorkload{ObserveWorkload: &wsv1.ObserveWorkload{Name: req.Name, Desired: w}}}
	default:
		return ClusterResult{}, fmt.Errorf("cluster: unknown operation %q", req.Op)
	}
	result, err := a.Gateway.Dispatch(ctx, node, op)
	if err != nil {
		return ClusterResult{}, &ClusterUnavailable{err}
	}
	if failure := result.GetError(); failure != nil {
		if req.Op == OpWorkloadStatus && failure.GetCode() == wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
			return ClusterResult{}, nil
		}
		err := fmt.Errorf("cluster %s %s: %s: %s", req.Op, req.Name, failure.GetCode(), failure.GetMessage())
		if failure.GetCode() == wsv1.ErrorCode_ERROR_CODE_RUNTIME || strings.Contains(failure.GetMessage(), "leadership") || strings.Contains(failure.GetMessage(), "leader-scoped") || strings.Contains(failure.GetMessage(), "leader epoch") || strings.Contains(failure.GetMessage(), "active cluster leadership") {
			return ClusterResult{}, &ClusterUnavailable{err}
		}
		return ClusterResult{}, err
	}
	switch req.Op {
	case OpApplyWorkload:
		r := result.GetApplyWorkload()
		if r == nil {
			return ClusterResult{}, fmt.Errorf("cluster: missing ApplyWorkload result")
		}
		return ClusterResult{Found: true, UID: r.GetUid(), ResourceVersion: r.GetResourceVersion(), Generation: r.GetGeneration(), ServiceUID: r.GetServiceUid(), ServiceResourceVersion: r.GetServiceResourceVersion()}, nil
	case OpDeleteWorkload:
		if result.GetDeleteWorkload() == nil {
			return ClusterResult{}, fmt.Errorf("cluster: missing DeleteWorkload result")
		}
		return ClusterResult{}, nil
	default:
		r := result.GetObserveWorkload()
		if r == nil {
			return ClusterResult{}, fmt.Errorf("cluster: missing ObserveWorkload result")
		}
		return ClusterResult{Found: true, UID: r.GetUid(), ResourceVersion: r.GetResourceVersion(), Revision: r.GetRevision(), PlanID: r.GetPlanId(), StepDigest: r.GetStepDigest(), ServiceUID: r.GetServiceUid(), ServiceResourceVersion: r.GetServiceResourceVersion(), ServiceRevision: r.GetServiceRevision(), MatchesDesired: r.GetMatchesDesired(), Generation: r.GetGeneration(), ObservedGeneration: r.GetObservedGeneration(), ReadyReplicas: int(r.GetReadyReplicas()), Replicas: int(r.GetReplicas())}, nil
	}
}

func wireWorkload(w *WorkloadSpec) (*wsv1.WorkloadSpec, error) {
	if w == nil {
		return nil, nil
	}
	for _, value := range w.Env {
		if strings.HasPrefix(value, "secret/") {
			return nil, fmt.Errorf("cluster: first stateless workload cannot carry unresolved secret references")
		}
	}
	if len(w.Mounts) != 0 || len(w.Devices) != 0 || w.ShmSize != "" || w.HostNetwork || len(w.Ports) != 0 || len(w.ServicePorts) == 0 {
		return nil, fmt.Errorf("cluster: first stateless workload requires a Service port and cannot carry Docker host ports, mounts, devices, shmSize or host networking")
	}
	if w.Resources != nil && w.Resources.PidsLimit != 0 {
		return nil, fmt.Errorf("cluster: pidsLimit has no reviewed Kubernetes mapping")
	}
	if w.Resources != nil {
		if err := validateClusterMemory(w.Resources.Memory); err != nil {
			return nil, err
		}
		if err := validateClusterCPU(w.Resources.Cpus); err != nil {
			return nil, err
		}
	}
	strategy := wsv1.WorkloadStrategy_WORKLOAD_STRATEGY_RECREATE
	if w.Strategy == StrategyRolling {
		strategy = wsv1.WorkloadStrategy_WORKLOAD_STRATEGY_ROLLING_UPDATE
	}
	ports := make([]*wsv1.WorkloadPort, 0, len(w.ServicePorts))
	for _, p := range w.ServicePorts {
		protocol := wsv1.PortProtocol_PORT_PROTOCOL_TCP
		if p.Protocol == "udp" {
			protocol = wsv1.PortProtocol_PORT_PROTOCOL_UDP
		}
		ports = append(ports, &wsv1.WorkloadPort{Name: p.Name, ContainerPort: p.ContainerPort, ServicePort: p.ServicePort, Protocol: protocol})
	}
	out := &wsv1.WorkloadSpec{Name: w.Name, Package: w.Package, Resource: w.Resource, Image: w.Image, Command: w.Command, Env: w.Env, Ports: ports, Replicas: uint32(w.Replicas), Strategy: strategy, NodeSelector: w.NodeSelector,
		Security: &wsv1.WorkloadSecurity{ReadOnlyRootfs: w.Security.ReadOnlyRootfs, NoNewPrivileges: w.Security.NoNewPrivileges, CapAdd: w.Security.CapAdd, CapDrop: w.Security.CapDrop}}
	if w.Resources != nil {
		out.Resources = &wsv1.ContainerResources{Memory: w.Resources.Memory, Cpus: w.Resources.Cpus}
	}
	return out, nil
}

// validateClusterMemory keeps proposal preflight aligned with Wardstone's
// Docker-size-to-Kubernetes-quantity boundary. Resource validation checks the
// spelling; this additional bound prevents an approved value from overflowing
// only after it reaches the leader.
func validateClusterMemory(value string) error {
	if value == "" {
		return nil
	}
	multiplier := uint64(1)
	number := value
	switch value[len(value)-1] {
	case 'b', 'B':
		number = value[:len(value)-1]
	case 'k', 'K':
		multiplier, number = 1<<10, value[:len(value)-1]
	case 'm', 'M':
		multiplier, number = 1<<20, value[:len(value)-1]
	case 'g', 'G':
		multiplier, number = 1<<30, value[:len(value)-1]
	}
	n, err := strconv.ParseUint(number, 10, 63)
	if err != nil || n == 0 || n > uint64(^uint64(0)>>1)/multiplier {
		return fmt.Errorf("cluster: memory %q is not a bounded Docker size string", value)
	}
	return nil
}

// validateClusterCPU rejects precision Kubernetes cannot represent before a
// Plan is filed. Wardstone repeats the check at its typed execution boundary.
func validateClusterCPU(value string) error {
	if value == "" {
		return nil
	}
	if !clusterDecimalCPU.MatchString(value) {
		return fmt.Errorf("cluster: cpu %q is not a decimal core count", value)
	}
	cores, ok := new(big.Rat).SetString(value)
	if !ok || cores.Sign() <= 0 {
		return fmt.Errorf("cluster: cpu %q is not a positive decimal core count", value)
	}
	milli := new(big.Rat).Mul(cores, big.NewRat(1_000, 1))
	if !milli.IsInt() {
		return fmt.Errorf("cluster: cpu %q is finer than Kubernetes' 1m precision", value)
	}
	return nil
}
