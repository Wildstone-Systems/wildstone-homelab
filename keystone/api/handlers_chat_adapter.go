package api

// Chat adapter credentials (ADR-W026 A3, work package G4).
//
// A chat adapter needs one Keystone token, and which ROLE that token
// carries is the whole security boundary: a family adapter is a
// concierge (converse and nothing else), an admin adapter is a butler
// (read, propose, converse — never approve). This endpoint mints the
// right one for a capability and places it on the node as the provided
// secret the package's container references.
//
// It is deliberately the same shape as publisher minting (ADR-W024
// A10) — mint, place, re-create dependents, all as one recorded plan —
// and reuses its executor, because "a capability gets a credential" is
// one operation with two callers, not two operations.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// chatAdapterPrincipalPrefix names the principal minted for a chat
// capability's adapter.
const chatAdapterPrincipalPrefix = "chat-"

type mintAdapterRequest struct {
	Node string `json:"node"`
	// Butler is the Agent this bot belongs to. It replaced Surface, and
	// the difference is ADR-W033 B7: a bot serves one Agent, so
	// credentialing it and assigning it are one act. A surface named a
	// class of Agents, which left "which one answers here" to whatever
	// the roster returned.
	Butler string `json:"butler"`
}

// roleForSurface is the mapping that makes the two-bots rule real: a
// family adapter can never hold more than converse, whatever the
// package says or the operator types.
//
// The surface is the AGENT's class now rather than a string the
// operator typed, so the rule holds without anybody having to keep the
// capability name and the flag in agreement.
func roleForSurface(surface runtimedb.ButlerSurface) (runtimedb.Role, error) {
	switch surface {
	case runtimedb.ButlerSurfaceFamily:
		return runtimedb.RoleConcierge, nil
	case runtimedb.ButlerSurfaceAdmin:
		return runtimedb.RoleValet, nil
	default:
		return "", fmt.Errorf("surface must be admin or family")
	}
}

