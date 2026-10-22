package pkgtool

import (
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
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

// An answered query may only claim to support params the closed query
// defines (docs/catalog-integration-contract.md §4).
func TestLintRejectsASupportedParamTheQueryDoesNotDefine(t *testing.T) {
	pkg, err := pkgspec.Parse([]byte(`
apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: m, version: 1.0.0, capability: mail }
params: [{ name: imap_host }]
templates: ["x"]
integrates:
  - domain: mail
    shape: connection-only
    adapter:
      contract: 1
      image: "gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab"
      program: wild-mail-adapter
    answers:
      - { query: mail.changes, supports: [folder, flags] }
`))
	if err != nil {
		t.Fatal(err)
	}
	got := integrationIssues(pkg)
	if len(got) != 1 || !strings.Contains(got[0].Msg, `"flags" is not a param of mail.changes`) {
		t.Fatalf("issues = %+v", got)
	}
}
