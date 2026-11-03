package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func coordinatorRequest(t *testing.T, h *harness, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/coordinator/promote", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestCoordinatorPromotionRequiresCurrentOwnerAuthorityAndFencesTheOldEpoch(t *testing.T) {
	h := freshnessHarness(t)
	h.s.CoordinatorHolder = "keystone-replacement"
	old, err := h.s.DB.AcquireCoordinator("keystone-original")
	if err != nil {
		t.Fatal(err)
	}
	pub := prepareVerifiedRestoreRelease(t, h)
	if _, err := h.s.DB.ReleaseRestoreQuarantine("mika", pub); err != nil {
		t.Fatal(err)
	}

	// An authenticated operator can diagnose the lease but cannot take it.
	if resp := h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/coordinator", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("operator coordinator read = %d", resp.StatusCode)
	}
	if resp := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/coordinator/promote", nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operator promotion = %d, want 403", resp.StatusCode)
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/coordinator/promote", nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("administrator bearer promotion = %d, want 403", resp.StatusCode)
	}
	if resp := h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/coordinator/promote", nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Agent promotion = %d, want 403", resp.StatusCode)
	}

	// Revocation is checked on this request, not cached when the token was
	// minted. A bearer that used to act for an Owner cannot recover authority.
	revoked := h.console(runtimedb.RoleAdmin, "former-owner")
	if err := h.s.Principals.Revoke(revoked); err != nil {
		t.Fatal(err)
	}
	resp := coordinatorRequest(t, h, revoked)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked Owner promotion = %d, want 401", resp.StatusCode)
	}
	lease, err := h.s.DB.GetCoordinator()
	if err != nil || lease.Holder != old.Holder || lease.Epoch != old.Epoch {
		t.Fatalf("refused promotion changed lease: %+v, %v", lease, err)
	}

	fresh := sessionCookie(h.s.StartConsoleSession("mika", h.s.now()))
	code, out := h.sessionRequest(http.MethodPost, "/api/v1/coordinator/promote", ``, "mika", fresh)
	if code != http.StatusOK {
		t.Fatalf("Owner promotion = %d, want 200: %+v", code, out)
	}
	recovery, ok := out["recoveryPromotion"].(map[string]any)
	if !ok || recovery["promotedBy"] != "mika" || recovery["restoreGeneration"] == "" || recovery["evidenceId"] == "" {
		t.Fatalf("recovery promotion response = %+v", out)
	}
	promoted, err := h.s.DB.GetCoordinator()
	if err != nil || promoted.Holder != "keystone-replacement" || promoted.Epoch != old.Epoch+1 || promoted.PromotedFrom != old.Holder {
		t.Fatalf("promoted lease = %+v, %v", promoted, err)
	}
	if err := h.s.DB.CheckCoordinator(old.Holder, old.Epoch); !errors.Is(err, runtimedb.ErrCoordinatorFenced) {
		t.Fatalf("old coordinator check = %v, want ErrCoordinatorFenced", err)
	}
	// This process still carries the old in-memory lease. Promotion changes
	// durable authority first; the documented restart is required before the
	// replacement can dispatch.
	if err := h.s.DB.CheckHouseholdExecution(); !errors.Is(err, runtimedb.ErrCoordinatorFenced) {
		t.Fatalf("pre-restart replacement execution = %v, want fenced", err)
	}
	if _, err := h.s.DB.AcquireCoordinator("keystone-replacement"); !errors.Is(err, runtimedb.ErrCoordinatorRestoreFenced) {
		t.Fatalf("same process acquired promoted epoch without restart: %v", err)
	}
	audit, err := h.s.DB.ListAudit(20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range audit {
		if row.Path == "/api/v1/coordinator/promote" && row.Status == http.StatusOK && row.ActingFor == "mika" {
			found = true
		}
	}
	if !found {
		t.Fatalf("successful promotion request missing from audit: %+v", audit)
	}
	code, _ = h.sessionRequest(http.MethodPost, "/api/v1/coordinator/promote", ``, "mika", fresh)
	if code != http.StatusConflict {
		t.Fatalf("repeated promotion = %d, want 409", code)
	}
}

func TestRestoredCoordinatorPromotionRefusesInactiveOwner(t *testing.T) {
	h := freshnessHarness(t)
	h.s.CoordinatorHolder = "keystone-replacement"
	if _, err := h.s.DB.AcquireCoordinator("keystone-original"); err != nil {
		t.Fatal(err)
	}
	pub := prepareVerifiedRestoreRelease(t, h)
	if _, err := h.s.DB.ReleaseRestoreQuarantine("mika", pub); err != nil {
		t.Fatal(err)
	}
	h.owner("olive")
	session := sessionCookie(h.s.StartConsoleSession("mika", h.s.now()))
	if err := h.s.DB.DisableButlerMember("mika", "olive"); err != nil {
		t.Fatal(err)
	}
	code, _ := h.sessionRequest(http.MethodPost, "/api/v1/coordinator/promote", ``, "mika", session)
	if code != http.StatusForbidden && code != http.StatusUnauthorized {
		t.Fatalf("inactive Owner promotion = %d", code)
	}
	lease, err := h.s.DB.GetCoordinator()
	if err != nil || lease.Holder != "keystone-original" || lease.Epoch != 1 {
		t.Fatalf("inactive Owner changed lease: %+v, %v", lease, err)
	}
}
