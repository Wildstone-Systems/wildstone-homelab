package api

// Owner approval of an agent_subscriptions proposal (ADR-W039 §3.2
// Events & subscriptions, §3.6; Phase 4, #341): the moment — and the
// ONLY moment — a subscription becomes enabled.
//
// A subscription is not desired state (like plan.SettingChange, its
// Plan JSON carries no Put/Delete and does not wait on the Statefile
// head), so nothing exists in agent_subscriptions until this runs: the
// row is created and approved in the same call, never left sitting
// around unapproved while a Plan is merely proposed.
//
// Scheduled subscriptions (epic #326 S03, #456) add the public surface
// Console, CLI and Butler share for them: list and inspect (schedule,
// health, checkpoint position, recent runs), pause/resume/disable,
// cadence, and edit — an edit bumps the version, stops dispatch at once,
// and files the Plan an Owner approves that exact version through.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/proposals"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// applySubscriptionPlan approves a subscription-only proposal.
func (s *Server) applySubscriptionPlan(planID string, stored storedProposal, id principal.Identity) (any, int, error) {
	sub := stored.Subscription
	if sub.Revises != 0 {
		return s.applySubscriptionRevision(planID, stored, id)
	}
	b, err := s.DB.GetButler(sub.Butler)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("subscription names an unknown agent %q: %w", sub.Butler, err)
	}
	var capabilities []string
	if sub.CapabilitySet != "" {
		if err := json.Unmarshal([]byte(sub.CapabilitySet), &capabilities); err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("stored subscription capability set corrupt: %w", err)
		}
	}
	var cadence time.Duration
	if sub.Cadence != "" {
		if cadence, err = time.ParseDuration(sub.Cadence); err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("stored subscription cadence corrupt: %w", err)
		}
	}
	subID, err := s.DB.ProposeAgentSubscription(runtimedb.AgentSubscriptionSpec{
		Butler: b.Seq, TriggerKind: sub.Trigger, MatchKind: sub.MatchKind, MatchJSON: sub.MatchJSON,
		Audience: sub.Audience, Capabilities: capabilities, BudgetJSON: sub.BudgetJSON,
		Cadence: cadence, SourceConnection: sub.Source,
	})
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := s.DB.ApproveAgentSubscription(subID, actingMember(id)); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return s.finishSubscriptionPlan(planID, stored, id)
}

// applySubscriptionRevision approves exactly the edited version a
// revision Plan names. If the row was edited again after this Plan was
// filed, approval fails and enables nothing: the Owner reviews the
// newer Plan instead.
func (s *Server) applySubscriptionRevision(planID string, stored storedProposal, id principal.Identity) (any, int, error) {
	sub := stored.Subscription
	if err := s.DB.ApproveAgentSubscriptionVersion(sub.Revises, sub.Version, actingMember(id)); err != nil {
		return nil, http.StatusConflict, err
	}
	return s.finishSubscriptionPlan(planID, stored, id)
}

