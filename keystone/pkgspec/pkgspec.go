// Package pkgspec defines Keystone's Package format v1 (ADR-W005 A1): the
// versioned-data unit that templates one profile of a capability. A Package
// is loaded/validated here; expanding it into resource.Resource values is
// keystone/capability's job.
//
// Packages are data, not code. Lifecycle hooks and agentTools are parsed
// and schema-checked by Validate but are NEVER executed by this package —
// execution is a later phase (ADR-W005 A1). A malformed hook or agentTool
// block is rejected at Validate; nothing here ever shells out, execs a
// container, or otherwise runs a hook/tool.
package pkgspec

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ApiVersion is the only supported apiVersion for Package documents.
const ApiVersion = "wildstone.dev/v1"

// Kind is the only supported kind for Package documents.
const Kind = "Package"

// Metadata identifies the package: the user-facing capability it
// implements, the concrete profile, and its version.
type Metadata struct {
	Name       string `yaml:"name"`
	Version    string `yaml:"version"`
	Capability string `yaml:"capability"`
	Profile    string `yaml:"profile"`
}

// Requirement is one entry of a package's `requires` list: a trait the
// package needs satisfied by some other enabled package in the catalog
// (ADR-W005 A2), e.g. { trait: identity, mode: oidc }.
type Requirement struct {
	Trait string `yaml:"trait"`
	Mode  string `yaml:"mode,omitempty"`
	Class string `yaml:"class,omitempty"`
}

// Provide is one entry of a package's `provides` list: a trait/mode this
// package advertises to the catalog, e.g. { trait: identity, mode: oidc }
// for the Identity package. Requirement resolution in keystone/capability
// matches a Requirement against a Provide by (Trait, Mode) (Class is not
// currently matched — reserved for storage-class style requirements).
type Provide struct {
	Trait string `yaml:"trait"`
	Mode  string `yaml:"mode,omitempty"`
	Class string `yaml:"class,omitempty"`
}

// ParamDecl declares one templating input a package accepts. Required
// params must be supplied by the caller; non-required params fall back to
// Default when omitted.
type ParamDecl struct {
	Name     string `yaml:"name"`
	Required bool   `yaml:"required,omitempty"`
	Default  string `yaml:"default,omitempty"`
}

// Policies carries the mandatory policy inputs a package declares about the
// data it handles.
type Policies struct {
	Classifications []string `yaml:"classifications,omitempty"`
	DataSensitivity string   `yaml:"dataSensitivity,omitempty"`
}

// HookSpec is one lifecycle hook. Per ADR-W005 A1, hooks run AS CONTAINERS
// with declared mounts — never host scripts — but at the spike, hooks are
// parsed and schema-checked only, never executed.
type HookSpec struct {
	Image  string  `yaml:"image"`
	Mounts []Mount `yaml:"mounts,omitempty"`
}

// Mount is a lifecycle hook's declared bind mount.
type Mount struct {
	Source   string `yaml:"source"`
	Target   string `yaml:"target"`
	ReadOnly bool   `yaml:"readOnly,omitempty"`
}

// Lifecycle groups a package's pre/post-install hooks.
type Lifecycle struct {
	PreInstall  []HookSpec `yaml:"preInstall,omitempty"`
	PostInstall []HookSpec `yaml:"postInstall,omitempty"`
}

// riskTiers is the closed set of accepted AgentToolSpec.Risk values
// (ADR-W001 concierge block).
var riskTiers = map[string]bool{
	"read":     true,
	"low":      true,
	"medium":   true,
	"high":     true,
	"domestic": true,
}

// AgentToolSpec declares one concierge-agent tool a package exposes
// (ADR-W001). Parsed and schema-checked only at the spike, never executed.
type AgentToolSpec struct {
	Name string `yaml:"name"`
	Risk string `yaml:"risk"`
}

// Package is the parsed, in-memory form of a Package document (ADR-W005
// A1).
type Package struct {
	ApiVersion string        `yaml:"apiVersion"`
	Kind       string        `yaml:"kind"`
	Metadata   Metadata      `yaml:"metadata"`
	Provides   []Provide     `yaml:"provides,omitempty"`
	Requires   []Requirement `yaml:"requires,omitempty"`
	Params     []ParamDecl   `yaml:"params,omitempty"`
	Policies   Policies      `yaml:"policies,omitempty"`
	// Templates are raw text/template strings that render to resource YAML
	// documents (one Resource per template). Rendering happens in
	// keystone/capability, not here.
	Templates  []string        `yaml:"templates,omitempty"`
	Lifecycle  Lifecycle       `yaml:"lifecycle,omitempty"`
	AgentTools []AgentToolSpec `yaml:"agentTools,omitempty"`
}

