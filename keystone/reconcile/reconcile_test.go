package reconcile

// Integration-style tests: a real gateway.Gateway + the minimal real
// Wardstone-protocol test agent from testagent_test.go, exercising the
// Reconciler's op surface for real (ADR-W004 B1's "reuse the S1/E1 test
// style"). Only Docker/managed-filesystem state is faked, and it's faked
// as plain in-memory/on-disk state rather than a second HTTP layer.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

const testManagedPrefix = "/wildstone-managed"

func containerResource(name, image string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindContainer,
		Metadata:   resource.Metadata{Name: name, Owner: "system", Node: "node-a"},
		Container:  &resource.ContainerSpec{Image: image},
	}
}

func volumeResource(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindVolume,
		Metadata:   resource.Metadata{Name: name, Owner: "system", Node: "node-a"},
		Volume:     &resource.VolumeSpec{Name: name},
	}
}

func routeResource(name, host, upstream string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindRoute,
		Metadata:   resource.Metadata{Name: name, Owner: "system", Node: "node-a"},
		Route: &resource.RouteSpec{
			Host: host, Upstream: upstream, Auth: "none", Classification: "family_public",
		},
	}
}

func newReconciler(gwApplier Applier) *Reconciler {
	return &Reconciler{
		Applier:            gwApplier,
		ManagedPrefix:      testManagedPrefix,
		MaxApplyRetries:    2,
		RetryBackoff:       5 * time.Millisecond,
		HealthGateAttempts: 3,
		HealthGateBackoff:  10 * time.Millisecond,
	}
}

func TestConvergeEmptyNodeToVolumeContainerRoute(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	desired := []resource.Resource{
		volumeResource("data"),
		containerResource("app", testDigestImage),
		routeResource("app-route", "app.example.internal", upstream.Listener.Addr().String()),
	}

	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	outcome, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !outcome.Applied || outcome.PendingApproval || outcome.Rolledback || outcome.Incident != "" {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
	if outcome.Plan.OverallRisk != plan.RiskLow {
		t.Fatalf("expected Low risk for an all-additive plan, got %v", outcome.Plan.OverallRisk)
	}
	// The reconciler must pull before ensuring: a digest-pinned image is
	// never resolved implicitly by Docker, so on a fresh node a container
	// create without a preceding PullImage can never succeed (found by
	// the first real VM install).
	if agent.opCount("PullImage") < 1 {
		t.Fatal("container was ensured without a preceding PullImage")
	}
	if len(outcome.Plan.Steps) != 3 {
		t.Fatalf("expected 3 create steps, got %d: %+v", len(outcome.Plan.Steps), outcome.Plan.Steps)
	}

	if !agent.volumeExists("data") {
		t.Error("volume was not created in the fake")
	}
	if running, ok := agent.containerExists("app"); !ok || !running {
		t.Errorf("container was not created+running in the fake: ok=%v running=%v", ok, running)
	}
	frag := agent.readFragmentForTest(t, "app-route")
	if len(frag) == 0 {
		t.Fatal("route fragment was not written")
	}
	want := renderRouteFragment(desired[2].Route, DefaultAutheliaAddr, terminatedUpstream)
	if string(frag) != string(want) {
		t.Errorf("route fragment mismatch:\ngot:  %q\nwant: %q", frag, want)
	}
}

func TestReconcileIdenticalIsNoOp(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	desired := []resource.Resource{
		volumeResource("data"),
		containerResource("app", testDigestImage),
		routeResource("app-route", "app.example.internal", upstream.Listener.Addr().String()),
	}

	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	first, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil || !first.Applied {
		t.Fatalf("first reconcile: outcome=%+v err=%v", first, err)
	}

	ensureContainerBefore := agent.opCount("EnsureContainer")
	ensureVolumeBefore := agent.opCount("EnsureVolume")
	writeFileBefore := agent.opCount("WriteManagedFile")

	second, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if !second.Applied {
		t.Fatalf("second reconcile not applied: %+v", second)
	}
	if len(second.Plan.Steps) != 0 {
		t.Fatalf("re-reconcile of identical desired state produced steps: %+v", second.Plan.Steps)
	}
	if got := agent.opCount("EnsureContainer"); got != ensureContainerBefore {
		t.Errorf("EnsureContainer called again on a no-op reconcile: before=%d after=%d", ensureContainerBefore, got)
	}
	if got := agent.opCount("EnsureVolume"); got != ensureVolumeBefore {
		t.Errorf("EnsureVolume called again on a no-op reconcile: before=%d after=%d", ensureVolumeBefore, got)
	}
	if got := agent.opCount("WriteManagedFile"); got != writeFileBefore {
		t.Errorf("WriteManagedFile called again on a no-op reconcile: before=%d after=%d", writeFileBefore, got)
	}
}

func TestOutOfBandDriftIsRepaired(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	desired := []resource.Resource{containerResource("app", testDigestImage)}
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	if outcome, err := r.Reconcile(context.Background(), "node-a", desired); err != nil || !outcome.Applied {
		t.Fatalf("initial reconcile: outcome=%+v err=%v", outcome, err)
	}

	// Out-of-band removal: the container vanishes without Keystone's
	// involvement (e.g. `docker rm -f` run by hand on the node).
	agent.killContainer("app")
	if _, ok := agent.containerExists("app"); ok {
		t.Fatal("test setup: container still present after killContainer")
	}

	outcome, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("repair reconcile: %v", err)
	}
	if len(outcome.Plan.Steps) != 1 || outcome.Plan.Steps[0].Action != plan.ActionCreate {
		t.Fatalf("expected a single repair Create step, got: %+v", outcome.Plan.Steps)
	}
	if !outcome.Applied {
		t.Fatalf("repair was not applied: %+v", outcome)
	}
	if running, ok := agent.containerExists("app"); !ok || !running {
		t.Errorf("container was not repaired: ok=%v running=%v", ok, running)
	}
}

