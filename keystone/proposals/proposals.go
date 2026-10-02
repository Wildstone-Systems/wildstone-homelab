// Package proposals is the ONE proposal pipeline (ADR-W017 A3), moved
// out of keystone/api so that keystone/agentgateway — which cannot
// import keystone/api without a cycle — can file a Plan for a runtime
// exactly as the four HTTP-reachable callers do (ADR-W039 Phase 3,
// #339).
//
// This package holds no HTTP handler and no Server: it is handed a
// Deps value (a database, a Statefile, a policy engine, a way to
// record an operational event) and a caller's principal.Identity, and
// it validates, diffs, checks standing policy and inserts the
// `proposed` plan record. Everything above that line — who may call
// it, what the caller called it FOR (the operator's POST /plans, a
// Butler's propose_change, a capability install, an Agent's Propose)
// — stays with the caller. keystone/api keeps thin wrappers over File
// so its own four call sites and their tests are unchanged (#339's own
// evidence bar).
//
// keystone/proposals must never import keystone/api — see
// structural_test.go — because keystone/agentgateway imports this
// package, and agentgateway already cannot import api (Phase 2c's own
// note: "api importing agentgateway is fine; only agentgateway->api is
// forbidden"). A cycle through proposals would reintroduce exactly
// that.
package proposals

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/research"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

// ErrNoOp is a proposal that would change nothing: every resource it puts
// is already desired exactly as given, and it deletes nothing.
var ErrNoOp = errors.New("proposal is a no-op against current desired state")

// StoredProposal and DeletionRef are the shared shapes from
// keystone/plan (the reconcile loop's drift path writes the same
// document). Aliased here so this package's own field types read as
// its own vocabulary; keystone/api aliases these same names in turn so
// its call sites are unchanged.
type (
	StoredProposal = plan.StoredProposal
	DeletionRef    = plan.DeletionRef
)

// Request is the plan-proposal body (ADR-W007 K3): the ONLY way any
// principal initiates a change (ground rule 4). Resources travel as
// canonical YAML documents (the Statefile's own schema).
type Request struct {
	Summary string        `json:"summary"`
	Put     []string      `json:"put,omitempty"`
	Delete  []DeletionRef `json:"delete,omitempty"`
}

// Options carries what a caller knows that the generic propose entry
// does not. Both fields exist for ADR-W021: Origin records what a
// capability install was ASKED FOR (A2), and DryRun returns the plan a
// proposal WOULD make without recording it (A5). PrivateOrigin is set
// by every Butler/Agent-originated proposal so it does not surface as
// a capability install in the Console's own listing.
type Options struct {
	PrivateOrigin bool
	Origin        *plan.CapabilityOrigin
	DryRun        bool
}

// Deps is what File needs from whoever is filing: the Statefile, the
// standing policy engine, the database the plan record and its audit
// event land in, and a logger. Event mirrors (*api.Server).event's own
// shape without this package importing api; a nil Event silently
// drops the operational event rather than panicking, which is the
// safe default for a caller (a test fixture, an early gateway wiring)
// that has not wired one yet.
type Deps struct {
	DB     *runtimedb.DB
	State  *statefile.Store
	Policy *policy.Engine
	Log    *slog.Logger
	// Event records an operational event ("plan.proposed",
	// "policy.rejected", "policy.exempted") the same way
	// (*api.Server).event does. planID is empty for an event that
	// precedes the plan record (a policy exemption or rejection).
	Event func(typ, planID string, id principal.Identity, msg string)
	// Preflight, when set, checks the proposed resources against the
	// node before a plan is recorded: today, whether a BackupPlan's local
	// copy fits its disk (ADR-W045 A3.1). A refusal is a 422 carrying its
	// reason; nil skips the check.
	Preflight func(puts []resource.Resource) error
}

func (d Deps) event(typ, planID string, id principal.Identity, msg string) {
	if d.Event == nil {
		return
	}
	d.Event(typ, planID, id, msg)
}

// PlanView is what a proposal pipeline returns for a plan record. The
// runtime DB stores the proposal as one JSON blob (plan_json);
// returning that blob verbatim made every consumer parse JSON out of
// a JSON string, so this parses it once and returns the plan the way
// every other field already reads.
type PlanView struct {
	Research  *research.Change `json:"research,omitempty"`
	AuditOnly bool             `json:"auditOnly,omitempty"`
	runtimedb.PlanRecord
	// PlanJSON must compete for the embedded field's NAME to suppress
	// it. `json:"-"` does not shadow — it removes this field from
	// consideration entirely, leaving PlanRecord.PlanJSON the only
	// candidate for "planJson", which is how the blob kept being
	// served after the commit that meant to drop it (F25). Same name
	// plus omitempty: this field wins on depth and is omitted for
	// being empty.
	PlanJSON    string        `json:"planJson,omitempty"`
	BasedOnHead string        `json:"basedOnHead,omitempty"`
	Plan        *plan.Plan    `json:"plan,omitempty"`
	Put         []string      `json:"put,omitempty"`
	Delete      []DeletionRef `json:"delete,omitempty"`
	// Origin is what a capability install was asked for (ADR-W021 A2).
	Origin *plan.CapabilityOrigin `json:"origin,omitempty"`
	// Setting is a household setting the plan changes instead of
	// desired state (ADR-W033 E2).
	Setting *plan.SettingChange `json:"setting,omitempty"`
	Routine *plan.RoutineChange `json:"routine,omitempty"`
}

