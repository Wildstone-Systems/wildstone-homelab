package reconcile

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// managedPathForTest maps a managed path to its on-disk location under the
// test agent's temp prefix.
func managedPathForTest(a *testAgent, managed string) string {
	return filepath.Join(a.managed, filepath.FromSlash(managed))
}

// TestPrereqFilesAreRepairedOnATick pins F34, and it is the security-
// relevant half: the base Caddyfile carries the ":80 { abort }" default
// deny that closed F26. It was written only by ensureNodePrereqs, which
// runs immediately before a Container ensure and nowhere else, and it is
// not a resource, so drift detection could not see it either. Deleting
// the deny block brought F26 back and nothing repaired it across
// multiple ticks — recovery required deploying some container, which is
// no kind of answer to "my front door stopped denying unknown hosts".
func TestPrereqFilesAreRepairedOnATick(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	rec := newReconciler(&GatewayApplier{Gateway: h.gw})

	// A tick with no resources at all must still assert the prereqs.
	out, err := rec.Reconcile(t.Context(), "node-a", nil)
	if err != nil {
		t.Fatalf("first tick: %v", err)
	}
	base := managedPathForTest(agent, baseCaddyfilePath(testManagedPrefix))
	placeholder := managedPathForTest(agent, placeholderFragmentPath(testManagedPrefix))
	// The merged Authelia clients file is a prereq for the same reason
	// the other two are: its correct contents are a pure function of
	// desired state, so nothing but this repair asserts them.
	clients := managedPathForTest(agent, autheliaClientsPath(testManagedPrefix))
	// And the merged LLDAP group list, for the same reason again.
	groups := managedPathForTest(agent, lldapGroupsPath(testManagedPrefix))
	for _, p := range []string{base, placeholder, clients, groups} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("prereq %s not written on a resourceless tick: %v", p, err)
		}
	}
	// Counted from the list rather than restated. nodePrereqFiles was
	// extracted so a test could do exactly this; hardcoding the number
	// here meant adding a prereq broke the test that was supposed to
	// bound it, instead of covering it.
	if want := len(nodePrereqFiles(testManagedPrefix, nil)); len(out.PrereqRepaired) != want {
		t.Fatalf("first tick repaired %v, want all %d prereq files written", out.PrereqRepaired, want)
	}

	// A second tick with everything intact must be a no-op: repair that
	// rewrites unconditionally is churn, and would make the event below
	// meaningless.
	out, err = rec.Reconcile(t.Context(), "node-a", nil)
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if len(out.PrereqRepaired) != 0 {
		t.Fatalf("intact files reported as repaired: %v", out.PrereqRepaired)
	}

	// Now the reported tamper: strip the default-deny block.
	if err := os.WriteFile(base, []byte("{\n\tadmin off\n}\n\nimport routes/*.caddy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// And empty the placeholder, which is what made F30 come back and stay.
	if err := os.WriteFile(placeholder, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err = rec.Reconcile(t.Context(), "node-a", nil)
	if err != nil {
		t.Fatalf("repair tick: %v", err)
	}
	if len(out.PrereqRepaired) != 2 {
		t.Fatalf("repair tick reported %v, want both files repaired", out.PrereqRepaired)
	}
	got, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(renderBaseCaddyfile(defaultTrustedProxies)) {
		t.Errorf("base Caddyfile not restored:\n%s", got)
	}
	gotPh, err := os.ReadFile(placeholder)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotPh) != string(renderPlaceholderFragment()) {
		t.Errorf("placeholder not restored:\n%s", gotPh)
	}
}

// TestReservedRouteNameIsRefused pins F35. The placeholder fragment lives
// at a fixed path with no reservation on the Route name that renders
// there, so a Route could claim it: it applied and served, the next
// container ensure overwrote the fragment with the placeholder, the tick
// after raised a drift plan, approving that rewrote it, and the next
// container ensure clobbered it again — two writers oscillating.
func TestReservedRouteNameIsRefused(t *testing.T) {
	r := routeResource(resource.ReservedRouteName, "collide.example.internal", "container/x:80")
	err := r.Validate()
	if err == nil {
		t.Fatal("a Route named the reserved placeholder validated; it will oscillate with the reconciler")
	}
	// And the reconciler must render to that same name, or the
	// reservation protects the wrong path.
	if placeholderFragmentPath(testManagedPrefix) != routeFragmentPath(testManagedPrefix, resource.ReservedRouteName) {
		t.Error("placeholder path and reserved name have drifted apart")
	}
	// An ordinary Route is unaffected.
	ok := routeResource("kuma", "kuma.example.internal", "container/kuma:3001")
	if err := ok.Validate(); err != nil {
		t.Errorf("ordinary Route rejected: %v", err)
	}
}
