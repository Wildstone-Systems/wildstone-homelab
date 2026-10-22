package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/catalog"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/research"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func TestManagedDDGSOwnerPlanLifecycle(t *testing.T) {
	h := newHarness(t)
	c, err := catalog.LoadDir("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	h.s.Catalog = c
	h.s.NodeID = "n1"
	if h.s.ResearchTest != nil {
		t.Fatal("production probe unexpectedly active")
	}
	if _, err := h.s.ResolveManagedResearchEndpoint(context.Background(), research.ManagedEndpointRef); err == nil {
		t.Fatal("absent service resolved")
	}
	params := map[string]any{"node": "n1", "worker_image": "registry.example.internal/ddgs@sha256:" + strings.Repeat("1", 64), "port": "18081"}
	var proposed map[string]any
	if res := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/capabilities", map[string]any{"package": research.ManagedPackage, "params": params}, &proposed); res.StatusCode != http.StatusCreated {
		t.Fatalf("propose: %d %v", res.StatusCode, proposed)
	}
	if _, err := h.s.ResolveManagedResearchEndpoint(context.Background(), research.ManagedEndpointRef); err == nil {
		t.Fatal("proposal installed service")
	}
	id := proposed["id"].(string)
	for _, role := range []runtimedb.Role{runtimedb.RoleButler, runtimedb.RoleOperator} {
		if res := h.do(role, "POST", "/api/v1/plans/"+id+"/approve", nil, nil); res.StatusCode != http.StatusForbidden {
			t.Fatalf("machine approved: %d", res.StatusCode)
		}
	}
	if res := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/plans/"+id+"/approve", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("approve: %d", res.StatusCode)
	}
	if endpoint, err := h.s.ResolveManagedResearchEndpoint(context.Background(), research.ManagedEndpointRef); err != nil || endpoint != "http://127.0.0.1:18081" {
		t.Fatalf("endpoint: %s %v", endpoint, err)
	}
	if ps, err := h.s.DB.SearchProviders(); err != nil || len(ps) != 0 {
		t.Fatal("package created provider configuration")
	}
	// Simulate an explicit, previously successful Owner test. This fixture does
	// not claim an upstream call, image build or live reconciliation succeeded.
	config := research.Config{Name: "Fixture", Kind: research.DDGS, EndpointRef: research.ManagedEndpointRef}
	providerID := research.NewID()
	raw, err := json.Marshal(map[string]any{"research": research.Change{Operation: "create", ProviderID: providerID, Config: &config}})
	if err != nil {
		t.Fatal(err)
	}
	providerPlan, err := h.s.DB.InsertPlan("owner", "", "fixture", "medium", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.ApproveResearchPlan(providerPlan, "owner"); err != nil {
		t.Fatal(err)
	}
	markReady := func() {
		t.Helper()
		p, err := h.s.DB.SearchProvider(providerID)
		if err != nil {
			t.Fatal(err)
		}
		probe, err := h.s.DB.BeginSearchProviderTest(p.ID, p.ConfigRevision)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.RecordSearchProviderTest(p.ID, p.ConfigRevision, probe, true); err != nil {
			t.Fatal(err)
		}
	}
	assertUnavailable := func() {
		t.Helper()
		p, err := h.s.DB.SearchProvider(providerID)
		if err != nil || p.State != "unavailable" || p.TestedConfigRevision != "" {
			t.Fatalf("stale readiness: %+v %v", p, err)
		}
	}
	markReady()
	params["port"] = "18082"
	installCapability(t, h, research.ManagedPackage, params)
	assertUnavailable()
	if endpoint, err := h.s.ResolveManagedResearchEndpoint(context.Background(), research.ManagedEndpointRef); err != nil || endpoint != "http://127.0.0.1:18082" {
		t.Fatalf("reconfigure: %s %v", endpoint, err)
	}
	markReady()
	if res := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/capabilities/disable", map[string]any{"package": research.ManagedPackage}, &proposed); res.StatusCode != http.StatusCreated {
		t.Fatalf("remove propose: %d %v", res.StatusCode, proposed)
	}
	p, _ := h.s.DB.SearchProvider(providerID)
	if p.State != "ready" {
		t.Fatal("unapproved removal changed readiness")
	}
	if res := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/plans/"+proposed["id"].(string)+"/approve", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("remove approve: %d", res.StatusCode)
	}
	assertUnavailable()
	if _, err := h.s.ResolveManagedResearchEndpoint(context.Background(), research.ManagedEndpointRef); err == nil {
		t.Fatal("removed service resolved")
	}
	params["node"] = "n2"
	if res := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/capabilities", map[string]any{"package": research.ManagedPackage, "params": params}, nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong node accepted: %d", res.StatusCode)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.s.ResolveManagedResearchEndpoint(ctx, research.ManagedEndpointRef); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
}