func (s *Server) finishSubscriptionPlan(planID string, stored storedProposal, id principal.Identity) (any, int, error) {
	if err := s.DB.TransitionPlan(planID, runtimedb.PlanApproved, id.Name); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("plan.approved", planID, id, stored.Summary)
	rec, err := s.DB.GetPlan(planID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.recordProposalOutcomeDelivery(planID, rec.Principal, "approved", stored.Summary)
	// A Personal source's adapter runs only under an approved
	// subscription; this approval may be what it was waiting for.
	source := stored.Subscription.Source
	if stored.Subscription.Revises != 0 {
		if sub, err := s.DB.GetAgentSubscription(stored.Subscription.Revises); err == nil {
			source = sub.SourceConnection
		}
	}
	s.refileAdapterRender(source, actingMember(id))
	return viewPlan(rec), http.StatusOK, nil
}

// subscriptionView is one subscription as the public API shows it.
type subscriptionView struct {
	ID              int64    `json:"id"`
	Agent           string   `json:"agent"`
	Trigger         string   `json:"trigger"`
	MatchKind       string   `json:"matchKind"`
	Match           string   `json:"match"`
	Audience        string   `json:"audience,omitempty"`
	Capabilities    []string `json:"capabilities"`
	Cadence         string   `json:"cadence,omitempty"`
	Source          string   `json:"source,omitempty"`
	Person          string   `json:"person,omitempty"`
	Version         int      `json:"version"`
	ApprovedVersion int      `json:"approvedVersion"`
	// AwaitingApproval is true while an edit's version has not been
	// approved: nothing dispatches until it is.
	AwaitingApproval bool                            `json:"awaitingApproval"`
	Enabled          bool                            `json:"enabled"`
	Schedule         *runtimedb.SubscriptionSchedule `json:"schedule,omitempty"`
	// Checkpoint is the committed source position — an opaque cursor
	// (for mail: UIDVALIDITY and UID per folder), never message content.
	Checkpoint *runtimedb.SubscriptionCheckpoint `json:"checkpoint,omitempty"`
	Runs       []runtimedb.SubscriptionRun       `json:"runs,omitempty"`
}

// subscriptionReader is who is asking: their Person, and whether they
// are an active Owner (who sees every subscription). Anybody else sees
// only the subscriptions bound to their own Personal sources.
type subscriptionReader struct {
	member runtimedb.ButlerMember
	owner  bool
}

func (s *Server) subscriptionReader(id principal.Identity) (subscriptionReader, error) {
	m, err := s.DB.GetButlerMember(actingMember(id))
	if err != nil || !m.Active() {
		return subscriptionReader{}, fmt.Errorf("subscriptions are shown to an admitted household member")
	}
	return subscriptionReader{member: m, owner: m.Role == runtimedb.RoleOwner}, nil
}

func (r subscriptionReader) mayRead(sub runtimedb.AgentSubscription) bool {
	return r.owner || (sub.PersonID != "" && sub.PersonID == r.member.PersonID)
}

func (s *Server) viewSubscription(sub runtimedb.AgentSubscription, detail bool) (subscriptionView, error) {
	v := subscriptionView{
		ID: sub.ID, Trigger: sub.TriggerKind, MatchKind: sub.MatchKind, Match: sub.MatchJSON,
		Audience: sub.Audience, Capabilities: sub.CapabilitySet, Source: sub.SourceConnection,
		Version: sub.Version, ApprovedVersion: sub.ApprovedVersion, Enabled: sub.Enabled,
		AwaitingApproval: sub.ApprovedVersion != sub.Version,
	}
	if sub.Cadence > 0 {
		v.Cadence = sub.Cadence.String()
	}
	if b, err := s.DB.GetButlerBySeq(sub.Butler); err == nil {
		v.Agent = b.Name
	}
	if sub.PersonID != "" {
		if handle, err := s.DB.PersonHandle(sub.PersonID); err == nil {
			v.Person = handle
		}
	}
	if sub.TriggerKind != runtimedb.SubscriptionTriggerSchedule {
		return v, nil
	}
	sched, err := s.DB.GetSubscriptionSchedule(sub.ID)
	if err == nil {
		v.Schedule = &sched
	} else if !errors.Is(err, runtimedb.ErrNotFound) {
		return v, err
	}
	if !detail {
		return v, nil
	}
	cp, err := s.DB.GetSubscriptionCheckpoint(sub.ID, sub.SourceConnection)
	if err != nil {
		return v, err
	}
	v.Checkpoint = &cp
	if v.Runs, err = s.DB.ListSubscriptionRuns(sub.ID, 10); err != nil {
		return v, err
	}
	return v, nil
}

// listAgentSubscriptions: GET /api/v1/agent-subscriptions.
func (s *Server) listAgentSubscriptions(_ *http.Request, id principal.Identity) (any, int, error) {
	reader, err := s.subscriptionReader(id)
	if err != nil {
		return nil, http.StatusForbidden, err
	}
	subs, err := s.DB.ListAllAgentSubscriptions()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := []subscriptionView{}
	for _, sub := range subs {
		if !reader.mayRead(sub) {
			continue
		}
		v, err := s.viewSubscription(sub, false)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		out = append(out, v)
	}
	return map[string]any{"subscriptions": out}, http.StatusOK, nil
}

// readableSubscription resolves {id} for a reader, answering 404 for a
// subscription they may not see, so its existence is not disclosed.
func (s *Server) readableSubscription(r *http.Request, id principal.Identity) (runtimedb.AgentSubscription, subscriptionReader, int, error) {
	reader, err := s.subscriptionReader(id)
	if err != nil {
		return runtimedb.AgentSubscription{}, reader, http.StatusForbidden, err
	}
	subID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return runtimedb.AgentSubscription{}, reader, http.StatusBadRequest, fmt.Errorf("subscription id must be a number")
	}
	sub, err := s.DB.GetAgentSubscription(subID)
	if err != nil || !reader.mayRead(sub) {
		return runtimedb.AgentSubscription{}, reader, http.StatusNotFound, fmt.Errorf("no such subscription")
	}
	return sub, reader, http.StatusOK, nil
}

