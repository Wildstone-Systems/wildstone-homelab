package butler

import (
	"context"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
)

type RoutineProposalCall struct{ Name, OutputAudience, Member string }

var routineProposalSpec = provider.Tool{
	Name:        "propose_household_plan",
	Description: "Submit an existing household routine draft to the Owner Review Queue. State who may receive its output separately from its delivery behavior. This never activates a routine; only an Owner can approve the exact version in Console.",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{
		"name": map[string]any{"type": "string"}, "outputAudience": map[string]any{"type": "string", "description": "The declared audience ID; aud_household for the whole household."}}, "required": []any{"name", "outputAudience"}, "additionalProperties": false},
}

func handleRoutineProposal(ctx context.Context, gw *HouseholdGateway, input map[string]any) (ToolOutcome, error) {
	if gw.member == "" || gw.seams.ProposeRoutine == nil {
		return ToolOutcome{Rejected: true, Content: "No admitted proposal path is available."}, nil
	}
	id, err := gw.seams.ProposeRoutine(ctx, RoutineProposalCall{Name: stringInput(input, "name"), OutputAudience: stringInput(input, "outputAudience"), Member: gw.member})
	if err != nil {
		return ToolOutcome{}, err
	}
	return ToolOutcome{ProposalConfirmed: true, Content: "Plan " + id + " is awaiting Owner review in Console. The routine has not been activated."}, nil
}
