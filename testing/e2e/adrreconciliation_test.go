package e2e

import (
	"os"
	"strings"
	"testing"
)

// ADR-W030, W031 and W032 must identify what the shipped slices actually
// settled and what remains open, in terms of ADR-W033. This is a documentation contract; the named
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
			required: []string{"ADR-W033 C1–C8", "Existing records were classified"},
		},
		{
			file:     "../../docs/decisions/ADR-W032-who-may-open-the-console.md",
			required: []string{"member-name normalisation (A4) is not", "What is still open", "TestLegacyRoleOperatorIsUntouched"},
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
