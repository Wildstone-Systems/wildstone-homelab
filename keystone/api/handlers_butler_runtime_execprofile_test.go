package api

// H6 (#352, design doc §5.5): a bound packaged runtime's execution
// profile or processing class does not take effect in place — Hermes
// freezes a session's tool set at start — so changing either one on
// the Agent record re-renders the installed agent-hermes-<instance>
// capability and files the result as an ordinary Plan, exactly the
// reviewed flow enableCapability itself uses, and returns that Plan's
// ref so the Console can say "approval needed to restart the runtime".
// Nothing is applied here: the Owner still approves.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// hermesCatalogHarness is a household running the shipped catalog, so
// agent-hermes is installable, with a node to place its containers on.
func hermesCatalogHarness(t *testing.T) *harness {
	t.Helper()
	h := butlerConsole(t)
	c, err := catalog.LoadDir("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	h.s.Catalog = c
	h.s.APIURL = "http://100.64.0.1:9441"
	if err := h.s.DB.RecordNodeJoin("n1"); err != nil {
		t.Fatal(err)
	}
	return h
}

// hermesParams is the smallest install request agent-hermes accepts:
// every other param has a default (package.yaml).
func hermesParams(instance string) map[string]any {
	return map[string]any{
		"instance": instance, "node": "n1", "keystone_url": "http://100.64.0.1:9441",
		"model_name": "llama3.2",
	}
}

// installCapability proposes and approves a package the way the
// Console's own capability form does (POST /capabilities, then approve
// the plan it returns), and returns the approved plan's id.
func installCapability(t *testing.T, h *harness, pkg string, params map[string]any) string {
	t.Helper()
	var proposed map[string]any
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/capabilities",
		map[string]any{"package": pkg, "params": params}, &proposed); res.StatusCode != http.StatusCreated {
		t.Fatalf("proposing %s = %d %v", pkg, res.StatusCode, proposed)
	}
	planID, _ := proposed["id"].(string)
	if planID == "" {
		t.Fatalf("proposing %s came back without a plan id: %v", pkg, proposed)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+planID+"/approve", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("approving %s's install = %d", pkg, res.StatusCode)
	}
	return planID
}

// runtimeWithPlan is the runtime record plus the H6 "plan" field the
// contract type (agentapi.RuntimeRecord) does not itself carry.
type runtimeWithPlan struct {
	agentapi.RuntimeRecord
	Plan string `json:"plan"`
}

// configContentFor finds a rendered ConfigFile by name, failing the
// test if it is missing — every case here expects one.
func configContentFor(t *testing.T, rendered []resource.Resource, name string) string {
	t.Helper()
	for _, r := range rendered {
		if r.Kind == resource.KindConfigFile && r.Metadata.Name == name && r.ConfigFile != nil {
			return r.ConfigFile.Content
		}
	}
	t.Fatalf("no ConfigFile named %q among %d rendered resources", name, len(rendered))
	return ""
}

// Changing the execution profile of an Agent already running the
// packaged Hermes runtime files exactly one Plan re-rendering that
// capability with the new profile, returns its ref, and applies
// nothing: the live ConfigFile keeps the OLD profile's toolsets until
// an Owner approves.
func TestExecutionProfileChangeFilesARestartPlanForAPackagedRuntime(t *testing.T) {
	h := hermesCatalogHarness(t)
	createAgent(t, h, "health")
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/runtime",
		map[string]any{"type": "hermes"}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("PUT native->hermes = %d", res.StatusCode)
	}
	installCapability(t, h, "agent-hermes", hermesParams("health"))

	before, err := h.s.DB.ListPlans(0)
	if err != nil {
		t.Fatal(err)
	}

	var rec runtimeWithPlan
	res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/runtime",
		map[string]any{"type": "hermes", "executionProfileRef": "sandboxed"}, &rec)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT executionProfileRef=sandboxed = %d", res.StatusCode)
	}
	if rec.ExecutionProfileRef != "sandboxed" {
		t.Errorf("the runtime record's own profile = %q, want sandboxed", rec.ExecutionProfileRef)
	}
	if rec.Plan == "" {
		t.Fatal("changing a packaged runtime's execution profile filed no restart plan")
	}

	after, err := h.s.DB.ListPlans(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("plan count = %d, want %d (exactly one new plan)", len(after), len(before)+1)
	}

	planRec, err := h.s.DB.GetPlan(rec.Plan)
	if err != nil {
		t.Fatalf("the returned plan ref does not resolve: %v", err)
	}
	if planRec.State != runtimedb.PlanProposed {
		t.Errorf("restart plan state = %q, want proposed — it must not apply itself", planRec.State)
	}

	current, err := h.s.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	if content := configContentFor(t, current, "agent-hermes-health-config"); strings.Contains(content, "- terminal") {
		t.Errorf("the restart plan applied itself: the live ConfigFile already carries the sandboxed toolsets:\n%s", content)
	}
}

