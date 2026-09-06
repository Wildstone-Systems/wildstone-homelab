package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// maxAskBytes bounds an ask body (ADR-W017 A6: hostile user messages are
// bounded input). A legitimate question is a paragraph, not a payload.
const maxAskBytes = 16 << 10

// butlerPrincipal is the fixed acting principal every Butler-originated
// proposal is recorded under (ADR-W017 A1/A3): principal "butler"
// acting-for the authenticated caller. The RoleButler row in the RBAC
// table (read+propose, never approve) is what bounds it.
const butlerPrincipal = "butler"

// ButlerHost is everything the ask endpoint needs to run a session
// (ADR-W017 A2/A4) — cmd/keystone wires it from -butler-provider et al.
// A nil host (provider off/unconfigured) leaves the ask endpoint
// answering 503; session history stays readable regardless.
type ButlerHost struct {
	// mu guards the fields a runtime config change replaces (ADR-W017
	// A9). Handlers read them while requests are in flight, so swapping
	// a provider is a write to shared state, not a reassignment nobody
	// is watching.
	mu sync.RWMutex

	Provider provider.Provider
	Gateway  *butler.ToolGateway
	// Mode is the startup probe's verdict (ADR-W006 A4 / ADR-W017 A4):
	// ButlerModeQAOnly runs sessions with the read-only tool set and
	// dispatch-level refusal of propose_change.
	Mode runtimedb.ButlerSessionMode
	// Household carries the family surface's injected seams (ADR-W026
	// A1): the household timeline reader and the domestic-tier action
	// dispatcher a family session runs on. A zero value is a working
	// configuration — the family endpoint then answers that this
	// deployment has no household surface wired, rather than pretending
	// to have one.
	Household butler.HouseholdSeams
	// Model and Endpoint are the RESOLVED backend the provider talks to —
	// the flag value when one was given, otherwise the client's own
	// default. They are display facts for the status endpoint so an
	// operator can see what "-butler-provider ollama" actually resolved
	// to without reading the process's command line.
	Model    string
	Endpoint string
}

