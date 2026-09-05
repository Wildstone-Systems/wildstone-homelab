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
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"

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
	Trait string `yaml:"trait" json:"trait"`
	Mode  string `yaml:"mode,omitempty" json:"mode,omitempty"`
	Class string `yaml:"class,omitempty" json:"class,omitempty"`
}

// Provide is one entry of a package's `provides` list: a trait/mode this
// package advertises to the catalog, e.g. { trait: identity, mode: oidc }
// for the Identity package. Requirement resolution in keystone/capability
// matches a Requirement against a Provide by (Trait, Mode) (Class is not
// currently matched — reserved for storage-class style requirements).
type Provide struct {
	Trait string `yaml:"trait" json:"trait"`
	Mode  string `yaml:"mode,omitempty" json:"mode,omitempty"`
	Class string `yaml:"class,omitempty" json:"class,omitempty"`
}

// ParamDecl declares one templating input a package accepts. Required
// params must be supplied by the caller; non-required params fall back to
// Default when omitted.
type ParamDecl struct {
	Name     string `yaml:"name" json:"name"`
	Required bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Default  string `yaml:"default,omitempty" json:"default,omitempty"`
	// Description is what this param MEANS, in words an operator who
	// does not know the internals can act on. It exists because the
	// generated form (ADR-W021 A5) could previously render only a name:
	// "admin_domain *" is an unanswerable question for the person
	// ADR-W023 A8 targets, and the explanation was sitting three lines
	// away in a YAML comment the form cannot see.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Source names a fact the DEPLOYMENT already knows, so the form can
	// offer it rather than ask for it. One of paramSources; empty means
	// the value is genuinely the operator's to invent.
	//
	// A closed set on purpose: each value is a query Keystone knows how
	// to answer, and an unrecognised one must fail review rather than
	// silently render an empty dropdown.
	Source string `yaml:"source,omitempty" json:"source,omitempty"`
}

