package policy

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Exemption suppresses exactly one rule for exactly one (kind, name),
// with a required human reason and approver (ADR-W009 A3). Loaded from a
// root-owned file on the Keystone host — editing it is itself an
// authenticated act — and every USE is surfaced by the caller as a
// policy.exempted event.
type Exemption struct {
	Rule       string `yaml:"rule"`
	Kind       string `yaml:"kind"`
	Name       string `yaml:"name"`
	Reason     string `yaml:"reason"`
	ApprovedBy string `yaml:"approvedBy"`
}

var knownRules = map[string]bool{
	RuleClassificationVocabulary: true,
	RulePublicRequiresAuth:       true,
	RuleAdminStructural:          true,
	RuleSecretValuesForbidden:    true,
	RuleButlerAdminScope:         true,
}

// Validate rejects a malformed exemption: unknown rule, missing target,
// or missing accountability fields. A bad exemption fails startup loudly
// (ADR-W009 A3) rather than lurking as a no-op.
func (e Exemption) Validate() error {
	if !knownRules[e.Rule] {
		return fmt.Errorf("policy: exemption references unknown rule %q", e.Rule)
	}
	if e.Kind == "" || e.Name == "" {
		return fmt.Errorf("policy: exemption for rule %s must name a kind and name (broad exemptions are not a thing)", e.Rule)
	}
	if e.Reason == "" || e.ApprovedBy == "" {
		return fmt.Errorf("policy: exemption for %s on %s/%s must carry reason and approvedBy", e.Rule, e.Kind, e.Name)
	}
	return nil
}

// LoadExemptions reads <path> (YAML list of Exemption). A missing file
// means no exemptions — the common case; any present file must be fully
// valid.
func LoadExemptions(path string) ([]Exemption, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("policy: reading exemptions %s: %w", path, err)
	}
	var out []Exemption
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("policy: parsing exemptions %s: %w", path, err)
	}
	for i, ex := range out {
		if err := ex.Validate(); err != nil {
			return nil, fmt.Errorf("policy: exemptions[%d]: %w", i, err)
		}
	}
	return out, nil
}
