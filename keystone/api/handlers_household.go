package api

// Household events (ADR-W022 A2/A3/A10): the intake every publisher
// speaks to, the cursor every consumer follows, and the one admin verb
// that gives a capability its publisher credential.
//
// The intake stamps `source` from the caller's token. A publisher
// principal holds the emit verb and nothing else, so the worst a
// compromised shim can do is lie about the house under its own name.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/household"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// maxEnvelopeBytes bounds one intake request: the payload bound plus
// generous room for the envelope's own fields.
const maxEnvelopeBytes = household.MaxPayloadBytes + 2048

// publisherPrincipalPrefix names the principal minted for a capability's
// shim: `publisher-<capability>`.
const publisherPrincipalPrefix = "publisher-"

// capabilityNameRegexp mirrors the resource-name discipline for the
// capability path segment.
var capabilityNameRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,50}[a-z0-9])?$`)

func (s *Server) householdStore() *household.Store {
	return &household.Store{DB: s.DB}
}

// emitHouseholdEvent is the canonical intake. 201 records a new fact;
// 200 says this delivery was a retry of one already recorded (same
// publisher, same event id) and nothing was written.
func (s *Server) emitHouseholdEvent(r *http.Request, id principal.Identity) (any, int, error) {
	var env household.Envelope
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxEnvelopeBytes)).Decode(&env); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing envelope: %w", err)
	}
	receipt, err := s.householdStore().Append(env, id.Name)
	if err != nil {
		var ve *household.ValidationError
		if errors.As(err, &ve) {
			return nil, http.StatusBadRequest, err
		}
		return nil, http.StatusInternalServerError, err
	}
	if receipt.Duplicate {
		return receipt, http.StatusOK, nil
	}
	return receipt, http.StatusCreated, nil
}

// listHouseholdEvents serves the household stream with the same
// `after`/`limit`/`waitMs` cursor shape as the operational stream.
func (s *Server) listHouseholdEvents(r *http.Request, _ principal.Identity) (any, int, error) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	waitMs, _ := strconv.Atoi(r.URL.Query().Get("waitMs"))
	events, err := longPoll(r, waitMs, func() ([]runtimedb.HouseholdEvent, error) {
		return s.DB.HouseholdEventsAfter(after, limit)
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return events, http.StatusOK, nil
}

// longPoll runs fetch until it returns something, the wait elapses, or
// the client goes away. With waitMs 0 it is one fetch. The result is
// never nil, so an empty stream serializes as [] rather than null.
func longPoll[T any](r *http.Request, waitMs int, fetch func() ([]T, error)) ([]T, error) {
	const maxWait = 30_000
	if waitMs > maxWait {
		waitMs = maxWait
	}
	deadline := time.Now().Add(time.Duration(waitMs) * time.Millisecond)
	for {
		items, err := fetch()
		if err != nil {
			return nil, err
		}
		if len(items) > 0 || time.Now().After(deadline) {
			if items == nil {
				items = []T{}
			}
			return items, nil
		}
		select {
		case <-r.Context().Done():
			return []T{}, nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

type mintPublisherRequest struct {
	Node string `json:"node"`
}

// mintPublisher gives a capability its publisher credential (ADR-W022
// A10): the principal `publisher-<capability>` (role publisher, created
// on first use), a fresh token, and that token placed on the node as the
// provided secret `<capability>-publisher` — the name the capability
// engine synthesises, so a package's shim finds it under
// .Resolved.PublisherSecretRef with nothing else to wire.
//
// The token never appears in the response. It exists in the principal
// store as a hash and on the node encrypted, and nowhere else.
//
// Minting again is rotation: every earlier token for the principal is
// revoked first, the secret is overwritten, and containers on the node
// whose env references it are re-created so they pick up the new value —
// the secrets-rotation contract (ADR-W012 A5), recorded as the same kind
// of system/secrets plan.
func (s *Server) mintPublisher(r *http.Request, id principal.Identity) (any, int, error) {
	if s.Agents == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("agent plane not wired")
	}
	capName := r.PathValue("capability")
	if !capabilityNameRegexp.MatchString(capName) {
		return nil, http.StatusBadRequest, fmt.Errorf("capability must be lowercase [a-z0-9._-], 1–52 chars")
	}
	var req mintPublisherRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing request: %w", err)
	}
	if req.Node == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("node required: the token is placed on the node as a provided secret")
	}

	name := publisherPrincipalPrefix + capName
	existing, err := s.DB.GetPrincipal(name)
	switch {
	case errors.Is(err, runtimedb.ErrNotFound):
		if err := s.Principals.Create(name, runtimedb.RolePublisher, ""); err != nil {
			return nil, http.StatusBadRequest, err
		}
		s.event("principal.created", "", id, name+" (publisher)")
	case err != nil:
		return nil, http.StatusInternalServerError, err
	case existing.Role != runtimedb.RolePublisher:
		// A name collision with a hand-made principal of another role is
		// not something to silently widen or narrow.
		return nil, http.StatusConflict, fmt.Errorf("principal %s exists with role %s, not publisher", name, existing.Role)
	}

	secretName := capability.PublisherSecretName(capName)
	dependents, err := s.secretDependents(req.Node, secretName)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	summary := fmt.Sprintf("mint publisher token for capability %s → secret/%s on %s (%d dependent container(s) re-created)",
		capName, secretName, req.Node, len(dependents))
	stored, _ := json.Marshal(map[string]any{"summary": summary, "secret": secretName, "node": req.Node,
		"principal": name, "dependents": dependents})
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
			s.Log.Error("publisher mint plan transition", "plan", planID, "err", terr)
		}
		s.appendEvent("publisher.mint-failed", req.Node, secretName, id.Name)
		return nil, http.StatusBadGateway, fmt.Errorf("%s (plan %s)", failure, planID)
	}
	if terr := s.DB.TransitionPlan(planID, runtimedb.PlanApplied, id.Name); terr != nil {
		s.Log.Error("publisher mint plan transition", "plan", planID, "err", terr)
	}
	s.appendEvent("publisher.minted", req.Node, secretName, id.Name)
	if len(dependents) > 0 && s.OnApproved != nil {
		s.OnApproved()
	}
	return map[string]any{
		"principal": name, "secret": "secret/" + secretName, "node": req.Node,
		"version": version, "dependentsRemoved": dependents, "plan": planID,
	}, http.StatusOK, nil
}

// executePublisherMint revokes, mints, places, and re-creates — in that
// order, so a failure to place the token on the node leaves no live
// credential that nothing holds.
func (s *Server) executePublisherMint(r *http.Request, name, node, secretName string, dependents []string) (uint32, error) {
	if _, err := s.DB.RevokeTokensFor(name); err != nil {
		return 0, err
	}
	token, err := s.Principals.Mint(name, "", 0)
	if err != nil {
		return 0, err
	}
	res, err := s.Agents.Apply(r.Context(), node, &wsv1.Operation{Op: &wsv1.Operation_PutSecret{
		PutSecret: &wsv1.PutSecret{Name: secretName, Value: token},
	}})
	if err != nil {
		s.DB.RevokeTokensFor(name) //nolint:errcheck // best effort: the token never reached the node
		return 0, fmt.Errorf("node %s: %w", node, err)
	}
	if oe := res.GetError(); oe != nil {
		s.DB.RevokeTokensFor(name) //nolint:errcheck // best effort: the token never reached the node
		return 0, fmt.Errorf("node %s: %s", node, oe.GetMessage())
	}
	version := res.GetPutSecret().GetVersion()
	if err := s.recreateDependents(r, node, dependents); err != nil {
		return version, err
	}
	return version, nil
}