// mintChatAdapter gives a chat capability its Keystone credential.
// Minting again is rotation: earlier tokens are revoked before the new
// one is placed, and containers referencing the secret are re-created.
func (s *Server) mintChatAdapter(r *http.Request, id principal.Identity) (any, int, error) {
	capName := r.PathValue("capability")
	if !capabilityNameRegexp.MatchString(capName) {
		return nil, http.StatusBadRequest, fmt.Errorf("capability must be lowercase [a-z0-9._-], 1–52 chars")
	}
	var req mintAdapterRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing request: %w", err)
	}
	if req.Node == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("node required: the token is placed on the node as a provided secret")
	}
	if req.Butler == "" {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"butler required: a bot belongs to one agent (ADR-W033 B7), so credentialing it says which")
	}
	agent, err := s.DB.GetButler(req.Butler)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusBadRequest, fmt.Errorf("no butler called %q", req.Butler)
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// The ceiling comes from the AGENT, so a family bot cannot be wired
	// to an Owner Agent by mistyping a flag. This used to be checked by
	// making the capability name and the -surface flag agree, which
	// could only ever tell the two classes apart — not one Owner Agent
	// from another.
	role, err := roleForSurface(agent.Surface)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	transport, ok := capabilityTransport(capName)
	if !ok {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"capability %q is not a chat capability: it should be chat-<transport>-<something>", capName)
	}
	// One bot, one Agent. Credentialing a bot that already answers for
	// somebody else is refused rather than quietly repointed: the token
	// this call is about to place is that Agent's, and two Agents
	// sharing one would make "which agent answered" unanswerable —
	// which is the whole reason B7 binds a Connection to exactly one.
	//
	// This half only READS. The address is written further down, after
	// every remaining refusal, because a write before them is a write
	// the operator never agreed to: a 503 or a ceiling conflict would
	// come back as a refusal while the bot had already been handed over
	// — and handing a family package's bot to an Owner Agent is exactly
	// what the ceiling conflict exists to prevent.
	needsAddress := false
	switch at, err := s.DB.ChatConnectionAt(transport, capName); {
	case err == nil && at.Butler != agent.Name:
		return nil, http.StatusConflict, fmt.Errorf(
			"%s already answers at %s, so that bot's token is theirs; revoke the address before giving it to %s",
			at.Butler, capName, agent.Name)
	case err != nil && !errors.Is(err, runtimedb.ErrNoConnection):
		return nil, http.StatusInternalServerError, err
	case errors.Is(err, runtimedb.ErrNoConnection):
		needsAddress = true
	}

	// Everything the REQUEST can get wrong is answered before anything
	// the deployment can: an operator who mistyped a surface should be
	// told so whether or not a node happens to be connected.
	if s.Agents == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("agent plane not wired")
	}

	name := chatAdapterPrincipalPrefix + capName
	existing, err := s.DB.GetPrincipal(name)
	switch {
	case errors.Is(err, runtimedb.ErrNotFound):
		if err := s.Principals.Create(name, role, ""); err != nil {
			return nil, http.StatusBadRequest, err
		}
		s.event("principal.created", "", id, fmt.Sprintf("%s (%s, for %s)", name, role, agent.Name))
	case err != nil:
		return nil, http.StatusInternalServerError, err
	case existing.Role != role:
		// Re-minting an adapter for an Agent of the other class would
		// silently move a bot from one ceiling to the other. Refuse: the
		// operator should retire the capability and install the other.
		return nil, http.StatusConflict, fmt.Errorf(
			"principal %s exists with role %s, not %s — a chat adapter cannot change ceiling by re-minting",
			name, existing.Role, role)
	}

	// Assigning the bot IS part of credentialing it, and now that
	// nothing above can still refuse, this is where it happens. Two
	// separate steps would leave a credentialed bot answering for
	// nobody, which reads as working right up until somebody sends it a
	// message.
	if needsAddress {
		if _, err := s.DB.AddButlerAddress(agent.Name, transport, capName, "", actingMember(id)); err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("giving %s the address %s: %w", agent.Name, capName, err)
		}
	}

	secretName := capability.AdapterSecretName(capName)
	dependents, err := s.secretDependents(req.Node, secretName)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	summary := fmt.Sprintf("mint a chat token for %s at capability %s → secret/%s on %s (%d dependent container(s) re-created)",
		agent.Name, capName, secretName, req.Node, len(dependents))
	stored, _ := json.Marshal(map[string]any{"summary": summary, "secret": secretName, "node": req.Node,
		"principal": name, "role": string(role), "butler": agent.Name, "dependents": dependents})
	planID, err := s.DB.InsertPlan(secretsPrincipal, id.Name, summary, "medium", string(stored))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	for _, st := range []runtimedb.PlanState{runtimedb.PlanApproved, runtimedb.PlanApplying} {
		if err := s.DB.TransitionPlan(planID, st, id.Name); err != nil {
			return nil, http.StatusInternalServerError, err
		}
	}

	version, failure := s.executePublisherMint(r, name, req.Node, secretName, dependents)
	if failure != nil {
		if terr := s.DB.TransitionPlan(planID, runtimedb.PlanFailed, id.Name); terr != nil {
			s.Log.Error("chat adapter mint plan transition", "plan", planID, "err", terr)
		}
		s.appendEvent("chat-adapter.mint-failed", req.Node, secretName, id.Name)
		return nil, http.StatusBadGateway, fmt.Errorf("%s (plan %s)", failure, planID)
	}
	if terr := s.DB.TransitionPlan(planID, runtimedb.PlanApplied, id.Name); terr != nil {
		s.Log.Error("chat adapter mint plan transition", "plan", planID, "err", terr)
	}
	s.appendEvent("chat-adapter.minted", req.Node, secretName, id.Name)
	if len(dependents) > 0 && s.OnApproved != nil {
		s.OnApproved()
	}
	return map[string]any{
		"principal": name, "role": string(role), "butler": agent.Name,
		"secret": "secret/" + secretName, "node": req.Node,
		"version": version, "dependentsRemoved": dependents, "plan": planID,
	}, http.StatusOK, nil
}
