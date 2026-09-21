package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// maxProposalBytes bounds a plan-proposal body; a legitimate proposal is
// a handful of YAML documents, not megabytes.
const maxProposalBytes = 1 << 20

// proposalRequest is the plan-proposal body (ADR-W007 K3): the ONLY way
// any principal initiates a change (ground rule 4). Resources travel as
// canonical YAML documents (the Statefile's own schema).
type proposalRequest struct {
	Summary string        `json:"summary"`
	Put     []string      `json:"put,omitempty"`
	Delete  []deletionRef `json:"delete,omitempty"`
}

// storedProposal and deletionRef are the shared shapes from keystone/plan.
// They moved there because the reconcile loop's drift path writes this same
// document, and building it as an untyped map is how BasedOnHead came to be
// omitted (F28). Aliases keep this file's call sites unchanged.
type (
	storedProposal = plan.StoredProposal
	deletionRef    = plan.DeletionRef
)

// planView is what the API returns for a plan record. The runtime DB
// stores the proposal as one JSON blob (plan_json); returning that blob
// verbatim made every consumer parse JSON out of a JSON string — the
// Console did it inline, with a raw <pre> dump as its fallback. The view
// parses it once, here, so the approval screen reads the plan the same
// way it reads every other field. planJson is dropped from the response
// rather than duplicated: it carried nothing this doesn't.
type planView struct {
	runtimedb.PlanRecord
	// PlanJSON must compete for the embedded field's NAME to suppress it.
	// `json:"-"` does not shadow — it removes this field from
	// consideration entirely, leaving PlanRecord.PlanJSON the only
	// candidate for "planJson", which is how the blob kept being served
	// after the commit that meant to drop it (F25). Same name plus
	// omitempty: this field wins on depth and is omitted for being empty.
	PlanJSON    string        `json:"planJson,omitempty"`
	BasedOnHead string        `json:"basedOnHead,omitempty"`
	Plan        *plan.Plan    `json:"plan,omitempty"`
	Put         []string      `json:"put,omitempty"`
	Delete      []deletionRef `json:"delete,omitempty"`
	// Origin is what a capability install was asked for (ADR-W021 A2).
	// Surfaced here because a plan the Console cannot attribute to a
	// package and its params is back to describing consequences.
	Origin *plan.CapabilityOrigin `json:"origin,omitempty"`
	// Setting is a household setting the plan changes instead of desired
	// state — an Agent's proposal an Owner decides on (ADR-W033 E2).
	Setting *plan.SettingChange `json:"setting,omitempty"`
}

// viewPlan projects one record. A blob that will not parse is not fatal:
// the record's own columns (state, risk, summary) are the audit trail and
// are still worth returning, so the plan field is simply absent — a
// corrupt blob already fails loudly at approval, which is the moment it
// matters.
func viewPlan(rec runtimedb.PlanRecord) planView {
	v := planView{PlanRecord: rec}
	if rec.PlanJSON == "" {
		return v
	}
	var stored storedProposal
	if err := json.Unmarshal([]byte(rec.PlanJSON), &stored); err != nil {
		return v
	}
	v.BasedOnHead, v.Plan, v.Put, v.Delete = stored.BasedOnHead, stored.Plan, stored.Put, stored.Delete
	v.Origin = stored.Origin
	v.Setting = stored.Setting
	return v
}

func viewPlans(recs []runtimedb.PlanRecord) []planView {
	out := make([]planView, 0, len(recs))
	for _, rec := range recs {
		out = append(out, viewPlan(rec))
	}
	return out
}

func (s *Server) listPlans(r *http.Request, id principal.Identity) (any, int, error) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	plans, err := s.DB.ListPlans(limit)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	visible := plans[:0]
	for _, p := range plans {
		if err := s.DB.CheckPlanPrivacy(p.ID, runtimedb.ModelLocal, apiMemoryDestination(id)); err != nil {
			if errors.Is(err, runtimedb.ErrPrivacy) {
				continue
			}
			return nil, http.StatusInternalServerError, err
		}
		visible = append(visible, p)
	}
	return viewPlans(visible), http.StatusOK, nil
}

func (s *Server) getPlan(r *http.Request, id principal.Identity) (any, int, error) {
	rec, err := s.DB.GetPlan(r.PathValue("id"))
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("plan not found")
	}
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := s.DB.CheckPlanPrivacy(rec.ID, runtimedb.ModelLocal, apiMemoryDestination(id)); err != nil {
		return nil, http.StatusForbidden, err
	}
	return viewPlan(rec), http.StatusOK, nil
}

