package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace"
)

func workspaceHarness(t *testing.T) *harness {
	t.Helper()
	h := freshnessHarness(t)
	c, err := catalog.LoadDir("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	h.s.Catalog = c
	h.s.NodeID = "n1"
	if err := h.s.DB.RecordNodeJoin("n1"); err != nil {
		t.Fatal(err)
	}
	return h
}
func workspaceProposal(t *testing.T, h *harness, params map[string]string) runtimedb.PlanRecord {
	t.Helper()
	var rec runtimedb.PlanRecord
	if r := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/capabilities", enableRequest{Package: workspace.Package, Params: params}, &rec); r.StatusCode != 201 {
		t.Fatalf("propose = %d, %+v", r.StatusCode, rec)
	}
	return rec
}
func TestWorkspacePlansFreshnessReconfigurationAndRemoval(t *testing.T) {
	h := workspaceHarness(t)
	root := t.TempDir()
	retained := filepath.Join(root, "retained.txt")
	if err := os.WriteFile(retained, []byte("private fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	params := map[string]string{"node": "n1", "workspace_path": root}
	rec := workspaceProposal(t, h, params)
	if _, err := h.s.WorkspaceSettings(); !errors.Is(err, workspace.ErrDisabled) {
		t.Fatal("proposal activated storage", err)
	}
	stale := sessionCookie(h.s.StartConsoleSession("mika", time.Now().Add(-20*time.Minute)))
	if code, _ := h.sessionRequest("POST", "/api/v1/plans/"+rec.ID+"/approve", "", "mika", stale); code != 403 {
		t.Fatalf("stale approval = %d", code)
	}
	if r := h.do(runtimedb.RoleButler, "POST", "/api/v1/plans/"+rec.ID+"/approve", nil, nil); r.StatusCode != 403 {
		t.Fatalf("Agent approval = %d", r.StatusCode)
	}
	fresh := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	approve := func(id string, want int) {
		t.Helper()
		if code, out := h.sessionRequest("POST", "/api/v1/plans/"+id+"/approve", "", "mika", fresh); code != want {
			t.Fatalf("approval = %d %v", code, out)
		}
	}
	approve(rec.ID, 200)
	settings, err := h.s.WorkspaceSettings()
	if err != nil || settings.Root != root {
		t.Fatalf("%+v %v", settings, err)
	}
	params["quota_bytes"] = "2147483648"
	change := workspaceProposal(t, h, params)
	params["quota_bytes"] = "3221225472"
	stalePlan := workspaceProposal(t, h, params)
	before, err := h.s.WorkspaceSettings()
	if err != nil || before.Revision != settings.Revision {
		t.Fatal("unapproved config took effect", err)
	}
	approve(change.ID, 200)
	approve(stalePlan.ID, 409)
	after, err := h.s.WorkspaceSettings()
	if err != nil || after.QuotaBytes != 2147483648 || after.Revision == settings.Revision {
		t.Fatalf("config not refreshed: %+v %v", after, err)
	}
	var removed runtimedb.PlanRecord
	if r := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/capabilities/disable", map[string]string{"package": workspace.Package}, &removed); r.StatusCode != 201 {
		t.Fatalf("disable = %d", r.StatusCode)
	}
	approve(removed.ID, 200)
	if _, err := h.s.WorkspaceSettings(); !errors.Is(err, workspace.ErrDisabled) {
		t.Fatal("removal did not disable seam", err)
	}
	if body, err := os.ReadFile(retained); err != nil || string(body) != "private fixture" {
		t.Fatal("removal lost bytes", err)
	}
	params["quota_bytes"] = "1073741824"
	reinstall := workspaceProposal(t, h, params)
	approve(reinstall.ID, 200)
	if _, err := h.s.WorkspaceSettings(); err != nil {
		t.Fatal("reinstall unavailable", err)
	}
}
func TestWorkspaceBadSettingsAndMissingStorage(t *testing.T) {
	h := workspaceHarness(t)
	for _, params := range []map[string]string{
		{"node": "n2"}, {"node": "n1", "workspace_path": "relative"},
		{"node": "n1", "max_file_bytes": "10485761"}, {"node": "n1", "quota_bytes": "1.5"},
		{"node": "n1", "required_mount": "/outside"},
	} {
		if r := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/capabilities", enableRequest{Package: workspace.Package, Params: params}, nil); r.StatusCode != 400 {
			t.Fatalf("bad settings status = %d", r.StatusCode)
		}
	}
	root := filepath.Join(t.TempDir(), "not-created")
	rec := workspaceProposal(t, h, map[string]string{"node": "n1", "workspace_path": root, "required_mount": filepath.Dir(root)})
	if r := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/plans/"+rec.ID+"/approve", nil, nil); r.StatusCode != 200 {
		t.Fatalf("approval = %d", r.StatusCode)
	}
	if _, err := h.s.WorkspaceSettings(); !errors.Is(err, workspace.ErrMount) {
		t.Fatal("missing mount ready", err)
	}
	current, err := h.s.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(h.s.workspaceBlockers(current)) != 1 {
		t.Fatal("missing visible blocker")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("replacement directory created", err)
	}
}
