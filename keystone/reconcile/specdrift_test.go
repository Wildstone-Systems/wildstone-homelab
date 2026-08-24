package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// TestSpecDriftDetectedAndConverged pins ADR-W008 R2c: a running,
// already-managed container whose DESIRED spec changes in a way the old
// presence-only observe could not see (env, not image) is detected as
// drift via the spec-hash label and converged by an Update, over the real
// gateway + agent + fake docker.
func TestSpecDriftDetectedAndConverged(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	base := containerResource("app", testDigestImage)
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	first, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{base})
	if err != nil || !first.Applied {
		t.Fatalf("first reconcile: outcome=%+v err=%v", first, err)
	}

	// Same image, changed env: invisible to presence-only observe, and
	// invisible to the agent's old image-only replace rule.
	changed := containerResource("app", testDigestImage)
	changed.Container.Env = map[string]string{"NEW_SETTING": "on"}

	// Approve the Medium-risk container Update the drift plan carries.
	r.Approve = func(*plan.Plan) bool { return true }

	ensureBefore := agent.opCount("EnsureContainer")
	second, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{changed})
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(second.Plan.Steps) != 1 {
		t.Fatalf("env drift not planned: steps=%+v", second.Plan.Steps)
	}
	if !second.Applied {
		t.Fatalf("drift not converged: %+v", second)
	}
	if got := agent.opCount("EnsureContainer"); got != ensureBefore+1 {
		t.Fatalf("EnsureContainer calls = %d, want %d (one replace)", got, ensureBefore+1)
	}

	// Third pass with the same desired spec: hashes now match — NoOp.
	third, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{changed})
	if err != nil || len(third.Plan.Steps) != 0 {
		t.Fatalf("converged spec still planning steps: %+v err=%v", third.Plan, err)
	}
}
