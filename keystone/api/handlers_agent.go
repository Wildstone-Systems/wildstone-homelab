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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentgateway"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerlive"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerstore"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/oauthresource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/redact"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

var externalTaskOpaqueID = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

// agentAdmitTask is the model-invisible OAuth-only conversation admission
// seam from ADR-W049 §2.3. Authentication and the closed OAuth route gate
// have already run before this method can read the bounded body.
func (s *Server) agentAdmitTask(r *http.Request, _ principal.Identity) (any, int, error) {
	auth, ok := oauthresource.FromContext(r.Context())
	if !ok {
		return nil, http.StatusForbidden, errors.New("task admission requires the OAuth profile")
	}
	if s.SecretVault == nil {
		return nil, http.StatusServiceUnavailable, errors.New("encrypted task admission is unavailable")
	}
	var req agentapi.TaskAdmissionRequest
	if err := decodeAgentBody(r, &req); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if !externalTaskOpaqueID.MatchString(req.ClientSessionID) || !externalTaskOpaqueID.MatchString(req.IdempotencyKey) {
		return nil, http.StatusBadRequest, errors.New("clientSessionId and idempotencyKey must be 1-128 safe opaque characters")
	}
	objective := strings.TrimSpace(strings.ReplaceAll(req.Objective, "\r\n", "\n"))
	if objective == "" || len(objective) > 4096 || !utf8.ValidString(objective) {
		return nil, http.StatusBadRequest, errors.New("objective must contain 1-4096 bytes of valid UTF-8 text")
	}
	// Re-introspection is deliberately after parsing and immediately before
	// the atomic store boundary; the resolver's initial check protected body
	// decoding, while this one protects the write.
	id, err := auth.Recheck(r.Context())
	if err != nil {
		return nil, http.StatusUnauthorized, oauthresource.ErrUnauthenticated
	}
	sum := sha256.Sum256([]byte("external-task-admission/v1\x00" + objective))
	admitted, err := s.SecretVault.AdmitExternalTask(runtimedb.ExternalTaskAdmissionInput{
		Identity: runtimedb.ExternalTaskAdmissionIdentity{
			RegistrationID: id.RegistrationID, PersonID: id.PersonID, Butler: id.AgentID,
			Connection: id.ConnectionID, RuntimePrincipal: id.RuntimePrincipal,
			CredentialEpoch: id.CredentialEpoch,
		},
		ClientSessionID: req.ClientSessionID,
		IdempotencyKey:  req.IdempotencyKey,
		PayloadDigest:   hex.EncodeToString(sum[:]),
		Objective:       objective,
	})
	if errors.Is(err, runtimedb.ErrExternalAdmissionConflict) {
		return nil, http.StatusConflict, err
	}
	if errors.Is(err, runtimedb.ErrNotAdmitted) || errors.Is(err, runtimedb.ErrSecretDenied) {
		return nil, http.StatusForbidden, errors.New("task admission refused")
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return agentapi.TaskAdmission{TaskID: admitted.TaskID, Handle: agentapi.Handle(admitted.Handle)}, http.StatusCreated, nil
}

// maxDeliveryPoll bounds the deliveries long-poll (ADR-W039 §3.6, Phase
// 4, #341), the same cap ADR-W040 §8 records and the same shape
// maxAdapterPoll gives the integration adapter's own long poll.
const maxDeliveryPoll = 30 * time.Second

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
	ctx := agentgateway.WithPrincipal(r.Context(), id.Name)
	if auth, ok := oauthresource.FromContext(r.Context()); ok {
		ctx = agentgateway.WithExternalRegistration(ctx, auth.Identity.RegistrationID)
	}
	return ctx, http.StatusOK, nil
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
	if _, err := s.DB.GetButlerRuntime(butlerSeq); err != nil {
		return err
	}
	// unregistered → connected is the runtime's first ever call;
	// offline → connected is a reconnect after the heartbeat sweeper
	// (or an outage) flipped it (ADR-W039 §3.7, Phase 4, #340). Both
	// are an arrival, and both re-offer whatever this Agent's tasks
	// were left waiting on it — once: MarkRuntimeArrived lets only the
	// one call that made the flip see it, however many arrive together.
	reconnected, err := s.DB.MarkRuntimeArrived(butlerSeq)
	if err != nil {
		return err
	}
	if err := s.DB.TouchRuntimeHeartbeat(butlerSeq); err != nil && !errors.Is(err, runtimedb.ErrNotFound) {
		return err
	}
	if reconnected {
		if err := s.reofferWaitingTasks(butlerSeq); err != nil {
			return err
		}
	}
	return nil
}

