package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// F61: a container removed from desired state must actually stop.
//
// If it does not, it keeps its published ports, so a replacement binding
// the same port cannot start, and it keeps reloading managed config it
// no longer owns. During the F60 outage an orphan of exactly this kind
// was what accidentally kept the ingress serving, which is not a
// defence.
func TestRemovingAContainerFromDesiredStopsIt(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	desired := []resource.Resource{containerResource("app", testDigestImage)}
	if _, err := r.Reconcile(context.Background(), "node-a", desired); err != nil {
		t.Fatal(err)
	}
	if running, ok := agent.containerExists("app"); !ok || !running {
		t.Fatalf("setup: container not running: ok=%v running=%v", ok, running)
	}

	// The operator removes it, and approves the removal: a delete is
	// High risk and waits for a human by design (ADR-W004).
	r.Approve = func(*plan.Plan) bool { return true }
	outcome, err := r.Reconcile(context.Background(), "node-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Applied {
		t.Fatalf("removal did not apply: %+v", outcome)
	}
	if agent.opCount("StopContainer") < 1 {
		t.Error("the container was removed from desired state without ever being stopped")
	}
	if running, ok := agent.containerExists("app"); ok {
		t.Errorf("a container removed from desired state is still there: running=%v", running)
	}
}

// The same, across a restart. Keystone's record of what it manages is
// rebuilt from the observed rows (F76); if a container removed from
// desired while Keystone was down is not in that record, no delete is
// ever planned and it runs forever — which is how F61 was actually hit.
func TestAContainerRemovedWhileKeystoneWasDownIsStillStopped(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	if _, err := r.Reconcile(context.Background(), "node-a",
		[]resource.Resource{containerResource("app", testDigestImage)}); err != nil {
		t.Fatal(err)
	}

	// Keystone restarts: a new reconciler, seeded from the observed rows
	// the old one wrote, and a desired state that no longer has "app".
	restarted := newReconciler(&GatewayApplier{Gateway: h.gw})
	restarted.Approve = func(*plan.Plan) bool { return true }
	restarted.SeedManaged("node-a", []ManagedRef{{Kind: resource.KindContainer, Name: "app"}})
	if _, err := restarted.Reconcile(context.Background(), "node-a", nil); err != nil {
		t.Fatal(err)
	}
	if running, ok := agent.containerExists("app"); ok {
		t.Errorf("a container removed from desired state while Keystone was down is still running "+
			"(running=%v); it holds its published ports against any replacement", running)
	}
}
