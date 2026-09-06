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
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// chatAdapterPrincipalPrefix names the principal minted for a chat
// capability's adapter.
const chatAdapterPrincipalPrefix = "chat-"

type mintAdapterRequest struct {
	Node    string `json:"node"`
	Surface string `json:"surface"`
}

// roleForSurface is the mapping that makes the two-bots rule real: a
// family adapter can never hold more than converse, whatever the
// package says or the operator types.
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

// capabilitySurface reads the surface out of a chat capability's name.
//
// The catalog convention is chat-<transport>-<surface>, and it is
// load-bearing rather than decorative: without this check, `wild chat
// adapter chat-telegram-family -surface admin` would mint an
// admin-surface credential into the FAMILY package's secret, and the
// household's bot would come up talking to the operator's Butler. One
// typo would defeat the two-package boundary the whole design rests on
// (ADR-W026 A3), silently and at install time.
func capabilitySurface(capability string) (runtimedb.ButlerSurface, bool) {
	switch {
	case strings.HasSuffix(capability, "-"+string(runtimedb.ButlerSurfaceFamily)):
		return runtimedb.ButlerSurfaceFamily, true
	case strings.HasSuffix(capability, "-"+string(runtimedb.ButlerSurfaceAdmin)):
		return runtimedb.ButlerSurfaceAdmin, true
	}
	return "", false
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
	surface := runtimedb.ButlerSurface(req.Surface)
	role, err := roleForSurface(surface)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	// The capability names the surface it is for; the request must agree
	// with it. Minting the wrong one is not a permission error the
	// operator would see later — it is a family bot wired to the
	// operator's Butler, working perfectly, for as long as nobody looks.
	if named, ok := capabilitySurface(capName); !ok {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"capability %q does not name a surface: a chat capability is chat-<transport>-admin or chat-<transport>-family", capName)
	} else if named != surface {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"capability %q is the %s surface, but -surface %s was asked for; install the %s package instead",
			capName, named, surface, "chat-<transport>-"+string(surface))
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
		s.event("principal.created", "", id, fmt.Sprintf("%s (%s, %s surface)", name, role, req.Surface))
	case err != nil:
		return nil, http.StatusInternalServerError, err
	case existing.Role != role:
		// Re-minting an adapter for the other surface would silently
		// move a bot from one ceiling to the other. Refuse: the operator
		// should retire the capability and install the other one.
		return nil, http.StatusConflict, fmt.Errorf(
			"principal %s exists with role %s, not %s — a chat adapter cannot change surface by re-minting",
			name, existing.Role, role)
	}

	secretName := capability.AdapterSecretName(capName)
	dependents, err := s.secretDependents(req.Node, secretName)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	summary := fmt.Sprintf("mint %s-surface chat token for capability %s → secret/%s on %s (%d dependent container(s) re-created)",
		req.Surface, capName, secretName, req.Node, len(dependents))
	stored, _ := json.Marshal(map[string]any{"summary": summary, "secret": secretName, "node": req.Node,
		"principal": name, "role": string(role), "surface": req.Surface, "dependents": dependents})
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
		"principal": name, "role": string(role), "surface": req.Surface,
		"secret": "secret/" + secretName, "node": req.Node,
		"version": version, "dependentsRemoved": dependents, "plan": planID,
	}, http.StatusOK, nil
}
