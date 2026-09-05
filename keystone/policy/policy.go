// Package policy is the E4 Policy Engine (ADR-W009): the proposal §9.2
// standing invariants as validation rules over every proposed target
// tree, on every write path — API proposal, out-of-band ingestion, and
// the Butler. Rules cannot be turned off by a bad prompt, a bad UI
// action, or a bad package; the only way past one is an explicit, logged
// exemption (ADR-W009 A3).
package policy

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// Rule IDs (stable: exemptions and violations reference them).
const (
	RuleClassificationVocabulary = "classification-vocabulary"
	RulePublicRequiresAuth       = "public-requires-auth"
	RuleAdminStructural          = "admin-structural"
	RuleSecretValuesForbidden    = "secret-values-forbidden"
	RuleButlerAdminScope         = "butler-admin-scope"
	RuleAutomationActionTier     = "automation-action-tier"
)

// automationTiers is the closed set of action tiers a household rule may
// carry (ADR-W022 A5): read, and domestic — the concierge tier the
// domestic gateway executes. Infrastructure tiers (low, medium, high)
// belong to plans; a rule cannot restart a container, whoever wrote it.
var automationTiers = map[string]bool{"read": true, "domestic": true}

// Classifications is the closed vocabulary, adopted unchanged from the
// reference deployment (ADR-W009 A2 / scope Appendix B).
var Classifications = map[string]bool{
	"family_public":   true,
	"family_private":  true,
	"admin_tailscale": true,
	"internal_only":   true,
	"deprecated":      true,
}

const (
	classPublic  = "family_public"
	classPrivate = "family_private"
	classAdmin   = "admin_tailscale"
	authNone     = "none"
)

