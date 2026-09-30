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
	"sort"
	"strings"
	"text/template"

	"github.com/chickenrat17/wildstone-homelab/keystone/trait"
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
	// Exclusive says that only one profile of this capability can run on
	// a deployment, so installing this one REPLACES a sibling.
	//
	// Off by default, because most household capabilities are plural and
	// were only ever singular by accident. A household comparing
	// Calibre-Web against Kavita, or running Jellyfin beside Plex, is
	// doing something ordinary; the engine used to uninstall one to
	// install the other, and nobody chose that (ADR-W029 A7).
	//
	// On for the few where two genuinely cannot coexist: one identity
	// provider, one reverse proxy owning the ports, one resolver on :53.
	// Those replacements are real, and being declared they are also
	// visible — the deletions appear in the plan, to be read before the
	// install is approved.
	Exclusive bool `yaml:"exclusive,omitempty"`
	// Instanced says the package may be installed more than once, each
	// install an INSTANCE with its own name (#246; ADR-W033 B7.1). A chat
	// platform is the case that needs it: one package, one bot per
	// install, as many bots as the household has Agents to give them.
	//
	// An instance is a capability of its own, named
	// "<capability>-<instance>" — so its resources, its owner, its
	// settings and its removal are separate from every other instance's,
	// through the same machinery a single-install package already uses.
	// The instance name is the package's required `instance` param, and
	// templates name their resources from .Capability so two instances
	// never render the same resource.
	Instanced bool `yaml:"instanced,omitempty"`
}

// InstanceParam is the param an instanced package's install is named by.
const InstanceParam = "instance"