// Neither a native Agent nor a hermes-typed Agent with no packaged
// capability actually installed has anything for a Plan to re-render:
// the profile change succeeds and files nothing.
func TestExecutionProfileChangeWithNoPackagedRuntimeFilesNoPlan(t *testing.T) {
	for _, runtimeType := range []string{"native", "hermes"} {
		t.Run(runtimeType, func(t *testing.T) {
			h := hermesCatalogHarness(t)
			createAgent(t, h, "health")
			if runtimeType != "native" {
				if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/runtime",
					map[string]any{"type": runtimeType}, nil); res.StatusCode != http.StatusOK {
					t.Fatalf("PUT native->%s = %d", runtimeType, res.StatusCode)
				}
			}
			before, err := h.s.DB.ListPlans(0)
			if err != nil {
				t.Fatal(err)
			}

			var rec runtimeWithPlan
			res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/runtime",
				map[string]any{"type": runtimeType, "executionProfileRef": "sandboxed"}, &rec)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("PUT executionProfileRef=sandboxed = %d", res.StatusCode)
			}
			if rec.Plan != "" {
				t.Errorf("a %s runtime with no packaged capability filed a plan: %q", runtimeType, rec.Plan)
			}
			after, err := h.s.DB.ListPlans(0)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Errorf("plan count changed for a %s runtime: %d -> %d", runtimeType, len(before), len(after))
			}
		})
	}
}

