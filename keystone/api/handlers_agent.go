package api

// The Agent API over HTTP (ADR-W039 Phase 2c, #337; ADR-W040): the wire
// transport ADR-W040 chose — plain REST over the existing Keystone API,
// one route per keystone/agentapi.Client method, no second schema.
//
// Every handler here does the same five things, ADR-W040 §2's "an
// ordinary HTTP client of Keystone, exactly as a chat adapter or an
// integration adapter is today":
//
//  1. s.handle has already authenticated the bearer and checked the
//     route's RBAC verb (authn → RBAC → rate limit, api.go's fixed
//     chain).
//  2. agentContext additionally requires the caller to be a runtime
//     principal — RuntimePrincipalButler(id.Name) — since RoleButler
//     covers wider credentials than a runtime's (§4.1: the credential
//     names one Agent for its life, and nothing here trusts a butler-
//     role token that is not one). It also touches the runtime's
//     arrival: unregistered flips to connected on the first
//     authenticated call, and every call stamps the heartbeat.
//  3. The body decodes as the agentapi.* request type 1:1 — no
//     wire-specific shape, per §2's "no second schema".
//  4. The SAME s.AgentGateway method the in-process client calls runs
//     the call. A refusal it returns as an Outcome (denied, waiting,
//     rejected) is not an HTTP error: it is a 200 body, exactly as the
//     in-process client sees it, so the two are byte-identical.
//  5. An error IS a transport or protocol failure (agentapi.Client's
//     own contract): a handle refused maps to 403, an unimplemented
//     method to 501 naming which phase serves it, and anything else to
//     500. Malformed JSON never reaches the gateway at all: 400.
//
// No route names a handle in its path or its query — every one is a
// POST with the handle inside the body — so a URL, a log line and an
// audit row never carry one (ADR-W040 §7).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/agentgateway"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// maxAgentBodyBytes bounds one Agent API request (ADR-W040 §7: bodies
// pass through a bounded reader so an oversized one cannot exhaust
// memory before validation, the chat adapter's own discipline). 1 MiB
// matches a plan proposal's own ceiling (maxProposalBytes): the largest
// bodies here are a propose_change's document (through invoke's Args)
// and a checkpoint's stored result, neither larger than a Plan.
const maxAgentBodyBytes = 1 << 20

// errNotRuntimePrincipal is every agent route's refusal for a caller
// that is not a runtime principal at all — an admin token, an operator,
// a name nobody bound. It says nothing about which Agent or which
// handle was involved: the caller gets the same answer whatever it
// tried to call, which is the point of asking before any body is even
// read.
var errNotRuntimePrincipal = errors.New("this credential is not a runtime principal")

// agentContext is the second half of every agent route's authentication
// (see the file doc comment). It returns the context an
// agentapi.Client call runs under, carrying the runtime principal for
// agentgateway.WithPrincipal's own binding check (§4.1: a handle for
// another Agent's task is refused there, however valid the handle is).
func (s *Server) agentContext(r *http.Request, id principal.Identity) (context.Context, int, error) {
	if s.AgentGateway == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("the agent api is not wired")
	}
	seq, ok, err := s.DB.RuntimePrincipalButler(id.Name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if !ok {
		return nil, http.StatusForbidden, errNotRuntimePrincipal
	}
	if err := s.touchRuntimeArrival(seq); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return agentgateway.WithPrincipal(r.Context(), id.Name), http.StatusOK, nil
}

// touchRuntimeArrival is Phase 1b's loose end (#336): the first
// successfully authenticated call from a runtime whose health is still
// `unregistered` flips it to `connected` (§4.1 "authenticate: ... health:
// connected"), and every authenticated call stamps the heartbeat
// (TouchRuntimeHeartbeat). A revoked runtime never reaches here — its
// principal is disabled before s.handle's authn step — so
// TouchRuntimeHeartbeat's own refusal for a revoked runtime is not an
// error this needs to surface; ErrNotFound (its only failure mode
// besides a real database error) is ignored for the same reason.
func (s *Server) touchRuntimeArrival(butlerSeq int64) error {
	rt, err := s.DB.GetButlerRuntime(butlerSeq)
	if err != nil {
		return err
	}
	if rt.Health == agentapi.HealthUnregistered {
		if err := s.DB.SetRuntimeHealth(butlerSeq, agentapi.HealthConnected); err != nil {
			return err
		}
	}
	if err := s.DB.TouchRuntimeHeartbeat(butlerSeq); err != nil && !errors.Is(err, runtimedb.ErrNotFound) {
		return err
	}
	return nil
}

