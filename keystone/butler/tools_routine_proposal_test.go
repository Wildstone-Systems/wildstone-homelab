package butler

import (
	"context"
	"strings"
	"testing"
)

func TestRoutineProposalCannotSelectAnotherPersonOrActivate(t *testing.T) {
	var got RoutineProposalCall
	gw := newTestHouseholdGateway("sam", HouseholdSeams{ProposeRoutine: func(_ context.Context, c RoutineProposalCall) (string, error) { got = c; return "p-42", nil }})
	out, err := gw.Dispatch(context.Background(), "propose_household_plan", map[string]any{"name": "morning", "outputAudience": "aud_household", "member": "mika", "approve": true})
	if err != nil || !out.ProposalConfirmed || out.EffectConfirmed || out.Rejected || got.Member != "sam" || !strings.Contains(out.Content, "not been activated") {
		t.Fatalf("unsafe proposal: %+v %+v %v", got, out, err)
	}
	for _, name := range []string{"approve_plan", "activate_routine", "approve_household_plan"} {
		out, err = gw.Dispatch(context.Background(), name, map[string]any{"plan": "p-42"})
		if err == nil && !out.Rejected {
			t.Fatal("Agent has an approval tool", name)
		}
	}
	for _, spec := range gw.OfferedSpecs(true) {
		if spec.Name == "propose_household_plan" {
			t.Fatal("read-only session can submit")
		}
	}
}