// Current returns the live backend under the lock, so a handler that is
// mid-request keeps a coherent view even if the operator changes the
// model while it runs.
//
// A session in flight finishes against the provider it started with.
// Swapping under it would produce a transcript whose tool calls and
// final answer came from different models, which is worse than a slow
// change taking effect on the next question.
func (h *ButlerHost) Current() (provider.Provider, *butler.ToolGateway, runtimedb.ButlerSessionMode, string, string) {
	if h == nil {
		return nil, nil, "", "", ""
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.Provider, h.Gateway, h.Mode, h.Model, h.Endpoint
}

// Swap replaces the backend in place. A nil provider means "configured
// off", which is a legitimate choice and not a failure.
func (h *ButlerHost) Swap(p provider.Provider, mode runtimedb.ButlerSessionMode, model, endpoint string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Provider, h.Mode, h.Model, h.Endpoint = p, mode, model, endpoint
}

// butlerStatus is what GET /api/v1/butler serves: enough for the Console
// to render the Butler surface honestly whether or not a provider is
// configured. Reason is filled ONLY when configured is false, and says
// what to do about it — an unconfigured Butler is the normal state of a
// fresh install, not an error.
type butlerStatus struct {
	Configured bool     `json:"configured"`
	Provider   string   `json:"provider,omitempty"`
	Model      string   `json:"model,omitempty"`
	Endpoint   string   `json:"endpoint,omitempty"`
	Mode       string   `json:"mode,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Tools      []string `json:"tools,omitempty"`
}

// getButlerStatus reports whether a Butler is wired and what it is wired
// to. It carries the read verb like the rest of the Butler routes, and
// returns no credential: the endpoint is passed through redact as
// defense-in-depth, since an openai-compatible base URL is operator-
// supplied text that could carry a key in a query string.
func (s *Server) getButlerStatus(_ *http.Request, _ principal.Identity) (any, int, error) {
	prov, gw, mode, model, endpoint := s.ButlerHost.Current()
	host := s.ButlerHost
	if host == nil || prov == nil || gw == nil {
		return butlerStatus{
			Configured: false,
			Reason:     "no Butler provider configured; start keystone with -butler-provider (off|anthropic|openai|ollama), -butler-model and -butler-endpoint",
		}, http.StatusOK, nil
	}
	// The offered set, not the registered one: a QA-only session is
	// handed the read tools only, and the surface should say so rather
	// than advertise a propose_change the session would refuse.
	specs := gw.ToolSpecs()
	if mode == runtimedb.ButlerModeQAOnly {
		specs = gw.ReadOnlyToolSpecs()
	}
	tools := make([]string, 0, len(specs))
	for _, t := range specs {
		tools = append(tools, t.Name)
	}
	return butlerStatus{
		Configured: true,
		Provider:   prov.Name(),
		Model:      model,
		Endpoint:   redact.String(endpoint),
		Mode:       string(mode),
		Tools:      tools,
	}, http.StatusOK, nil
}

type askRequest struct {
	Message string `json:"message"`
}

// askResponse is what one completed ask returns: the session id (its
// durable audit record), the model's final text, and the ids of any plan
// records the session's accepted proposals produced — all in state
// `proposed`, waiting on a human approve verb.
type askResponse struct {
	SessionID string   `json:"sessionId"`
	Mode      string   `json:"mode"`
	FinalText string   `json:"finalText"`
	Capped    bool     `json:"capped,omitempty"`
	PlanIDs   []string `json:"planIds,omitempty"`
	// ProposalErrors reports proposals the session produced that the
	// shared pipeline then refused (e.g. standing policy) — surfaced, not
	// swallowed, so the caller sees the Butler tried and was blocked.
	ProposalErrors []string `json:"proposalErrors,omitempty"`
}

// transcriptRecord is the persisted form of one audited tool call: only
// post-Dispatch (already redacted/enveloped/bounded) content, per
// ADR-W017 A1.
type transcriptRecord struct {
	Name     string         `json:"name"`
	Input    map[string]any `json:"input,omitempty"`
	Result   string         `json:"result,omitempty"`
	Error    string         `json:"error,omitempty"`
	Rejected bool           `json:"rejected,omitempty"`
}

type storedTranscript struct {
	Provider string             `json:"provider"`
	Calls    []transcriptRecord `json:"calls,omitempty"`
	PlanIDs  []string           `json:"planIds,omitempty"`
}

// askButler runs one Butler session for the authenticated caller
// (ADR-W017 A2). The route carries the read verb — conversing is not a
// write; the only writes a session can cause are `proposed` plan records
// entered through the same submitProposal pipeline as the HTTP propose
// endpoint, as principal "butler" acting-for the caller.
func (s *Server) askButler(r *http.Request, id principal.Identity) (any, int, error) {
	message, st, err := readAskMessage(r)
	if err != nil {
		return nil, st, err
	}
	// The unconfigured check lives in the session runner, with the one
	// snapshot it takes — checking here as well would read the backend
	// twice for one request, which is the thing ADR-W017 A9's snapshot
	// exists to prevent.
	return s.runAdminButlerSession(r.Context(), message, id.Name)
}

// runAdminButlerSession runs one operator-surface session on behalf of
// actingFor and returns the ask response. It is shared by the HTTP ask
// endpoint and the chat inbound endpoint (ADR-W026 A8), so a question
// asked over Telegram and one asked in the Console take the same path
// through the same gateway, produce the same audit record, and reach
// the same ceiling.
//
// It takes its OWN snapshot of the backend (ADR-W017 A9) rather than
// receiving one from its caller: every read of the provider, gateway and
// mode for one session comes from that single snapshot. Mixing a
// captured mode with a live provider is how a newly selected model that
// FAILED its probe would run with the previous model's `full` verdict
// and be handed proposal tools.
func (s *Server) runAdminButlerSession(ctx context.Context, message, actingFor string) (any, int, error) {
	askProv, askGw, askMode, _, _ := s.ButlerHost.Current()
	if askProv == nil || askGw == nil {
		return nil, http.StatusServiceUnavailable,
			fmt.Errorf("butler provider not configured (start keystone with -butler-provider)")
	}
	session := &butler.Session{
		Gateway:  askGw,
		ReadOnly: askMode == runtimedb.ButlerModeQAOnly,
	}
	tr, err := session.Run(ctx, askProv, message)
	if err != nil {
		// The provider failed mid-session; nothing durable happened (no
		// proposals are submitted until the session completes).
		return nil, http.StatusBadGateway, fmt.Errorf("butler session: %w", err)
	}

	// Submit every accepted proposal through the ONE proposal pipeline
	// (ADR-W017 A3), as principal "butler" acting-for the caller. Policy
	// runs again here with the real engine — the session's own gate is a
	// pre-check, not the authority.
	butlerID := principal.Identity{Name: butlerPrincipal, Role: runtimedb.RoleButler, ActingFor: actingFor}
	var planIDs, proposalErrs []string
	for _, call := range tr.Audit.Calls {
		if call.ResourceYAML == "" || call.Rejected {
			continue
		}
		summary := "butler proposal"
		if call.Plan != nil && call.Plan.Summary != "" {
			summary = "butler: " + call.Plan.Summary
		}
		body, st, err := s.submitProposal(butlerID, proposalRequest{
			Summary: summary,
			Put:     []string{call.ResourceYAML},
		})
		if err != nil {
			proposalErrs = append(proposalErrs, fmt.Sprintf("%d: %v", st, err))
			continue
		}
		if rec, ok := body.(planView); ok {
			planIDs = append(planIDs, rec.ID)
		}
	}

	// Persist the session (ADR-W017 A1): principal-bound, transcript
	// included. Everything in the transcript is post-Dispatch content, and
	// the whole serialized record passes redact once more as
	// defense-in-depth against secret-shaped text in model inputs/output.
	stored := storedTranscript{Provider: tr.Audit.Provider, PlanIDs: planIDs}
	for _, call := range tr.Audit.Calls {
		stored.Calls = append(stored.Calls, transcriptRecord{
			Name:     call.Name,
			Input:    call.Input,
			Result:   call.Result,
			Error:    call.Error,
			Rejected: call.Rejected,
		})
	}
	transcriptJSON, err := json.Marshal(stored)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("encoding transcript: %w", err)
	}
	finalText := redact.String(tr.FinalText)
	sessionID, err := s.DB.InsertButlerSession(runtimedb.ButlerSession{
		Principal:      butlerPrincipal,
		ActingFor:      actingFor,
		Provider:       tr.Audit.Provider,
		Mode:           askMode,
		Surface:        runtimedb.ButlerSurfaceAdmin,
		FinalText:      finalText,
		Capped:         tr.CappedIterations,
		TranscriptJSON: redact.String(string(transcriptJSON)),
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("butler.session", "", principal.Identity{Name: butlerPrincipal, ActingFor: actingFor},
		fmt.Sprintf("session %s: %d tool calls, %d proposals", sessionID, len(tr.Audit.Calls), len(planIDs)))

	return askResponse{
		SessionID:      sessionID,
		Mode:           string(askMode),
		FinalText:      finalText,
		Capped:         tr.CappedIterations,
		PlanIDs:        planIDs,
		ProposalErrors: proposalErrs,
	}, http.StatusOK, nil
}

// listButlerSessions lists recorded sessions from BOTH surfaces, newest
// first. ?surface=admin|family narrows it — the audit question "what has
// the household's butler been asked to do" is a different question from
// "what has the operator's", and one list that cannot tell them apart
// answers neither (ADR-W026 A1).
func (s *Server) listButlerSessions(r *http.Request, _ principal.Identity) (any, int, error) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	surface := runtimedb.ButlerSurface(r.URL.Query().Get("surface"))
	if surface != "" && !runtimedb.ValidButlerSurface(surface) {
		return nil, http.StatusBadRequest, fmt.Errorf("unknown surface %q (admin|family)", surface)
	}
	sessions, err := s.DB.ListButlerSessions(limit, surface)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if sessions == nil {
		sessions = []runtimedb.ButlerSession{}
	}
	return sessions, http.StatusOK, nil
}

func (s *Server) getButlerSession(r *http.Request, _ principal.Identity) (any, int, error) {
	sess, err := s.DB.GetButlerSession(r.PathValue("id"))
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("butler session not found")
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return sess, http.StatusOK, nil
}

// readAskMessage reads and bounds one ask body, shared by both surfaces
// (ADR-W026 A6: inbound text is bounded before anything else looks at
// it, on the family surface exactly as on the operator's).
func readAskMessage(r *http.Request) (string, int, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAskBytes+1))
	if err != nil {
		return "", http.StatusBadRequest, fmt.Errorf("reading body: %w", err)
	}
	if len(body) > maxAskBytes {
		return "", http.StatusRequestEntityTooLarge, fmt.Errorf("message exceeds %d bytes", maxAskBytes)
	}
	var req askRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", http.StatusBadRequest, fmt.Errorf("parsing ask: %w", err)
	}
	if req.Message == "" {
		return "", http.StatusBadRequest, fmt.Errorf("message required")
	}
	return req.Message, http.StatusOK, nil
}
