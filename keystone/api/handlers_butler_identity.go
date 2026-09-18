package api

// Butler admission, identity links, and handoff (ADR-W030 A1/A2/A2.1,
// work package P6).
//
// Butler admission is runtime membership, separate from the LdapUser a
// household member is in the Statefile. That separation is what lets an
// offboarding deny access immediately instead of waiting on a directory
// write, and it is why these endpoints exist beside the household member
// ones rather than inside them.
//
// Everything that GRANTS requires manage. Butler cannot grant itself
// manage authority (A2.1), and the butler role does not hold it, so
// there is no request the Butler can make that reaches these writes.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func (s *Server) listButlerMembers(_ *http.Request, _ principal.Identity) (any, int, error) {
	members, err := s.DB.ListButlerMembers()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := make([]map[string]any, 0, len(members))
	for _, m := range members {
		links, err := s.DB.ListSurfaceLinks(m.Member)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		out = append(out, map[string]any{"member": m, "links": links, "active": m.Active()})
	}
	return map[string]any{"members": out}, http.StatusOK, nil
}

// recordButlerMember stages a member without admitting them. A child
// record is created here and can never be admitted — the store refuses,
// and so does the schema.
func (s *Server) recordButlerMember(r *http.Request, _ principal.Identity) (any, int, error) {
	var req struct {
		Member string `json:"member"`
		// Role is the RBAC axis and defaults to member: staging someone
		// must not make them an Owner by omission (design §2.3).
		Role string `json:"role"`
		// Profile is the constrained-Member preset, normally empty.
		Profile  string `json:"profile"`
		Guardian string `json:"guardian"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if req.Role == "" {
		req.Role = runtimedb.RoleMember
	}
	if err := s.DB.RecordButlerMember(req.Member, req.Role, req.Profile, req.Guardian); err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"recorded": req.Member}, http.StatusOK, nil
}

// admitButlerMember admits someone to the Butler. It is a separate act
// from creating the record, because admission is the decision and the
// record is only the fact that a person exists.
func (s *Server) admitButlerMember(r *http.Request, id principal.Identity) (any, int, error) {
	member := r.PathValue("member")
	if err := s.DB.AdmitButlerMember(member, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no butler record for %q — record the member first", member)
		}
		return nil, http.StatusBadRequest, err
	}
	return s.memberState(member)
}

func (s *Server) disableButlerMember(r *http.Request, id principal.Identity) (any, int, error) {
	member := r.PathValue("member")
	if err := s.DB.DisableButlerMember(member, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no butler record for %q", member)
		}
		return nil, http.StatusBadRequest, err
	}
	return s.memberState(member)
}

// offboardButlerMember is the runtime denial half of offboarding: one
// audited action that stops retrieval, dispatch, ingestion and proactive
// delivery now. Desired-state cleanup follows through Plans, and failed
// cleanup never restores access — which is why this endpoint does not
// wait for or depend on it.
func (s *Server) offboardButlerMember(r *http.Request, id principal.Identity) (any, int, error) {
	member := r.PathValue("member")
	if err := s.DB.OffboardButlerMember(member, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no butler record for %q", member)
		}
		return nil, http.StatusBadRequest, err
	}
	// Unfinished personal work is paused in the same breath, so an
	// offboarding does not leave tasks retrying on behalf of someone who
	// no longer has access (ADR-W030 A4).
	paused, err := s.pausePersonalWork(member)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	state, code, err := s.memberState(member)
	if err != nil {
		return nil, code, err
	}
	m, _ := state.(map[string]any)
	m["pausedTasks"] = paused
	return m, code, nil
}

// pausePersonalWork moves the member's unfinished tasks to
// paused-offboarded: no retries, no notices, no model calls, and nothing
// deleted (ADR-W030 A4).
func (s *Server) pausePersonalWork(member string) (int, error) {
	tasks, err := s.DB.ListButlerTasks(500, member)
	if err != nil {
		return 0, err
	}
	paused := 0
	for _, t := range tasks {
		switch t.State {
		case runtimedb.ButlerTaskPending, runtimedb.ButlerTaskRunning, runtimedb.ButlerTaskWaiting:
		default:
			continue
		}
		if err := s.DB.TransitionButlerTask(t.ID, runtimedb.ButlerTaskPausedOffboarded,
			"owner offboarded", "offboarding"); err != nil {
			return paused, err
		}
		paused++
	}
	return paused, nil
}

func (s *Server) memberState(member string) (any, int, error) {
	m, err := s.DB.GetButlerMember(member)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	links, err := s.DB.ListSurfaceLinks(member)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"member": m, "links": links, "active": m.Active()}, http.StatusOK, nil
}

// linkButlerIdentity binds an authenticated surface identity to a
// member. Equal names are not proof of identity, so this is the only way
// one becomes the other.
func (s *Server) linkButlerIdentity(r *http.Request, id principal.Identity) (any, int, error) {
	member := r.PathValue("member")
	var req struct {
		LinkType   string `json:"linkType"`
		ExternalID string `json:"externalId"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.LinkSurfaceIdentity(req.LinkType, req.ExternalID, member, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no butler record for %q", member)
		}
		return nil, http.StatusBadRequest, err
	}
	return s.memberState(member)
}

