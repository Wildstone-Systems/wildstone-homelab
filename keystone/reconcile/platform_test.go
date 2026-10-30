package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

type changedPlatformApplier struct {
	ops int
}

func (a *changedPlatformApplier) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	a.ops++
	if op.GetHostFacts() == nil {
		return nil, nil
	}
	return &wsv1.OpResult{Result: &wsv1.OpResult_HostFacts{HostFacts: &wsv1.HostFactsResult{Os: "linux", Architecture: "arm64"}}}, nil
}

func TestPlatformChangedAfterProposalRefusesWorkloadBeforeMutation(t *testing.T) {
	for _, kind := range []resource.Kind{resource.KindContainer, resource.KindJob} {
		t.Run(string(kind), func(t *testing.T) {
			applier := &changedPlatformApplier{}
			r := newReconciler(applier)
			want := resource.Resource{Kind: kind, Metadata: resource.Metadata{Name: "app", Owner: "capability/test", Node: "node-a", Platforms: "linux/amd64"}}
			step := plan.PlanStep{Kind: kind, Name: "app", Action: plan.ActionCreate}
			if kind == resource.KindContainer {
				want.Container = &resource.ContainerSpec{}
			} else {
				want.Job = &resource.JobSpec{}
			}
			desired := map[resKey]resource.Resource{{kind, "app"}: want}
			var result StepResult
			if kind == resource.KindContainer {
				result = r.applyContainerStep(t.Context(), "node-a", step, desired, nil)
			} else {
				result = r.applyJobStep(t.Context(), "node-a", step, desired, nil)
			}
			if result.Applied || !strings.Contains(result.Error, "linux/arm64") || applier.ops != 1 {
				t.Fatalf("changed platform should refuse before mutation: result=%+v operations=%d", result, applier.ops)
			}
		})
	}
}
