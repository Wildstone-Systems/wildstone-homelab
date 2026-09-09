package api

// The household routine review path (ADR-W030 A6/A8/A8.1, work package
// P13).
//
// An admitted adult should be able to approve "the morning routine now
// also checks the calendar" without holding Console credentials, and
// should be able to approve nothing else that way. Two mechanisms do
// that, and they answer different questions:
//
//   Authelia, in front of the review page, establishes WHO the person
//   is. This handler trusts the identity header only from the trusted
//   ingress and maps it to an admitted member through the same link
//   table everything else uses — equal names are not proof of identity.
//
//   A review token establishes WHAT they may do: approve one named
//   proposal, at one revision, once, before one expiry. It is not a
//   session. A stolen link is useless to anyone else, and useless to
//   its owner for anything but that one plan.
//
// Chat cannot approve. A Telegram callback carries a token bound to a
// proposal and is redeemed through this same path; a chat MESSAGE
// saying "yes" approves nothing, because silence and agreement in a
// group thread are indistinguishable and because the person typing it
// has not been authenticated.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// TrustedIngressHeader is the header the identity proxy sets. It is
// honoured only when TrustedIngressCIDRs matches the peer, because a
// header anyone can set is not an authentication.
const TrustedIngressHeader = "Remote-User"

// listHouseholdRoutines shows a household its routines.
func (s *Server) listHouseholdRoutines(_ *http.Request, _ principal.Identity) (any, int, error) {
	routines, err := s.DB.ListHouseholdRoutines()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"routines": routines}, http.StatusOK, nil
}

// createHouseholdRoutine records a routine, native or Wildstone-run.
func (s *Server) createHouseholdRoutine(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Name       string `json:"name"`
		Ownership  string `json:"ownership"`
		ExternalID string `json:"externalId"`
		Connection string `json:"connection"`
		Document   string `json:"document"`
		State      string `json:"state"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	seq, err := s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: req.Name, Ownership: req.Ownership, ExternalID: req.ExternalID,
		Connection: req.Connection, Document: req.Document, State: req.State,
		Author: actingMember(id),
	})
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{
		"seq": seq,
		"note": "a new routine starts as a draft; put it in trial to watch what it would do " +
			"before letting it act",
	}, http.StatusOK, nil
}

// editHouseholdRoutine records a new revision, which supersedes any
// review token pinned to the old one.
func (s *Server) editHouseholdRoutine(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Document string `json:"document"`
		Note     string `json:"note"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	revision, err := s.DB.EditHouseholdRoutine(name, req.Document, actingMember(id), req.Note)
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no routine named %q", name)
		}
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{
		"revision": revision,
		"note": "any review link issued for the previous revision now approves nothing: " +
			"it was for a version nobody has read",
	}, http.StatusOK, nil
}

// setHouseholdRoutineState moves a routine between draft, trial and
// enabled, or disables it exactly and immediately.
func (s *Server) setHouseholdRoutineState(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		State string `json:"state"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	var err error
	if req.State == runtimedb.RoutineDisabled {
		err = s.DB.DisableHouseholdRoutine(name, actingMember(id))
	} else {
		err = s.DB.SetHouseholdRoutineState(name, req.State, actingMember(id))
	}
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no routine named %q", name)
		}
		return nil, http.StatusBadRequest, err
	}
	routine, err := s.DB.GetHouseholdRoutine(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{
		"routine":         routine,
		"performsActions": routine.PerformsActions(),
	}, http.StatusOK, nil
}

// issueHouseholdReviewToken mints a review link for one proposal.
//
// It refuses anything that is not a pure household-routine plan. That
// refusal is the load-bearing one: "mostly routines" is exactly the
// shape an escalation would take, so a plan that adds a routine and
// opens a route goes back to the Console where a route change belongs.
func (s *Server) issueHouseholdReviewToken(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		PlanID  string `json:"planId"`
		Member  string `json:"member"`
		Routine string `json:"routine"`
		TTLMins int    `json:"ttlMinutes"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	record, err := s.DB.GetPlan(req.PlanID)
	if err != nil {
		return nil, http.StatusNotFound, fmt.Errorf("no such plan")
	}
	var parsed plan.Plan
	if err := json.Unmarshal([]byte(record.PlanJSON), &parsed); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("reading the plan: %w", err)
	}
	class := plan.Classify(&parsed)
	if class != plan.ClassHouseholdRoutine {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"plan %s is %s, not a pure household-routine change, so it is reviewed in the Console — "+
				"a plan that also touches infrastructure or identity is an infrastructure change with a "+
				"routine in it", req.PlanID, class)
	}
	// The member must actually be admitted, now. A link issued to
	// someone who has since been disabled must not work.
	member, err := s.DB.GetButlerMember(req.Member)
	if err != nil || !member.Active() {
		return nil, http.StatusBadRequest,
			fmt.Errorf("%q is not currently admitted to the Butler", req.Member)
	}

	var routineSeq int64
	var revision int
	if req.Routine != "" {
		routine, err := s.DB.GetHouseholdRoutine(req.Routine)
		if err != nil {
			return nil, http.StatusNotFound, fmt.Errorf("no routine named %q", req.Routine)
		}
		routineSeq, revision = routine.Seq, routine.Revision
	}
	secret, err := s.DB.IssueHouseholdReviewToken(req.PlanID, req.Member, actingMember(id),
		routineSeq, revision, time.Duration(req.TTLMins)*time.Minute)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return reviewTokenResponse{
		Token: secret, PlanID: req.PlanID, Member: req.Member,
		Note: "single use, for this plan at this revision, for this person, and it expires. " +
			"It is not a login: the review page authenticates separately",
	}, http.StatusOK, nil
}

