package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

type platformFactsApplier struct {
	os, arch string
	err      error
	opErr    string
	ops      []string
}

func (a *platformFactsApplier) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	if op.GetHostFacts() == nil {
		a.ops = append(a.ops, "mutation")
		return nil, errors.New("unexpected workload mutation")
	}
	a.ops = append(a.ops, "HostFacts")
	if a.err != nil {
		return nil, a.err
	}
	if a.opErr != "" {
		return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{
			Code: wsv1.ErrorCode_ERROR_CODE_RUNTIME, Message: a.opErr,
		}}}, nil
	}
	return &wsv1.OpResult{Result: &wsv1.OpResult_HostFacts{HostFacts: &wsv1.HostFactsResult{
		Os: a.os, Architecture: a.arch,
	}}}, nil
}

func TestPlatformDispatchCheckFailureFamily(t *testing.T) {
	for _, tc := range []struct {
		name       string
		allowed    []string
		applier    *platformFactsApplier
		want       string
		wantProbes int
	}{
		{name: "omitted compatibility", applier: &platformFactsApplier{}, wantProbes: 0},
		{name: "matching first", allowed: []string{"linux/amd64"}, applier: &platformFactsApplier{os: "linux", arch: "amd64"}, wantProbes: 1},
		{name: "matching second", allowed: []string{"linux/amd64", "linux/arm64"}, applier: &platformFactsApplier{os: "linux", arch: "arm64"}, wantProbes: 1},
		{name: "changed architecture", allowed: []string{"linux/amd64"}, applier: &platformFactsApplier{os: "linux", arch: "arm64"}, want: "reports linux/arm64", wantProbes: 1},
		{name: "missing architecture", allowed: []string{"linux/amd64"}, applier: &platformFactsApplier{os: "linux"}, want: "did not report", wantProbes: 1},
		{name: "transport failure", allowed: []string{"linux/amd64"}, applier: &platformFactsApplier{err: errors.New("offline")}, want: "offline", wantProbes: 1},
		{name: "operation refusal", allowed: []string{"linux/amd64"}, applier: &platformFactsApplier{opErr: "facts refused"}, want: "facts refused", wantProbes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReconciler(tc.applier)
			got := r.refuseIfPlatformMismatch(t.Context(), "node-a", tc.allowed)
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("platform check = %q, want substring %q", got, tc.want)
			}
			if len(tc.applier.ops) != tc.wantProbes {
				t.Fatalf("operations = %v, want %d probes", tc.applier.ops, tc.wantProbes)
			}
		})
	}
}

func TestPlatformChangedAfterApprovalRefusesEveryWorkloadBeforeMutation(t *testing.T) {
	for _, kind := range []resource.Kind{resource.KindContainer, resource.KindJob} {
		t.Run(string(kind), func(t *testing.T) {
			a := &platformFactsApplier{os: "linux", arch: "arm64"}
			r := newReconciler(a)
			want := resource.Resource{Kind: kind, Metadata: resource.Metadata{Name: "app", Owner: "capability/test", Node: "node-a"}}
			spec := resource.ContainerSpec{Image: testDigestImage, Platforms: []string{"linux/amd64"}}
			if kind == resource.KindContainer {
				want.Container = &spec
			} else {
				want.Job = &resource.JobSpec{Run: spec}
			}
			step := plan.PlanStep{Kind: kind, Name: "app", Action: plan.ActionCreate, Digest: plan.Digest(want)}
			desired := map[resKey]resource.Resource{{kind, "app"}: want}
			var result StepResult
			if kind == resource.KindContainer {
				result = r.applyContainerStep(t.Context(), "node-a", step, desired, nil)
			} else {
				result = r.applyJobStep(t.Context(), "node-a", step, desired, nil)
			}
			if result.Applied || !strings.Contains(result.Error, "reports linux/arm64") || strings.Join(a.ops, ",") != "HostFacts" {
				t.Fatalf("changed platform should refuse before mutation: result=%+v operations=%v", result, a.ops)
			}
		})
	}
}

func TestClusterPlatformChangeRefusesBeforeLeaderMutation(t *testing.T) {
	want := clusterDesired()[0]
	want.Container.Platforms = []string{"linux/amd64"}
	a := &platformFactsApplier{os: "linux", arch: "arm64"}
	m := &clusterMemory{}
	r := newReconciler(a)
	r.Cluster = m
	r.ClusterAuthority = func(plan.PlanStep) (Authority, error) {
		return Authority{AcceptedRevision: "rev", PlanID: "plan", StepDigest: "digest", LeaderEpoch: 1}, nil
	}
	step := plan.PlanStep{Kind: resource.KindContainer, Name: want.Metadata.Name, Action: plan.ActionCreate}
	result := r.applyClusterContainerStep(t.Context(), "@cluster", step,
		map[resKey]resource.Resource{{want.Kind, want.Metadata.Name}: want}, []resource.Resource{want}, nil)
	if result.Applied || !strings.Contains(result.Error, "reports linux/arm64") || m.lastApply.Op != "" || strings.Join(a.ops, ",") != "HostFacts" {
		t.Fatalf("cluster platform change should refuse before mutation: result=%+v request=%+v operations=%v", result, m.lastApply, a.ops)
	}
}

func TestClusterIntentMapsSinglePlatformToNodeSelector(t *testing.T) {
	desired := clusterDesired()
	want := desired[0]
	want.Container.Platforms = []string{"linux/arm64"}
	desired[0] = want
	intent, err := ClusterIntent(want, desired)
	if err != nil {
		t.Fatal(err)
	}
	if intent.NodeSelector["kubernetes.io/os"] != "linux" || intent.NodeSelector["kubernetes.io/arch"] != "arm64" {
		t.Fatalf("platform selector = %+v", intent.NodeSelector)
	}
}

func TestClusterIntentNeedsNoArchitectureSelectorWhenBothAreAllowed(t *testing.T) {
	desired := clusterDesired()
	want := desired[0]
	want.Container.Platforms = []string{"linux/amd64", "linux/arm64"}
	desired[0] = want
	intent, err := ClusterIntent(want, desired)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := intent.NodeSelector["kubernetes.io/arch"]; ok {
		t.Fatalf("both supported architectures produced a narrowing selector: %+v", intent.NodeSelector)
	}
}
