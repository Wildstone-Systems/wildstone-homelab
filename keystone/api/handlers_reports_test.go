package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/report"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func TestMorningReportEndpoint(t *testing.T) {
	h := newHarness(t)
	if err := h.s.DB.UpsertObserved(runtimedb.Observed{
		Kind: "Container", Name: "immich-postgres", Healthy: false, Detail: "exited",
	}); err != nil {
		t.Fatalf("UpsertObserved: %v", err)
	}

	var rep report.Report
	resp := h.do(runtimedb.RoleButler, "GET", "/api/v1/reports/morning", nil, &rep)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(rep.Unhealthy) != 1 || rep.Unhealthy[0].Name != "immich-postgres" {
		t.Fatalf("unhealthy = %+v", rep.Unhealthy)
	}
	if !strings.Contains(rep.Summary, "1 unhealthy") {
		t.Fatalf("summary = %q", rep.Summary)
	}
}
