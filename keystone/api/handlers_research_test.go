package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/research"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func researchRequest(t *testing.T, h *harness, method, path string, body any, user string, at time.Time, site string) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(TrustedIngressHeader, user)
	req.Header.Set(ConsoleGroupsHeader, ConsoleGroup)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", site)
	req.AddCookie(sessionCookie(h.s.StartConsoleSession(user, at)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err = json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}
func TestResearchOwnerPlanLifecycle(t *testing.T) {
	h := freshnessHarness(t)
	call := func(method, path string, body any) map[string]any {
		t.Helper()
		code, out := researchRequest(t, h, method, path, body, "mika", time.Now(), "same-origin")
		if code < 200 || code >= 300 {
			t.Fatalf("%s %s: %d %v", method, path, code, out)
		}
		return out
	}
	req := map[string]any{"config": map[string]any{"name": "Public research", "kind": "ddgs", "endpointRef": "service/ddgs", "credentialRef": "secret/ddgs"}}
	proposed := call("POST", "/api/v1/research/providers", req)
	provider := proposed["providerId"].(string)
	planID := proposed["planId"].(string)
	if ps, err := h.s.DB.SearchProviders(); err != nil || len(ps) != 0 {
		t.Fatalf("proposal mutated registry: %v %v", ps, err)
	}
	for _, role := range []runtimedb.Role{runtimedb.RoleButler, runtimedb.RoleOperator, runtimedb.RoleAdmin} {
		var detail map[string]any
		if resp := h.do(role, "GET", "/api/v1/plans/"+planID, nil, &detail); resp.StatusCode != http.StatusForbidden || detail["research"] != nil {
			t.Fatalf("non-Owner %s read research Plan: %d %v", role, resp.StatusCode, detail)
		}
		if resp := h.do(role, "POST", "/api/v1/plans/"+planID+"/reject", nil, &detail); resp.StatusCode != http.StatusForbidden || detail["research"] != nil {
			t.Fatalf("non-Owner %s rejected/read research Plan: %d %v", role, resp.StatusCode, detail)
		}
		if resp := h.do(role, "POST", "/api/v1/plans/"+planID+"/approve", nil, nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("bearer %s approved: %d", role, resp.StatusCode)
		}
	}
	view := call("GET", "/api/v1/plans/"+planID, nil)
	if view["research"] == nil {
		t.Fatal("Plan review omitted configuration")
	}
	call("POST", "/api/v1/plans/"+planID+"/approve", map[string]any{})
	for _, role := range []runtimedb.Role{runtimedb.RoleButler, runtimedb.RoleOperator, runtimedb.RoleAdmin} {
		if resp := h.do(role, "GET", "/api/v1/plans/"+planID, nil, nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("non-Owner read applied research Plan: %d", resp.StatusCode)
		}
	}
	p, err := h.s.DB.SearchProvider(provider)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "untested" {
		t.Fatalf("created ready: %+v", p)
	}
	path := "/api/v1/research/providers/" + provider
	testReq := map[string]any{"expectedConfigRevision": p.ConfigRevision}
	out := call("POST", path+"/test", testReq)
	if out["state"] != "unavailable" {
		t.Fatalf("nil seam: %v", out)
	}
	h.s.ResearchTest = func(ctx context.Context, p research.Provider) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("unbounded test")
		}
		if p.EndpointRef != "service/ddgs" {
			t.Error("unexpected stored config")
		}
		return nil
	}
	out = call("POST", path+"/test", testReq)
	if out["state"] != "ready" || out["testedConfigRevision"] != p.ConfigRevision {
		t.Fatalf("probe: %v", out)
	}
	h.s.ResearchTest = func(context.Context, research.Provider) error { return errors.New("REPLACED-private-upstream-error") }
	out = call("POST", path+"/test", testReq)
	data, _ := json.Marshal(out)
	if out["state"] != "unavailable" || strings.Contains(string(data), "REPLACED-private") {
		t.Fatalf("probe leaked or left ready: %v", out)
	}
	selection := call("PUT", "/api/v1/research/default-provider", map[string]any{"providerId": provider, "expectedConfigRevision": ""})
	code, _ := researchRequest(t, h, "PUT", "/api/v1/research/default-provider", map[string]any{"providerId": nil, "expectedConfigRevision": ""}, "mika", time.Now(), "same-origin")
	if code != 409 {
		t.Fatalf("stale default: %d", code)
	}
	call("PUT", "/api/v1/research/default-provider", map[string]any{"providerId": nil, "expectedConfigRevision": selection["configRevision"]})
	update := map[string]any{"config": req["config"], "expectedConfigRevision": p.ConfigRevision}
	pending := call("PATCH", path, update)
	call("POST", "/api/v1/plans/"+pending["planId"].(string)+"/approve", map[string]any{})
	p, err = h.s.DB.SearchProvider(provider)
	if err != nil || p.State != "untested" || p.TestedConfigRevision != "" {
		t.Fatalf("update: %+v %v", p, err)
	}
	code, _ = researchRequest(t, h, "POST", path+"/test", testReq, "mika", time.Now(), "same-origin")
	if code != 409 {
		t.Fatalf("stale test: %d", code)
	}
	pending = call("DELETE", path, map[string]any{"expectedConfigRevision": p.ConfigRevision})
	call("POST", "/api/v1/plans/"+pending["planId"].(string)+"/approve", map[string]any{})
	code, _ = researchRequest(t, h, "GET", path, nil, "mika", time.Now(), "same-origin")
	if code != 404 {
		t.Fatalf("deleted: %d", code)
	}
}
func TestResearchOwnerAdmissionAndStrictInput(t *testing.T) {
	h := freshnessHarness(t)
	req := map[string]any{"config": map[string]any{"name": "Public", "kind": "ddgs"}}
	for _, tc := range []struct {
		user, site string
		at         time.Time
	}{
		{"sandy", "same-origin", time.Now()}, {"mika", "same-origin", time.Now().Add(-time.Hour)},
		{"mika", "cross-site", time.Now()}, {"mika", "", time.Now()},
	} {
		code, _ := researchRequest(t, h, "POST", "/api/v1/research/providers", req, tc.user, tc.at, tc.site)
		if code < 400 {
			t.Fatalf("admitted %+v: %d", tc, code)
		}
	}
	for _, body := range []any{
		map[string]any{"config": map[string]any{"name": "Public", "kind": "tavily"}},
		map[string]any{"config": map[string]any{"name": "Public", "kind": "ddgs", "endpointRef": "https://example.com"}},
		map[string]any{"config": map[string]any{"name": "Public", "kind": "ddgs", "apiKey": "REPLACED-secret"}},
		map[string]any{"config": map[string]any{"name": "Public", "kind": "ddgs", "state": "ready"}},
	} {
		code, out := researchRequest(t, h, "POST", "/api/v1/research/providers", body, "mika", time.Now(), "same-origin")
		if code != 400 {
			t.Fatalf("invalid config: %d %v", code, out)
		}
	}
	plans, err := h.s.DB.PlansInState(runtimedb.PlanProposed)
	if err != nil || len(plans) != 0 {
		t.Fatalf("refused input persisted: %v %v", plans, err)
	}
}

