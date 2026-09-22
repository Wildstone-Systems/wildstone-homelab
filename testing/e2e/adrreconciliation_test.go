package e2e

import (
	"os"
	"strings"
	"testing"
)

// The three historical ADRs must identify what the shipped slices actually
// settled and what remains open. This is a documentation contract; the named
// implementation tests are run in Keystone's own suite.
func TestNoADRClaimsAnUnimplementedProperty(t *testing.T) {
	cases := []struct {
		file     string
		required []string
	}{
		{
			file:     "../../docs/decisions/ADR-W030-butler-continuity-durable-work-and-scoped-integrations.md",
			required: []string{"conversation_work", "TestConversationParentCarriesBudgetAcrossTurnLedgers", "TestConversationParentPreservesUncertainOutcome", "TestEveryRequiredEffectNeedsMatchingEvidence", "remain design requirements"},
		},
		{
			file:     "../../docs/decisions/ADR-W031-butlers-are-user-created-specialists.md",
			required: []string{"Amended by ADR-W033 baseline section 11, slice 13 (#214)", "retroactively"},
		},
		{
			file:     "../../docs/decisions/ADR-W032-who-may-open-the-console.md",
			required: []string{"A3 is not fully", "listButlers", "butlerScope", "TestLegacyRoleOperatorIsUntouched"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			body, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			for _, phrase := range tc.required {
				if !strings.Contains(string(body), phrase) {
					t.Errorf("reconciliation lost %q", phrase)
				}
			}
		})
	}
}