// getAgentSubscription: GET /api/v1/agent-subscriptions/{id}.
func (s *Server) getAgentSubscription(r *http.Request, id principal.Identity) (any, int, error) {
	sub, _, code, err := s.readableSubscription(r, id)
	if err != nil {
		return nil, code, err
	}
	v, err := s.viewSubscription(sub, true)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return v, http.StatusOK, nil
}

// controlAgentSubscription: POST /api/v1/agent-subscriptions/{id}/control.
//
// pause and disable only narrow, and any reader who may control the
// subscription may do them — the Butler acting for its Person included.
// resume, enable and cadence let it run again or more often; they take
// a human credential, never the Butler's (it holds no approve verb and
// must not be able to undo its Person's pause). enable is an Owner's:
// it re-arms an approved row an Owner or the Person switched off, and
// the Person's own authorization is still required at dispatch.
func (s *Server) controlAgentSubscription(r *http.Request, id principal.Identity) (any, int, error) {
	sub, reader, code, err := s.readableSubscription(r, id)
	if err != nil {
		return nil, code, err
	}
	return s.controlSubscription(r, sub, reader, principal.Allowed(id.Role, principal.VerbApprove), id.Name, id.ActingFor)
}

// homeControlAgentSubscription: POST
// /api/v1/home/agent-subscriptions/{id}/control — the Homepage
// Person's own session (epic #326 S07). Only a subscription that runs
// for them, whatever their role: the Homepage is not an Owner surface,
// so enable (an Owner's) is refused here. It is a person's own
// credential, so resume and cadence are allowed.
func (s *Server) homeControlAgentSubscription(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	m, err := s.DB.GetButlerMember(member)
	if err != nil || !m.Active() {
		return nil, http.StatusForbidden, fmt.Errorf("subscriptions are shown to an admitted household member")
	}
	reader := subscriptionReader{member: m}
	subID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("subscription id must be a number")
	}
	sub, err := s.DB.GetAgentSubscription(subID)
	if err != nil || !reader.mayRead(sub) {
		return nil, http.StatusNotFound, fmt.Errorf("no such subscription")
	}
	return s.controlSubscription(r, sub, reader, true, member, member)
}