// ViewPlan projects one record. A blob that will not parse is not
// fatal: the record's own columns (state, risk, summary) are the
// audit trail and are still worth returning, so the plan field is
// simply absent — a corrupt blob already fails loudly at approval,
// which is the moment it matters.
func ViewPlan(rec runtimedb.PlanRecord) PlanView {
	v := PlanView{PlanRecord: rec}
	if rec.PlanJSON == "" {
		return v
	}
	var stored StoredProposal
	if err := json.Unmarshal([]byte(rec.PlanJSON), &stored); err != nil {
		return v
	}
	v.BasedOnHead, v.Plan, v.Put, v.Delete = stored.BasedOnHead, stored.Plan, stored.Put, stored.Delete
	v.Origin = stored.Origin
	v.Research = stored.Research
	v.Setting = stored.Setting
	v.Routine = stored.Routine
	v.AuditOnly = stored.AuditOnly
	return v
}

// ViewPlans projects a list of records, in place order.
func ViewPlans(recs []runtimedb.PlanRecord) []PlanView {
	out := make([]PlanView, 0, len(recs))
	for _, rec := range recs {
		out = append(out, ViewPlan(rec))
	}
	return out
}

// ApplyChanges returns current + puts − deletes, replacing by (kind,
// name).
func ApplyChanges(current, puts []resource.Resource, deletes []DeletionRef) []resource.Resource {
	type key struct {
		kind resource.Kind
		name string
	}
	dropped := make(map[key]bool, len(deletes)+len(puts))
	for _, d := range deletes {
		dropped[key{resource.Kind(d.Kind), d.Name}] = true
	}
	replaced := make(map[key]bool, len(puts))
	for _, p := range puts {
		replaced[key{p.Kind, p.Metadata.Name}] = true
	}
	out := make([]resource.Resource, 0, len(current)+len(puts))
	for _, r := range current {
		k := key{r.Kind, r.Metadata.Name}
		if dropped[k] || replaced[k] {
			continue
		}
		out = append(out, r)
	}
	return append(out, puts...)
}

