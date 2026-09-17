package api

// The Assistant surface (#198): a household member talks to a Butler
// from the dashboard, without the dashboard becoming an agent platform.
//
// THE WHOLE OF THIS FILE IS A CEILING. There is no model here, no
// provider, no tool, no memory and no authority evaluation — all of
// that already exists behind butlerlive, and a second implementation of
// any of it on the household's surface would be a second place for the
// answer to "may this person do this" to be wrong. What is here is the
// four things this DOOR decides:
//
//  1. WHO is asking. The member comes from the forwarded identity the
//     ingress proved (auth_portal.go), never from the request body.
//  2. WHICH Butlers exist for them. The roster is filtered in the
//     store, by the access list, so a person does not learn that the
//     Butlers they were not granted exist at all.
//  3. WHAT KIND of Butler may answer here. A household surface reaches
//     the family surface only, and never an operator-profile Butler —
//     rechecked on every message, so exposing one by configuration
//     mistake still does not let it answer a dashboard.
//  4. AS WHOM. Never as the owner. `asOwner` is the exemption that
//     gives the Console holder every Butler (ADR-W031 A10); a request
//     that arrived through a household dashboard is not that, whoever
//     is signed in.
//
// Deliberately NOT the session endpoints #198 sketches. A session id
// there is minted before the first message; here a conversation IS its
// recorded session, which already exists, is already audited, and is
// already what a follow-up is addressed to (ADR-W027). Minting a second
// kind of id in front of it would be a second lifetime to get wrong.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// maxPortalMessage bounds one message from the widget.
//
// A household member types a sentence. Anything much longer arriving
// from a text box is either a paste nobody meant to send or somebody
// testing what this endpoint forwards to a model, and both are better
// refused at the door than sent to a provider and billed for.
const maxPortalMessage = 4000

// assistantAgent is one Butler as the widget's selector shows it.
//
// leavesTheHouse rides along for the reason butlerView carries it: it
// is the most consequential fact about a Butler, a person deciding what
// to type is relying on it, and a client that has to fetch it
// separately is one that eventually forgets to.
type assistantAgent struct {
	Name           string `json:"name"`
	Directive      string `json:"directive,omitempty"`
	LeavesTheHouse bool   `json:"leavesTheHouse"`
	Reachable      bool   `json:"reachable"`
	CarriesActions bool   `json:"carriesActions"`
}

// assistantAgents answers "which assistants may I talk to from here",
// filtered server-side.
//
// #198 lists what the filter must cover: the initiating person, their
// membership, the Butler's visibility and scope, the surface, and
// revocation. Every one of those is already the store's answer —
// ButlersFor is the access list, Live() is enabled-and-not-retired, and
// the surface test is below. The client may hide entries for UX; it is
// never what decides.
func (s *Server) assistantAgents(_ http.ResponseWriter, _ *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant is configured on this deployment")
	}
	all, err := s.DB.ButlersFor(member)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	agents := make([]assistantAgent, 0, len(all))
	for _, b := range all {
		if !portalMayReach(b) {
			continue
		}
		agents = append(agents, assistantAgent{
			Name:           b.Name,
			Directive:      b.Directive,
			LeavesTheHouse: b.ModelKind == runtimedb.ModelCloud,
			Reachable:      b.Reachable(),
			CarriesActions: b.CarriesActionAuthority(),
		})
	}
	return map[string]any{"agents": agents, "member": member}, http.StatusOK, nil
}

// portalMayReach is the Homepage connection ceiling, as one function.
//
// Two conditions, and the second is not implied by the first even
// though CreateButler makes them agree today: the surface says where a
// Butler answers, the profile says what it may do, and a household
// dashboard is entitled to neither the operator's surface nor the
// operator's profile. Checking both means a row that somehow holds a
// mismatched pair is refused rather than admitted by whichever half was
// tested.
//
// It is called on the roster AND on every message, because a filter
// applied only when building a list is a filter a hand-written request
// skips — which is precisely the acceptance criterion #198 states.
func portalMayReach(b runtimedb.Butler) bool {
	return b.Surface == runtimedb.ButlerSurfaceFamily && b.Profile != runtimedb.ProfileOperator
}

