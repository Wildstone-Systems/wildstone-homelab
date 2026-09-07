package pkgtool

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// theShippedComment is the ConfigFile content the home-assistant package
// actually shipped. It rendered correctly, mounted correctly, and left
// the service answering 400 to every request, because the step that made
// it work was addressed to a person in a comment inside a file mounted
// read-only inside a container.
const theShippedComment = `# Managed by Wildstone — do not edit. Changes are reverted on the
# next reconcile tick.
#
# Include from /config/configuration.yaml (once):
#   homeassistant:
#     packages: !include_dir_named /config/wildstone
http:
  use_x_forwarded_for: true
`

func TestManualStepCatchesTheHomeAssistantComment(t *testing.T) {
	pkg := &pkgspec.Package{}
	rendered := []resource.Resource{{
		Kind:       resource.KindConfigFile,
		Metadata:   resource.Metadata{Name: "home-assistant-wildstone", Owner: "capability/home-assistant"},
		ConfigFile: &resource.ConfigFileSpec{Filename: "wildstone.yaml", Content: theShippedComment},
	}}
	issues := manualStepIssues(pkg, rendered)
	if len(issues) != 1 {
		t.Fatalf("want 1 manual-step issue for the shipped comment, got %d: %v", len(issues), issues)
	}
	if !strings.Contains(issues[0].Msg, "SeedFile") {
		t.Errorf("the finding should point at the fix, got: %s", issues[0].Msg)
	}
}

// The same content is FINE when the reader is genuinely outside
// Wildstone and the package says so — that is home-automation, which
// renders a config for a Home Assistant on someone's own hardware.
func TestManualStepExcusesDeclaredExternalConfig(t *testing.T) {
	pkg := &pkgspec.Package{ExternalConfig: []pkgspec.ExternalConfigDecl{
		{Filename: "wildstone.yaml", Consumer: "Home Assistant", Reason: "runs outside Wildstone"},
	}}
	rendered := []resource.Resource{{
		Kind:       resource.KindConfigFile,
		Metadata:   resource.Metadata{Name: "home-assistant-wildstone", Owner: "capability/home-automation"},
		ConfigFile: &resource.ConfigFileSpec{Filename: "wildstone.yaml", Content: theShippedComment},
	}}
	if issues := manualStepIssues(pkg, rendered); len(issues) != 0 {
		t.Fatalf("a declared externalConfig file may instruct its carrier, got %v", issues)
	}
}

// A seed has no externalConfig escape: it lands in a volume Wildstone
// owns, so there is no gap for a person to carry anything across.
func TestManualStepHasNoEscapeForSeeds(t *testing.T) {
	pkg := &pkgspec.Package{ExternalConfig: []pkgspec.ExternalConfigDecl{
		{Filename: "configuration.yaml", Consumer: "Home Assistant", Reason: "x"},
	}}
	rendered := []resource.Resource{{
		Kind:     resource.KindSeedFile,
		Metadata: resource.Metadata{Name: "s", Owner: "capability/home-assistant"},
		SeedFile: &resource.SeedFileSpec{Volume: "v", Path: "configuration.yaml", Content: theShippedComment},
	}}
	if issues := manualStepIssues(pkg, rendered); len(issues) != 1 {
		t.Fatalf("want the seed flagged despite the externalConfig entry, got %v", issues)
	}
}

// The rule must stay quiet on ordinary config prose, or authors will
// learn to ignore it.
func TestManualStepIgnoresOrdinaryConfig(t *testing.T) {
	for _, content := range []string{
		"# Managed by Wildstone — do not edit.\nserver:\n  port: 8080\n",
		"# Include the standard ruleset below.\nrules:\n  - allow\n",
		"# This file is yours: edit it freely, Wildstone will not rewrite it.\nfoo: bar\n",
		"# Set by the install form from the domain you supplied.\nhost: example.com\n",
	} {
		rendered := []resource.Resource{{
			Kind:       resource.KindConfigFile,
			Metadata:   resource.Metadata{Name: "c", Owner: "capability/x"},
			ConfigFile: &resource.ConfigFileSpec{Filename: "c.yaml", Content: content},
		}}
		if issues := manualStepIssues(&pkgspec.Package{}, rendered); len(issues) != 0 {
			t.Errorf("false positive on %q: %v", content, issues)
		}
	}
}