func TestResearchInFlightProbeCannotBlessEditedConfiguration(t *testing.T) {
	h := freshnessHarness(t)
	call := func(method, path string, body any) (int, map[string]any) {
		return researchRequest(t, h, method, path, body, "mika", time.Now(), "same-origin")
	}
	config := research.Config{Name: "Public", Kind: research.DDGS, EndpointRef: "service/ddgs"}
	code, out := call("POST", "/api/v1/research/providers", map[string]any{"config": config})
	if code != 202 {
		t.Fatal(code, out)
	}
	provider := out["providerId"].(string)
	code, out = call("POST", "/api/v1/plans/"+out["planId"].(string)+"/approve", map[string]any{})
	if code != 200 {
		t.Fatal(code, out)
	}
	p, err := h.s.DB.SearchProvider(provider)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/research/providers/" + provider
	code, out = call("PATCH", path, map[string]any{"config": config, "expectedConfigRevision": p.ConfigRevision})
	if code != 202 {
		t.Fatal(code, out)
	}
	edit := out["planId"].(string)
	h.s.ResearchTest = func(context.Context, research.Provider) error { return h.s.DB.ApproveResearchPlan(edit, "mika") }
	code, out = call("POST", path+"/test", map[string]any{"expectedConfigRevision": p.ConfigRevision})
	if code != 409 {
		t.Fatalf("stale success applied: %d %v", code, out)
	}
	got, err := h.s.DB.SearchProvider(provider)
	if err != nil || got.State != "untested" || got.TestedConfigRevision != "" {
		t.Fatalf("stale readiness: %+v %v", got, err)
	}
}
