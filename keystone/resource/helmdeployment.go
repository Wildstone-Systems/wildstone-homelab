package resource

import (
	"regexp"
	"time"
)

// HelmDeploymentSpec is canonical desired input, not arbitrary Helm values.
// Placement binds separately owned namespace/claim/credential inventory. Status,
// output YAML and mutable execution state stay out of the Statefile.
type HelmDeploymentSpec struct {
	Package   string `yaml:"package"`
	Version   string `yaml:"version"`
	Timezone  string `yaml:"timezone"`
	Placement string `yaml:"placement"`
}

func validateHelmDeployment(r *Resource) error {
	s := r.HelmDeployment
	invalid := func(field, msg string) error {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: field, Msg: msg}
	}
	if s == nil {
		return invalid("spec", "required")
	}
	if r.Metadata.Owner != "capability/audiobooks" || s.Package != "audiobooks-audiobookshelf" {
		return invalid("spec.package", "only the reviewed Audiobookshelf profile is supported")
	}
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(s.Version) {
		return invalid("spec.version", "pinned package version required")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(s.Placement) {
		return invalid("spec.placement", "exact installation placement required")
	}
	if s.Timezone == "" || s.Timezone == "Local" {
		return invalid("spec.timezone", "explicit IANA timezone required")
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return invalid("spec.timezone", "invalid IANA timezone")
	}
	return nil
}