// decodeAgentBody decodes one Agent API request body 1:1 into an
// agentapi.* type: bounded (maxAgentBodyBytes), unknown fields refused
// — the same discipline decodeStrict uses elsewhere, spelled out here so
// this file does not import it for one line.
func decodeAgentBody(r *http.Request, into any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxAgentBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("parsing request: %w", err)
	}
	return nil
}

// notImplementedBody is a NotImplementedError rendered for a runtime:
// which method, which phase serves it, and why, so an operator reading
// a 501 knows what it is waiting on without reading Keystone's source.
type notImplementedBody struct {
	Method string `json:"method"`
	Phase  string `json:"phase"`
	Detail string `json:"detail,omitempty"`
}

// agentErrorResponse maps an agentapi.Client error to this route's HTTP
// answer (the file doc comment's step 5). It is never called for a
// refusal the gateway already turned into an Outcome — Invoke's denied,
// waiting and rejected never reach here as errors.
func agentErrorResponse(err error) (any, int, error) {
	var nie *agentapi.NotImplementedError
	if errors.As(err, &nie) {
		return notImplementedBody{Method: nie.Method, Phase: nie.Phase, Detail: nie.Reason}, http.StatusNotImplemented, err
	}
	if errors.Is(err, butlerstore.ErrHandleRefused) || errors.Is(err, agentgateway.ErrRefused) {
		return nil, http.StatusForbidden, err
	}
	return nil, http.StatusInternalServerError, err
}

// agentManifest is POST /api/v1/agent/manifest.
func (s *Server) agentManifest(r *http.Request, id principal.Identity) (any, int, error) {
	ctx, code, err := s.agentContext(r, id)
	if err != nil {
		return nil, code, err
	}
	var req struct {
		Handle agentapi.Handle `json:"handle"`
	}
	if err := decodeAgentBody(r, &req); err != nil {
		return nil, http.StatusBadRequest, err
	}
	res, err := s.AgentGateway.Manifest(ctx, req.Handle)
	if err != nil {
		return agentErrorResponse(err)
	}
	return res, http.StatusOK, nil
}

// agentInvoke is POST /api/v1/agent/invoke.
func (s *Server) agentInvoke(r *http.Request, id principal.Identity) (any, int, error) {
	ctx, code, err := s.agentContext(r, id)
	if err != nil {
		return nil, code, err
	}
	var req agentapi.InvokeRequest
	if err := decodeAgentBody(r, &req); err != nil {
		return nil, http.StatusBadRequest, err
	}
	res, err := s.AgentGateway.Invoke(ctx, req)
	if err != nil {
		return agentErrorResponse(err)
	}
	// denied, waiting and rejected are ordinary answers (ADR-W039 §4
	// item 5), not HTTP errors: the outcome travels in a 200 body
	// exactly as the in-process client returns it.
	return res, http.StatusOK, nil
}

// agentRecall is POST /api/v1/agent/recall. Not implemented until
// ADR-W039 Phase 3 (agentgateway.Gateway.Recall); the route exists now
// so a runtime gets a clear 501 rather than a 404 for a family ADR-W040
// names.
func (s *Server) agentRecall(r *http.Request, id principal.Identity) (any, int, error) {
	ctx, code, err := s.agentContext(r, id)
	if err != nil {
		return nil, code, err
	}
	var req agentapi.RecallRequest
	if err := decodeAgentBody(r, &req); err != nil {
		return nil, http.StatusBadRequest, err
	}
	hits, err := s.AgentGateway.Recall(ctx, req)
	if err != nil {
		return agentErrorResponse(err)
	}
	if hits == nil {
		hits = []agentapi.MemoryHit{}
	}
	return hits, http.StatusOK, nil
}

// agentRemember is POST /api/v1/agent/remember.
func (s *Server) agentRemember(r *http.Request, id principal.Identity) (any, int, error) {
	ctx, code, err := s.agentContext(r, id)
	if err != nil {
		return nil, code, err
	}
	var req agentapi.RememberRequest
	if err := decodeAgentBody(r, &req); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := s.AgentGateway.Remember(ctx, req); err != nil {
		return agentErrorResponse(err)
	}
	return nil, http.StatusOK, nil
}