// reviewTokenResponse is the third body shape allowed to carry a secret,
// alongside a minted API token and a join token: the review link, in the
// only moment it ever exists in a response. Without the carve-out the
// redaction pass would strip the very thing the caller asked for, and
// the failure would look like an empty field rather than a refusal.
type reviewTokenResponse struct {
	Token  string `json:"token"`
	PlanID string `json:"planId"`
	Member string `json:"member"`
	Note   string `json:"note"`
}

// householdReview is the narrow acting-for service route.
//
// It sits behind Authelia and holds NO API verb of its own — it is
// registered unauthenticated because the identity comes from the
// trusted ingress, and the authority comes entirely from the token. A
// request without both gets nothing.
func (s *Server) householdReview(w http.ResponseWriter, r *http.Request) {
	member, ok := s.trustedIngressUser(r)
	if !ok {
		// Not "who are you": a request that did not come through the
		// identity proxy learns nothing about what lives here.
		s.writeError(w, http.StatusNotFound, "not found")
		return
	}
	var req struct {
		Token   string `json:"token"`
		Approve bool   `json:"approve"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		s.writeError(w, code, err.Error())
		return
	}
	planID, err := s.DB.RedeemHouseholdReviewToken(req.Token, member)
	if err != nil {
		// Every reason collapses into one: unknown, expired, used,
		// wrong person, superseded revision. Distinguishing them would
		// help somebody guessing.
		s.writeError(w, http.StatusForbidden, "this review link does not work")
		return
	}
	state := runtimedb.PlanRejected
	if req.Approve {
		state = runtimedb.PlanApproved
	}
	if err := s.DB.TransitionPlan(planID, state, member); err != nil {
		s.writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"planId": planID, "state": state, "by": member})
}

// trustedIngressUser resolves the identity the proxy asserted, and only
// from the proxy.
//
// Two checks, and both are necessary. The peer must be the trusted
// ingress, because a header anyone can set is not an authentication.
// And the asserted name must resolve through the identity-link table to
// an admitted member, because equal names are not proof of identity
// (ADR-W030 A2.1).
func (s *Server) trustedIngressUser(r *http.Request) (string, bool) {
	asserted := strings.TrimSpace(r.Header.Get(TrustedIngressHeader))
	if asserted == "" {
		return "", false
	}
	if !s.peerIsTrustedIngress(r) {
		return "", false
	}
	member, err := s.DB.ResolveSurfaceIdentity(runtimedb.LinkTrustedIngress, asserted)
	if err != nil {
		return "", false
	}
	return member.Member, true
}

// peerIsTrustedIngress reports whether this request arrived through the
// configured identity proxy.
//
// TrustedIngress is a list of exact remote addresses rather than a CIDR
// or a hostname: on this deployment the proxy is one container on one
// network, the list is short, and an exact match cannot be widened by a
// DNS answer.
func (s *Server) peerIsTrustedIngress(r *http.Request) bool {
	if len(s.TrustedIngress) == 0 {
		// Nothing configured means the household review path is not
		// available, rather than available to everyone. An unconfigured
		// trust boundary must fail closed.
		return false
	}
	host := remoteHost(r)
	for _, allowed := range s.TrustedIngress {
		if allowed == host {
			return true
		}
	}
	return false
}