// instanceNameRegexp bounds an instance name: short enough that
// "<capability>-<instance>" stays a valid capability name, and nothing
// but lowercase letters, digits and inner dashes.
var instanceNameRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,22}[a-z0-9])?$`)

// maxCapabilityName is the longest capability name the API accepts.
const maxCapabilityName = 52

// CapabilityFor is the capability an install of this package with these
// params is: the package's own for a single-install package, and
// "<capability>-<instance>" for an instanced one.
func (p *Package) CapabilityFor(params map[string]string) (string, error) {
	if !p.Metadata.Instanced {
		return p.Metadata.Capability, nil
	}
	inst := params[InstanceParam]
	if !instanceNameRegexp.MatchString(inst) {
		return "", fmt.Errorf("%s: instance %q must be lowercase letters, digits and dashes, 1–24 characters", p.Metadata.Name, inst)
	}
	c := p.Metadata.Capability + "-" + inst
	if len(c) > maxCapabilityName {
		return "", fmt.Errorf("%s: instance %q makes the capability name %q longer than %d characters", p.Metadata.Name, inst, c, maxCapabilityName)
	}
	return c, nil
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
	// Exports fill the `.Resolved` keys this trait mode carries
	// (keystone/trait), for every package expanded against a catalog
	// containing this one. Each value is a template rendered over the
	// PROVIDER's params plus the addressing functions in
	// trait.ExportFuncs — see keystone/capability's buildResolved for the
	// exact context.
	//
	// This block is what makes a new integration data rather than a code
	// change. It used to be Go: the engine looked up "identity"/"oidc" by
	// name and computed the three OIDC values itself, so the second trait
	// wanting to hand a consumer anything was a patch to the engine
	// rather than a package someone wrote.
	//
	// An export that renders EMPTY is not published. That is the honest
	// answer while the fact it derives from is unknown — a portal origin
	// built from a domain nobody has supplied yet is not a better value
	// than no value, it is a login loop — and it is why exports are
	// conventionally written with `with`:
	//
	//	IssuerURL: '{{ with .Params.public_domain }}{{ origin (print "auth." .) }}{{ end }}'
	Exports map[string]string `yaml:"exports,omitempty" json:"exports,omitempty"`
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
	// Example is a syntactically valid value for this param, used by lint
	// to dry-run the package and by the install form as a placeholder.
	//
	// It exists because lint's auto-filler invents "lint-<name>", which is
	// a fine stand-in for a domain or an id and a nonsense one for
	// anything with a shape: the ingress packages ask for a listen
	// address, got "lint-listen-addr", and failed validation on a value
	// no operator would ever have typed. The author of the package is the
	// only person who knows what shape the value has, so this is where
	// they say it — once, in the manifest, rather than as a `-set` flag
	// every person who lints the package has to rediscover.
	//
	// Never a real value: a param carrying secret material is refused by
	// lint outright, and an example is written into the repository.
	Example string `yaml:"example,omitempty" json:"example,omitempty"`
	// Validate names the SHAPE this param's value must have, from the
	// closed vocabulary in ParamShapes. Empty means the value is
	// free-form and nothing checks it.
	//
	// It exists because Example and Source both describe the value to a
	// HUMAN — a placeholder in the form, a picker instead of a blank box
	// — and neither is consulted once a human has typed something else.
	// A package that documents "Your timezone, e.g. Europe/London" and
	// receives "CST" has been given a value its own description rules
	// out, and the first thing to notice was the app's crash log.
	//
	// Checked in two places on purpose: lint checks the package's own
	// Default and Example, and Expand checks the value a deployment
	// actually supplies. The second is the one that matters — a bad
	// value is almost never the author's.
	Validate string `yaml:"validate,omitempty" json:"validate,omitempty"`
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
	// The public API's base URL as something running ON A NODE can
	// reach it — the same routable address the pairing command already
	// advertises for the gateway, with the API's port. The control
	// plane knows this; making every operator work it out by hand (and
	// discover that 127.0.0.1 means the container, not the host) is a
	// question it can answer itself.
	"keystone_url": true,
	// A folder on the node, under the prefixes a bind mount may name.
	// The form offers a picker rather than a blank box: a free-text path
	// is the field that let a corrupted default through unnoticed — the
	// operator was shown "/[REDACTED]", had nothing to compare it
	// against, and accepted what they were shown. It also makes the real
	// constraint visible, since a path outside those prefixes is refused
	// at apply time and a picker simply never offers one.
	"directory": true,
	// Where household events are POSTed: the API's address plus the
	// intake path, as something running ON A NODE reaches it.
	//
	// Distinct from keystone_url, which is the base. A package that
	// hands the value straight to an external system — Home Assistant's
	// rest_command, say — needs the whole endpoint, and making the
	// operator append /api/v1/household/events is asking them to look up
	// a path the control plane already knows. Sourcing only the base
	// instead, and appending in the template, doubles the path for
	// anyone who supplied the full URL under the old contract.
	"household_intake_url": true,
	// The household's time zone, which Keystone already keeps for the
	// automation engine's timeOfDay rules.
	//
	// Twenty-four packages ask for this, and it is the same answer every
	// time — a household has one clock. Asking each package separately
	// is how one service ends up an hour off from the rest, and it is
	// also twenty-four chances to type an abbreviation instead of a
	// zone. The deployment knows this; it should not be a blank box.
	"timezone": true,
}

// ParamSourcesSelectingAControl are the sources that tell the form which
// CONTROL to render rather than supplying a value to prefill.
//
// The distinction is real and worth stating once. "node" and "domain"
// are answers — Keystone knows them and the form offers them.
// "directory" is not an answer: only the operator knows which folder
// they mean, and the deployment's contribution is that it can show them
// the folders that exist. Conflating the two makes a source that
// resolves to nothing look like a bug, which is exactly what the
// completeness check should keep catching for every source that IS
// supposed to resolve.
var ParamSourcesSelectingAControl = map[string]bool{
	"directory": true,
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
	// confirm: acts, but only with a person in the loop who has said
	// yes to this specific call (ADR-W024 A5.1). Added because the two
	// household tiers were one axis where the world has two questions —
	// what the action does, and whether anybody is there.
	"confirm": true,
}

// EventTypeRegexp is the household event type shape (ADR-W024 A2),
// duplicated from keystone/household so this package stays a leaf; a test
// there pins the two equal.
var EventTypeRegexp = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// publishShapes is the closed set of ways a package's events reach the
// household intake (ADR-W024 A10).
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
// (ADR-W024 A8). Mapping is best-effort per app; "none" is an honest
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
// they reach the intake (ADR-W024 A10). Parsed and validated here;
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
// consumer Wildstone does NOT run (ADR-W024 A10, the "config" shim
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
	// OptIn marks an unusually consequential tool (ADR-W033 design
	// §6.2): excluded from a service bundle and from a new Agent's
	// defaults even when its class matches, and reached only by being
	// named.
	//
	// Distinct from Risk, which says how careful the HOUSEHOLD must be
	// about one call — whether it needs confirming. This says how a tool
	// is handed out in the first place. A `confirm`-tier tool that every
	// Agent has is ordinary; an opt-in tool nobody has until somebody
	// says so is a different question, and collapsing them would mean
	// raising a tool's risk tier silently narrowed who could hold it.
	OptIn bool `yaml:"optIn,omitempty"`
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
	// (ADR-W024 A10). nil means it emits none.
	Publishes *PublishesSpec `yaml:"publishes,omitempty"`
	// Integrates declares the package's integration adapters and the
	// closed-vocabulary queries they answer (integrates.go). Declaring an
	// adapter runs nothing at install: it is rendered per connection
	// placement, so a bundled adapter stays dormant until someone
	// creates a connection.
	Integrates []IntegrationSpec `yaml:"integrates,omitempty"`
	// ExternalConfig declares rendered ConfigFiles read by something
	// Wildstone does not run.
	ExternalConfig []ExternalConfigDecl `yaml:"externalConfig,omitempty"`
	// ImageSets group images that must be upgraded together.
	ImageSets []ImageSet `yaml:"imageSets,omitempty"`
	// AccessGroups names the household groups this service is for
	// (ADR-W033 design §4, slice 8).
	//
	// A DECLARATION, NOT A ROSTER. It says which of the household's
	// groups the service is meant for — `adults` for the password
	// manager, `family` for the media library — and the household
	// decides who is in those groups. A package author has never met
	// them and must not be naming people.
	//
	// SEED KEYS ONLY, and the closed set is checked by lint rather than
	// here (ADR-W025: this package must still LOAD a manifest written
	// against a newer vocabulary). The three seeded roles are the only
	// groups a package can name at all, because a household's own
	// groups have no name an author could have known — those are
	// assigned in the Console, against a group id.
	//
	// Empty means the service names no audience of its own. That is not
	// the same as "everyone": what it grants is decided by the
	// entitlement rows, which default to deny.
	AccessGroups []string `yaml:"accessGroups,omitempty" json:"accessGroups,omitempty"`
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
	if p.Metadata.Instanced {
		// Replacing a sibling makes no sense for a package whose point
		// is to run beside copies of itself.
		if p.Metadata.Exclusive {
			return &ValidationError{Field: "metadata.instanced", Msg: "an instanced package cannot also be exclusive"}
		}
		named := false
		for _, param := range p.Params {
			if param.Name == InstanceParam && param.Required {
				named = true
			}
		}
		if !named {
			return &ValidationError{Field: "params", Msg: "an instanced package must declare a required \"instance\" param"}
		}
	}

	for i, param := range p.Params {
		if param.Name == "" {
			return &ValidationError{Field: fmt.Sprintf("params[%d].name", i), Msg: "must not be empty"}
		}
		// The shape name itself, not the values — a package written
		// against a vocabulary this binary does not have must still
		// LOAD, so what a shape ACCEPTS is checked by lint and Expand.
		//
		// `enum:` is the one shape whose values are the package's own to
		// invent rather than a fixed ParamShapes entry (see
		// enumShapeValues), so it is checked structurally here instead of
		// by map lookup: a package author who writes `validate: enum:`
		// with nothing after the colon, or repeats a value, learns it at
		// `wild pkg lint` rather than shipping a picker with no options.
		if param.Validate != "" {
			if values, isEnum := enumShapeValues(param.Validate); isEnum {
				if err := validateEnumShapeDecl(values); err != nil {
					return &ValidationError{Field: fmt.Sprintf("params[%d].validate", i), Msg: err.Error()}
				}
			} else if ParamShapes[param.Validate] == nil {
				return &ValidationError{Field: fmt.Sprintf("params[%d].validate", i),
					Msg: fmt.Sprintf("%q is not a value shape (one of: %s, or enum:a|b|c for a closed choice)",
						param.Validate, strings.Join(ParamShapeNames(), ", "))}
			}
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
		if err := validateExports(p, prov, fmt.Sprintf("provides[%d].exports", i)); err != nil {
			return err
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

	if err := validateIntegrates(p); err != nil {
		return err
	}

	// accessGroups: shape only — non-empty and not repeated. WHICH
	// group names exist is vocabulary, and vocabulary is lint's
	// (ADR-W025), so a manifest naming a group this binary has never
	// heard of still loads.
	seenGroup := map[string]bool{}
	for i, g := range p.AccessGroups {
		field := fmt.Sprintf("accessGroups[%d]", i)
		if strings.TrimSpace(g) == "" {
			return &ValidationError{Field: field, Msg: "must not be empty"}
		}
		if seenGroup[g] {
			return &ValidationError{Field: field, Msg: fmt.Sprintf("%q declared twice", g)}
		}
		seenGroup[g] = true
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

	// A BLANK template entry is a mistake every time. It matters more
	// than it reads: capability.Expand treats a template that RENDERS to
	// nothing as a resource the package declined to produce for these
	// params, which is what makes a conditional resource expressible.
	// That reading is only safe while emptiness has to be written as an
	// action — text with nothing in it at all is the typo the old
	// "rendered to empty document" error was really aimed at, and this
	// is where it is still caught.
	for i, t := range p.Templates {
		if strings.TrimSpace(t) == "" {
			return &ValidationError{Field: fmt.Sprintf("templates[%d]", i),
				Msg: "is blank. A template that renders to nothing for SOME params is how a resource is made " +
					"conditional, but one with no content at all produces nothing for any of them"}
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

// exportStubFuncs is a FuncMap with the right NAMES and no behaviour, so
// an export template can be parsed here without pkgspec knowing anything
// about addressing. Parsing is what catches a misspelled function —
// text/template refuses `{{ orign ... }}` at parse time — and that is the
// error worth catching at lint, because the alternative is a render
// failure during an install the operator has already approved.
var exportStubFuncs = func() template.FuncMap {
	fm := template.FuncMap{}
	for _, name := range trait.ExportFuncs {
		fm[name] = func(...any) string { return "" }
	}
	return fm
}()

// paramFieldRegexp matches a .Params.<name> reference ANYWHERE in a
// template action, including nested in a function call — unlike
// paramRefRegexp below, which matches only a bare `{{ .Params.x }}` and
// exists for a different job (resolving defaults when parsing a template
// as YAML).
var paramFieldRegexp = regexp.MustCompile(`\.Params\.([A-Za-z_][A-Za-z0-9_]*)`)

// validateExports checks one `provides` entry's exports as STRUCTURE:
// non-empty values, templates that parse against the known functions, and
// param references the package actually declares.
//
// Whether the KEY is one this trait mode carries is a vocabulary question
// and belongs to lint (pkgtool), alongside the classification check, for
// the same reason: pkgspec must stay able to load a package written
// against a vocabulary this binary does not have, so that the diagnostic
// comes from the tool built to explain it rather than from a parser.
//
// The param check is the one that pays for itself. An export referencing
// .Params.public_domian renders empty under missingkey=zero, the export
// is dropped for being empty, and the consumer's template then fails with
// a missing .Resolved key three packages away from the typo.
func validateExports(p *Package, prov Provide, field string) error {
	if len(prov.Exports) == 0 {
		return nil
	}
	declared := map[string]bool{}
	for _, d := range p.Params {
		declared[d.Name] = true
	}
	keys := make([]string, 0, len(prov.Exports))
	for k := range prov.Exports {
		keys = append(keys, k)
	}
	sort.Strings(keys) // map order must not make the first failure vary between runs
	for _, key := range keys {
		kf := field + "." + key
		expr := prov.Exports[key]
		if strings.TrimSpace(expr) == "" {
			return &ValidationError{Field: kf, Msg: "must not be empty: omit the key rather than exporting nothing under it"}
		}
		if _, err := template.New(kf).Funcs(exportStubFuncs).Parse(expr); err != nil {
			return &ValidationError{Field: kf, Msg: fmt.Sprintf(
				"is not a valid template (%v); the functions available are %s plus Go's own built-ins",
				err, strings.Join(trait.ExportFuncs, ", "))}
		}
		for _, m := range paramFieldRegexp.FindAllStringSubmatch(expr, -1) {
			if !declared[m[1]] {
				return &ValidationError{Field: kf, Msg: fmt.Sprintf(
					"references .Params.%s, which this package does not declare — an export renders over "+
						"the PROVIDER's own params, so it can only offer what this package asks for", m[1])}
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
		return &ValidationError{Field: field + ".risk", Msg: "must be one of read, low, medium, high, domestic, confirm"}
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
	for _, r := range Routes(pkg) {
		label, _, _ := strings.Cut(r.Host, ".")
		if label != "" && !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	return out
}

// RouteDecl is one Route a package declares, read from the template as
// YAML with param references resolved to their defaults — the same way
// RouteLabels and ContainerNames read theirs, and for the reason F86
// gives: a regex understands only the shipped catalog's own formatting,
// so a package written in ordinary YAML is silently skipped.
//
// Enough of the spec to answer "what would the dashboard show for this",
// which is the caller this exists for: the reconciler's presentation
// table is keyed on the upstream container, and `portal` Routes are
// never listed at all.
type RouteDecl struct {
	Name           string
	Host           string
	Upstream       string
	Auth           string
	Classification string
}

// UpstreamContainer is the container half of "container/<name>:<port>",
// or "" for a Route pointing at an "ip:port" this package does not run.
func (r RouteDecl) UpstreamContainer() string {
	rest, ok := strings.CutPrefix(r.Upstream, "container/")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, ":")
	return name
}

// Routes returns every Route a package declares.
func Routes(pkg *Package) []RouteDecl {
	var out []RouteDecl
	for _, doc := range packageDocs(pkg) {
		if docString(doc, "kind") != "Route" {
			continue
		}
		out = append(out, RouteDecl{
			Name:           docString(doc, "metadata", "name"),
			Host:           docString(doc, "spec", "host"),
			Upstream:       docString(doc, "spec", "upstream"),
			Auth:           docString(doc, "spec", "auth"),
			Classification: docString(doc, "spec", "classification"),
		})
	}
	return out
}

// ResourceKey names one resource a package declares: its kind and its
// metadata name, which together are its identity in desired state.
type ResourceKey struct {
	Kind string
	Name string
}

// ResourceKeys are the resources a package declares, rendered with its
// param defaults. A name that renders empty under the defaults is left
// out: it has no identity until an operator supplies one.
func ResourceKeys(pkg *Package) []ResourceKey {
	// An instanced package's names come from .Capability, which is
	// different for every instance and every package; unrendered they
	// are all "-bot", which is no name at all. Its instances cannot
	// collide with each other or with anything else by construction,
	// and a lint renders them to check exactly that.
	if pkg.Metadata.Instanced {
		return nil
	}
	seen := map[ResourceKey]bool{}
	var out []ResourceKey
	for _, doc := range packageDocs(pkg) {
		k := ResourceKey{Kind: docString(doc, "kind"), Name: docString(doc, "metadata", "name")}
		if k.Kind == "" || k.Name == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
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
	// An instanced package names these per instance, from .Capability,
	// so there is no one name to report: the instance's own rendered
	// resources are the answer, and callers ask those instead.
	if pkg.Metadata.Instanced {
		return nil
	}
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

// ProvidedSecrets are the Secret resources a package declares as
// `provided: true` — the ones an operator must supply, because nothing
// in the control plane can invent them (a vendor's bot token, an API
// key).
//
// The names differ per package and are not derivable from a convention:
// the Telegram adapter reads chat-telegram-family-bot and the Matrix one
// chat-matrix-family-token. A form that assumed either would write the
// operator's token into a secret nothing reads, leave the declared one
// absent, and produce a container the reconciler correctly refuses to
// start — with the screen showing the step as done.
func ProvidedSecrets(pkg *Package) []string {
	// An instanced package names these per instance, from .Capability,
	// so there is no one name to report: the instance's own rendered
	// resources are the answer, and callers ask those instead.
	if pkg.Metadata.Instanced {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, doc := range packageDocs(pkg) {
		if docString(doc, "kind") != "Secret" {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		if provided, _ := spec["provided"].(bool); !provided {
			continue
		}
		name := docString(doc, "spec", "name")
		if name == "" {
			name = docString(doc, "metadata", "name")
		}
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

var (
	templateActionRegexp = regexp.MustCompile(`\{\{[^}]*\}\}`)
	paramRefRegexp       = regexp.MustCompile(`\{\{\s*\.Params\.([a-z_]+)\s*\}\}`)
)
