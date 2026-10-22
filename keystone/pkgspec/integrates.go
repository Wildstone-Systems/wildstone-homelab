package pkgspec

// The `integrates:` block (docs/catalog-integration-contract.md §4,
// ADR-W030 A5/A5.1, epic #326 S01).
//
// It declares what a package's integration adapter NEEDS and which
// closed-vocabulary queries it ANSWERS. It never says where the adapter
// runs: placement, fallback nodes and grants are connection state set by
// an approved Plan, and the moment a package names a node, adding a
// service stops being data (§4, "No placement in the package").
//
// Declaring an adapter does not run one. Installing the package renders
// nothing from this block: the adapter container is rendered per
// connection placement (capability.Engine.ExpandAdapter), and a
// connection exists only once a Person creates one. That is what keeps a
// bundled adapter DORMANT — installing email reads no mailbox.
//
// FAIL CLOSED, UNLIKE TRAITS. Trait and event vocabulary is lint's, so a
// manifest written against a newer vocabulary still loads (ADR-W025).
// This block is different in kind: it is a dispatch surface. A Keystone
// that cannot understand an adapter declaration must refuse the package
// rather than half-understand it, so Validate checks the domain, every
// answered query and the adapter contract version against what THIS
// binary knows. An older Keystone refuses it one step earlier — strict
// parsing rejects the unknown `integrates` key — which is the same
// answer.

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/integration"
)

// AdapterContract is the newest adapter contract this Keystone
// understands: the integration-work claim/result envelope, the
// environment the adapter is rendered with (AdapterEnv*), and the
// program mount below. A package declaring a newer one is refused at
// load rather than rendered into a container this control plane cannot
// drive.
const AdapterContract = 1

// AdapterProgramDir is where the installer puts Wildstone's own adapter
// programs on every node, and AdapterProgramMount where a rendered
// adapter container sees them — the chat-adapter/agent-bridge layout
// (ADR-W033 B7.1), so no image of Wildstone's is published anywhere.
const (
	AdapterProgramDir   = "/srv/wildstone/bin"
	AdapterProgramMount = "/opt/wildstone/bin"
)

// AdapterPrograms are the adapter programs a release of this Keystone
// ships and its installer places under AdapterProgramDir. Closed for the
// same fail-closed reason as the contract: a package naming a program
// this release did not ship would render a container that cannot start
// on any node, so it is refused at load instead.
var AdapterPrograms = map[string]bool{
	// keystone/cmd/wild-mail-adapter (epic #326).
	"wild-mail-adapter": true,
}

// Integration shapes: the package's overall relationship to the
// application (§5). Distinct from publishes.shape, which only says how
// events reach the intake.
var integrationShapes = map[string]bool{
	"native": true, "sidecar": true, "config": true,
	// adapter: a Wildstone adapter runs beside a Wildstone-managed app.
	"adapter": true,
	// connection-only: the app is not Wildstone's to run (a mail
	// provider), but the adapter runtime still is — "connection-only
	// describes ownership of the remote app, not an unmanaged adapter
	// runtime" (ADR-W030 A5.1).
	"connection-only": true,
}

// shapesWithAdapter are the shapes whose runtime is a Wildstone adapter.
var shapesWithAdapter = map[string]bool{"adapter": true, "connection-only": true}

// secretScopes: whose credential an adapter secret is (§4).
var secretScopes = map[string]bool{"household": true, "member": true}

var (
	domainNameRegexp    = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	adapterProgramRegex = regexp.MustCompile(`^[a-z][a-z0-9-]{1,40}$`)
	adapterSecretRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
	// credentialParamRegexp is a param name no adapter may inherit: the
	// adapter's credentials come from the node secret path into its
	// scoped environment, never from package params (ADR-W030 A5.1).
	credentialParamRegexp = regexp.MustCompile(`(password|passwd|secret|token|apikey|api_key|credential)`)
)

// maxAnswerDeadline bounds a declared query deadline. The interactive
// default is 5s end to end (A5.1); a minute is already a background job.
const maxAnswerDeadline = 60 * time.Second

// IntegrationSpec is one `integrates:` entry. A list, not one block,
// because one package can integrate more than one domain at different
// shapes — mail-roundcube is mail (connection-only, against the
// provider) and, later, calendar (against Davis) — and that is cheap to
// allow now and expensive later (§8 Q5).
type IntegrationSpec struct {
	// Domain is a closed keystone/integration domain.
	Domain string `yaml:"domain" json:"domain"`
	// Shape is one of integrationShapes.
	Shape string `yaml:"shape" json:"shape"`
	// Adapter is required for the adapter and connection-only shapes and
	// refused for the others.
	Adapter *AdapterSpec `yaml:"adapter,omitempty" json:"adapter,omitempty"`
	// Answers are the queries this implementation answers, from the
	// domain's closed vocabulary. Queries only: actions stay in
	// agentTools (§2), and a read-only domain has none to name.
	Answers []AnswerSpec `yaml:"answers,omitempty" json:"answers,omitempty"`
}