// agentCheckpoint is POST /api/v1/agent/checkpoint.
func (s *Server) agentCheckpoint(r *http.Request, id principal.Identity) (any, int, error) {
	ctx, code, err := s.agentContext(r, id)
	if err != nil {
		return nil, code, err
	}
	var req agentapi.Checkpoint
	if err := decodeAgentBody(r, &req); err != nil {
		return nil, http.StatusBadRequest, err
	}
	res, err := s.AgentGateway.Checkpoint(ctx, req)
	if err != nil {
		return agentErrorResponse(err)
	}
	return res, http.StatusOK, nil
}

// agentStart is POST /api/v1/agent/start.
func (s *Server) agentStart(r *http.Request, id principal.Identity) (any, int, error) {
	ctx, code, err := s.agentContext(r, id)
	if err != nil {
		return nil, code, err
	}
	var req agentapi.WorkStart
	if err := decodeAgentBody(r, &req); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := s.AgentGateway.Start(ctx, req); err != nil {
		return agentErrorResponse(err)
	}
	return nil, http.StatusOK, nil
}

// agentWait is POST /api/v1/agent/wait.
func (s *Server) agentWait(r *http.Request, id principal.Identity) (any, int, error) {
	ctx, code, err := s.agentContext(r, id)
	if err != nil {
		return nil, code, err
	}
	var req agentapi.WorkWait
	if err := decodeAgentBody(r, &req); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := s.AgentGateway.Wait(ctx, req); err != nil {
		return agentErrorResponse(err)
	}
	return nil, http.StatusOK, nil
}

// agentComplete is POST /api/v1/agent/complete.
func (s *Server) agentComplete(r *http.Request, id principal.Identity) (any, int, error) {
	ctx, code, err := s.agentContext(r, id)
	if err != nil {
		return nil, code, err
	}
	var req agentapi.WorkComplete
	if err := decodeAgentBody(r, &req); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := s.AgentGateway.Complete(ctx, req); err != nil {
		return agentErrorResponse(err)
	}
	return nil, http.StatusOK, nil
}

// agentFail is POST /api/v1/agent/fail.
func (s *Server) agentFail(r *http.Request, id principal.Identity) (any, int, error) {
	ctx, code, err := s.agentContext(r, id)
	if err != nil {
		return nil, code, err
	}
	var req agentapi.WorkFail
	if err := decodeAgentBody(r, &req); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := s.AgentGateway.Fail(ctx, req); err != nil {
		return agentErrorResponse(err)
	}
	return nil, http.StatusOK, nil
}

// agentPropose is POST /api/v1/agent/propose. Not implemented until
// ADR-W039 Phase 3 (agentgateway.Gateway.Propose).
func (s *Server) agentPropose(r *http.Request, id principal.Identity) (any, int, error) {
	ctx, code, err := s.agentContext(r, id)
	if err != nil {
		return nil, code, err
	}
	var req agentapi.ProposeRequest
	if err := decodeAgentBody(r, &req); err != nil {
		return nil, http.StatusBadRequest, err
	}
	res, err := s.AgentGateway.Propose(ctx, req)
	if err != nil {
		return agentErrorResponse(err)
	}
	return res, http.StatusOK, nil
}

// agentNotify is POST /api/v1/agent/notify. Not implemented until
// ADR-W039 Phase 4 (agentgateway.Gateway.Notify).
func (s *Server) agentNotify(r *http.Request, id principal.Identity) (any, int, error) {
	ctx, code, err := s.agentContext(r, id)
	if err != nil {
		return nil, code, err
	}
	var req agentapi.NotifyRequest
	if err := decodeAgentBody(r, &req); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := s.AgentGateway.Notify(ctx, req); err != nil {
		return agentErrorResponse(err)
	}
	return nil, http.StatusOK, nil
}

// agentHeartbeat is POST /api/v1/agent/heartbeat. It carries no handle
// (Heartbeat is a fact about the runtime the credential names, not a
// task); agentContext's own TouchRuntimeHeartbeat call already recorded
// this arrival, so the gateway's Heartbeat here is the in-process
// client's own path for the identical write, kept byte-identical
// between the two rather than special-cased away.
func (s *Server) agentHeartbeat(r *http.Request, id principal.Identity) (any, int, error) {
	ctx, code, err := s.agentContext(r, id)
	if err != nil {
		return nil, code, err
	}
	var req agentapi.Heartbeat
	if err := decodeAgentBody(r, &req); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := s.AgentGateway.Heartbeat(ctx, req); err != nil {
		return agentErrorResponse(err)
	}
	return nil, http.StatusOK, nil
}