// File is the ONE proposal pipeline (ADR-W017 A3): validate every
// document, check standing policy, compute the plan, and insert the
// `proposed` record. keystone/api's HTTP propose endpoint, the
// Butler/Agent host and every capability install funnel through here
// (via keystone/api's thin wrappers, or directly for
// keystone/agentgateway), so a Butler- or Agent-originated proposal is
// held to exactly the checks a human's is — same validation, same
// policy engine, same 422-on-violation, same audit events.
func File(deps Deps, id principal.Identity, req Request, opts Options) (any, int, error) {
	if req.Summary == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("summary required")
	}
	if len(req.Put) == 0 && len(req.Delete) == 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("empty proposal: nothing to put or delete")
	}

	// Parse + validate every proposed resource up front; a proposal
	// with one bad document is rejected whole.
	puts := make([]resource.Resource, 0, len(req.Put))
	for i, doc := range req.Put {
		res, err := resource.Unmarshal([]byte(doc))
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("put[%d]: %w", i, err)
		}
		if err := res.Validate(); err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("put[%d]: %w", i, err)
		}
		puts = append(puts, res)
	}
	for i, d := range req.Delete {
		if !resource.KnownKind(resource.Kind(d.Kind)) {
			return nil, http.StatusBadRequest, fmt.Errorf("delete[%d]: unknown kind %q", i, d.Kind)
		}
		if d.Name == "" {
			return nil, http.StatusBadRequest, fmt.Errorf("delete[%d]: name required", i)
		}
	}

	// Compute the plan against the current desired state: desired' =
	// current + puts − deletes; the diff is what approval will commit.
	current, err := deps.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("loading statefile: %w", err)
	}
	head, err := deps.State.Head()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	desired := ApplyChanges(current, puts, req.Delete)

	// Standing invariants (ADR-W009 A1): a policy-invalid change never
	// becomes a plan record, whoever proposed it. Exemption uses are
	// surfaced as events — never silent.
	violations, exempted := deps.Policy.Validate(policy.Change{
		Proposed:  desired,
		Current:   current,
		Principal: id.Name,
		Role:      string(id.Role),
	})
	for _, v := range exempted {
		deps.event("policy.exempted", "", id, v.String())
	}
	if len(violations) > 0 {
		msgs := make([]string, 0, len(violations))
		for _, v := range violations {
			msgs = append(msgs, v.String())
		}
		deps.event("policy.rejected", "", id, strings.Join(msgs, "; "))
		return nil, http.StatusUnprocessableEntity,
			fmt.Errorf("proposal violates standing policy: %s", strings.Join(msgs, "; "))
	}

	for _, res := range puts {
		if res.Kind == resource.KindAutomation {
			return nil, http.StatusBadRequest, fmt.Errorf("Automation changes require an exact-version routine Plan; save a routine draft with its output audience and submit it for Owner review")
		}
	}

	if deps.Preflight != nil {
		if err := deps.Preflight(puts); err != nil {
			deps.event("preflight.rejected", "", id, err.Error())
			return nil, http.StatusUnprocessableEntity, fmt.Errorf("proposal does not fit the node: %w", err)
		}
	}

	p := plan.Diff(desired, current)
	if len(p.Steps) == 0 {
		return nil, http.StatusBadRequest, ErrNoOp
	}
	p.Summary = req.Summary

	// A dry run stops here, having run every check a real proposal
	// runs — validation, standing policy, the diff — and recorded none
	// of it. Stopping earlier would preview something weaker than what
	// approval would face, which is the one thing a preview must not
	// do.
	if opts.DryRun {
		out := previewOf(p, puts)
		if current, err := deps.State.Load(); err == nil {
			if b := expandedRouteBlockers(current, puts); len(b) > 0 {
				out["blockers"] = b
			}
		}
		if current, err := deps.State.Load(); err == nil {
			if w := authPolicyWarning(current, puts); w != "" {
				out["authPolicyWarning"] = w
			}
		}
		return out, http.StatusOK, nil
	}

	stored, err := json.Marshal(StoredProposal{
		BasedOnHead: head,
		Summary:     req.Summary,
		Put:         req.Put,
		Delete:      req.Delete,
		Plan:        p,
		Origin:      opts.Origin,
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	planID, err := deps.DB.InsertPlan(id.Name, id.ActingFor, req.Summary, p.OverallRisk.String(), string(stored), opts.PrivateOrigin)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	deps.event("plan.proposed", planID, id, req.Summary)
	rec, err := deps.DB.GetPlan(planID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return ViewPlan(rec), http.StatusCreated, nil
}

// SubscriptionRequest is what FileSubscription needs beyond a plain
// Request: which Agent the subscription is for and the matcher/
// capability/budget document a runtime's ProposeRequest carries
// (ADR-W039 §3.6, Phase 4, #341). It is proposals' own shape — not
// agentapi.SubscriptionProposal — so this package never imports
// agentapi, which agentgateway already forbids by import direction.
type SubscriptionRequest struct {
	Butler        string
	MatchKind     string
	MatchJSON     string
	Audience      string
	CapabilitySet []string
	BudgetJSON    string
	Summary       string
	// Trigger, Cadence and Source describe a scheduled subscription
	// (epic #326 S03); empty Trigger is the event trigger. Cadence is a
	// Go duration string, validated against the store's own bounds.
	Trigger string
	Cadence string
	Source  string
	// Revises and Version make this an edit's approval Plan rather than
	// a new subscription (see plan.SubscriptionChange).
	Revises int64
	Version int
}

// normalizeSubscriptionTrigger checks a proposal's trigger against the
// same bounds runtimedb enforces, so an Owner is never shown a schedule
// approval could not apply.
func normalizeSubscriptionTrigger(req SubscriptionRequest) (trigger, cadence string, err error) {
	var d time.Duration
	if req.Cadence != "" {
		if d, err = time.ParseDuration(req.Cadence); err != nil {
			return "", "", fmt.Errorf("a subscription cadence must be a duration like 15m: %w", err)
		}
	}
	trigger, d, err = runtimedb.NormalizeSubscriptionTrigger(req.Trigger, d, req.Source)
	if err != nil {
		return "", "", err
	}
	if trigger == runtimedb.SubscriptionTriggerSchedule {
		cadence = d.String()
	}
	return trigger, cadence, nil
}

// FileSubscription proposes an Owner-approved event subscription
// (ADR-W039 §3.6, Phase 4, #341): a Plan record like any other, but
// carrying a plan.SubscriptionChange instead of Put/Delete — the same
// shape a household-setting proposal takes, because a subscription is
// neither desired state nor a Statefile document, and approving it
// enables a row in agent_subscriptions rather than committing anything
// to the Statefile. Nothing here is enabled: enabling happens only
// where the approve path (keystone/api's approvePlan Subscription
// branch) calls runtimedb.ApproveAgentSubscription.
func FileSubscription(deps Deps, id principal.Identity, req SubscriptionRequest) (PlanView, error) {
	if req.Butler == "" {
		return PlanView{}, fmt.Errorf("a subscription needs the agent it is for")
	}
	if req.MatchKind == "" {
		return PlanView{}, fmt.Errorf("a subscription needs a match kind")
	}
	if len(req.CapabilitySet) == 0 {
		return PlanView{}, fmt.Errorf("a subscription needs at least one capability")
	}
	trigger, cadence, err := normalizeSubscriptionTrigger(req)
	if err != nil {
		return PlanView{}, err
	}
	if req.Revises != 0 && req.Version < 2 {
		return PlanView{}, fmt.Errorf("an edit's approval names the edited version")
	}
	if req.Summary == "" {
		req.Summary = fmt.Sprintf("%s subscribes to %s events", req.Butler, req.MatchKind)
		if trigger == runtimedb.SubscriptionTriggerSchedule {
			req.Summary = fmt.Sprintf("%s runs %s every %s", req.Butler, req.MatchKind, cadence)
		}
	}
	matchJSON := req.MatchJSON
	if matchJSON == "" {
		matchJSON = "{}"
	}
	budgetJSON := req.BudgetJSON
	if budgetJSON == "" {
		budgetJSON = "{}"
	}
	capsJSON, err := json.Marshal(req.CapabilitySet)
	if err != nil {
		return PlanView{}, err
	}
	stored := StoredProposal{
		Summary: req.Summary,
		Subscription: &plan.SubscriptionChange{
			Butler: req.Butler, MatchKind: req.MatchKind, MatchJSON: matchJSON,
			Audience: req.Audience, CapabilitySet: string(capsJSON), BudgetJSON: budgetJSON,
			Trigger: trigger, Cadence: cadence, Source: req.Source,
			Revises: req.Revises, Version: req.Version,
		},
	}
	if head, err := deps.State.Head(); err == nil {
		stored.BasedOnHead = head
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return PlanView{}, err
	}
	planID, err := deps.DB.InsertPlan(id.Name, id.ActingFor, req.Summary, "low", string(raw), true)
	if err != nil {
		return PlanView{}, fmt.Errorf("filing a subscription proposal: %w", err)
	}
	deps.event("plan.proposed", planID, id, req.Summary)
	rec, err := deps.DB.GetPlan(planID)
	if err != nil {
		return PlanView{}, err
	}
	return ViewPlan(rec), nil
}

// previewOf is what a dry run returns: the plan, and the secrets only
// the operator can supply (ADR-W021 A5).
func previewOf(p *plan.Plan, puts []resource.Resource) map[string]any {
	provided := []string{}
	purposes := map[string]string{}
	for _, r := range puts {
		if r.Kind == resource.KindSecret && r.Secret != nil && r.Secret.Provided {
			provided = append(provided, r.Secret.Name)
			if r.Secret.Purpose != "" {
				purposes[r.Secret.Name] = r.Secret.Purpose
			}
		}
	}
	sort.Strings(provided)
	return map[string]any{"plan": p, "providedSecrets": provided, "secretPurposes": purposes}
}

// expandedRouteBlockers are reasons the Routes THIS proposal renders
// will not give a working service here, computed from the expanded
// resources rather than the package (a Route's label comes from a
// param an operator can override).
func expandedRouteBlockers(current, puts []resource.Resource) []string {
	posture := ingressorigin.Of(current)
	if posture.TLS != "tailscale" {
		return nil
	}
	var out []string
	for _, r := range puts {
		if r.Kind != resource.KindRoute || r.Route == nil || r.Route.Host == "" {
			continue
		}
		label := ingressorigin.ServiceLabel(r.Route.Host)
		if _, pinned := ingressorigin.WellKnownPortFor(label); pinned {
			continue
		}
		out = append(out, fmt.Sprintf(
			"Route %q has no fixed address under a tailnet, so the links this service builds for itself "+
				"(an OIDC redirect, its own base URL) will not match where it is served — it will install, "+
				"report healthy, and you will not be able to sign in", r.Metadata.Name))
	}
	sort.Strings(out)
	return out
}

// authPolicyWarning is what installing this would do to how EVERYONE
// signs in (F77), under a posture where every Route shares one
// hostname and Authelia enforces one policy for the whole deployment.
func authPolicyWarning(current, adding []resource.Resource) string {
	before := reconcile.AccessPolicies(current)
	after := reconcile.AccessPolicies(append(append([]resource.Resource{}, current...), adding...))
	for domain, want := range after {
		if reconcile.PolicyStricter(want, before[domain]) && before[domain] != "" {
			return fmt.Sprintf(
				"installing this raises the sign-in requirement to %q for EVERY service on %s, not just this one — "+
					"under this addressing everything shares one hostname, so one policy applies to all of it",
				want, domain)
		}
	}
	return ""
}