// AdapterSpec is what an adapter needs, never where it runs.
type AdapterSpec struct {
	// Contract is the adapter contract version this declaration is
	// written against; see AdapterContract.
	Contract int `yaml:"contract" json:"contract"`
	// Image is the digest-pinned base the program runs in.
	Image string `yaml:"image" json:"image"`
	// Program is the Wildstone adapter program the installer ships under
	// AdapterProgramDir, e.g. wild-mail-adapter.
	Program string `yaml:"program" json:"program"`
	// Inherits names the installed package's own params the adapter is
	// given, so a connection reuses the endpoint the household already
	// configured (for mail: imap_host, imap_port, imap_encryption,
	// username_domain) rather than asking for it again. A param that
	// could hold a credential cannot be inherited.
	Inherits []string `yaml:"inherits,omitempty" json:"inherits,omitempty"`
	// Needs lists mounts and secrets.
	Needs AdapterNeeds `yaml:"needs,omitempty" json:"needs,omitempty"`
}

// AdapterNeeds is what the adapter reaches its app with.
type AdapterNeeds struct {
	Mounts  []Mount             `yaml:"mounts,omitempty" json:"mounts,omitempty"`
	Secrets []AdapterSecretNeed `yaml:"secrets,omitempty" json:"secrets,omitempty"`
}

// AdapterSecretNeed is a credential an adapter needs, by reference. The
// value never appears in a package, a param, a work item or a result.
type AdapterSecretNeed struct {
	Name string `yaml:"name" json:"name"`
	// Scope is household or member. A member-scoped secret is a Person's
	// own credential: its connections default to personal, read-only,
	// live-query-only (§4), and it exists only once that Person onboards
	// it — never at install.
	Scope   string `yaml:"scope" json:"scope"`
	Purpose string `yaml:"purpose" json:"purpose"`
}

// AnswerSpec is one query an implementation answers.
type AnswerSpec struct {
	Query string `yaml:"query" json:"query"`
	// Deadline is a Go duration, within maxAnswerDeadline. Empty means
	// the interactive default.
	Deadline string `yaml:"deadline,omitempty" json:"deadline,omitempty"`
	// Supports is the subset of the query's params this implementation
	// honours (§4). Checked against the vocabulary by lint.
	Supports []string `yaml:"supports,omitempty" json:"supports,omitempty"`
}

// AdapterFor returns the adapter declaration of the given domain, if the
// package declares one.
func (p *Package) AdapterFor(domain string) (IntegrationSpec, bool) {
	for _, in := range p.Integrates {
		if in.Domain == domain && in.Adapter != nil {
			return in, true
		}
	}
	return IntegrationSpec{}, false
}

func validateIntegrates(p *Package) error {
	declared := map[string]bool{}
	for _, param := range p.Params {
		declared[param.Name] = true
	}
	seenDomain := map[string]bool{}
	for i, in := range p.Integrates {
		field := fmt.Sprintf("integrates[%d]", i)
		if !domainNameRegexp.MatchString(in.Domain) {
			return &ValidationError{Field: field + ".domain", Msg: fmt.Sprintf("%q must be a lowercase domain name", in.Domain)}
		}
		domain, known := integration.Lookup(in.Domain)
		if !known {
			return &ValidationError{Field: field + ".domain", Msg: fmt.Sprintf(
				"%q is not an integration domain this Keystone knows; an adapter declaration it cannot understand is refused, not half-run", in.Domain)}
		}
		if seenDomain[in.Domain] {
			return &ValidationError{Field: field + ".domain", Msg: fmt.Sprintf("%q declared twice", in.Domain)}
		}
		seenDomain[in.Domain] = true
		if !integrationShapes[in.Shape] {
			return &ValidationError{Field: field + ".shape", Msg: "must be one of native, sidecar, config, adapter, connection-only"}
		}
		if shapesWithAdapter[in.Shape] != (in.Adapter != nil) {
			if in.Adapter == nil {
				return &ValidationError{Field: field + ".adapter", Msg: fmt.Sprintf("shape %s needs an adapter block", in.Shape)}
			}
			return &ValidationError{Field: field + ".adapter", Msg: fmt.Sprintf("shape %s runs no Wildstone adapter; only adapter and connection-only do", in.Shape)}
		}
		if in.Adapter != nil {
			if err := validateAdapter(*in.Adapter, field+".adapter", declared); err != nil {
				return err
			}
		}
		seenQuery := map[string]bool{}
		for j, a := range in.Answers {
			af := fmt.Sprintf("%s.answers[%d]", field, j)
			if integration.IsAction(a.Query) {
				return &ValidationError{Field: af + ".query", Msg: fmt.Sprintf(
					"%q is an action; answers carries reads only, and actions are declared as agentTools", a.Query)}
			}
			if !strings.HasPrefix(a.Query, domain.Name+".") || !integration.IsQuery(a.Query) {
				return &ValidationError{Field: af + ".query", Msg: fmt.Sprintf(
					"%q is not a %s query this Keystone knows (%s)", a.Query, domain.Name, strings.Join(queryNames(domain), ", "))}
			}
			if seenQuery[a.Query] {
				return &ValidationError{Field: af + ".query", Msg: fmt.Sprintf("%q declared twice", a.Query)}
			}
			seenQuery[a.Query] = true
			if a.Deadline != "" {
				d, err := time.ParseDuration(a.Deadline)
				if err != nil || d <= 0 || d > maxAnswerDeadline {
					return &ValidationError{Field: af + ".deadline", Msg: fmt.Sprintf("%q must be a duration above 0 and at most %s", a.Deadline, maxAnswerDeadline)}
				}
			}
			seenSupport := map[string]bool{}
			for k, s := range a.Supports {
				if strings.TrimSpace(s) == "" || seenSupport[s] {
					return &ValidationError{Field: fmt.Sprintf("%s.supports[%d]", af, k), Msg: fmt.Sprintf("%q is empty or repeated", s)}
				}
				seenSupport[s] = true
			}
		}
	}
	return nil
}

