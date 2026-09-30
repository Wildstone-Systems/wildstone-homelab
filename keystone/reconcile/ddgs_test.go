package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/research"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// The gateway and typed Wardstone operations are real; the agent's Docker
// state is a fixture. This is not container-build or deployment evidence.
func TestDDGSPackageUsesOrdinaryReconcileLifecycle(t *testing.T) {
	h := startGateway(t, []string{"REPLACED-test"})
	agent := newTestAgent(t, h, "node-a", "REPLACED-test")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent not connected")
	}
	p, err := pkgspec.Load("../../packages/research-ddgs/package.yaml")
	if err != nil {
		t.Fatal(err)
	}
	render := func(port string) []resource.Resource {
		t.Helper()
		rs, err := capability.NewEngine().Expand(p, map[string]string{"node": "node-a", "worker_image": testDigestImage, "port": port}, capability.NewCatalog(p))
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	for _, port := range []string{"18080", "18081"} {
		desired := render(port)
		outcome, err := r.Reconcile(context.Background(), "node-a", desired)
		if port == "18081" {
			if err != nil || !outcome.PendingApproval {
				t.Fatalf("reconfigure did not wait for approval: %+v %v", outcome, err)
			}
			r.Approve = func(*plan.Plan) bool { return true } // Owner decision is exercised by the API fixture.
			outcome, err = r.Reconcile(context.Background(), "node-a", desired)
		}
		if err != nil || !outcome.Applied || outcome.Incident != "" {
			t.Fatalf("reconcile %s: %+v %v", port, outcome, err)
		}
		if running, ok := agent.containerExists(research.ManagedContainer); !ok || !running {
			t.Fatal("worker not running")
		}
		before := agent.opCount("EnsureContainer")
		outcome, err = r.Reconcile(context.Background(), "node-a", desired)
		if err != nil || len(outcome.Plan.Steps) != 0 || agent.opCount("EnsureContainer") != before {
			t.Fatalf("not idempotent: %+v %v", outcome, err)
		}
	}
	outcome, err := r.Reconcile(context.Background(), "node-a", nil)
	if err != nil || !outcome.Applied {
		t.Fatalf("remove: %+v %v", outcome, err)
	}
	if _, ok := agent.containerExists(research.ManagedContainer); ok {
		t.Fatal("worker survived removal")
	}
	if agent.opCount("PullImage") == 0 || agent.opCount("StopContainer") == 0 || agent.opCount("RemoveContainer") == 0 {
		t.Fatal("ordinary container lifecycle bypassed")
	}
}