// unlinkButlerIdentity withdraws a link and bumps the member's
// generation, so anything derived from it goes stale immediately.
func (s *Server) unlinkButlerIdentity(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		LinkType   string `json:"linkType"`
		ExternalID string `json:"externalId"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.RevokeSurfaceLink(req.LinkType, req.ExternalID, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no live link for that identity")
		}
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"revoked": true}, http.StatusOK, nil
}

// startButlerConversation opens a conversation identity for the caller.
// It is bound to the member the CALLER resolves to, never to a member
// named in the body: the model cannot assert acting identity, and
// neither can a request (A2).
func (s *Server) startButlerConversation(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Subject string `json:"subject"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	convID, err := s.DB.StartButlerConversation(actingMember(id), req.Subject)
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotAdmitted) || errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusForbidden,
				fmt.Errorf("you are not admitted to the Butler; an administrator has to admit you first")
		}
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"conversationId": convID}, http.StatusOK, nil
}

// handoffButlerConversation moves a conversation between the two bots.
//
// Authority is recomputed at the destination rather than carried: the
// caller must own the conversation, must still be admitted, and must
// hold the operator profile to reach the operator bot. What crosses is
// the destination surface's own turns, text only.
func (s *Server) handoffButlerConversation(r *http.Request, id principal.Identity) (any, int, error) {
	convID := r.PathValue("id")
	conv, err := s.DB.GetButlerConversation(convID)
	if err != nil {
		return nil, http.StatusNotFound, fmt.Errorf("no such conversation")
	}
	if conv.Member != actingMember(id) {
		// Not 403: a conversation you do not own is one you should not
		// be able to confirm the existence of.
		return nil, http.StatusNotFound, fmt.Errorf("no such conversation")
	}
	var req struct {
		From          string `json:"from"`
		To            string `json:"to"`
		FromSessionID string `json:"fromSessionId"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	ctx, err := s.DB.HandoffConversation(convID,
		runtimedb.ButlerSurface(req.From), runtimedb.ButlerSurface(req.To), req.FromSessionID)
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotAdmitted) {
			return nil, http.StatusForbidden, err
		}
		return nil, http.StatusBadRequest, err
	}
	return ctx, http.StatusOK, nil
}

// decodeStrict decodes a JSON body, refusing unknown fields — the same
// discipline the Butler config endpoint uses, so a field nobody reads is
// an error rather than a silent no-op.
func decodeStrict(r *http.Request, into any) (int, error) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return http.StatusBadRequest, fmt.Errorf("parsing body: %w", err)
	}
	return http.StatusOK, nil
}
