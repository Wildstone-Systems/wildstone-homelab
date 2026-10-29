package reconcile

import (
	"context"
	"fmt"
	"sort"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// The cluster side of the backend seam (W050, #158 and #211). The
// reconciler sends these typed requests only for an explicitly approved
// k3s Container; Docker remains the default.

// TargetKind says what a resource is applied to.
type TargetKind string

const (
	// TargetNode is one node's Wardstone, through the Docker translator.
	TargetNode TargetKind = "node"
	// TargetCluster is the cluster's leader Wardstone, through the
	// cluster translator.
	TargetCluster TargetKind = "cluster"
)

// Target is where a resource's operations go.
type Target struct {
	Kind TargetKind
	// Node is the node id for TargetNode.
	Node string
}

// TargetOf names a resource's target. Only an explicit, accepted k3s
// Container choice reaches the cluster; all legacy resources stay on Docker.
func TargetOf(r resource.Resource) Target {
	if r.Kind == resource.KindContainer && r.Container != nil && r.Container.Backend == "k3s" {
		return Target{Kind: TargetCluster}
	}
	return Target{Kind: TargetNode, Node: r.Metadata.Node}
}

// Authority is what every mutating cluster request carries (leader ops
// §1.3; safety contract §2): the Statefile commit it was planned from,
// the approved step it executes, and the leadership term that sends it.
// The API server checks the epoch; the leader checks the revision.
type Authority struct {
	AcceptedRevision string
	PlanID           string
	StepDigest       string
	LeaderEpoch      uint64
}

// WorkloadPolicy is the household's choice for one component (capabilities
// contract §8). The zero value is today's behaviour: one copy, on the node
// it was installed on.
type WorkloadPolicy struct {
	// Copies is how many to run; 0 and 1 both mean one.
	Copies int
	// KeepRunning lets the component restart on another eligible node.
	KeepRunning bool
}

// Strategies a one-copy and a many-copy component are updated with.
const (
	StrategyRecreate = "Recreate"
	StrategyRolling  = "RollingUpdate"
)

// WorkloadSpec is ApplyWorkload's typed body. Every field maps from
// resource.ContainerSpec or from the deployment declaration and policy;
// there is no manifest, patch or free-form field to carry anything else.
type WorkloadSpec struct {
	Name         string
	Package      string
	Resource     string
	Image        string
	Command      []string
	Env          map[string]string
	Ports        []string
	ServicePorts []WorkloadPort
	Mounts       []resource.Mount
	Devices      []resource.DeviceMapping
	Resources    *resource.ContainerResources
	// ShmSize is the container's /dev/shm size, as Docker's --shm-size.
	ShmSize     string
	HostNetwork bool
	Security    WorkloadSecurity
	Replicas    int
	Strategy    string
	// NodeSelector pins the component. A component that may not move is
	// pinned to the node it runs on today; one that may move carries only
	// the labels its devices need.
	NodeSelector map[string]string
}

type WorkloadPort struct {
	Name          string
	ContainerPort uint32
	ServicePort   uint32
	Protocol      string
}

// WorkloadSecurity is the container's hardening, as declared.
type WorkloadSecurity struct {
	ReadOnlyRootfs  bool
	NoNewPrivileges bool
	CapAdd          []string
	CapDrop         []string
}

// ClusterOp names one leader operation.
type ClusterOp string

const (
	OpApplyWorkload  ClusterOp = "ApplyWorkload"
	OpDeleteWorkload ClusterOp = "DeleteWorkload"
	OpWorkloadStatus ClusterOp = "WorkloadStatus"
)

// ClusterRequest is one typed request to the leader.
type ClusterRequest struct {
	Op        ClusterOp
	Name      string
	Authority Authority
	// Workload is set for OpApplyWorkload.
	Workload *WorkloadSpec
	// UID is the object the Plan saw, for OpDeleteWorkload: the leader
	// stamps it with the current epoch, then deletes with the stamp's
	// resourceVersion as the precondition (safety contract §2.4, §3).
	UID                     string
	ResourceVersion         string
	ServiceUID              string
	ServiceResourceVersion  string
	ExpectedRevision        string
	ExpectedServiceRevision string
}

// ClusterResult is the leader's answer: Kubernetes facts, not Docker ones.
type ClusterResult struct {
	Found                  bool
	UID                    string
	ResourceVersion        string
	Revision               string
	PlanID                 string
	StepDigest             string
	ServiceUID             string
	ServiceResourceVersion string
	ServiceRevision        string
	MatchesDesired         bool
	Generation             int64
	ObservedGeneration     int64
	ReadyReplicas          int
	Replicas               int
	Node                   string
	Conditions             []string
	// RolloutPending distinguishes an owned workload whose managed fields
	// already match from actual spec drift while Kubernetes is still
	// observing the generation or bringing the requested replicas Ready.
	// It is derived by Keystone during observation and is never sent over
	// the Wardstone protocol.
	RolloutPending bool
}

// ClusterApplier sends one request to the leader.
type ClusterApplier interface {
	Apply(ctx context.Context, req ClusterRequest) (ClusterResult, error)
}

// clusterTranslator builds cluster requests, as dockerTranslator builds
// Docker operations.
type clusterTranslator struct{}

var cluster clusterTranslator

// applyWorkload translates one Container into ApplyWorkload. The package
// declares what the component may do (decl), the household chooses within
// that (policy), and anything the policy asks beyond the declaration is
// refused, never quietly reduced (capabilities contract §2, §8).
func (clusterTranslator) applyWorkload(r resource.Resource, decl pkgspec.ComponentDecl, policy WorkloadPolicy, devices []pkgspec.DeviceDecl, auth Authority) (ClusterRequest, error) {
	if r.Kind != resource.KindContainer || r.Container == nil {
		return ClusterRequest{}, fmt.Errorf("cluster: %s/%s is not a Container", r.Kind, r.Metadata.Name)
	}
	if auth.AcceptedRevision == "" || auth.PlanID == "" || auth.StepDigest == "" || auth.LeaderEpoch == 0 {
		return ClusterRequest{}, fmt.Errorf("cluster: %s: a write needs its accepted revision, plan step and leader epoch", r.Metadata.Name)
	}
	spec := r.Container
	// A Deployment's pods always restart. A spec that asks for anything
	// else cannot be carried, so it is refused rather than changed.
	switch spec.Restart {
	case "", "always", "unless-stopped":
	default:
		return ClusterRequest{}, fmt.Errorf("cluster: %s: restart %q has no cluster equivalent; a workload always restarts", r.Metadata.Name, spec.Restart)
	}
	replicas := 1
	if policy.Copies > 1 {
		if decl.Copies != pkgspec.CopiesMany {
			return ClusterRequest{}, fmt.Errorf("cluster: %s: the household asked for %d copies, but the package declares one", r.Metadata.Name, policy.Copies)
		}
		replicas = policy.Copies
	}
	if policy.KeepRunning && !decl.Relocate {
		return ClusterRequest{}, fmt.Errorf("cluster: %s: keep-running needs relocate: true, which the package does not declare", r.Metadata.Name)
	}
	strategy := StrategyRecreate
	if decl.Copies == pkgspec.CopiesMany {
		strategy = StrategyRolling
	}
	w := &WorkloadSpec{
		Name:        r.Metadata.Name,
		Package:     r.Metadata.Package,
		Resource:    r.Metadata.Name,
		Image:       spec.Image,
		Command:     spec.Command,
		Env:         spec.Env,
		Ports:       spec.Ports,
		Mounts:      spec.Mounts,
		Devices:     spec.Devices,
		Resources:   spec.Resources,
		ShmSize:     spec.ShmSize,
		HostNetwork: spec.HostNetwork(),
		Security: WorkloadSecurity{
			ReadOnlyRootfs: spec.ReadOnlyRootfs, NoNewPrivileges: spec.NoNewPrivileges,
			CapAdd: spec.CapAdd, CapDrop: spec.CapDrop,
		},
		Replicas:     replicas,
		Strategy:     strategy,
		NodeSelector: nodeSelector(r.Metadata.Node, policy.KeepRunning, devices),
	}
	return ClusterRequest{Op: OpApplyWorkload, Name: w.Name, Authority: auth, Workload: w}, nil
}

// nodeSelector pins a component that may not move to its node; one that
// may keeps only the labels its class devices need.
func nodeSelector(node string, keepRunning bool, devices []pkgspec.DeviceDecl) map[string]string {
	sel := map[string]string{}
	if !keepRunning {
		sel["kubernetes.io/hostname"] = node
	}
	classes := map[string]bool{}
	for _, d := range devices {
		if d.Kind == pkgspec.DeviceClass && d.Class != "" {
			classes[d.Class] = true
		}
	}
	names := make([]string, 0, len(classes))
	for c := range classes {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		sel["wildstone.dev/"+c] = "true"
	}
	return sel
}

// deleteWorkload translates a delete. It carries the UID the Plan saw, so
// the leader's stamp-then-delete refuses an object that was replaced.
func (clusterTranslator) deleteWorkload(name, uid, serviceUID string, auth Authority) (ClusterRequest, error) {
	if uid == "" && serviceUID == "" {
		return ClusterRequest{}, fmt.Errorf("cluster: %s: a delete needs a Deployment or Service UID the Plan saw", name)
	}
	if auth.AcceptedRevision == "" || auth.PlanID == "" || auth.StepDigest == "" || auth.LeaderEpoch == 0 {
		return ClusterRequest{}, fmt.Errorf("cluster: %s: a write needs its accepted revision, plan step and leader epoch", name)
	}
	return ClusterRequest{Op: OpDeleteWorkload, Name: name, UID: uid, ServiceUID: serviceUID, Authority: auth}, nil
}

// workloadStatus reads; it needs no authority.
func (clusterTranslator) workloadStatus(name string) ClusterRequest {
	return ClusterRequest{Op: OpWorkloadStatus, Name: name}
}
