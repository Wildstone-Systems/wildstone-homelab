package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"io"
	"net/http"
	"strconv"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/proposals"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// maxProposalBytes bounds a plan-proposal body; a legitimate proposal is
// a handful of YAML documents, not megabytes.
const maxProposalBytes = 1 << 20

// proposalRequest, storedProposal and deletionRef are keystone/proposals'
// own types (ADR-W039 Phase 3, #339): the pipeline moved there so
// keystone/agentgateway can file a Plan without an import cycle through
// keystone/api. These aliases keep every call site and every test in
// this package unchanged — the four callers (POST /plans below,
// propose_change's post-turn filing in handlers_butler.go, and the two
// capability handlers) see the identical types and behaviour they always
// did.
type (
	proposalRequest = proposals.Request
	storedProposal  = plan.StoredProposal
	deletionRef     = plan.DeletionRef
)

// planView, viewPlan and viewPlans are keystone/proposals' own (see
// above); aliased so this file's many other call sites (household
// settings, portal authoring, owner review, chat commands) are
// unchanged.
type planView = proposals.PlanView

func viewPlan(rec runtimedb.PlanRecord) planView { return proposals.ViewPlan(rec) }

func viewPlans(recs []runtimedb.PlanRecord) []planView { return proposals.ViewPlans(recs) }

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

// proposalOpts is keystone/proposals' own Options (see the type alias
// above this file's other proposal aliases): what a caller knows that
// the generic propose endpoint does not. Both fields exist for
// ADR-W021: Origin records what a capability install was ASKED FOR
// (A2), and DryRun returns the plan a proposal WOULD make without
// recording it (A5).
type proposalOpts = proposals.Options

// submitProposal is the generic entry: no origin, not a dry run.
func (s *Server) submitProposal(id principal.Identity, req proposalRequest) (any, int, error) {
	return s.submitProposalOpts(id, req, proposalOpts{})
}

// proposalDeps is the keystone/proposals.Deps this server files
// proposals through: the same Statefile, policy engine and database
// every proposal-filing call site in this package already used before
// the pipeline moved to keystone/proposals (ADR-W039 Phase 3, #339).
// Exported as ProposalDeps so cmd/keystone can wire the identical deps
// into keystone/agentgateway.Gateway.Proposals — one pipeline, whether
// a Plan is filed by an HTTP caller or by a runtime through the Agent
// Gateway.
func (s *Server) proposalDeps() proposals.Deps {
	return proposals.Deps{DB: s.DB, State: s.State, Policy: s.Policy, Log: s.Log, Event: s.event}
}

// ProposalDeps is proposalDeps, exported for cmd/keystone's wiring of
// keystone/agentgateway.Gateway.Proposals.
func (s *Server) ProposalDeps() proposals.Deps { return s.proposalDeps() }

// submitProposalOpts is a thin wrapper over keystone/proposals.File —
// the ONE proposal pipeline (ADR-W017 A3) moved there in Phase 3 so
// keystone/agentgateway can file a Plan for a runtime without this
// package's cycle. Every call site here (the HTTP propose endpoint,
// the Butler host, the two capability handlers) and their tests see
// the identical behaviour: same validation, same policy engine, same
// 422-on-violation, same audit events.
func (s *Server) submitProposalOpts(id principal.Identity, req proposalRequest, opts proposalOpts) (any, int, error) {
	return proposals.File(s.proposalDeps(), id, req, opts)
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

	if stored.Routine != nil {
		if len(stored.Put) > 0 || len(stored.Delete) > 0 || stored.Setting != nil || stored.Plan != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("routine Plans cannot include infrastructure or settings changes")
		}
		if body, code, err := s.freshOwnerReview(r, id); err != nil {
			return body, code, err
		}
		document, err := automation.ParseDraft(stored.Routine.Document)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		if err := s.checkRoutinePlanPolicy(document, id); err != nil {
			return nil, http.StatusUnprocessableEntity, err
		}
		if err := s.DB.ApproveRoutinePlan(planID, actingMember(id)); err != nil {
			return nil, http.StatusConflict, err
		}
		rec, err = s.DB.GetPlan(planID)
		return viewPlan(rec), http.StatusOK, err
	}

	// Check the whole proposal before any write, including Plans saved before
	// routine review became mandatory. A mixed proposal must not partially apply.
	for i, doc := range stored.Put {
		res, err := resource.Unmarshal([]byte(doc))
		if err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("stored put[%d] corrupt: %w", i, err)
		}
		if res.Kind == resource.KindAutomation {
			return nil, http.StatusConflict, fmt.Errorf("Automation changes require an exact-version routine Plan; resubmit the routine with its output audience for Owner review")
		}
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
	s.recordProposalOutcomeDelivery(planID, rec.Principal, "approved", stored.Summary)
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
	var stored storedProposal
	if err := json.Unmarshal([]byte(rec.PlanJSON), &stored); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if stored.Routine != nil {
		if body, code, err := s.freshOwnerReview(r, id); err != nil {
			return body, code, err
		}
	}
	if err := s.DB.TransitionPlan(planID, runtimedb.PlanRejected, id.Name); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("plan.rejected", planID, id, rec.Summary)
	s.recordProposalOutcomeDelivery(planID, rec.Principal, "rejected", rec.Summary)
	rec, err = s.DB.GetPlan(planID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return viewPlan(rec), http.StatusOK, nil
}

// applyChanges is keystone/proposals.ApplyChanges (see the type
// aliases above): current + puts − deletes, replacing by (kind, name).
// Aliased so handlers_portal_authoring.go and household_timezone.go —
// neither a submitProposalOpts caller — keep this exact call.
func applyChanges(current, puts []resource.Resource, deletes []deletionRef) []resource.Resource {
	return proposals.ApplyChanges(current, puts, deletes)
}

// recordProposalOutcomeDelivery is Phase 3's own write to Phase 4's
// table (ADR-W039 §3.2 Proposals, §3.6; migration 0084, #339): the
// moment a Plan is decided, if the principal that filed it is a runtime
// principal, record a proposal_outcome delivery for its Agent. A Plan a
// human or the legacy provider filed has no runtime principal
// (RuntimePrincipalButler answers false) and gets no delivery — there
// is no runtime waiting to be told.
//
// No consumer reads this yet; Phase 4 adds the long-poll that does
// (GET /api/v1/agent/deliveries?wait=). A failure here is logged, never
// returned: the approval or rejection itself already committed, and a
// runtime that missed this notice can still poll a Plan's own state —
// losing the notice is not losing the decision.
func (s *Server) recordProposalOutcomeDelivery(planID, filedBy, outcome, summary string) {
	if filedBy == "" {
		return
	}
	seq, ok, err := s.DB.RuntimePrincipalButler(filedBy)
	if err != nil {
		s.Log.Error("checking whether a plan's principal is a runtime, for a proposal_outcome delivery", "plan", planID, "err", err)
		return
	}
	if !ok {
		return
	}
	payload, err := json.Marshal(map[string]any{"plan": planID, "outcome": outcome, "summary": summary})
	if err != nil {
		s.Log.Error("encoding a proposal_outcome delivery", "plan", planID, "err", err)
		return
	}
	if _, err := s.DB.RecordAgentDelivery(seq, "proposal_outcome", string(payload)); err != nil {
		s.Log.Error("recording a proposal_outcome delivery", "plan", planID, "err", err)
	}
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

// previewOf, expandedRouteBlockers and authPolicyWarning moved to
// keystone/proposals with the rest of the dry-run pipeline (ADR-W039
// Phase 3, #339): they were called only from within submitProposalOpts,
// which is now proposals.File.
