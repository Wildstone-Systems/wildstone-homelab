package butler

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func TestGetCapabilityStatus(t *testing.T) {
	store := newTestStore(t)
	seedResource(t, store, containerResourceWithSecret("immich"))
	seedResource(t, store, oidcClientResource("immich"))

	gw := newTestToolGateway(store, nil)
	outcome, err := gw.Dispatch(context.Background(), "get_capability_status", nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !contains(outcome.Content, "capability/immich") {
		t.Fatalf("get_capability_status missing owner grouping: %q", outcome.Content)
	}
	if !contains(outcome.Content, "Container/immich") || !contains(outcome.Content, "OidcClient/immich") {
		t.Fatalf("get_capability_status missing resources: %q", outcome.Content)
	}
}

func TestExplainResource_RedactsSecretPreservesRef(t *testing.T) {
	store := newTestStore(t)
	seedResource(t, store, containerResourceWithSecret("immich"))
	seedResource(t, store, oidcClientResource("immich"))
	gw := newTestToolGateway(store, nil)
	ctx := context.Background()

	cOut, err := gw.Dispatch(ctx, "explain_resource", map[string]any{"kind": "Container", "name": "immich"})
	if err != nil {
		t.Fatalf("Dispatch container: %v", err)
	}
	if contains(cOut.Content, planted64HexSecret) {
		t.Fatalf("explain_resource leaked the planted hex secret: %q", cOut.Content)
	}

	oOut, err := gw.Dispatch(ctx, "explain_resource", map[string]any{"kind": "OidcClient", "name": "immich"})
	if err != nil {
		t.Fatalf("Dispatch oidcclient: %v", err)
	}
	if !contains(oOut.Content, "secret/immich-db") {
		t.Fatalf("explain_resource dropped the secret reference: %q", oOut.Content)
	}
}

func TestGetHealth_RealAgentFakeDocker(t *testing.T) {
	h := startButlerGateway(t, []string{"tok"})
	agent := newButlerTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	agent.setContainer("jellyfin", true)
	agent.setHealth(true, "steady state")

	store := newTestStore(t)
	gw := newTestToolGateway(store, &AgentReader{Gateway: h.gw})
	gw.DefaultNode = "node-a"
	ctx := context.Background()

	nodeOut, err := gw.Dispatch(ctx, "get_health", map[string]any{"node": "node-a"})
	if err != nil {
		t.Fatalf("Dispatch node health: %v", err)
	}
	if !contains(nodeOut.Content, "ok=true") || !contains(nodeOut.Content, "steady state") {
		t.Fatalf("get_health node result missing expected content: %q", nodeOut.Content)
	}

	ctrOut, err := gw.Dispatch(ctx, "get_health", map[string]any{"node": "node-a", "container": "jellyfin"})
	if err != nil {
		t.Fatalf("Dispatch container health: %v", err)
	}
	if !contains(ctrOut.Content, "running") {
		t.Fatalf("get_health container result missing expected content: %q", ctrOut.Content)
	}

	absentOut, err := gw.Dispatch(ctx, "get_health", map[string]any{"node": "node-a", "container": "nope"})
	if err != nil {
		t.Fatalf("Dispatch absent container health: %v", err)
	}
	if !contains(absentOut.Content, "not running") {
		t.Fatalf("get_health absent container result missing expected content: %q", absentOut.Content)
	}
}

func TestGetPlan_PreviewOnly(t *testing.T) {
	store := newTestStore(t)
	gw := newTestToolGateway(store, nil)

	out, err := gw.Dispatch(context.Background(), "get_plan", map[string]any{
		"kind":  "Volume",
		"node":  "node-a",
		"name":  "photos-data",
		"owner": "capability/photos",
		"spec":  map[string]any{"name": "photos-data"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if out.Plan == nil {
		t.Fatalf("get_plan did not produce a Plan")
	}
	if len(out.Plan.Steps) != 1 || out.Plan.Steps[0].Action.String() != "Create" {
		t.Fatalf("get_plan produced unexpected steps: %+v", out.Plan.Steps)
	}

	// get_plan must not have written anything to the Statefile.
	refs, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("get_plan wrote to the Statefile: %+v", refs)
	}
}

func TestToolSpecs_IncludesAllToolsSorted(t *testing.T) {
	gw := newTestToolGateway(newTestStore(t), nil)
	specs := gw.ToolSpecs()
	want := []string{
		"describe_capabilities", "diagnose", "dry_run_automation", "explain_resource",
		"get_automation_schema", "get_capability_status", "get_commitment_schema",
		"get_commitments", "get_container_logs",
		"get_health", "get_plan", "get_system_overview", "propose_change",
	}
	if len(specs) != len(want) {
		t.Fatalf("ToolSpecs() = %d tools, want %d: %+v", len(specs), len(want), specs)
	}
	for i, w := range want {
		if specs[i].Name != w {
			t.Fatalf("ToolSpecs()[%d] = %q, want %q", i, specs[i].Name, w)
		}
	}
}

func TestReadOnlyToolSpecs_OmitsProposeChange(t *testing.T) {
	gw := newTestToolGateway(newTestStore(t), nil)
	for _, spec := range gw.ReadOnlyToolSpecs() {
		if spec.Name == "propose_change" {
			t.Fatalf("ReadOnlyToolSpecs() included propose_change")
		}
	}
}

func TestDispatch_TruncatesLargeContent(t *testing.T) {
	store := newTestStore(t)
	// Put all 200 fixtures, then commit ONCE: the test is about output
	// truncation, not commit history, and per-resource commits made this
	// 200 rapid add/commit cycles — slow locally (~30s) and the exact
	// pattern that hit a "invalid object ... Error building trees" race
	// in git 2.54 on the CI runner.
	for i := 0; i < 200; i++ {
		r := resource.Resource{
			ApiVersion: resource.ApiVersion,
			Kind:       resource.KindVolume,
			Metadata:   resource.Metadata{Name: "vol-with-a-very-long-name-padding-padding-padding" + strconv.Itoa(i), Owner: "system", Node: "node-a"},
			Volume:     &resource.VolumeSpec{Name: "v"},
		}
		if err := store.Put(r); err != nil {
			t.Fatalf("seed Put %d: %v", i, err)
		}
	}
	if err := store.Commit("test", "seed", "seed fixture"); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}
	gw := newTestToolGateway(store, nil)
	gw.MaxContentBytes = 500
	out, err := gw.Dispatch(context.Background(), "get_capability_status", nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(out.Content) > 600 { // bound + truncation marker slack
		t.Fatalf("Dispatch did not bound content size: %d bytes", len(out.Content))
	}
	if !contains(out.Content, "truncated") {
		t.Fatalf("truncated content missing truncation marker: %q", out.Content[:80])
	}
}
