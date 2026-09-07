package pkgtool

// The "manual-step" rule: a file a package renders may not carry
// instructions for a person.
//
// This is the rule the home-assistant package would have failed. It
// rendered a correct reverse-proxy config, mounted it into its own
// container, and put the step that made it take effect in a COMMENT
// inside that file:
//
//	# Include from /config/configuration.yaml (once):
//	#   homeassistant:
//	#     packages: !include_dir_named /config/wildstone
//
// Nobody reads a comment inside a file mounted read-only inside a
// container. The install reported success — every resource it owned had
// converged — and the service answered 400 to every request for the rest
// of its life. The instruction was addressed to a person who was never
// told the file existed.
//
// So: instruction-shaped text in rendered content is an error, UNLESS the
// file is declared in `externalConfig`. That exception is the honest
// case and not a loophole — an externalConfig file is BY DEFINITION read
// by something Wildstone does not run (home-automation renders one for a
// Home Assistant on someone's Pi), so a human carrying it across the gap
// is the mechanism, and the declaration already names the consumer and
// the reason where a reviewer and the console can both see it.
//
// Everything else must be automatic: derived from what Wildstone already
// knows (.Resolved), asked for up front as a param, or seeded into the
// app's own volume before it starts.

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// manualStepPatterns match text that addresses a person rather than a
// machine. Deliberately a short list of phrasings that only ever appear
// in an instruction, rather than a general "is this imperative" guess:
// this rule has to be precise enough that a package author trusts it, and
// a false positive on 53 packages costs more than the rule saves.
var manualStepPatterns = []*regexp.Regexp{
	// Someone is told to do something.
	regexp.MustCompile(`(?i)\bthe operator (adds|must|should|edits|includes|creates|sets|copies|runs)\b`),
	regexp.MustCompile(`(?i)\byou (must|should|need to|will need to) (add|edit|include|create|set|copy|run)\b`),
	// Pointing at a file the package cannot reach and asking for an edit.
	regexp.MustCompile(`(?i)\binclude (this|the rendered file|from)\b[^\n]{0,60}\bconfiguration\.ya?ml\b`),
	regexp.MustCompile(`(?i)\badd (these|the following|the two) lines\b`),
	// The tell-tale "do this once, by hand".
	regexp.MustCompile(`(?i)\b(once|manually),? on first (setup|boot|start|run)\b`),
	regexp.MustCompile(`(?i)\bon first setup\b`),
	regexp.MustCompile(`(?i)\bby hand\b`),
}

// manualStepIssues reports rendered ConfigFile/SeedFile content that
// instructs a person, excusing only files declared in externalConfig.
func manualStepIssues(pkg *pkgspec.Package, rendered []resource.Resource) []Issue {
	external := map[string]bool{}
	for _, d := range pkg.ExternalConfig {
		external[d.Filename] = true
	}

	var issues []Issue
	for _, r := range rendered {
		var content, filename string
		switch r.Kind {
		case resource.KindConfigFile:
			if r.ConfigFile == nil {
				continue
			}
			content, filename = r.ConfigFile.Content, r.ConfigFile.Filename
		case resource.KindSeedFile:
			if r.SeedFile == nil {
				continue
			}
			// A seed has no externalConfig counterpart and needs none:
			// it lands in a volume Wildstone owns, so there is nothing
			// on the far side of a gap for a person to carry.
			content, filename = r.SeedFile.Content, r.SeedFile.Path
		default:
			continue
		}
		if r.Kind == resource.KindConfigFile && external[filename] {
			continue
		}
		for _, pat := range manualStepPatterns {
			if m := pat.FindString(content); m != "" {
				issues = append(issues, Issue{Rule: "manual-step", Msg: fmt.Sprintf(
					"%s/%s (%s) tells a person to do something (%q). An install that needs a human step "+
						"reports success and then does not work, because nobody reads a file the package "+
						"mounted into a container. Derive it from .Resolved, ask for it as a param so the "+
						"install form collects it up front, or seed it with kind SeedFile before the app "+
						"first starts. If the reader really is outside Wildstone, declare the file in "+
						"externalConfig and say who reads it.",
					r.Kind, r.Metadata.Name, filename, strings.TrimSpace(m))})
				break
			}
		}
	}
	return issues
}
