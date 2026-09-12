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
	"sort"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// TrustedIngressHeader is the header the identity proxy sets. It is
// honoured only when TrustedIngressCIDRs matches the peer, because a
// header anyone can set is not an authentication.
const TrustedIngressHeader = "Remote-User"

// listHouseholdRoutines shows a household its routines.
//
// specs carries each Wildstone routine's parsed document, keyed by
// routine name. A list row wants the trigger and the action tier, and
// neither is a column — both live inside the document. Parsing here
// rather than in the client is the same rule the detail read follows: a
// second parser in a browser is a second place the closed grammar can
// drift from the engine's. A document that will not parse is simply
// absent from the map; the row renders without its chips rather than
// failing.
func (s *Server) listHouseholdRoutines(_ *http.Request, _ principal.Identity) (any, int, error) {
	routines, err := s.DB.ListHouseholdRoutines()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	specs := map[string]*resource.AutomationSpec{}
	for _, r := range routines {
		if r.Ownership != runtimedb.RoutineWildstone {
			continue
		}
		parsed, perr := automation.ParseDraft(r.Document)
		if perr != nil {
			continue
		}
		specs[r.Name] = parsed.Automation
	}
	return map[string]any{"routines": routines, "specs": specs}, http.StatusOK, nil
}

// maxRoutineRuns bounds the run history one detail read pulls off the
// household stream.
const maxRoutineRuns = 200

// routineDetail is one routine with everything the detail view needs to
// say what it does and what it has been doing.
type routineDetail struct {
	Routine runtimedb.HouseholdRoutine `json:"routine"`
	// Rule is the name the ENGINE knows this routine by: the document's
	// metadata.name, which need not be the name it is filed under. Served
	// because a caller that wants to replay this rule has no other way to
	// learn it without parsing the document itself.
	Rule string `json:"rule"`
	// Spec is the parsed document. Parsing happens here rather than in
	// the client because the engine parses it here too, and a second
	// parser in a browser is a second place the grammar can drift.
	Spec *resource.AutomationSpec `json:"spec"`
	// SpecError says why a document did not parse. A routine whose spec
	// is unreadable is exactly the one someone needs to look at, so this
	// is a 200 with an explanation rather than a 500.
	SpecError string                      `json:"specError,omitempty"`
	Revisions []runtimedb.RoutineRevision `json:"revisions"`
	Runs      []runtimedb.HouseholdEvent  `json:"runs"`
}

// getHouseholdRoutine serves one routine: its spec, its edit history and
// what it has done in the last week.
//
// Runs come off the household stream, not from butler tasks. The task
// kind "routine" is declared and nothing constructs one, so reading it
// would serve an empty list forever; what the engine actually records is
// automation.fired / suppressed / acted / action_failed against the
// rule, which is the same evidence a replay reasons over.
func (s *Server) getHouseholdRoutine(r *http.Request, _ principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	routine, err := s.DB.GetHouseholdRoutine(name)
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no routine named %q", name)
		}
		return nil, http.StatusInternalServerError, err
	}

	out := routineDetail{
		Routine:   routine,
		Revisions: []runtimedb.RoutineRevision{},
		Runs:      []runtimedb.HouseholdEvent{},
	}

	// The rule the engine knows is the document's metadata.name, which
	// need not equal the registry name this routine is filed under. Ask
	// the parsed document first: matching on the wrong name serves an
	// empty runs table for a routine that has been firing all week,
	// which reads as "it never ran".
	rule := routine.Name
	if parsed, perr := automation.ParseDraft(routine.Document); perr != nil {
		out.SpecError = perr.Error()
	} else {
		out.Spec = parsed.Automation
		if parsed.Metadata.Name != "" {
			rule = parsed.Metadata.Name
		}
	}

	out.Rule = rule

	revisions, err := s.DB.ListHouseholdRoutineRevisions(name)
	if err != nil && !errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusInternalServerError, err
	}
	if revisions != nil {
		out.Revisions = revisions
	}

	// A native routine runs inside its own application and Wildstone
	// records only that it exists, so there is nothing on our stream to
	// find. Asking anyway would be a query that always returns nothing.
	if routine.Ownership == runtimedb.RoutineWildstone {
		runs, err := s.routineRuns(name, rule)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		out.Runs = runs
	}
	return out, http.StatusOK, nil
}

