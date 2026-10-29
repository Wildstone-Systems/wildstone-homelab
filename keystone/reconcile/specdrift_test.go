package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
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

func TestForgedCurrentLabelCannotHideHardeningDrift(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	gid := int64(1001)
	want := containerResource("app", testDigestImage)
	want.Container.RuntimeIdentity = &resource.ContainerRuntimeIdentity{UID: 1000, GID: &gid}
	want.Container.ReadOnlyRootfs = true
	want.Container.CapDrop = []string{"ALL"}
	want.Container.NoNewPrivileges = true
	want.Container.Resources = &resource.ContainerResources{Memory: "512m", Cpus: "0.5", PidsLimit: 128}
	want.Container.ShmSize = "1g"
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	if first, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{want}); err != nil || !first.Applied {
		t.Fatalf("first reconcile: outcome=%+v err=%v", first, err)
	}

	agent.mu.Lock()
	c := agent.containers["app"]
	currentHash := c.labels[specHashLabel]
	agent.mu.Unlock()
	tests := map[string]func(*fakeContainer){
		"runtime uid":       func(c *fakeContainer) { c.runtimeIdentity.Uid++ },
		"runtime gid":       func(c *fakeContainer) { *c.runtimeIdentity.Gid++ },
		"read-only rootfs":  func(c *fakeContainer) { c.readOnlyRootfs = false },
		"capability drop":   func(c *fakeContainer) { c.capDrop = []string{"NET_RAW"} },
		"no-new-privileges": func(c *fakeContainer) { c.noNewPrivileges = false },
		"memory":            func(c *fakeContainer) { c.resources.Memory = "256m" },
		"cpu":               func(c *fakeContainer) { c.resources.Cpus = "1" },
		"pid":               func(c *fakeContainer) { c.resources.PidsLimit = 64 },
		"shm":               func(c *fakeContainer) { c.shmSize = "512m" },
	}
	for name, mutate := range tests {
		agent.mu.Lock()
		gidCopy := int64(1001)
		c.runtimeIdentity = &wsv1.ContainerRuntimeIdentity{Uid: 1000, Gid: &gidCopy}
		c.readOnlyRootfs, c.capDrop, c.noNewPrivileges = true, []string{"ALL"}, true
		c.resources = &wsv1.ContainerResources{Memory: "512m", Cpus: "0.5", PidsLimit: 128}
		c.shmSize = "1g"
		mutate(c)
		if c.labels[specHashLabel] != currentHash {
			t.Fatal("test changed the forged-current label")
		}
		agent.mu.Unlock()
		out, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{want})
		if err != nil || len(out.Plan.Steps) != 1 || out.Plan.Steps[0].Action != plan.ActionUpdate {
			t.Errorf("%s drift: outcome=%+v err=%v", name, out, err)
		}
	}
}

func TestRuntimeIdentityLifecycle(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	r.Approve = func(*plan.Plan) bool { return true }
	want := containerResource("app", testDigestImage)
	want.Container.RuntimeIdentity = &resource.ContainerRuntimeIdentity{UID: 1000}
	if out, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{want}); err != nil || !out.Applied {
		t.Fatalf("create: outcome=%+v err=%v", out, err)
	}
	if out, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{want}); err != nil || len(out.Plan.Steps) != 0 {
		t.Fatalf("healthy observation: outcome=%+v err=%v", out, err)
	}

	// A stopped but otherwise matching container restarts through the approved
	// Update path without losing its pinned identity.
	agent.mu.Lock()
	agent.containers["app"].running = false
	agent.mu.Unlock()
	if out, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{want}); err != nil || !out.Applied {
		t.Fatalf("restart: outcome=%+v err=%v", out, err)
	}

	changed := containerResource("app", testDigestImage)
	changed.Container.RuntimeIdentity = &resource.ContainerRuntimeIdentity{UID: 1001}
	if out, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{changed}); err != nil || !out.Applied || len(out.Plan.Steps) != 1 {
		t.Fatalf("approved identity update: outcome=%+v err=%v", out, err)
	}
	agent.mu.Lock()
	gotUID := agent.containers["app"].runtimeIdentity.GetUid()
	agent.mu.Unlock()
	if gotUID != 1001 {
		t.Fatalf("updated uid = %d, want 1001", gotUID)
	}
	if out, err := r.Reconcile(context.Background(), "node-a", nil); err != nil || !out.Applied {
		t.Fatalf("delete: outcome=%+v err=%v", out, err)
	}
	if _, exists := agent.containerExists("app"); exists {
		t.Fatal("container still exists after approved delete")
	}
}