// Violation is one standing-invariant breach in a proposed target tree.
type Violation struct {
	Rule string `json:"rule"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	Msg  string `json:"msg"`
}

func (v Violation) String() string {
	return fmt.Sprintf("%s: %s/%s: %s", v.Rule, v.Kind, v.Name, v.Msg)
}

// Change is one proposed transition for validation: the FULL target tree
// the change would produce, the current tree it departs from, and who is
// making it.
type Change struct {
	Proposed []resource.Resource
	Current  []resource.Resource
	// Principal names the actor (audit context in violation events).
	Principal string
	// Role is the RBAC role string ("admin" | "operator" | "butler");
	// "" for system paths (ingestion). Only "butler" changes behavior
	// (RuleButlerAdminScope).
	Role string
}

// Engine validates Changes against the standing invariants. The zero
// value enforces every rule with no exemptions.
type Engine struct {
	exemptions []Exemption
}

// NewEngine builds an engine with the given exemptions (already
// validated by LoadExemptions or Exemption.Validate).
func NewEngine(exemptions []Exemption) *Engine {
	return &Engine{exemptions: exemptions}
}

// Validate checks the change against every rule. It returns the
// violations that stand, and separately the violations that were
// suppressed by an exemption — callers MUST surface the exempted ones
// (event/log): silent exemptions are how invariants rot (ADR-W009 A3).
//
// Only resources the change TOUCHES (added, modified, or deleted
// relative to Current) can produce violations: a pre-existing violation
// on an untouched resource must not wedge every unrelated change — it is
// legacy state to surface as drift, not a reason to block a new volume
// (ADR-W009 A1). The transition rules are unaffected: they compare
// current vs proposed of the same name, which is by definition a touched
// resource.
func (e *Engine) Validate(ch Change) (violations, exempted []Violation) {
	currentRoutes := map[string]*resource.RouteSpec{}
	currentByKey := map[string]resource.Resource{}
	for _, r := range ch.Current {
		if r.Kind == resource.KindRoute && r.Route != nil {
			currentRoutes[r.Metadata.Name] = r.Route
		}
		currentByKey[string(r.Kind)+"/"+r.Metadata.Name] = r
	}
	proposedRoutes := map[string]bool{}

	var all []Violation
	for _, r := range ch.Proposed {
		if cur, ok := currentByKey[string(r.Kind)+"/"+r.Metadata.Name]; ok && reflect.DeepEqual(cur, r) {
			if r.Kind == resource.KindRoute && r.Route != nil {
				proposedRoutes[r.Metadata.Name] = true
			}
			continue // untouched: carried over verbatim
		}
		switch r.Kind {
		case resource.KindRoute:
			if r.Route == nil {
				continue
			}
			proposedRoutes[r.Metadata.Name] = true
			all = append(all, routeRules(r, currentRoutes, ch.Role)...)
		case resource.KindContainer:
			if r.Container == nil {
				continue
			}
			all = append(all, envSecretRules(r, r.Container.Env)...)
		case resource.KindJob:
			// A Job carries a full ContainerSpec and runs it, so its env
			// is subject to exactly the rules a Container's is. It is
			// also the likeliest place for a literal to appear: a
			// provisioning job needs an admin password to do its work,
			// which makes "just paste it in" the obvious wrong move.
			if r.Job == nil {
				continue
			}
			all = append(all, envSecretRules(r, r.Job.Run.Env)...)
		case resource.KindAutomation:
			if r.Automation == nil {
				continue
			}
			all = append(all, automationRules(r)...)
		}
	}
	// Deletions of admin routes by the butler (RuleButlerAdminScope
	// covers modify/create above; a delete is a route present in current
	// and absent from proposed).
	if ch.Role == "butler" {
		for name, spec := range currentRoutes {
			if spec.Classification == classAdmin && !proposedRoutes[name] {
				all = append(all, Violation{
					Rule: RuleButlerAdminScope, Kind: string(resource.KindRoute), Name: name,
					Msg: "butler principals may not delete admin_tailscale routes",
				})
			}
		}
	}

	for _, v := range all {
		if e.isExempt(v) {
			exempted = append(exempted, v)
		} else {
			violations = append(violations, v)
		}
	}
	return violations, exempted
}

// routeRules applies the Route-scoped invariants to one proposed route.
func routeRules(r resource.Resource, currentRoutes map[string]*resource.RouteSpec, role string) []Violation {
	var out []Violation
	name := r.Metadata.Name
	spec := r.Route
	cur := currentRoutes[name]

	if !Classifications[spec.Classification] {
		out = append(out, Violation{
			Rule: RuleClassificationVocabulary, Kind: string(r.Kind), Name: name,
			Msg: fmt.Sprintf("classification %q is not in the closed vocabulary %s",
				spec.Classification, vocabList()),
		})
	}

	// §9.2-2, with the S4 downgrade-laundering guard: "currently public"
	// keeps the rule armed even when the same change re-classifies.
	proposedPublic := spec.Classification == classPublic
	currentlyPublic := cur != nil && cur.Classification == classPublic
	if spec.Auth == authNone && (proposedPublic || currentlyPublic) {
		reason := "is classified " + classPublic
		if !proposedPublic {
			reason = "is currently classified " + classPublic +
				" (a classification downgrade in the same change does not lift the rule)"
		}
		out = append(out, Violation{
			Rule: RulePublicRequiresAuth, Kind: string(r.Kind), Name: name,
			Msg: fmt.Sprintf("route %s and may not have auth disabled", reason),
		})
	}

	// §9.2-1: admin surfaces never migrate onto family/public ingress.
	if cur != nil && cur.Classification == classAdmin &&
		(spec.Classification == classPublic || spec.Classification == classPrivate) {
		out = append(out, Violation{
			Rule: RuleAdminStructural, Kind: string(r.Kind), Name: name,
			Msg: fmt.Sprintf("admin_tailscale route may not be re-classified to %q (admin surfaces are structural, §9.2-1)",
				spec.Classification),
		})
	}

	// Per-principal scoping: the butler never touches admin surfaces.
	if role == "butler" {
		touchesAdmin := spec.Classification == classAdmin || (cur != nil && cur.Classification == classAdmin)
		changed := cur == nil || *cur != *spec
		if touchesAdmin && changed {
			out = append(out, Violation{
				Rule: RuleButlerAdminScope, Kind: string(r.Kind), Name: name,
				Msg: "butler principals may not create or modify admin_tailscale routes",
			})
		}
	}
	return out
}

// automationRules applies the household-rule invariants (ADR-W022 A5) to
// one proposed Automation, for every principal: the Butler is the
// expected author, but a human pasting a rule is held to the same line.
//
// The tier rule is the load-bearing one. An Automation's action runs
// without a per-firing approval — that is what makes it a rule — so the
// only actions it may carry are the ones the domestic gateway is allowed
// to execute unattended. Anything that would change infrastructure is a
// plan, and stays a plan.
//
// Its params are held to the container-env secret rule: a rule that
// carries a token in a param has put a secret in the Statefile, and the
// engine would hand it to a tool and, through escalation, to a model.
func automationRules(r resource.Resource) []Violation {
	var out []Violation
	a := r.Automation
	if !automationTiers[a.Action.Tier] {
		out = append(out, Violation{
			Rule: RuleAutomationActionTier, Kind: string(r.Kind), Name: r.Metadata.Name,
			Msg: fmt.Sprintf("action tier %q: a household rule may only carry read or domestic actions; infrastructure changes are plans, not rules (ADR-W022 A5)", a.Action.Tier),
		})
	}
	out = append(out, envSecretRules(r, a.Action.Params)...)
	return out
}

// secretKeyRegexp mirrors keystone/redact's key heuristic: an env KEY
// that names a credential. Values under such keys must be secret/<name>
// references (ADR-W009 A2: the Statefile stays clean now so E8's secrets
// path has nothing to migrate).
var secretKeyRegexp = regexp.MustCompile(`(?i)(^|[_-])(password|passwd|secret|token|api[_-]?key)([_-]|$)`)

// secretRefValueRegexp matches a full secret reference value.
var secretRefValueRegexp = regexp.MustCompile(`^secret/[A-Za-z0-9._-]+$`)

// envSecretRules checks a container environment for literal secrets. It
// takes the env map rather than reading it off r.Container, because more
// than one kind carries a ContainerSpec now: a rule that reached through
// a single field would silently stop applying the moment a second kind
// could run a container, which is exactly what happened when Job was
// added and this function still said r.Container.Env.
func envSecretRules(r resource.Resource, env map[string]string) []Violation {
	var out []Violation
	for key, value := range env {
		switch {
		case secretKeyRegexp.MatchString(key) && !secretRefValueRegexp.MatchString(value):
			out = append(out, Violation{
				Rule: RuleSecretValuesForbidden, Kind: string(r.Kind), Name: r.Metadata.Name,
				Msg: fmt.Sprintf("env %q is credential-shaped and must carry a secret/<name> reference, not a literal (§9.2-3)", key),
			})
		case redact.SecretShaped(value):
			out = append(out, Violation{
				Rule: RuleSecretValuesForbidden, Kind: string(r.Kind), Name: r.Metadata.Name,
				Msg: fmt.Sprintf("env %q value is secret-shaped; literal secrets never enter the Statefile (§9.2-3)", key),
			})
		}
	}
	return out
}

func (e *Engine) isExempt(v Violation) bool {
	for _, ex := range e.exemptions {
		if ex.Rule == v.Rule && ex.Kind == v.Kind && ex.Name == v.Name {
			return true
		}
	}
	return false
}

// ClassificationNames returns the closed vocabulary, sorted. Exported so
// callers outside the engine — pkgtool's lint, which checks a package's
// declared classifications (F31) — name the same set the engine enforces
// rather than keeping their own copy of it.
func ClassificationNames() []string {
	names := make([]string, 0, len(Classifications))
	for c := range Classifications {
		names = append(names, c)
	}
	sort.Strings(names)
	return names
}

func vocabList() string {
	return "[" + strings.Join(ClassificationNames(), ", ") + "]"
}