func (s *Server) proposePlan(r *http.Request, id principal.Identity) (any, int, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxProposalBytes+1))
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("reading body: %w", err)
	}
	if len(body) > maxProposalBytes {
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("proposal exceeds %d bytes", maxProposalBytes)
	}
	var req proposalRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing proposal: %w", err)
	}
	return s.submitProposal(id, req)
}

// proposalOpts carries what a caller knows that the generic propose
// endpoint does not. Both fields exist for ADR-W021: Origin records what
// a capability install was ASKED FOR (A2), and DryRun returns the plan a
// proposal WOULD make without recording it, which is what lets the
// install form show its consequences and collect operator-supplied
// secrets before anything is proposed (A5).
type proposalOpts struct {
	PrivateOrigin bool
	Origin        *plan.CapabilityOrigin
	DryRun        bool
}

// submitProposal is the generic entry: no origin, not a dry run.
func (s *Server) submitProposal(id principal.Identity, req proposalRequest) (any, int, error) {
	return s.submitProposalOpts(id, req, proposalOpts{})
}

// submitProposal is the ONE proposal pipeline (ADR-W017 A3): validate every
// document, check standing policy, compute the plan, and insert the
// `proposed` record. The HTTP propose endpoint and the Butler host both
// funnel through here, so a Butler-originated proposal is held to exactly
// the checks a human's is — same validation, same policy engine, same
// 422-on-violation, same audit events.
func (s *Server) submitProposalOpts(id principal.Identity, req proposalRequest, opts proposalOpts) (any, int, error) {
	if req.Summary == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("summary required")
	}
	if len(req.Put) == 0 && len(req.Delete) == 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("empty proposal: nothing to put or delete")
	}

	// Parse + validate every proposed resource up front; a proposal with
	// one bad document is rejected whole.
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
	current, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("loading statefile: %w", err)
	}
	head, err := s.State.Head()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	desired := applyChanges(current, puts, req.Delete)

	// Standing invariants (ADR-W009 A1): a policy-invalid change never
	// becomes a plan record, whoever proposed it. Exemption uses are
	// surfaced as events — never silent.
	violations, exempted := s.Policy.Validate(policy.Change{
		Proposed:  desired,
		Current:   current,
		Principal: id.Name,
		Role:      string(id.Role),
	})
	for _, v := range exempted {
		s.event("policy.exempted", "", id, v.String())
	}
	if len(violations) > 0 {
		msgs := make([]string, 0, len(violations))
		for _, v := range violations {
			msgs = append(msgs, v.String())
		}
		s.event("policy.rejected", "", id, strings.Join(msgs, "; "))
		return nil, http.StatusUnprocessableEntity,
			fmt.Errorf("proposal violates standing policy: %s", strings.Join(msgs, "; "))
	}

	p := plan.Diff(desired, current)
	if len(p.Steps) == 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("proposal is a no-op against current desired state")
	}
	p.Summary = req.Summary

	// A dry run stops here, having run every check a real proposal runs —
	// validation, standing policy, the diff — and recorded none of it.
	// Stopping earlier would preview something weaker than what approval
	// would face, which is the one thing a preview must not do.
	if opts.DryRun {
		out := previewOf(p, puts)
		// Blockers from the EXPANDED Routes, not the package's defaults.
		// The Services page can only see manifest defaults, so an
		// operator who overrides docs_subdomain or media_subdomain makes
		// the Route unpinned and is told nothing (F79). Here the real
		// hosts exist, so the answer is about what they actually asked
		// for.
		if current, err := s.State.Load(); err == nil {
			if b := expandedRouteBlockers(current, puts); len(b) > 0 {
				out["blockers"] = b
			}
		}
		// What this does to everyone else's sign-in, before approval
		// rather than after (F77).
		if current, err := s.State.Load(); err == nil {
			if w := authPolicyWarning(current, puts); w != "" {
				out["authPolicyWarning"] = w
			}
		}
		return out, http.StatusOK, nil
	}

	stored, err := json.Marshal(storedProposal{
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
	planID, err := s.DB.InsertPlan(id.Name, id.ActingFor, req.Summary, p.OverallRisk.String(), string(stored), opts.PrivateOrigin)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("plan.proposed", planID, id, req.Summary)
	rec, err := s.DB.GetPlan(planID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return viewPlan(rec), http.StatusCreated, nil
}

// approvePlan is the moment a proposal becomes desired state: the changes
// land in the Statefile with a structured commit naming the plan, and the
// statefile_head marker advances (ADR-W007 A6). The reconciler (E3)
// converges reality from there.
func (s *Server) approvePlan(r *http.Request, id principal.Identity) (any, int, error) {
	planID := r.PathValue("id")
	rec, err := s.DB.GetPlan(planID)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("plan not found")
	}
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if rec.State != runtimedb.PlanProposed {
		return nil, http.StatusConflict, fmt.Errorf("plan is %s, not proposed", rec.State)
	}
	var stored storedProposal
	if err := json.Unmarshal([]byte(rec.PlanJSON), &stored); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("stored plan corrupt: %w", err)
	}

	// A plan is only approvable against the exact desired state it was
	// computed from; if the Statefile moved since, the risk grades and
	// diff are stale — supersede and make the proposer re-propose.
	// A household setting is not desired state: it applies directly and
	// does not wait on the Statefile head (ADR-W033 E2).
	if stored.Setting != nil && len(stored.Put) == 0 && len(stored.Delete) == 0 {
		return s.applySettingPlan(planID, stored, id)
	}

	head, err := s.State.Head()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if head != stored.BasedOnHead {
		if err := s.DB.TransitionPlan(planID, runtimedb.PlanSuperseded, ""); err != nil {
			return nil, http.StatusInternalServerError, err
		}
		s.event("plan.superseded", planID, id, "statefile moved since proposal")
		return nil, http.StatusConflict, fmt.Errorf("statefile changed since proposal; plan superseded")
	}

	for i, doc := range stored.Put {
		res, err := resource.Unmarshal([]byte(doc))
		if err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("stored put[%d] corrupt: %w", i, err)
		}
		if err := s.State.Put(res); err != nil {
			return nil, http.StatusInternalServerError, err
		}
	}
	for _, d := range stored.Delete {
		if err := s.State.Delete(resource.Kind(d.Kind), d.Name); err != nil {
			return nil, http.StatusInternalServerError, err
		}
	}
	who := id.Name
	if id.ActingFor != "" {
		who = fmt.Sprintf("%s(%s)", id.Name, id.ActingFor)
	}
	if err := s.State.Commit(who, planID, stored.Summary); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	newHead, err := s.State.Head()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if err := s.DB.SetStatefileHead(newHead); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if err := s.DB.TransitionPlan(planID, runtimedb.PlanApproved, id.Name); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("plan.approved", planID, id, stored.Summary)
	if s.OnApproved != nil {
		s.OnApproved()
	}
	rec, err = s.DB.GetPlan(planID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return viewPlan(rec), http.StatusOK, nil
}

