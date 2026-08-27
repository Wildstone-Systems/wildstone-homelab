package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

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
	Provider provider.Provider
	Gateway  *butler.ToolGateway
	// Mode is the startup probe's verdict (ADR-W006 A4 / ADR-W017 A4):
	// ButlerModeQAOnly runs sessions with the read-only tool set and
	// dispatch-level refusal of propose_change.
	Mode runtimedb.ButlerSessionMode
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
	if s.ButlerHost == nil || s.ButlerHost.Provider == nil || s.ButlerHost.Gateway == nil {
		return nil, http.StatusServiceUnavailable,
			fmt.Errorf("butler provider not configured (start keystone with -butler-provider)")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAskBytes+1))
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("reading body: %w", err)
	}
	if len(body) > maxAskBytes {
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("message exceeds %d bytes", maxAskBytes)
	}
	var req askRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing ask: %w", err)
	}
	if req.Message == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("message required")
	}

	host := s.ButlerHost
	session := &butler.Session{
		Gateway:  host.Gateway,
		ReadOnly: host.Mode == runtimedb.ButlerModeQAOnly,
	}
	tr, err := session.Run(r.Context(), host.Provider, req.Message)
	if err != nil {
		// The provider failed mid-session; nothing durable happened (no
		// proposals are submitted until the session completes).
		return nil, http.StatusBadGateway, fmt.Errorf("butler session: %w", err)
	}

	// Submit every accepted proposal through the ONE proposal pipeline
	// (ADR-W017 A3), as principal "butler" acting-for the caller. Policy
	// runs again here with the real engine — the session's own gate is a
	// pre-check, not the authority.
	butlerID := principal.Identity{Name: butlerPrincipal, Role: runtimedb.RoleButler, ActingFor: id.Name}
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
		if rec, ok := body.(runtimedb.PlanRecord); ok {
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
		ActingFor:      id.Name,
		Provider:       tr.Audit.Provider,
		Mode:           host.Mode,
		FinalText:      finalText,
		Capped:         tr.CappedIterations,
		TranscriptJSON: redact.String(string(transcriptJSON)),
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("butler.session", "", principal.Identity{Name: butlerPrincipal, ActingFor: id.Name},
		fmt.Sprintf("session %s: %d tool calls, %d proposals", sessionID, len(tr.Audit.Calls), len(planIDs)))

	return askResponse{
		SessionID:      sessionID,
		Mode:           string(host.Mode),
		FinalText:      finalText,
		Capped:         tr.CappedIterations,
		PlanIDs:        planIDs,
		ProposalErrors: proposalErrs,
	}, http.StatusOK, nil
}

func (s *Server) listButlerSessions(r *http.Request, _ principal.Identity) (any, int, error) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	sessions, err := s.DB.ListButlerSessions(limit)
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
