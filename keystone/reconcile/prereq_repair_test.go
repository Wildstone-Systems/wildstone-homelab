package reconcile

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
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
	if string(got) != string(renderBaseCaddyfile(terminatedUpstream)) {
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

// TestDeclaredDashboardSettingsSurviveRepair pins #410: a household using
// the native dashboard declares its own ConfigFile at the exact path the
// generic Homepage renderer also writes to (dashboard/settings.yaml).
// Before the fix, repairNodePrereqs unconditionally rewrote that path on
// every tick — BEFORE observe runs — so the declared file was clobbered
// the moment it landed, observe then saw the ConfigFile as drifted
// (RiskMedium, requires approval), and no approval could ever converge it:
// approving the repair only restored the file the very next tick
// immediately overwrote again.
func TestDeclaredDashboardSettingsSurviveRepair(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	rec := newReconciler(&GatewayApplier{Gateway: h.gw})

	settings := configFile("dashboard-settings", "capability/dashboard", "settings.yaml",
		"householdName: The Smiths\naccentHue: 210\nsearchEngine: duckduckgo\n")
	desired := []resource.Resource{settings}

	if _, err := rec.Reconcile(t.Context(), "node-a", desired); err != nil {
		t.Fatalf("first tick: %v", err)
	}

	settingsPath := managedPathForTest(agent, homepageSettingsPath(testManagedPrefix))
	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != settings.ConfigFile.Content {
		t.Fatalf("declared dashboard settings not on disk after the first tick:\n%s", got)
	}

	// Three more ticks with no approval of anything: the file the
	// household declared must simply stay converged, not oscillate
	// between the declared content and Homepage's own generated chrome.
	for tick := 2; tick <= 4; tick++ {
		out, err := rec.Reconcile(t.Context(), "node-a", desired)
		if err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		for _, p := range out.PrereqRepaired {
			if p == homepageSettingsPath(testManagedPrefix) {
				t.Fatalf("tick %d: the prereq writer overwrote the declared dashboard settings", tick)
			}
		}
		if out.PendingApproval {
			t.Fatalf("tick %d: a drift proposal was manufactured for a file only Keystone itself was changing", tick)
		}
		got, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != settings.ConfigFile.Content {
			t.Fatalf("tick %d: declared dashboard settings overwritten:\n%s", tick, got)
		}
	}
}

// TestRelinquishingADashboardOverrideRestoresTheGeneratedFallback covers
// the handoff in the other direction, raised in review of #410's fix: a
// declared ConfigFile withdrawn from desired state is a Delete step, and
// a Delete ordinarily clears its path to empty (applyConfigFileStep — no
// delete-file op exists). But the SAME tick's prereq repair runs before
// observe/plan/apply and, the instant the claim disappears from desired
// state, already restores Homepage's own generated settings at that
// exact path. Clearing it in the Delete step that follows would throw
// that restore away and leave the household's dashboard reading a blank
// settings file until the next tick repairs it again. The Delete step
// must hand back the regenerated fallback instead.
func TestRelinquishingADashboardOverrideRestoresTheGeneratedFallback(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	rec := newReconciler(&GatewayApplier{Gateway: h.gw})
	// The Delete step for a ConfigFile grades Medium risk; auto-approve
	// so the handoff completes within the one tick this test checks.
	rec.Approve = func(*plan.Plan) bool { return true }

	settings := configFile("dashboard-settings", "capability/dashboard", "settings.yaml",
		"householdName: The Smiths\naccentHue: 210\nsearchEngine: duckduckgo\n")
	if _, err := rec.Reconcile(t.Context(), "node-a", []resource.Resource{settings}); err != nil {
		t.Fatalf("first tick: %v", err)
	}

	// The override is withdrawn: desired state no longer declares the
	// ConfigFile.
	out, err := rec.Reconcile(t.Context(), "node-a", nil)
	if err != nil {
		t.Fatalf("handoff tick: %v", err)
	}
	if !out.Applied {
		t.Fatalf("the Delete step relinquishing the override was not applied: %+v", out)
	}

	settingsPath := managedPathForTest(agent, homepageSettingsPath(testManagedPrefix))
	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := string(renderHomepageSettings(nil)); string(got) != want {
		t.Fatalf("after relinquishing the override, settings.yaml = %q, want the regenerated fallback %q", got, want)
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
