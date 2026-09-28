package pkgtool

import (
	"strings"
	"testing"
)

// #381: a package's events are held to the closed integration vocabulary
// wherever a domain is declared (keystone/integration). A pantry.* event
// nobody declared — a typo, or a query name used as an event — fails lint
// instead of becoming an event no routine will ever match.

func publishing(events ...string) string {
	var b strings.Builder
	b.WriteString("publishes:\n  shape: native\n  events:\n")
	for _, e := range events {
		b.WriteString("    - { type: " + e + ", subject: household }\n")
	}
	return b.String()
}

func TestLintRejectsAPantryEventOutsideTheVocabulary(t *testing.T) {
	got := issuesOfRule(t, writeTraitPkg(t, publishing("pantry.meal_loged")), "integration")
	if len(got) != 1 || !strings.Contains(got[0], `"pantry.meal_loged" is not a pantry event`) {
		t.Fatalf("issues = %q", got)
	}
}

func TestLintRejectsAPantryQueryUsedAsAnEvent(t *testing.T) {
	got := issuesOfRule(t, writeTraitPkg(t, publishing("pantry.expiring")), "integration")
	if len(got) != 1 || !strings.Contains(got[0], "is a pantry query or action, not an event") {
		t.Fatalf("issues = %q", got)
	}
}

func TestLintAcceptsWellKnownPantryEventsAndOpenNamespaces(t *testing.T) {
	// person.* has no declared domain: it stays open (ADR-W024).
	dir := writeTraitPkg(t, publishing("pantry.meal_logged", "pantry.item_spoiled", "person.arrived_home"))
	if got := issuesOfRule(t, dir, "integration"); len(got) != 0 {
		t.Fatalf("issues = %q", got)
	}
}