// The reason a profile change needs a restart at all, decoupled from
// the HTTP plumbing: re-expanding the shipped agent-hermes package
// with a changed execution_profile actually changes the managed
// config.yaml Hermes reads, adding the sandboxed/host-admin toolsets
// (terminal, file, code_execution) that platform-only withholds.
func TestReExpandingAgentHermesWithANewExecutionProfileChangesTheManagedConfig(t *testing.T) {
	c, err := catalog.LoadDir("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := c.Get("agent-hermes", "")
	if err != nil {
		t.Fatal(err)
	}
	platformOnly := map[string]string{
		"instance": "health", "node": "n1", "keystone_url": "http://100.64.0.1:9441",
		"model_name": "llama3.2", "execution_profile": "platform-only",
	}
	before, err := capability.NewEngine().Expand(pkg, platformOnly, capability.NewCatalog(pkg).WithCurrent(nil))
	if err != nil {
		t.Fatalf("expanding at platform-only: %v", err)
	}
	sandboxed := map[string]string{}
	for k, v := range platformOnly {
		sandboxed[k] = v
	}
	sandboxed["execution_profile"] = "sandboxed"
	after, err := capability.NewEngine().Expand(pkg, sandboxed, capability.NewCatalog(pkg).WithCurrent(nil))
	if err != nil {
		t.Fatalf("expanding at sandboxed: %v", err)
	}

	beforeContent := configContentFor(t, before, "agent-hermes-health-config")
	afterContent := configContentFor(t, after, "agent-hermes-health-config")
	if strings.Contains(beforeContent, "- terminal") {
		t.Errorf("platform-only's managed config already carries the sandboxed-only toolsets:\n%s", beforeContent)
	}
	if !strings.Contains(afterContent, "- terminal") {
		t.Errorf("re-expanding with execution_profile=sandboxed did not add its toolsets to the managed config:\n%s", afterContent)
	}
}

// A newer agent-hermes in the catalog can reach an installed runtime
// (#389). The runtime record names the package and both versions, and
// upgrading files one Plan at the catalog's version, applying nothing
// until it is approved. Before this, the Runtime tab re-rendered only on
// a profile or class change and the Packages screen re-expanded the
// installed version, which found nothing to change.
func TestAPackagedRuntimeCanBeUpgradedToTheCatalogsVersion(t *testing.T) {
	h := hermesCatalogHarness(t)
	createAgent(t, h, "health")
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/runtime",
		map[string]any{"type": "hermes"}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("PUT native->hermes = %d", res.StatusCode)
	}
	pkg, err := h.s.Catalog.Get("agent-hermes", "")
	if err != nil {
		t.Fatal(err)
	}
	current := pkg.Metadata.Version

	// Installed from a catalog whose agent-hermes is an older version,
	// then the catalog moves on to the shipped one.
	shipped := h.s.Catalog
	h.s.Catalog = olderHermesCatalog(t, current, "0.0.1")
	installCapability(t, h, "agent-hermes", hermesParams("health"))
	h.s.Catalog = shipped

	type runtimeView struct {
		Package *struct {
			Name      string `json:"name"`
			Instance  string `json:"instance"`
			Installed string `json:"installedVersion"`
			Available string `json:"availableVersion"`
		} `json:"package"`
	}
	read := func() runtimeView {
		t.Helper()
		var v runtimeView
		if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butlers/health/runtime", nil, &v); res.StatusCode != http.StatusOK {
			t.Fatalf("GET runtime = %d", res.StatusCode)
		}
		if v.Package == nil {
			t.Fatal("the runtime record does not name its package")
		}
		return v
	}
	upgrade := func() (int, map[string]any) {
		t.Helper()
		var out map[string]any
		res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers/health/runtime/upgrade", map[string]any{}, &out)
		return res.StatusCode, out
	}

	if v := read(); v.Package.Name != "agent-hermes" || v.Package.Instance != "health" ||
		v.Package.Installed != "0.0.1" || v.Package.Available != current {
		t.Fatalf("package = %+v, want agent-hermes#health installed at 0.0.1, available %s", *v.Package, current)
	}

	code, out := upgrade()
	if code != http.StatusOK {
		t.Fatalf("upgrade = %d %v", code, out)
	}
	planID, _ := out["plan"].(string)
	if planID == "" || out["from"] != "0.0.1" || out["to"] != current {
		t.Fatalf("upgrade answered %v, want a plan from 0.0.1 to %s", out, current)
	}
	planRec, err := h.s.DB.GetPlan(planID)
	if err != nil {
		t.Fatal(err)
	}
	if planRec.State != runtimedb.PlanProposed {
		t.Errorf("upgrade plan state = %q, want proposed", planRec.State)
	}
	if !strings.Contains(planRec.Summary, "upgrade health's runtime") {
		t.Errorf("upgrade plan summary = %q", planRec.Summary)
	}

	// Once that is applied it is current, and there is nothing to do.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+planID+"/approve", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("approving the upgrade = %d", res.StatusCode)
	}
	if v := read(); v.Package.Installed != current {
		t.Fatalf("installed after the upgrade = %s, want %s", v.Package.Installed, current)
	}
	if code, _ := upgrade(); code != http.StatusConflict {
		t.Fatalf("upgrading a current runtime = %d, want 409", code)
	}
}

// olderHermesCatalog is the shipped catalog with agent-hermes relabelled
// as version older, so an install can be made from it.
func olderHermesCatalog(t *testing.T, current, older string) *catalog.Catalog {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../packages")); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "agent-hermes", "package.yaml")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	relabelled := strings.Replace(string(raw), "version: "+current, "version: "+older, 1)
	if relabelled == string(raw) {
		t.Fatalf("agent-hermes's manifest does not say version: %s", current)
	}
	// And renders something different, as an older version would: the
	// upgrade must have a change to make.
	changed := strings.Replace(relabelled, "max_concurrent_runs: 1", "max_concurrent_runs: 2", 1)
	if changed == relabelled {
		t.Fatal("agent-hermes's manifest no longer sets max_concurrent_runs: 1")
	}
	relabelled = changed
	if err := os.WriteFile(manifest, []byte(relabelled), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := catalog.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