// assistantAsk runs one turn.
//
// A first message names an agent; a follow-up names the conversation,
// and the agent comes from the conversation's own stamp rather than
// from the request — the same rule continueButlerSession follows, and
// for the same reason: "I sent this to Sandy" and "this went to Alfred"
// must not both be true of one message.
func (s *Server) assistantAsk(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	var req struct {
		Agent   string `json:"agent"`
		Session string `json:"session"`
		Text    string `json:"text"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	text := strings.TrimSpace(req.Text)
	switch {
	case text == "":
		return nil, http.StatusBadRequest, fmt.Errorf("a question needs a message")
	case len(text) > maxPortalMessage:
		return nil, http.StatusRequestEntityTooLarge,
			fmt.Errorf("that message is longer than this surface carries (%d characters)", maxPortalMessage)
	case s.DB == nil:
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant is configured on this deployment")
	}

	var prior *turnContext
	agent := strings.TrimSpace(req.Agent)
	if id := strings.TrimSpace(req.Session); id != "" {
		sess, err := s.DB.GetButlerSession(id)
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("that conversation is not here any more")
		}
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		// Somebody else's conversation is not found rather than
		// forbidden: a 403 on a guessed id confirms the id.
		if sess.ActingFor != member || sess.Surface != runtimedb.ButlerSurfaceFamily {
			return nil, http.StatusNotFound, fmt.Errorf("that conversation is not here any more")
		}
		if sess.Butler != "" {
			if agent != "" && agent != sess.Butler {
				return nil, http.StatusConflict, fmt.Errorf(
					"this conversation is with %s — start a new one to talk to %s", sess.Butler, agent)
			}
			agent = sess.Butler
		}
		prior = &turnContext{SessionID: sess.ID, History: historyFrom(sess.Turns)}
	}
	if agent == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("name an assistant")
	}

	// The ceiling, rechecked here on every single message rather than
	// trusted from the roster this client was given.
	b, err := s.DB.ResolveButlerFor(agent, member)
	switch {
	case err == nil, errors.Is(err, runtimedb.ErrButlerUnreachable):
		// Unreachable is still a Butler this person may reach, and the
		// ceiling below applies to it; the ask path answers 503 with
		// which model is down rather than moving to another one.
	case errors.Is(err, runtimedb.ErrNoButlerAccess), errors.Is(err, runtimedb.ErrNotFound),
		errors.Is(err, runtimedb.ErrNotAdmitted):
		return nil, http.StatusNotFound, fmt.Errorf("no assistant answers there for you")
	default:
		return nil, http.StatusInternalServerError, err
	}
	if !portalMayReach(b) {
		// Same shape as "no such assistant": that an operator Butler
		// exists, and is called this, is not something a household
		// surface should be able to confirm.
		return nil, http.StatusNotFound, fmt.Errorf("no assistant answers there for you")
	}

	// Whether this deployment has any Butler at all is asked AFTER the
	// ceiling, not before. Both answers are true facts about the
	// deployment, and the one a refused caller is entitled to is the
	// narrower: "no assistant answers there for you" rather than "there
	// is one, and this control plane is configured like so".
	if !s.butlersConfigured() {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant is configured on this deployment")
	}

	// asOwner is false, always. Everything else — tools, memory scope,
	// authority, the model — comes from the Butler and the member, in
	// the one place that decides it for every surface.
	body, status, err := s.askNamedFamilyButler(r.Context(), text, agent, member, "", false, false, prior)
	if err != nil {
		return nil, status, err
	}
	ans, ok := body.(askResponse)
	if !ok {
		return body, status, nil
	}
	return map[string]any{
		"session":   ans.SessionID,
		"agent":     agent,
		"reply":     ans.FinalText,
		"capped":    ans.Capped,
		"proposals": len(ans.PlanIDs),
	}, status, nil
}

// assistantConversation replays one conversation into a reloaded
// widget, so a household member who refreshes the dashboard does not
// lose what they were in the middle of.
func (s *Server) assistantConversation(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant is configured on this deployment")
	}
	sess, err := s.DB.GetButlerSession(r.PathValue("id"))
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("that conversation is not here any more")
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if sess.ActingFor != member || sess.Surface != runtimedb.ButlerSurfaceFamily {
		return nil, http.StatusNotFound, fmt.Errorf("that conversation is not here any more")
	}
	messages := make([]map[string]string, 0, len(sess.Turns))
	for _, t := range sess.Turns {
		if t.Text == "" {
			continue
		}
		messages = append(messages, map[string]string{"role": t.Role, "text": t.Text})
	}
	return map[string]any{"session": sess.ID, "agent": sess.Butler, "messages": messages}, http.StatusOK, nil
}