// routineRuns collects a routine's last week off the household stream.
//
// An event is recorded against the rule's metadata.name AS IT WAS when
// the rule fired, and nothing stops an edit from changing that name. So
// asking only about the current name loses every run recorded under a
// previous one, and the screen reports "nothing triggered it this week"
// about a routine that fired all week — the same wrong answer this
// handler already avoids for the registry name, one revision further
// back.
//
// The names come from the recorded revisions, which is why the
// documents are worth a second read: they are the only record of what
// this routine used to be called.
func (s *Server) routineRuns(name, current string) ([]runtimedb.HouseholdEvent, error) {
	names := map[string]bool{current: true}
	docs, err := s.DB.HouseholdRoutineRevisionDocuments(name)
	if err != nil && !errors.Is(err, runtimedb.ErrNotFound) {
		return nil, err
	}
	for _, doc := range docs {
		// A revision that no longer parses is not an error here. It
		// simply contributes no name, and the ones that do parse still
		// find their runs.
		if parsed, perr := automation.ParseDraft(doc); perr == nil && parsed.Metadata.Name != "" {
			names[parsed.Metadata.Name] = true
		}
	}

	since := time.Now().Add(-automation.DefaultDraftWindow)
	seen := map[int64]bool{}
	out := []runtimedb.HouseholdEvent{}
	for rule := range names {
		events, err := s.DB.QueryHouseholdEvents(runtimedb.HouseholdQuery{
			Entity: rule,
			Source: automation.Source,
			From:   since,
			Newest: true,
			Limit:  maxRoutineRuns,
		})
		if err != nil {
			return nil, err
		}
		for _, e := range events {
			// A rename can leave two names pointing at overlapping
			// history; the seq is what makes an event one event.
			if !seen[e.Seq] {
				seen[e.Seq] = true
				out = append(out, e)
			}
		}
	}
	// Merged from one query per name, so the order has to be restored
	// rather than assumed.
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	if len(out) > maxRoutineRuns {
		out = out[:maxRoutineRuns]
	}
	return out, nil
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
	// A routine is born a DRAFT, whatever the caller asked for.
	//
	// This route needs only propose, which the Butler service principal
	// holds — so accepting a state verbatim let a compromised Butler
	// credential create an action-capable routine and skip the review
	// its activation is supposed to require (ADR-W030 A6/A7.1).
	// Activation goes through SetHouseholdRoutineState and the Plan
	// boundary, which is where a human is.
	//
	// Refused rather than silently downgraded: a caller that asked for
	// enabled and got a draft with a cheerful note has been told
	// something untrue about what it just did.
	if req.State != "" && req.State != runtimedb.RoutineDraft {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"a routine is created as a draft and activated separately, so it cannot be created %q — put it in trial or enable it once someone has reviewed it (ADR-W030 A6)",
			req.State)
	}
	seq, err := s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: req.Name, Ownership: req.Ownership, ExternalID: req.ExternalID,
		Connection: req.Connection, Document: req.Document,
		State:  runtimedb.RoutineDraft,
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
//
// Three things it will not do.
//
// It will not take a spec for a routine the caller has not read. An
// edit may carry basedOnRevision; when it does and the routine has moved
// since, the edit is refused rather than overwriting whatever the other
// person wrote. Two people editing at once used to mean the second one
// won silently.
//
// It will not let the rule's name move. Events are recorded against the
// document's metadata.name as it was when the rule fired, so renaming
// through an edit splits a routine's history across two names with
// nothing to join them. Ownership is already immutable at the schema for
// the same sort of reason; this is the same decision one level down.
//
// And it will not leave a changed routine acting. See routineAfterEdit.
func (s *Server) editHouseholdRoutine(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		// Document is one Automation YAML document — what the CLI and
		// the Butler send.
		Document string `json:"document"`
		// Spec is the parsed form, for a caller that holds a structured
		// spec and no YAML writer: the Console edits the spec the detail
		// read served it and sends it straight back. Serialising here
		// keeps one writer of this grammar, the same way parsing it here
		// keeps one reader.
		Spec *resource.AutomationSpec `json:"spec"`
		Note string                   `json:"note"`
		// BasedOnRevision is the revision the caller edited. Optional:
		// an older client that does not send it keeps working.
		BasedOnRevision int `json:"basedOnRevision"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if (req.Document == "") == (req.Spec == nil) {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"exactly one of document (an Automation YAML document) or spec (its parsed form) is required")
	}

	current, err := s.DB.GetHouseholdRoutine(name)
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no routine named %q", name)
		}
		return nil, http.StatusInternalServerError, err
	}
	if current.Ownership != runtimedb.RoutineWildstone {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"%q runs inside %s, so Wildstone records that it exists and cannot change it (ADR-W030 A6)",
			name, current.Connection)
	}
	if req.BasedOnRevision != 0 && req.BasedOnRevision != current.Revision {
		return nil, http.StatusConflict, fmt.Errorf(
			"this routine is on rev %d; your change was written against rev %d",
			current.Revision, req.BasedOnRevision)
	}

	// The rule keeps the name it already answers to, whichever form the
	// edit arrived in.
	existing, perr := automation.ParseDraft(current.Document)
	document := req.Document
	if req.Spec != nil {
		if perr != nil {
			return nil, http.StatusBadRequest, fmt.Errorf(
				"this routine's current document does not parse, so it cannot be edited as a spec — send a whole document: %w", perr)
		}
		rule := existing
		rule.Automation = req.Spec
		out, merr := resource.Marshal(rule)
		if merr != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("writing the rule: %w", merr)
		}
		document = string(out)
	}

	// A document sent whole is stored as sent, parseable or not — that is
	// what this route has always done, and CreateHouseholdRoutine does
	// not validate either, so refusing here would leave a household able
	// to write a broken routine and unable to replace it. The detail read
	// is built to show an unparseable document for exactly that reason.
	//
	// A spec is different: this handler wrote that document, so if it
	// does not parse the spec was not a rule and saying so is the only
	// useful answer.
	proposed, perr2 := automation.ParseDraft(document)
	if perr2 != nil && req.Spec != nil {
		return nil, http.StatusBadRequest, perr2
	}
	// The rename guard needs both names. When either side is unreadable
	// there is nothing to compare, and refusing on that basis would block
	// the edit that fixes it.
	if perr == nil && perr2 == nil && proposed.Metadata.Name != existing.Metadata.Name {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"this rule is called %q and an edit cannot rename it to %q: its runs are recorded under the name it had when it fired, "+
				"so renaming would split its history in two",
			existing.Metadata.Name, proposed.Metadata.Name)
	}

	revision, err := s.DB.EditHouseholdRoutine(name, document, actingMember(id), req.Note)
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no routine named %q", name)
		}
		return nil, http.StatusBadRequest, err
	}

	state, note := s.routineAfterEdit(name, current, id)
	return map[string]any{
		"revision": revision,
		"state":    state,
		"note":     note,
	}, http.StatusOK, nil
}

// routineAfterEdit puts a changed routine back under watch.
//
// A routine that was ENABLED acts on its next trigger. Editing what it
// does and leaving it enabled means the house starts doing something
// new with nobody having watched it once — and there is no approval step
// on this path to catch that. ADR-W024's guarantee is that a routine is
// watched before it acts, so an edit returns it to trial and says so.
//
// Draft, trial and disabled routines are left where they are: none of
// them acts, so there is nothing to take away.
func (s *Server) routineAfterEdit(name string, before runtimedb.HouseholdRoutine, id principal.Identity) (string, string) {
	const kept = "any review link issued for the previous revision now approves nothing: " +
		"it was for a version nobody has read"
	if before.State != runtimedb.RoutineEnabled {
		return before.State, kept
	}
	if err := s.DB.SetHouseholdRoutineState(name, runtimedb.RoutineTrial, actingMember(id)); err != nil {
		// The revision is already recorded. Report the state that is
		// actually true rather than the one that was intended.
		return before.State, kept + "; this routine is still acting — putting it back in trial failed: " + err.Error()
	}
	return runtimedb.RoutineTrial, "this routine was acting, so the change put it back in trial: it will watch and report " +
		"until someone lets it act again. " + kept
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