// reofferWaitingTasks is the other half of health-driven waiting
// (ADR-W039 §3.7, Phase 4, #340): a runtime that just reconnected gets
// a fresh handle and an agent_deliveries row (kind turn) for every one
// of its own tasks parked in waiting(runtime-offline), so its next
// long-poll (#341) discovers them without Wildstone pushing anything.
// The wait itself is left exactly as it is — it resolves the ordinary
// way, when the runtime reports back in with a Work Start (reason
// "delivery"), which butlerstore.Ledger.Running already treats as
// resolving any open wait.
//
// A failure here is not surfaced as this call's own error: the
// reconnect itself already succeeded (health is already connected),
// and a runtime that misses a re-offer here still has an open wait its
// own next Manifest/Invoke call will find failing for an entirely
// different reason if it tries to act without one — losing the notice
// is not losing the work.
func (s *Server) reofferWaitingTasks(butlerSeq int64) error {
	if s.Ledger == nil {
		return nil
	}
	b, err := s.DB.GetButlerBySeq(butlerSeq)
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil
		}
		return err
	}
	tasks, err := s.DB.ButlerTasksWaitingOnRuntime(b.Name, runtimedb.ButlerWaitRuntimeOffline)
	if err != nil {
		s.Log.Error("finding tasks waiting on a reconnected runtime", "butler", b.Name, "err", err)
		return nil
	}
	for _, task := range tasks {
		handle, err := s.Ledger.IssueHandle(task.ID)
		if err != nil {
			s.Log.Error("issuing a handle to re-offer a waiting task", "task", task.ID, "err", err)
			continue
		}
		// The turn's own words are not kept once it was parked, so the
		// runtime is given the task's objective: the person's question,
		// bounded and quoted (butlerlive.conversationObjective).
		payload, err := json.Marshal(butlerlive.ExternalTurn{Handle: string(handle), Text: task.Objective})
		if err != nil {
			s.Log.Error("encoding a turn delivery", "task", task.ID, "err", err)
			continue
		}
		if _, err := s.DB.RecordAgentDeliveryForTask(butlerSeq, agentapi.DeliveryTurn, task.ID, string(payload)); err != nil {
			s.Log.Error("recording a turn delivery", "task", task.ID, "err", err)
		}
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

// agentTools is GET /api/v1/agent/tools (#364): the calling runtime's
// Agent's own tool set, for a runtime that lists its tools before it
// holds any task (agentapi.ToolsClient).
func (s *Server) agentTools(r *http.Request, id principal.Identity) (any, int, error) {
	ctx, code, err := s.agentContext(r, id)
	if err != nil {
		return nil, code, err
	}
	tc, ok := s.AgentGateway.(agentapi.ToolsClient)
	if !ok {
		return nil, http.StatusNotImplemented, fmt.Errorf("this deployment's agent gateway cannot list tools without a task")
	}
	tools, err := tc.Tools(ctx)
	if err != nil {
		return agentErrorResponse(err)
	}
	return toolsResponse{Tools: tools}, http.StatusOK, nil
}

// toolsResponse is GET /api/v1/agent/tools's body.
type toolsResponse struct {
	Tools []agentapi.ToolSpec `json:"tools"`
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

// agentRecall is POST /api/v1/agent/recall (ADR-W039 Phase 3,
// agentgateway.Gateway.Recall, #338): a structured search of Wildstone
// memory for the reader on the handle, disclosed exactly as the
// "recall" tool discloses it over Invoke.
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

// agentPropose is POST /api/v1/agent/propose (ADR-W039 Phase 3,
// agentgateway.Gateway.Propose, #339): files a Plan for Owner approval,
// attributed to the runtime principal acting for the Person on the
// handle. Never applied here.
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

// agentNotify is POST /api/v1/agent/notify (ADR-W039 §3.2 Notifications,
// §4 item 8; Phase 4, #341): outbound text through the task's own
// Connection, via agentgateway.Gateway.Notify — the same outbound chat
// queue and the same "never a room, never a direct address, quiet
// hours" rules the stall notifier and QueueChatPush's own domestic
// executor already enforce for a household rule's push (see notify.go's
// own doc comment for why this reaches that queue directly rather than
// through api.QueueChatPush, which agentgateway cannot import).
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

// deliveriesResponse is what both GET /api/v1/agent/deliveries and the
// in-process Deliveries call return: unacked deliveries, oldest first,
// exactly as agentapi.Delivery carries one over the wire.
type deliveriesResponse struct {
	Deliveries []agentapi.Delivery `json:"deliveries"`
}

// redacted is this response as writeJSON sends it: every delivery
// redacted like any other body EXCEPT its task handle (#364). A handle
// is the bearer the runtime works the task with — its secret half is
// exactly the base64 shape redaction exists to catch, so a redacted
// handle ("bt-33.[REDACTED]") is refused on the runtime's very next
// call, and no delivery over HTTP could ever be acted on. It goes only
// to the runtime principal it was issued for, the same carve-out
// runtimeCredentialResponse makes for the credential itself. Everything
// else — the payload's text, history, ids — is still redacted.
func (d deliveriesResponse) redacted() ([]byte, error) {
	out := deliveriesResponse{Deliveries: make([]agentapi.Delivery, 0, len(d.Deliveries))}
	for _, dl := range d.Deliveries {
		handle := dl.Handle
		dl.Handle = ""
		// The payload may carry the handle too (a turn's, a re-offer's):
		// held out the same way and put back after.
		var fields map[string]json.RawMessage
		payloadHandle := json.RawMessage(nil)
		if len(dl.Payload) > 0 && json.Unmarshal(dl.Payload, &fields) == nil {
			if h, ok := fields["handle"]; ok {
				payloadHandle = h
				delete(fields, "handle")
				raw, err := json.Marshal(fields)
				if err != nil {
					return nil, err
				}
				dl.Payload = raw
			}
		}
		raw, err := json.Marshal(dl)
		if err != nil {
			return nil, err
		}
		var clean agentapi.Delivery
		if err := json.Unmarshal([]byte(redact.String(string(raw))), &clean); err != nil {
			return nil, err
		}
		clean.Handle = handle
		if payloadHandle != nil {
			fields = nil
			if err := json.Unmarshal(clean.Payload, &fields); err != nil {
				return nil, err
			}
			fields["handle"] = payloadHandle
			if clean.Payload, err = json.Marshal(fields); err != nil {
				return nil, err
			}
		}
		out.Deliveries = append(out.Deliveries, clean)
	}
	return json.Marshal(out)
}

// runtimeRequestCredential reauthenticates the exact bearer that entered an
// HTTP delivery request and reduces it to the principal plus stored token
// fingerprint used by the final transactional authority fence. The secret
// remains request-local and is never persisted, audited or passed to gateway
// business logic.
func (s *Server) runtimeRequestCredential(r *http.Request, expected principal.Identity) (runtimedb.RuntimeDeliveryCredential, error) {
	const scheme = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, scheme) {
		return runtimedb.RuntimeDeliveryCredential{}, principal.ErrUnauthenticated
	}
	secret := strings.TrimSpace(strings.TrimPrefix(auth, scheme))
	id, err := s.Principals.Authenticate(secret)
	if err != nil || id.Name != expected.Name || id.Role != expected.Role || id.ActingFor != expected.ActingFor {
		return runtimedb.RuntimeDeliveryCredential{}, principal.ErrUnauthenticated
	}
	hash, err := principal.TokenHash(secret)
	if err != nil {
		return runtimedb.RuntimeDeliveryCredential{}, err
	}
	return runtimedb.RuntimeDeliveryCredential{Principal: expected.Name, TokenHash: hash}, nil
}

// agentDeliveries is GET /api/v1/agent/deliveries?wait=<duration>&after=<id>
// (ADR-W039 §3.6, §4.1; Phase 4, #341): the long-poll
// pull path every runtime can use whether or not push ever reaches it.
// Modelled on claimIntegrationWork's own long poll (api/handlers_integrations.go):
// wait is a parameter, capped at maxDeliveryPoll, and an empty answer
// after the window is a normal quiet period, not an error.
func (s *Server) agentDeliveries(r *http.Request, id principal.Identity) (any, int, error) {
	if _, code, err := s.agentContext(r, id); err != nil {
		return nil, code, err
	}
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &after); err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("after must be a delivery id")
		}
	}
	wait := time.Duration(0)
	if v := r.URL.Query().Get("wait"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("wait must be a duration: %w", err)
		}
		wait = d
	}
	if wait > maxDeliveryPoll {
		wait = maxDeliveryPoll
	}
	if wait < 0 {
		wait = 0
	}
	auth, oauth := oauthresource.FromContext(r.Context())
	var cred runtimedb.RuntimeDeliveryCredential
	var err error
	if !oauth {
		cred, err = s.runtimeRequestCredential(r, id)
		if err != nil {
			return nil, http.StatusUnauthorized, principal.ErrUnauthenticated
		}
	}
	deadline := time.Now().Add(wait)
	for {
		var rows []runtimedb.AgentDelivery
		if oauth {
			fresh, recheckErr := auth.Recheck(r.Context())
			if recheckErr != nil {
				return nil, http.StatusUnauthorized, oauthresource.ErrUnauthenticated
			}
			rows, err = s.DB.PollExternalDeliveries(externalDeliveryIdentity(fresh), after, 50, time.Now)
		} else {
			rows, err = s.DB.PollRuntimeDeliveries(cred, after, 50, time.Now)
		}
		if errors.Is(err, runtimedb.ErrRuntimeDeliveryDenied) {
			return nil, http.StatusUnauthorized, principal.ErrUnauthenticated
		}
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		if len(rows) > 0 {
			if oauth {
				if _, err := auth.Recheck(r.Context()); err != nil {
					return nil, http.StatusUnauthorized, oauthresource.ErrUnauthenticated
				}
			}
			return deliveriesResponse{Deliveries: deliveriesFor(agentgateway.FilterDeliveries(s.DB, rows))}, http.StatusOK, nil
		}
		if time.Now().After(deadline) || r.Context().Err() != nil {
			return deliveriesResponse{Deliveries: []agentapi.Delivery{}}, http.StatusOK, nil
		}
		if s.deliveryPollWaiting != nil {
			s.deliveryPollWaiting()
		}
		select {
		case <-r.Context().Done():
			return deliveriesResponse{Deliveries: []agentapi.Delivery{}}, http.StatusOK, nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// deliveriesFor maps stored rows onto the wire shape. Handle is a
// best-effort read of the payload's own "handle" field — every kind
// this deployment writes puts one there except proposal_outcome, which
// has no task to point at — never a claim this table itself makes about
// what the opaque payload holds.
func deliveriesFor(rows []runtimedb.AgentDelivery) []agentapi.Delivery {
	out := make([]agentapi.Delivery, 0, len(rows))
	for _, row := range rows {
		var carried struct {
			Handle       string `json:"handle"`
			Subscription int64  `json:"subscription"`
		}
		_ = json.Unmarshal([]byte(row.Payload), &carried)
		subscription := ""
		if carried.Subscription != 0 {
			subscription = fmt.Sprintf("%d", carried.Subscription)
		}
		out = append(out, agentapi.Delivery{
			ID:           fmt.Sprintf("%d", row.ID),
			Kind:         row.Kind,
			Subscription: subscription,
			Cursor:       row.Cursor,
			Handle:       agentapi.Handle(carried.Handle),
			Payload:      json.RawMessage(row.Payload),
			Attempt:      1,
			ExpiresAt:    row.ExpiresAt,
		})
	}
	return out
}

// agentAckDelivery is POST /api/v1/agent/deliveries/{id}/ack (ADR-W039
// §3.6, §4 item 6; Phase 4, #341): idempotent, and refuses an id that
// belongs to another Agent or does not exist.
func (s *Server) agentAckDelivery(r *http.Request, id principal.Identity) (any, int, error) {
	if _, code, err := s.agentContext(r, id); err != nil {
		return nil, code, err
	}
	var deliveryID int64
	if _, err := fmt.Sscanf(r.PathValue("id"), "%d", &deliveryID); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("delivery id must be a number")
	}
	auth, oauth := oauthresource.FromContext(r.Context())
	var err error
	if oauth {
		fresh, recheckErr := auth.Recheck(r.Context())
		if recheckErr != nil {
			return nil, http.StatusUnauthorized, oauthresource.ErrUnauthenticated
		}
		err = s.DB.AckExternalDelivery(externalDeliveryIdentity(fresh), deliveryID)
	} else {
		cred, authErr := s.runtimeRequestCredential(r, id)
		if authErr != nil {
			return nil, http.StatusUnauthorized, principal.ErrUnauthenticated
		}
		err = s.DB.AckRuntimeDelivery(cred, deliveryID, time.Now)
	}
	if err != nil {
		if errors.Is(err, runtimedb.ErrRuntimeDeliveryDenied) {
			return nil, http.StatusUnauthorized, principal.ErrUnauthenticated
		}
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no such delivery for this agent")
		}
		return nil, http.StatusInternalServerError, err
	}
	return nil, http.StatusOK, nil
}

func externalDeliveryIdentity(id oauthresource.Identity) runtimedb.ExternalDeliveryIdentity {
	return runtimedb.ExternalDeliveryIdentity{RegistrationID: id.RegistrationID, PersonID: id.PersonID,
		Butler: id.AgentID, Connection: id.ConnectionID, RuntimePrincipal: id.RuntimePrincipal,
		CredentialEpoch: id.CredentialEpoch}
}