func (s *Server) rejectPlan(r *http.Request, id principal.Identity) (any, int, error) {
	planID := r.PathValue("id")
	rec, err := s.DB.GetPlan(planID)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("plan not found")
	}
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if rec.State != runtimedb.PlanProposed {
		return nil, http.StatusConflict, fmt.Errorf("plan is %s, not proposed", rec.State)
	}
	if err := s.DB.TransitionPlan(planID, runtimedb.PlanRejected, id.Name); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("plan.rejected", planID, id, rec.Summary)
	rec, err = s.DB.GetPlan(planID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return viewPlan(rec), http.StatusOK, nil
}

// applyChanges returns current + puts − deletes, replacing by (kind, name).
func applyChanges(current, puts []resource.Resource, deletes []deletionRef) []resource.Resource {
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

// event appends an operational event, logging rather than failing the
// request if the append itself errors (the plan record is the source of
// truth; the event stream is a projection).
func (s *Server) event(typ, planID string, id principal.Identity, msg string) {
	if _, err := s.DB.AppendEvent(runtimedb.Event{
		Type:    typ,
		Message: msg,
		Attrs:   map[string]string{"plan": planID, "principal": id.Name, "actingFor": id.ActingFor},
	}); err != nil {
		s.Log.Error("event append failed", "type", typ, "err", err)
	}
}

// previewOf is what a dry run returns: the plan, and the secrets only the
// operator can supply.
//
// The second half is the point (ADR-W021 A5). A package declares which
// secrets it cannot generate, so a render knows them before anything is
// applied — and an operator who is told up front is not the operator of
// F37, meeting them one ERROR_CODE_NOT_FOUND at a time, per tick, after
// approving a plan that could not have worked.
//
// Each secret's purpose travels with it, because the form now asks for
// the value right there, and "calendar-service-password" alone does not
// tell an operator it is the password of a directory account they had
// to create first.
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

// expandedRouteBlockers are reasons the Routes THIS proposal renders will
// not give a working service here.
//
// Computed from the expanded resources rather than the package, because
// a Route's label comes from a param an operator can override: pinning
// is a property of the name they chose, not of the manifest.
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
// signs in (F77).
//
// Under a posture where every Route shares one hostname, Authelia can
// enforce one policy for the whole deployment, so the strictest Route
// decides for all of them. Installing a monitoring tool classified
// admin_tailscale therefore put two-factor on a family dashboard — and
// nothing said so until the household could not log in.
//
// The dry run already computes the plan and holds the classifications.
// It has always been able to answer this; it was simply never asked.
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
