package api

// The closed grammar a routine form offers (#171).
//
// The point of this endpoint is that a form cannot offer something the
// validator or the policy engine will then refuse. So the tests are
// mostly about what does NOT come back.

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/catalog"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// grammarHarness gives the server a catalogue of two packages — one
// installed, one not — and returns the harness.
func grammarHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	dir := t.TempDir()
	write := func(name, doc string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "package.yaml"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("notifications", `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: notifications, version: 1.0.0, capability: notifications }
agentTools:
  - { name: notify, risk: domestic }
  - { name: peek, risk: read }
  - { name: restart, risk: high }
`)
	write("home-automation", `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: home-automation, version: 1.0.0, capability: home-automation }
agentTools:
  - { name: set_light, risk: domestic }
publishes:
  shape: config
  events:
    - type: person.arrived_home
      subject: member
      description: A tracked person's presence changed to home.
`)
	// A package in the catalogue that nobody installed. Its vocabulary
	// must not be offered: a rule triggering on an event nothing emits
	// never fires, and nothing tells its author why.
	write("pantry", `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: pantry, version: 1.0.0, capability: pantry }
agentTools:
  - { name: log_meal, risk: domestic }
publishes:
  shape: config
  events:
    - type: pantry.meal_logged
      subject: member
`)
	c, err := catalog.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	h.s.Catalog = c

	// Mark the first two installed, by the rule packageIsInstalled uses
	// for a package that runs no container: a stamped resource.
	for _, pkg := range []string{"notifications", "home-automation"} {
		if err := h.s.State.Put(resource.Resource{
			ApiVersion: "wildstone.dev/v1", Kind: resource.KindSecret,
			Metadata: resource.Metadata{
				Name: pkg + "-token", Owner: "capability/" + pkg, Package: pkg, Node: "n1",
			},
			Secret: &resource.SecretSpec{Name: pkg + "-token"},
		}); err != nil {
			t.Fatalf("seeding %s: %v", pkg, err)
		}
	}
	return h
}

func TestAutomationGrammarOffersOnlyWhatWillBeAccepted(t *testing.T) {
	h := grammarHarness(t)

	var got automationGrammar
	if resp := h.do(runtimedb.RoleOperator, http.MethodGet,
		"/api/v1/household/automations/grammar", nil, &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("grammar: %s", resp.Status)
	}

	events := map[string]bool{}
	for _, e := range got.EventTypes {
		events[e.Type] = true
	}
	if !events["person.arrived_home"] {
		t.Fatalf("an installed package's event is missing: %+v", got.EventTypes)
	}
	// Not installed, so not offered.
	if events["pantry.meal_logged"] {
		t.Fatalf("an uninstalled package's event was offered: %+v", got.EventTypes)
	}

	tools := map[string]string{}
	for _, a := range got.Actions {
		tools[a.Tool] = a.Tier
	}
	// Qualified as the schema requires: <capability>.<tool>.
	if tools["notifications.notify"] != "domestic" {
		t.Fatalf("the notify tool is not offered as the schema writes it: %+v", got.Actions)
	}
	if tools["notifications.peek"] != "read" {
		t.Fatalf("a read-tier tool should be offered: %+v", got.Actions)
	}
	// The automation-action-tier rule refuses anything above domestic, so
	// offering it would be offering a rejection.
	if _, ok := tools["notifications.restart"]; ok {
		t.Fatalf("a high-tier tool was offered to a household rule: %+v", got.Actions)
	}
	if _, ok := tools["pantry.log_meal"]; ok {
		t.Fatalf("an uninstalled package's tool was offered: %+v", got.Actions)
	}

	// The vocabularies come from the schema and the policy engine rather
	// than being restated here, so a form cannot drift from validation.
	if len(got.ConditionKinds) == 0 || len(got.ContextKinds) == 0 || len(got.Weekdays) != 7 {
		t.Fatalf("the closed vocabularies are not served: %+v", got)
	}
	for _, tier := range got.Tiers {
		if tier != "read" && tier != "domestic" {
			t.Fatalf("a tier a household rule may not carry was offered: %q", tier)
		}
	}
}

// Without a catalogue the vocabularies are still the truth. A form can
// render its condition and tier choices; it simply has nothing to
// trigger on.
func TestAutomationGrammarWithoutACatalogue(t *testing.T) {
	h := newHarness(t)
	var got automationGrammar
	if resp := h.do(runtimedb.RoleOperator, http.MethodGet,
		"/api/v1/household/automations/grammar", nil, &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("grammar: %s", resp.Status)
	}
	if len(got.Tiers) == 0 || len(got.ConditionKinds) == 0 {
		t.Fatalf("the closed vocabularies should not need a catalogue: %+v", got)
	}
	// Null would make a client's .map() throw on a 200.
	if got.EventTypes == nil || got.Actions == nil {
		t.Fatalf("empty lists must serialise as [], not null: %+v", got)
	}
}

func TestAutomationGrammarRoles(t *testing.T) {
	h := newHarness(t)
	h.addPublisher("publisher-home")
	for _, c := range []struct {
		role runtimedb.Role
		want int
	}{
		{runtimedb.RoleAdmin, http.StatusOK},
		{runtimedb.RoleOperator, http.StatusOK},
		{runtimedb.RoleButler, http.StatusOK},
		{runtimedb.RolePublisher, http.StatusForbidden},
		{"", http.StatusUnauthorized},
	} {
		if resp := h.do(c.role, http.MethodGet,
			"/api/v1/household/automations/grammar", nil, nil); resp.StatusCode != c.want {
			t.Fatalf("role %q: want %d, got %s", c.role, c.want, resp.Status)
		}
	}
}
