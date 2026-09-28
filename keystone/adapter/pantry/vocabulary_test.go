package pantry

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/integration"
)

// Every pantry-1 capability this adapter asks for is declared in the closed
// integration vocabulary (#381): a typo here is a CI failure, not a query
// no PantryOS will ever answer.
func TestEveryCapabilityTheAdapterUsesIsInTheVocabulary(t *testing.T) {
	queries := []string{CapList, CapInventory, CapExpiring, CapSummary, CapRecipes, CapRecipe, CapCookCurrent, CapTimers}
	actions := []string{CapAdd, CapCheck, CapCookStart, CapCookStep, CapCookFinish, CapCookAbandon,
		CapTimerSet, CapTimerCancel, CapMealLog, CapMealUndo}
	for _, q := range queries {
		if !integration.IsQuery(q) {
			t.Errorf("query %q is not in keystone/integration", q)
		}
	}
	for _, a := range actions {
		if !integration.IsAction(a) {
			t.Errorf("action %q is not in keystone/integration", a)
		}
	}
	if got, want := len(queries)+len(actions), len(integration.Pantry.Queries)+len(integration.Pantry.Actions); got != want {
		t.Errorf("the adapter names %d capabilities, the vocabulary declares %d: one side is missing some", got, want)
	}
}

// When PantryOS cannot be asked, the capability inventory is built from
// knownCapabilities alone, so it must name every vocabulary capability:
// a missing one would silently vanish from the "unavailable" report.
func TestTheFallbackInventoryNamesEveryVocabularyCapability(t *testing.T) {
	known := map[string]string{}
	for _, c := range knownCapabilities {
		known[c.Name] = c.Kind
	}
	for _, q := range integration.Pantry.Queries {
		if known[q.Name] != "query" {
			t.Errorf("query %q is missing from knownCapabilities", q.Name)
		}
	}
	for _, a := range integration.Pantry.Actions {
		if known[a.Name] != "action" {
			t.Errorf("action %q is missing from knownCapabilities", a.Name)
		}
	}
	if got, want := len(knownCapabilities), len(integration.Pantry.Queries)+len(integration.Pantry.Actions); got != want {
		t.Errorf("knownCapabilities has %d entries, the vocabulary declares %d", got, want)
	}
}