// ParamSources are the deployment facts a param may be sourced from.
var ParamSources = map[string]bool{
	// Which machine to install on. Options are the nodes the control
	// plane knows; with one node there is nothing to choose.
	"node": true,
	// The domain other capabilities on this deployment already use.
	// Asking each package separately is how two capabilities end up
	// under different domains for no reason anyone intended.
	"domain": true,
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

// EventTypeRegexp is the household event type shape (ADR-W022 A2),
// duplicated from keystone/household so this package stays a leaf; a test
// there pins the two equal.
var EventTypeRegexp = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// publishShapes is the closed set of ways a package's events reach the
// household intake (ADR-W022 A10).
var publishShapes = map[string]bool{
	// native: the app itself posts to the intake. No shim.
	"native": true,
	// sidecar: an adapter container declared in this package's templates
	// listens to the app and posts on its behalf.
	"sidecar": true,
	// config: the app can call a webhook itself and this package ships
	// that configuration as a ConfigFile.
	"config": true,
}

// eventSubjects is the closed set of what an event's subject identifies
// (ADR-W022 A8). Mapping is best-effort per app; "none" is an honest
// answer for a household-wide fact.
var eventSubjects = map[string]bool{
	"member": true, "device": true, "household": true, "none": true,
}

// eventFieldTypes is the closed set of payload field types a package may
// declare. Deliberately small: a payload is the few fields a handler
// needs, not the record.
var eventFieldTypes = map[string]bool{
	"string": true, "number": true, "bool": true, "timestamp": true,
}

// PublishesSpec declares the household events a package emits and how
// they reach the intake (ADR-W022 A10). Parsed and validated here;
// keystone/capability synthesises the publisher secret from it. A package
// that publishes nothing omits the block — but every package should
// answer the question; silence is not an answer.
type PublishesSpec struct {
	Shape  string           `yaml:"shape"`
	Events []PublishedEvent `yaml:"events"`
}

// PublishedEvent declares one event type a package emits.
type PublishedEvent struct {
	Type        string       `yaml:"type"`
	Subject     string       `yaml:"subject,omitempty"` // default "none"
	Description string       `yaml:"description,omitempty"`
	Fields      []EventField `yaml:"fields,omitempty"`
}

// EventField declares one payload field.
type EventField struct {
	Name     string `yaml:"name"`
	Type     string `yaml:"type"`
	Required bool   `yaml:"required,omitempty"`
}

// ExternalConfigDecl declares a ConfigFile this package renders for a
// consumer Wildstone does NOT run (ADR-W022 A10, the "config" shim
// shape). It is the counterpart of a Secret's `provided: true`: the
// thing on the other end of this file is outside Wildstone, so the
// invariant that every rendered config is mounted into a container of
// its own package cannot hold, and saying so is the point.
//
// Undeclared, such a file is indistinguishable from the Authelia bug
// that invariant exists to catch — a correct config on disk that the
// application never reads. Declared, it is a deliberate hand-off with a
// named consumer and a reason a reviewer can weigh.
type ExternalConfigDecl struct {
	// Filename matches the ConfigFile's spec.filename.
	Filename string `yaml:"filename"`
	// Consumer names what reads it, e.g. "Home Assistant".
	Consumer string `yaml:"consumer"`
	// Reason says why that consumer is outside Wildstone, and how the
	// file reaches it.
	Reason string `yaml:"reason"`
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
	// Publishes declares the household events this package emits
	// (ADR-W022 A10). nil means it emits none.
	Publishes *PublishesSpec `yaml:"publishes,omitempty"`
	// ExternalConfig declares rendered ConfigFiles read by something
	// Wildstone does not run.
	ExternalConfig []ExternalConfigDecl `yaml:"externalConfig,omitempty"`
	// ImageSets group images that must be upgraded together.
	ImageSets []ImageSet `yaml:"imageSets,omitempty"`
}

// ImageSet names images whose versions are not independent — refreshing
// one without the others produces a combination that cannot run.
//
// photos-immich is the case that motivated this: immich-server supports a
// fixed set of postgres vector extensions, and its postgres image is what
// provides one. The pinned pair had drifted apart and the package could
// not start at all:
//
//	Error: No vector extension found. Available extensions: vchord, vector
//
// Nothing could catch that. Lint never pulls an image, so it cannot know
// what an image contains, and RefreshDigests resolves each pin
// independently by regex — so the tool that most easily creates the
// mismatch was the one least able to see it. A package cannot make the
// combination valid by declaring it, but it can say which pins move
// together, which is enough to turn a silent broken deploy into a refusal
// the author reads before publishing.
type ImageSet struct {
	// Name identifies the set in diagnostics, e.g. "immich".
	Name string `yaml:"name"`
	// Images are the image references (the repository half, without the
	// @sha256: digest) that must be refreshed together.
	Images []string `yaml:"images"`
	// Reason is shown when a refresh is refused. Write it so the person
	// reading it knows what to do, not merely that they were stopped.
	Reason string `yaml:"reason,omitempty"`
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
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&pkg); err != nil {
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

	for i, set := range p.ImageSets {
		if set.Name == "" {
			return &ValidationError{Field: fmt.Sprintf("imageSets[%d].name", i), Msg: "must not be empty"}
		}
		if len(set.Images) < 2 {
			return &ValidationError{Field: fmt.Sprintf("imageSets[%d].images", i),
				Msg: "must name at least two images — a set of one has nothing to stay in step with"}
		}
		for j, img := range set.Images {
			if img == "" {
				return &ValidationError{Field: fmt.Sprintf("imageSets[%d].images[%d]", i, j), Msg: "must not be empty"}
			}
			if strings.Contains(img, "@") {
				return &ValidationError{Field: fmt.Sprintf("imageSets[%d].images[%d]", i, j),
					Msg: "name the image without its digest — the set identifies which pins move together, not which values they hold"}
			}
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

	if p.Publishes != nil {
		if err := validatePublishes(p.Publishes); err != nil {
			return err
		}
	}

	seenExternal := map[string]bool{}
	for i, ec := range p.ExternalConfig {
		field := fmt.Sprintf("externalConfig[%d]", i)
		for name, v := range map[string]string{"filename": ec.Filename, "consumer": ec.Consumer, "reason": ec.Reason} {
			if strings.TrimSpace(v) == "" {
				return &ValidationError{Field: field + "." + name, Msg: "must not be empty: an unread config is a defect unless someone says who reads it and why"}
			}
		}
		if seenExternal[ec.Filename] {
			return &ValidationError{Field: field + ".filename", Msg: fmt.Sprintf("%q declared twice", ec.Filename)}
		}
		seenExternal[ec.Filename] = true
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

func validatePublishes(p *PublishesSpec) error {
	if !publishShapes[p.Shape] {
		return &ValidationError{Field: "publishes.shape", Msg: "must be one of native, sidecar, config"}
	}
	if len(p.Events) == 0 {
		return &ValidationError{Field: "publishes.events", Msg: "must declare at least one event type (omit the block for a package that publishes nothing)"}
	}
	seen := map[string]bool{}
	for i, ev := range p.Events {
		field := fmt.Sprintf("publishes.events[%d]", i)
		if !EventTypeRegexp.MatchString(ev.Type) {
			return &ValidationError{Field: field + ".type", Msg: fmt.Sprintf("%q must be namespace.name in lowercase [a-z0-9_], e.g. person.left_home", ev.Type)}
		}
		if seen[ev.Type] {
			return &ValidationError{Field: field + ".type", Msg: fmt.Sprintf("%q declared twice", ev.Type)}
		}
		seen[ev.Type] = true
		if ev.Subject != "" && !eventSubjects[ev.Subject] {
			return &ValidationError{Field: field + ".subject", Msg: "must be one of member, device, household, none"}
		}
		names := map[string]bool{}
		for j, f := range ev.Fields {
			ff := fmt.Sprintf("%s.fields[%d]", field, j)
			if f.Name == "" {
				return &ValidationError{Field: ff + ".name", Msg: "must not be empty"}
			}
			if names[f.Name] {
				return &ValidationError{Field: ff + ".name", Msg: fmt.Sprintf("%q declared twice", f.Name)}
			}
			names[f.Name] = true
			if !eventFieldTypes[f.Type] {
				return &ValidationError{Field: ff + ".type", Msg: "must be one of string, number, bool, timestamp"}
			}
		}
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

// RouteLabels are the leading labels of the Route hosts a package
// declares; ContainerNames are the Containers it declares.
//
// Both parse the template AS YAML rather than pattern-matching it. The
// first attempt used regexes and only understood the shipped catalog's
// own formatting: it required a double-quoted host, so `host: app.example.com`
// yielded no label and the tailnet blocker was silently skipped, and it
// required inline-map metadata, so a conventional block `metadata:` with
// an indented `name:` yielded no container and every profile of a
// capability was attributed the installed one's resources.
//
// A template is not valid YAML on its own — it carries {{ ... }} actions
// — so each action is replaced by a placeholder first: a param reference
// by that param's default, anything else by an empty string. What
// remains parses, and the fields we want are read from the structure
// rather than guessed from its layout.
//
// A template that still will not parse (a conditional spanning
// structure, say) yields nothing rather than a wrong answer.
func packageDocs(pkg *Package) []map[string]any {
	defaults := map[string]string{}
	for _, p := range pkg.Params {
		if p.Default != "" {
			defaults[p.Name] = p.Default
		}
	}
	var out []map[string]any
	for _, t := range pkg.Templates {
		resolved := templateActionRegexp.ReplaceAllStringFunc(t, func(a string) string {
			if m := paramRefRegexp.FindStringSubmatch(a); m != nil {
				return defaults[m[1]]
			}
			return ""
		})
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(resolved), &doc); err != nil || doc == nil {
			continue
		}
		out = append(out, doc)
	}
	return out
}

func docString(doc map[string]any, path ...string) string {
	var cur any = doc
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[k]
	}
	sv, _ := cur.(string)
	return sv
}

// RouteLabels are the leading labels of the Route hosts a package
// declares, with param references resolved to their defaults.
//
// Defaults are the honest answer before anyone supplies params, which is
// when an operator is deciding whether to install — and an override
// makes the Route unpinned anyway, which the install preview reports
// from the expanded Routes.
func RouteLabels(pkg *Package) []string {
	seen := map[string]bool{}
	var out []string
	for _, doc := range packageDocs(pkg) {
		if docString(doc, "kind") != "Route" {
			continue
		}
		host := docString(doc, "spec", "host")
		label, _, _ := strings.Cut(host, ".")
		if label != "" && !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	return out
}

// ContainerNames are the Container resources a package declares.
//
// Several packages can provide one capability — the four ingress
// profiles do — and resources are owned by "capability/<name>", not by
// the package that rendered them. A container name is the cheapest thing
// that distinguishes them: caddy-tailnet-ts exists on a node running
// that profile and caddy-tailnet does not.
func ContainerNames(pkg *Package) []string {
	seen := map[string]bool{}
	var out []string
	for _, doc := range packageDocs(pkg) {
		if docString(doc, "kind") != "Container" {
			continue
		}
		if n := docString(doc, "metadata", "name"); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

var (
	templateActionRegexp = regexp.MustCompile(`\{\{[^}]*\}\}`)
	paramRefRegexp       = regexp.MustCompile(`\{\{\s*\.Params\.([a-z_]+)\s*\}\}`)
)