func validateAdapter(a AdapterSpec, field string, declared map[string]bool) error {
	if a.Contract < 1 {
		return &ValidationError{Field: field + ".contract", Msg: "must name the adapter contract version it was written against"}
	}
	if a.Contract > AdapterContract {
		return &ValidationError{Field: field + ".contract", Msg: fmt.Sprintf(
			"adapter contract %d is newer than this Keystone understands (%d); upgrade Wildstone before installing this package", a.Contract, AdapterContract)}
	}
	if !strings.Contains(a.Image, "@sha256:") {
		return &ValidationError{Field: field + ".image", Msg: "must be pinned by digest (image@sha256:...)"}
	}
	if !adapterProgramRegex.MatchString(a.Program) {
		return &ValidationError{Field: field + ".program", Msg: fmt.Sprintf("%q must be a Wildstone adapter program name, e.g. wild-mail-adapter", a.Program)}
	}
	if !AdapterPrograms[a.Program] {
		return &ValidationError{Field: field + ".program", Msg: fmt.Sprintf(
			"%q is not an adapter program this release ships; upgrade Wildstone before installing this package", a.Program)}
	}
	seen := map[string]bool{}
	for i, name := range a.Inherits {
		f := fmt.Sprintf("%s.inherits[%d]", field, i)
		if !declared[name] {
			return &ValidationError{Field: f, Msg: fmt.Sprintf("%q is not a param of this package", name)}
		}
		if credentialParamRegexp.MatchString(strings.ToLower(name)) {
			return &ValidationError{Field: f, Msg: fmt.Sprintf(
				"%q looks like a credential; an adapter's credentials come from the node secret path, never from package params", name)}
		}
		if seen[name] {
			return &ValidationError{Field: f, Msg: fmt.Sprintf("%q declared twice", name)}
		}
		seen[name] = true
	}
	for i, m := range a.Needs.Mounts {
		f := fmt.Sprintf("%s.needs.mounts[%d]", field, i)
		if m.Source == "" || m.Target == "" {
			return &ValidationError{Field: f, Msg: "needs a source and a target"}
		}
		if !m.ReadOnly {
			// An adapter reads its app's store; writing behind the app's
			// back desynchronises it (§7.4).
			return &ValidationError{Field: f + ".readOnly", Msg: "an adapter mount must be read-only"}
		}
	}
	seenSecret := map[string]bool{}
	for i, s := range a.Needs.Secrets {
		f := fmt.Sprintf("%s.needs.secrets[%d]", field, i)
		if !adapterSecretRegexp.MatchString(s.Name) {
			return &ValidationError{Field: f + ".name", Msg: fmt.Sprintf("%q must be a secret name", s.Name)}
		}
		if seenSecret[s.Name] {
			return &ValidationError{Field: f + ".name", Msg: fmt.Sprintf("%q declared twice", s.Name)}
		}
		seenSecret[s.Name] = true
		if !secretScopes[s.Scope] {
			return &ValidationError{Field: f + ".scope", Msg: "must be household or member"}
		}
		if strings.TrimSpace(s.Purpose) == "" {
			return &ValidationError{Field: f + ".purpose", Msg: "must say what the credential is for"}
		}
	}
	return nil
}

func queryNames(d integration.Domain) []string {
	out := make([]string, 0, len(d.Queries))
	for _, q := range d.Queries {
		out = append(out, q.Name)
	}
	return out
}