// ValidationError is a typed validation failure for a Package document.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("package: field %s: %s", e.Field, e.Msg)
}

// Parse decodes a single Package YAML document from data. It does not
// Validate — callers should call Validate explicitly (Load does both).
func Parse(data []byte) (*Package, error) {
	var pkg Package
	if err := yaml.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("pkgspec: parsing package: %w", err)
	}
	return &pkg, nil
}

// Load reads and parses the Package at path, then Validates it.
func Load(path string) (*Package, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pkgspec: reading %s: %w", path, err)
	}
	pkg, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := pkg.Validate(); err != nil {
		return nil, err
	}
	return pkg, nil
}

// Validate checks the package is well-formed per ADR-W005 A1: metadata
// required fields present, every param has a name, lifecycle hooks and
// agentTools are schema-checked (but never executed — Validate never runs
// anything), and templates are non-empty for a package that declares
// resources (i.e. one with no templates and no lifecycle/agentTools/
// provides is nonsensical, but an empty Templates list is only rejected
// when the package otherwise looks like it produces resources — see
// below).
func (p *Package) Validate() error {
	if p.Metadata.Name == "" {
		return &ValidationError{Field: "metadata.name", Msg: "must not be empty"}
	}
	if p.Metadata.Version == "" {
		return &ValidationError{Field: "metadata.version", Msg: "must not be empty"}
	}
	if p.Metadata.Capability == "" {
		return &ValidationError{Field: "metadata.capability", Msg: "must not be empty"}
	}

	for i, param := range p.Params {
		if param.Name == "" {
			return &ValidationError{Field: fmt.Sprintf("params[%d].name", i), Msg: "must not be empty"}
		}
	}

	for i, req := range p.Requires {
		if req.Trait == "" {
			return &ValidationError{Field: fmt.Sprintf("requires[%d].trait", i), Msg: "must not be empty"}
		}
	}

	for i, prov := range p.Provides {
		if prov.Trait == "" {
			return &ValidationError{Field: fmt.Sprintf("provides[%d].trait", i), Msg: "must not be empty"}
		}
	}

	// Lifecycle hooks: parsed + schema-checked only. Never executed here or
	// anywhere else in this package.
	for i, h := range p.Lifecycle.PreInstall {
		if err := validateHook(h, fmt.Sprintf("lifecycle.preInstall[%d]", i)); err != nil {
			return err
		}
	}
	for i, h := range p.Lifecycle.PostInstall {
		if err := validateHook(h, fmt.Sprintf("lifecycle.postInstall[%d]", i)); err != nil {
			return err
		}
	}

	// AgentTools: parsed + schema-checked only. Never executed here.
	for i, t := range p.AgentTools {
		if err := validateAgentTool(t, fmt.Sprintf("agentTools[%d]", i)); err != nil {
			return err
		}
	}

	// A package that declares no templates, no lifecycle hooks, no
	// agentTools, and no provides is producing nothing — reject it as
	// malformed rather than silently accepting a no-op package.
	if len(p.Templates) == 0 && len(p.Lifecycle.PreInstall) == 0 && len(p.Lifecycle.PostInstall) == 0 &&
		len(p.AgentTools) == 0 && len(p.Provides) == 0 {
		return &ValidationError{Field: "templates", Msg: "must not be empty: package declares no templates, hooks, agentTools, or provides"}
	}

	return nil
}

func validateHook(h HookSpec, field string) error {
	if h.Image == "" {
		return &ValidationError{Field: field + ".image", Msg: "must not be empty"}
	}
	for i, m := range h.Mounts {
		if m.Source == "" {
			return &ValidationError{Field: fmt.Sprintf("%s.mounts[%d].source", field, i), Msg: "must not be empty"}
		}
		if m.Target == "" {
			return &ValidationError{Field: fmt.Sprintf("%s.mounts[%d].target", field, i), Msg: "must not be empty"}
		}
	}
	return nil
}

func validateAgentTool(t AgentToolSpec, field string) error {
	if t.Name == "" {
		return &ValidationError{Field: field + ".name", Msg: "must not be empty"}
	}
	if !riskTiers[t.Risk] {
		return &ValidationError{Field: field + ".risk", Msg: "must be one of read, low, medium, high, domestic"}
	}
	return nil
}
