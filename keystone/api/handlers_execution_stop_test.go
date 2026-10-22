package api

import (
	"net/http"
	"testing"
	"time"
)

func TestOnlyFreshOwnerCanStopAndReleaseHouseholdExecution(t *testing.T) {
	h := freshnessHarness(t)
	member := sessionCookie(h.s.StartConsoleSession("sandy", time.Now()))
	if code, _ := h.sessionRequest(http.MethodPost, "/api/v1/household/execution-stop", `{"action":"stop","reason":"incident"}`, "sandy", member); code != http.StatusForbidden && code != http.StatusUnauthorized {
		t.Fatalf("Member stop status %d", code)
	}
	owner := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	if code, out := h.sessionRequest(http.MethodPost, "/api/v1/household/execution-stop", `{"action":"stop","reason":"incident"}`, "mika", owner); code != http.StatusOK {
		t.Fatalf("Owner stop %d %v", code, out)
	}
	state, err := h.s.DB.GetHouseholdExecutionStop()
	if err != nil || !state.Active {
		t.Fatalf("stop did not persist: %+v %v", state, err)
	}
	if code, out := h.sessionRequest(http.MethodPost, "/api/v1/household/execution-stop", `{"action":"resume"}`, "mika", owner); code != http.StatusOK {
		t.Fatalf("Owner resume %d %v", code, out)
	}
	state, err = h.s.DB.GetHouseholdExecutionStop()
	if err != nil || state.Active {
		t.Fatalf("stop remained active: %+v %v", state, err)
	}
}