func TestVolumeDeleteNotAutoApplied(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	r := newReconciler(&GatewayApplier{Gateway: h.gw}) // Approve is nil: default-deny
	desired := []resource.Resource{volumeResource("data")}
	if outcome, err := r.Reconcile(context.Background(), "node-a", desired); err != nil || !outcome.Applied {
		t.Fatalf("initial reconcile: outcome=%+v err=%v", outcome, err)
	}
	if !agent.volumeExists("data") {
		t.Fatal("test setup: volume was not created")
	}

	// Remove the volume from desired: diff should propose a Delete.
	outcome, err := r.Reconcile(context.Background(), "node-a", nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !outcome.PendingApproval {
		t.Fatalf("expected PendingApproval for a High-risk volume delete, got: %+v", outcome)
	}
	if outcome.Applied {
		t.Fatal("volume delete plan must not report Applied")
	}
	if outcome.Plan.OverallRisk != plan.RiskHigh {
		t.Fatalf("expected High overall risk, got %v", outcome.Plan.OverallRisk)
	}
	if !agent.volumeExists("data") {
		t.Fatal("volume was deleted despite no approval — data-safety violation")
	}
}

// TestVolumeDeleteRefusedEvenIfApproved is a defense-in-depth check on the
// apply-time guard: even a caller that WOULD approve a Volume delete gets
// refused, because the Wardstone op surface has no arm to delete a volume
// with at all (ADR-W004 A4/S2b spec: "reached apply... refused as a bug").
func TestVolumeDeleteRefusedEvenIfApproved(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	desired := []resource.Resource{volumeResource("data")}
	if outcome, err := r.Reconcile(context.Background(), "node-a", desired); err != nil || !outcome.Applied {
		t.Fatalf("initial reconcile: outcome=%+v err=%v", outcome, err)
	}

	r.Approve = func(*plan.Plan) bool { return true } // would approve anything
	outcome, err := r.Reconcile(context.Background(), "node-a", nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if outcome.PendingApproval {
		t.Fatal("Approve returned true; should not be PendingApproval")
	}
	if outcome.Applied {
		t.Fatal("a Volume Delete must never report Applied, even approved")
	}
	if outcome.Incident == "" {
		t.Fatal("expected an Incident explaining the refusal")
	}
	if !agent.volumeExists("data") {
		t.Fatal("volume was deleted — the apply-time safety guard did not hold")
	}
}

func TestHealthGateFailureRollsBackRoute(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	// Nothing listens here: ProbeHttp will never succeed.
	unreachable, err := unreachableAddr()
	if err != nil {
		t.Fatal(err)
	}

	desired := []resource.Resource{
		volumeResource("data"),
		containerResource("app", testDigestImage),
		routeResource("app-route", "app.example.internal", unreachable),
	}
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	r.HealthGateAttempts = 2
	r.HealthGateBackoff = 5 * time.Millisecond

	outcome, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !outcome.Rolledback {
		t.Fatalf("expected Rolledback, got: %+v", outcome)
	}
	if outcome.Applied || outcome.Incident != "" {
		t.Fatalf("Rolledback outcome should not also be Applied or carry an Incident: %+v", outcome)
	}
	// Volume and Container (ordered before Route) DID apply and pass
	// their own gates — S2b's rollback contract reverses only the step
	// whose OWN gate failed and halts, it does not tear down already-
	// healthy earlier steps.
	if !agent.volumeExists("data") {
		t.Error("volume should still exist: it applied and gated before the Route step failed")
	}
	if running, ok := agent.containerExists("app"); !ok || !running {
		t.Error("container should still exist and be running: it applied and gated before the Route step failed")
	}
	if len(outcome.Steps) != 3 {
		t.Fatalf("expected exactly 3 attempted steps (volume, container, route), got %d: %+v", len(outcome.Steps), outcome.Steps)
	}
	routeStep := outcome.Steps[2]
	if !routeStep.Applied || routeStep.HealthOK || !routeStep.Reversed {
		t.Fatalf("route step result: %+v", routeStep)
	}

	// Reverse restored the fragment to its prior (absent) state — since
	// there's no delete-file op, "restored" means written back empty
	// (documented in reconcile.go), not a re-appearance of NOT_FOUND.
	readBack := agent.readFragmentForTest(t, "app-route")
	if len(readBack) != 0 {
		t.Errorf("expected the Route fragment to be rolled back to empty, got %q", readBack)
	}
}

func TestForeignDriftNeverPlannedAsDelete(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	desired := []resource.Resource{containerResource("shared-name", testDigestImage)}
	if outcome, err := r.Reconcile(context.Background(), "node-a", desired); err != nil || !outcome.Applied {
		t.Fatalf("initial reconcile: outcome=%+v err=%v", outcome, err)
	}

	// Out-of-band: the wildstone-owned container is replaced by a foreign
	// one under the same name (e.g. someone manually ran a container with
	// that name after removing wildstone's).
	agent.killContainer("shared-name")
	agent.addForeignContainer("shared-name")

	removeBefore := agent.opCount("RemoveContainer")
	stopBefore := agent.opCount("StopContainer")

	// No longer desired: naively this should be a Delete, but the name is
	// now foreign, so it must never be planned or applied as one.
	outcome, err := r.Reconcile(context.Background(), "node-a", nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	found := false
	for _, name := range outcome.ForeignDrift {
		if name == "shared-name" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected ForeignDrift to include %q, got %v", "shared-name", outcome.ForeignDrift)
	}
	for _, step := range outcome.Plan.Steps {
		if step.Kind == "Container" && step.Name == "shared-name" {
			t.Fatalf("a foreign object must never appear in the plan at all, got step: %+v", step)
		}
	}
	if got := agent.opCount("RemoveContainer"); got != removeBefore {
		t.Errorf("RemoveContainer was called against the foreign object: before=%d after=%d", removeBefore, got)
	}
	if got := agent.opCount("StopContainer"); got != stopBefore {
		t.Errorf("StopContainer was called against the foreign object: before=%d after=%d", stopBefore, got)
	}
	if running, ok := agent.containerExists("shared-name"); !ok || !running {
		t.Error("the foreign container should be untouched (still present and running)")
	}
}
