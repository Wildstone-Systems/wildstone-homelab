package runtimedb

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/research"
)

func researchPlan(t *testing.T, db *DB, c research.Change) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"research": c})
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertPlan("owner", "", "research fixture", "medium", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func researchApply(t *testing.T, db *DB, c research.Change) research.Provider {
	t.Helper()
	if err := db.ApproveResearchPlan(researchPlan(t, db, c), "owner"); err != nil {
		t.Fatal(err)
	}
	p, err := db.SearchProvider(c.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func researchAgent(t *testing.T, db *DB) int64 {
	t.Helper()
	if err := db.UpsertModel(Model{Name: "local", Kind: ModelLocal, Provider: "ollama"}, "owner"); err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateButler(Butler{Name: "researcher", Surface: ButlerSurfaceFamily, AgentClass: AgentClassMember, Model: "local"}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func researchReady(t *testing.T, db *DB, p research.Provider) {
	t.Helper()
	probe, err := db.BeginSearchProviderTest(p.ID, p.ConfigRevision)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.RecordSearchProviderTest(p.ID, p.ConfigRevision, probe, true); err != nil {
		t.Fatal(err)
	}
}
func TestResearchMigrationAndSelectionNeverGrantTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.db")
	db, err := openThrough(t, path, "migrations/0097_agent_workspaces.sql")
	if err != nil {
		t.Fatal(err)
	}
	agent := researchAgent(t, db)
	if _, err = db.sql.Exec(`INSERT INTO butler_tool_assignments(butler,tool,assigned_at,assigned_by) VALUES(?,'node_status','2026-09-30T00:00:00Z','owner')`, agent); err != nil {
		t.Fatal(err)
	}
	if err = db.migrate(); err != nil {
		t.Fatal(err)
	}
	ps, err := db.SearchProviders()
	if err != nil || len(ps) != 0 {
		t.Fatalf("upgrade created providers: %v %v", ps, err)
	}
	if _, reason, err := db.ResolveSearchProvider(agent); err != nil || reason == "" {
		t.Fatalf("unconfigured ready: %q %v", reason, err)
	}
	config := research.Config{Name: "Public research", Kind: research.DDGS, EndpointRef: "service/ddgs"}
	p := researchApply(t, db, research.Change{Operation: "create", ProviderID: research.NewID(), Config: &config})
	if err = db.SetResearchSelection(0, &p.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, reason, err := db.ResolveSearchProvider(agent); err != nil || reason == "" {
		t.Fatalf("untested ready: %q %v", reason, err)
	}
	researchReady(t, db, p)
	if got, reason, err := db.ResolveSearchProvider(agent); err != nil || reason != "" || got.ID != p.ID {
		t.Fatalf("default: %+v %q %v", got, reason, err)
	}
	override := researchApply(t, db, research.Change{Operation: "create", ProviderID: research.NewID(), Config: &config})
	researchReady(t, db, override)
	if err = db.SetResearchSelection(agent, &override.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, reason, err := db.ResolveSearchProvider(agent); err != nil || reason != "" || got.ID != override.ID {
		t.Fatalf("override: %+v %q %v", got, reason, err)
	}
	if err = db.ApproveResearchPlan(researchPlan(t, db, research.Change{Operation: "delete", ProviderID: override.ID, ExpectedRevision: override.ConfigRevision}), "owner"); err != nil {
		t.Fatal(err)
	}
	if _, reason, err := db.ResolveSearchProvider(agent); err != nil || reason == "" {
		t.Fatalf("removed override fell back: %q %v", reason, err)
	}
	sel, err := db.ResearchSelection(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetResearchSelection(agent, nil, ""); !errors.Is(err, research.ErrConflict) {
		t.Fatalf("stale selection: %v", err)
	}
	if err = db.SetResearchSelection(agent, nil, sel.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	if got, reason, err := db.ResolveSearchProvider(agent); err != nil || reason != "" || got.ID != p.ID {
		t.Fatalf("inherit: %+v %q %v", got, reason, err)
	}
	var n int
	if err = db.sql.QueryRow(`SELECT count(*) FROM butler_tool_assignments`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("grants changed: %d %v", n, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got, reason, err := db.ResolveSearchProvider(agent); err != nil || reason != "" || got.ID != p.ID {
		t.Fatalf("restart: %+v %q %v", got, reason, err)
	}
}
func TestResearchPlansAndProbesFenceStaleResults(t *testing.T) {
	db := open(t)
	c := research.Config{Name: "Public research", Kind: research.DDGS, EndpointRef: "service/ddgs"}
	p := researchApply(t, db, research.Change{Operation: "create", ProviderID: research.NewID(), Config: &c})
	first, err := db.BeginSearchProviderTest(p.ID, p.ConfigRevision)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.BeginSearchProviderTest(p.ID, p.ConfigRevision)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.RecordSearchProviderTest(p.ID, p.ConfigRevision, first, true); !errors.Is(err, research.ErrConflict) {
		t.Fatalf("old test accepted: %v", err)
	}
	if err = db.RecordSearchProviderTest(p.ID, p.ConfigRevision, second, false); err != nil {
		t.Fatal(err)
	}
	got, _ := db.SearchProvider(p.ID)
	if got.State != "unavailable" {
		t.Fatalf("failed probe: %+v", got)
	}
	researchReady(t, db, p)
	change := research.Change{Operation: "update", ProviderID: p.ID, ExpectedRevision: p.ConfigRevision, Config: &c}
	one, two := researchPlan(t, db, change), researchPlan(t, db, change)
	if err = db.ApproveResearchPlan(one, "owner"); err != nil {
		t.Fatal(err)
	}
	if err = db.ApproveResearchPlan(two, "owner"); !errors.Is(err, research.ErrConflict) {
		t.Fatalf("stale Plan accepted: %v", err)
	}
	rec, _ := db.GetPlan(two)
	if rec.State != PlanProposed {
		t.Fatal("failed approval consumed Plan")
	}
	rec, _ = db.GetPlan(one)
	if rec.State != PlanApplied {
		t.Fatal("registry approval left infrastructure authorization")
	}
	got, _ = db.SearchProvider(p.ID)
	if got.State != "untested" || got.TestedConfigRevision != "" || got.ConfigRevision == p.ConfigRevision {
		t.Fatalf("edit retained readiness: %+v", got)
	}
	if err = db.RecordSearchProviderTest(p.ID, p.ConfigRevision, first, true); !errors.Is(err, research.ErrConflict) {
		t.Fatalf("edited config blessed: %v", err)
	}
	probe, err := db.BeginSearchProviderTest(got.ID, got.ConfigRevision)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.InvalidateSearchProvider(got.ID, got.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	if err = db.RecordSearchProviderTest(got.ID, got.ConfigRevision, probe, true); !errors.Is(err, research.ErrConflict) {
		t.Fatalf("unhealthy dependency blessed: %v", err)
	}
}

func TestManagedServiceInvalidationFencesProbeAndPreservesSelections(t *testing.T) {
	db := open(t)
	config := research.Config{Name: "Research", Kind: research.DDGS, EndpointRef: research.ManagedEndpointRef}
	p := researchApply(t, db, research.Change{Operation: "create", ProviderID: research.NewID(), Config: &config})
	agent := researchAgent(t, db)
	if err := db.SetResearchSelection(agent, &p.ID, ""); err != nil {
		t.Fatal(err)
	}
	probe, err := db.BeginSearchProviderTest(p.ID, p.ConfigRevision)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InvalidateSearchService(research.ManagedEndpointRef); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordSearchProviderTest(p.ID, p.ConfigRevision, probe, true); !errors.Is(err, research.ErrConflict) {
		t.Fatalf("stale probe revived service: %v", err)
	}
	if _, why, err := db.ResolveSearchProvider(agent); err != nil || why == "" {
		t.Fatalf("invalidated provider ready: %s %v", why, err)
	}
	selection, err := db.ResearchSelection(agent)
	if err != nil || selection.ProviderID == nil || *selection.ProviderID != p.ID {
		t.Fatal("invalidation changed selection")
	}
	researchReady(t, db, p)
	if err := db.InvalidateSearchService("service/other"); err != nil {
		t.Fatal(err)
	}
	if _, why, err := db.ResolveSearchProvider(agent); err != nil || why != "" {
		t.Fatalf("unrelated service invalidated: %s %v", why, err)
	}
	if err := db.InvalidateSearchService(research.ManagedEndpointRef); err != nil {
		t.Fatal(err)
	}
	if _, why, err := db.ResolveSearchProvider(agent); err != nil || why == "" {
		t.Fatal("successful test survived removal")
	}
}
