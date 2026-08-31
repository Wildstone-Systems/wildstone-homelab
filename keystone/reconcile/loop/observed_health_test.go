package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

const testImage = "docker.io/library/nginx@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const testImage2 = "docker.io/library/nginx@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

func container(name, node, image string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindContainer,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/test", Node: node},
		Container:  &resource.ContainerSpec{Image: image, Restart: "unless-stopped"},
	}
}

func observedFor(t *testing.T, db *runtimedb.DB, kind, name string) runtimedb.Observed {
	t.Helper()
	obs, err := db.ListObserved()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if o.Kind == kind && o.Name == name {
			return o
		}
	}
	t.Fatalf("no observed row for %s/%s: %+v", kind, name, obs)
	return runtimedb.Observed{}
}

// containerApplier answers InspectContainer from what EnsureContainer was
// actually asked for, so a container can converge. okApplier answers every
// inspect with an empty result, which reads as "present but spec-drifted"
// forever — fine for the Volume-shaped tests it was written for, but it
// makes every container permanently unconverged, which is precisely the
// state this test needs to be able to distinguish from healthy.
type containerApplier struct {
	okApplier
	live map[string]map[string]string
}

func (a *containerApplier) Apply(ctx context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	if a.live == nil {
		a.live = map[string]map[string]string{}
	}
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_EnsureContainer:
		a.ops++
		a.live[o.EnsureContainer.GetName()] = o.EnsureContainer.GetLabels()
		return &wsv1.OpResult{}, nil
	case *wsv1.Operation_InspectContainer:
		a.ops++
		labels, ok := a.live[o.InspectContainer.GetName()]
		if !ok {
			return &wsv1.OpResult{Result: &wsv1.OpResult_Error{
				Error: &wsv1.OpError{Code: wsv1.ErrorCode_ERROR_CODE_NOT_FOUND},
			}}, nil
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_InspectContainer{
			InspectContainer: &wsv1.InspectContainerResult{Labels: labels, Running: true},
		}}, nil
	}
	return a.okApplier.Apply(ctx, node, op)
}

// TestDriftAwaitingApprovalIsNotHealthy is defect A. Health was derived
// from the step results alone, so a tick that planned work and applied
// none of it — the PendingApproval case, which is the normal state of any
// Medium/High drift — implicated no resource in a failure and stamped
// every desired resource green.
//
// `wild status` therefore said "healthy" about precisely the resource the
// reconciler had just found drifted and filed a plan about. The status
// surface disagreeing with the reconciler is worse than it having no
// opinion: an operator opens it to decide whether to act.
func TestDriftAwaitingApprovalIsNotHealthy(t *testing.T) {
	l, state, db, _ := harness(t)
	l.Rec.Applier = &containerApplier{}
	if err := state.Put(container("app", "n1", testImage)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if o := observedFor(t, db, "Container", "app"); !o.Healthy {
		t.Fatalf("converged container is not healthy: %+v", o)
	}

	// Change the image: observe sees a spec mismatch, the diff is a
	// Medium Container Update, and a drift tick may not auto-apply it.
	if err := state.Put(container("app", "n1", testImage2)); err != nil {
		t.Fatal(err)
	}
	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatalf("drift tick: %v", err)
	}
	if out.DriftProposed == "" {
		t.Fatalf("drift tick = %+v, want a proposed drift plan", out)
	}
	o := observedFor(t, db, "Container", "app")
	if o.Healthy {
		t.Fatalf("container with an unapplied drift plan reported healthy: %+v", o)
	}
	if !strings.Contains(o.Detail, "not applied") {
		t.Fatalf("detail = %q, want it to say the step was not applied", o.Detail)
	}
}

// haltingApplier fails EnsureContainer for one name, so the plan halts on
// an Incident and every later step is never attempted.
type haltingApplier struct {
	containerApplier
	failName string
}

func (a *haltingApplier) Apply(ctx context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	if ec := op.GetEnsureContainer(); ec != nil && ec.GetName() == a.failName {
		return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{
			Code: wsv1.ErrorCode_ERROR_CODE_RUNTIME, Message: "boom",
		}}}, nil
	}
	return a.containerApplier.Apply(ctx, node, op)
}

// A halted plan leaves later steps with no result at all — the same empty
// evidence the PendingApproval case produces. Those resources are absent,
// and must not be reported green either.
func TestUnattemptedStepsAreNotHealthy(t *testing.T) {
	l, state, db, _ := harness(t)
	l.Rec.Applier = &haltingApplier{failName: "app-a"}
	for _, name := range []string{"app-a", "app-b"} {
		if err := state.Put(container(name, "n1", testImage)); err != nil {
			t.Fatal(err)
		}
	}
	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if out.Failed == "" {
		t.Fatalf("tick = %+v, want the incident surfaced", out)
	}
	for _, name := range []string{"app-a", "app-b"} {
		if o := observedFor(t, db, "Container", name); o.Healthy {
			t.Errorf("%s reported healthy after a halted plan: %+v", name, o)
		}
	}
}
