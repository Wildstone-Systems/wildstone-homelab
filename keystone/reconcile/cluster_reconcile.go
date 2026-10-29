package reconcile

import (
	"context"
	"fmt"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

func (r *Reconciler) observeClusterContainer(ctx context.Context, k resKey, want resource.Resource, isDesired bool, all []resource.Resource) (resource.Resource, bool, ClusterResult, error) {
	if r.Cluster == nil {
		return resource.Resource{}, false, ClusterResult{}, fmt.Errorf("cluster: no leader applier configured")
	}
	req := cluster.workloadStatus(k.Name)
	if isDesired {
		intent, err := ClusterIntent(want, all)
		if err != nil {
			return resource.Resource{}, false, ClusterResult{}, err
		}
		req.Workload = intent
	}
	result, err := r.Cluster.Apply(ctx, req)
	if err != nil {
		return resource.Resource{}, false, ClusterResult{}, err
	}
	if !result.Found {
		return resource.Resource{}, false, result, nil
	}
	if isDesired && result.MatchesDesired && result.ReadyReplicas == intentReplicas(req.Workload) && result.ObservedGeneration >= result.Generation {
		return want, true, result, nil
	}
	placeholder := placeholderResource(resource.KindContainer, k.Name)
	placeholder.Container.Backend = "k3s"
	return placeholder, true, result, nil
}

func intentReplicas(w *WorkloadSpec) int {
	if w == nil {
		return 0
	}
	return w.Replicas
}

func (r *Reconciler) applyClusterContainerStep(ctx context.Context, nodeID string, step plan.PlanStep, desiredByKey map[resKey]resource.Resource, all []resource.Resource, seen map[resKey]ClusterResult) StepResult {
	sr := StepResult{Step: step}
	if r.Cluster == nil || r.ClusterAuthority == nil {
		sr.Error = "cluster: leader applier or approved authority is unavailable"
		return sr
	}
	auth, err := r.ClusterAuthority(step)
	if err != nil {
		sr.Error = err.Error()
		sr.Retryable = true
		return sr
	}
	k := resKey{step.Kind, step.Name}
	before := seen[k]
	switch step.Action {
	case plan.ActionCreate, plan.ActionUpdate:
		want, ok := desiredByKey[k]
		if !ok {
			sr.Error = "cluster: desired Container is missing"
			return sr
		}
		intent, err := ClusterIntent(want, all)
		if err != nil {
			sr.Error = err.Error()
			return sr
		}
		req := ClusterRequest{Op: OpApplyWorkload, Name: step.Name, Authority: auth, Workload: intent, ExpectedRevision: before.Revision}
		if _, err := r.Cluster.Apply(ctx, req); err != nil {
			sr.Error = err.Error()
			sr.Retryable = true
			return sr
		}
		sr.Applied = true
		result, err := r.Cluster.Apply(ctx, ClusterRequest{Op: OpWorkloadStatus, Name: step.Name, Workload: intent})
		if err != nil {
			sr.Error = "cluster health observation: " + err.Error()
			sr.Retryable = true
			return sr
		}
		if !result.Found || !result.MatchesDesired || result.ReadyReplicas != intent.Replicas || result.ObservedGeneration < result.Generation {
			sr.Error = "cluster rollout is not ready; next tick will re-observe"
			sr.Retryable = true
			return sr
		}
		sr.HealthOK = true
		r.markManaged(nodeID, k, true)
		return sr
	case plan.ActionDelete:
		deploymentIdentity := before.UID != "" && before.ResourceVersion != ""
		serviceIdentity := before.ServiceUID != "" && before.ServiceResourceVersion != ""
		if !before.Found || (!deploymentIdentity && !serviceIdentity) {
			sr.Error = "cluster: delete has no complete observed Deployment or Service identity"
			sr.Retryable = true
			return sr
		}
		req, err := cluster.deleteWorkload(step.Name, before.UID, before.ServiceUID, auth)
		if err != nil {
			sr.Error = err.Error()
			return sr
		}
		req.ResourceVersion, req.ServiceUID, req.ServiceResourceVersion = before.ResourceVersion, before.ServiceUID, before.ServiceResourceVersion
		if _, err := r.Cluster.Apply(ctx, req); err != nil {
			sr.Error = err.Error()
			sr.Retryable = true
			return sr
		}
		sr.Applied = true
		result, err := r.Cluster.Apply(ctx, cluster.workloadStatus(step.Name))
		if err != nil {
			sr.Error = "cluster delete observation: " + err.Error()
			sr.Retryable = true
			return sr
		}
		if result.Found {
			sr.Error = "cluster workload still exists after delete"
			sr.Retryable = true
			return sr
		}
		sr.HealthOK = true
		r.markManaged(nodeID, k, false)
		return sr
	default:
		sr.Error = "unexpected cluster action " + step.Action.String()
		return sr
	}
}