// controlSubscription applies one control action for reader. human is
// whether the credential is a person's own (not an Agent's).
func (s *Server) controlSubscription(r *http.Request, sub runtimedb.AgentSubscription, reader subscriptionReader, human bool,
	principalName, actingFor string) (any, int, error) {
	var err error
	var req struct {
		Action  string `json:"action"`
		Reason  string `json:"reason,omitempty"`
		Cadence string `json:"cadence,omitempty"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if !reader.mayRead(sub) {
		return nil, http.StatusForbidden, fmt.Errorf("only the Person this subscription runs for, or an Owner, may change it")
	}
	widening := req.Action == "resume" || req.Action == "enable" || req.Action == "cadence"
	if widening && !human {
		return nil, http.StatusForbidden, fmt.Errorf("%s needs a person's own credential, not an Agent's", req.Action)
	}
	who := reader.member.Member
	at := time.Now().UTC()
	switch req.Action {
	case "pause":
		err = s.DB.PauseAgentSubscription(sub.ID, who, req.Reason, at)
	case "resume":
		err = s.DB.ResumeAgentSubscription(sub.ID, who, at)
	case "disable":
		err = s.DB.SetAgentSubscriptionEnabled(sub.ID, false, who)
	case "enable":
		if !reader.owner {
			return nil, http.StatusForbidden, fmt.Errorf("only an Owner may re-enable a subscription")
		}
		err = s.DB.SetAgentSubscriptionEnabled(sub.ID, true, who)
	case "cadence":
		var d time.Duration
		if d, err = time.ParseDuration(req.Cadence); err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("cadence must be a duration like 15m")
		}
		err = s.DB.SetAgentSubscriptionCadence(sub.ID, d)
	default:
		return nil, http.StatusBadRequest, fmt.Errorf("action must be pause, resume, disable, enable or cadence")
	}
	if err != nil {
		if errors.Is(err, runtimedb.ErrQuarantined) {
			return nil, http.StatusServiceUnavailable, err
		}
		return nil, http.StatusConflict, err
	}
	if _, err := s.DB.AppendEvent(runtimedb.Event{
		Type: "agent-subscription." + req.Action, Message: req.Reason,
		Attrs: map[string]string{"subscription": strconv.FormatInt(sub.ID, 10), "principal": principalName, "actingFor": actingFor},
	}); err != nil {
		s.Log.Error("event append failed", "type", "agent-subscription."+req.Action, "err", err)
	}
	if sub, err = s.DB.GetAgentSubscription(sub.ID); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if req.Action == "disable" || req.Action == "enable" {
		// The adapter reading a Personal source runs only while its
		// subscription is enabled.
		s.refileAdapterRender(sub.SourceConnection, who)
	}
	v, err := s.viewSubscription(sub, true)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return v, http.StatusOK, nil
}

// reviseAgentSubscription: PUT /api/v1/agent-subscriptions/{id}.
//
// An edit is material by definition (epic #326: scope, source,
// audience, behaviour): it takes effect on the row at once — so the
// running version stops dispatching and its run in flight is fenced —
// and files the Plan an Owner approves the new version through.
// Capabilities may only narrow here; widening goes through the Agent's
// own proposal path, which checks its live toolset.
func (s *Server) reviseAgentSubscription(r *http.Request, id principal.Identity) (any, int, error) {
	sub, reader, code, err := s.readableSubscription(r, id)
	if err != nil {
		return nil, code, err
	}
	var req struct {
		Match        *json.RawMessage `json:"match,omitempty"`
		Audience     *string          `json:"audience,omitempty"`
		Capabilities []string         `json:"capabilities,omitempty"`
		Source       *string          `json:"source,omitempty"`
		Summary      string           `json:"summary,omitempty"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if !reader.mayRead(sub) {
		return nil, http.StatusForbidden, fmt.Errorf("only the Person this subscription runs for, or an Owner, may edit it")
	}
	if req.Capabilities != nil {
		held := map[string]bool{}
		for _, c := range sub.CapabilitySet {
			held[c] = true
		}
		for _, c := range req.Capabilities {
			if !held[c] {
				return nil, http.StatusBadRequest, fmt.Errorf("%q is not in this subscription's capability set; an edit can only narrow it", c)
			}
		}
	}
	rev := runtimedb.AgentSubscriptionRevision{
		Audience: req.Audience, Capabilities: req.Capabilities, SourceConnection: req.Source,
	}
	if req.Match != nil {
		m := string(*req.Match)
		rev.MatchJSON = &m
	}
	who := actingMember(id)
	version, err := s.DB.ReviseAgentSubscription(sub.ID, rev, who)
	if err != nil {
		if errors.Is(err, runtimedb.ErrQuarantined) {
			return nil, http.StatusServiceUnavailable, err
		}
		return nil, http.StatusBadRequest, err
	}
	edited, err := s.DB.GetAgentSubscription(sub.ID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	b, err := s.DB.GetButlerBySeq(edited.Butler)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	summary := req.Summary
	if summary == "" {
		summary = fmt.Sprintf("%s edits subscription %d to version %d", who, sub.ID, version)
	}
	cadence := ""
	if edited.Cadence > 0 {
		cadence = edited.Cadence.String()
	}
	view, err := proposals.FileSubscription(s.proposalDeps(), id, proposals.SubscriptionRequest{
		Butler: b.Name, MatchKind: edited.MatchKind, MatchJSON: edited.MatchJSON, Audience: edited.Audience,
		CapabilitySet: edited.CapabilitySet, BudgetJSON: edited.BudgetJSON, Summary: summary,
		Trigger: edited.TriggerKind, Cadence: cadence, Source: edited.SourceConnection,
		Revises: sub.ID, Version: version,
	})
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("the edit was saved but its approval Plan could not be filed: %w", err)
	}
	v, err := s.viewSubscription(edited, true)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"subscription": v, "plan": view}, http.StatusOK, nil
}
