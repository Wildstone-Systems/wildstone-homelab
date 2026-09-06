package api

// The family surface's endpoint (ADR-W026 A1/A2, work package G1).
//
// It is a sibling of /api/v1/butler/ask, not a mode of it. The two
// endpoints build different gateways, run different system prompts,
// record different surfaces, and have different ceilings — and the
// separation is here, at the route, because that is the one place a
// request cannot argue with: the surface is a property of the door you
// came in through, never of the body you sent.
//
// What a family session CANNOT do is worth stating plainly, because
// none of it is a check in this file: it cannot read the Statefile, list
// plans, propose a change, or reach a node, because the gateway it is
// handed holds no capability that does any of those things. There is no
// filter to get wrong.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// householdConfigured reports whether a family surface is wired at all:
// a provider to talk to, and at least one household seam to talk about.
// A deployment with a provider but no household seams has an operator
// butler and no family one, which is a normal state (nothing household
// is installed yet), not an error.
func (h *ButlerHost) householdConfigured() bool {
	if h == nil || h.Provider == nil {
		return false
	}
	return h.Household.Events != nil || h.Household.Actions != nil
}

// householdGateway builds the per-session family gateway for member.
// Per-session rather than shared because the gateway is BOUND to the
// member (ADR-W026 A1/A4): the member it acts for is a construction
// argument, so no tool input, prompt, or injected event can name a
// different one.
func (h *ButlerHost) householdGateway(member string) *butler.HouseholdGateway {
	return butler.NewHouseholdGateway(member, h.Household)
}

// getHouseholdButlerStatus is the family surface's half of the status
// endpoint: whether it is wired, and what it can actually do here. The
// Console renders it so a household member is never shown a chat box
// that cannot answer.
func (s *Server) getHouseholdButlerStatus(_ *http.Request, id principal.Identity) (any, int, error) {
	host := s.ButlerHost
	if !host.householdConfigured() {
		reason := "no Butler provider configured; start keystone with -butler-provider (off|anthropic|openai|ollama)"
		if host != nil && host.Provider != nil {
			reason = "no household surface wired: this deployment has no household timeline or actions configured"
		}
		return butlerStatus{Configured: false, Reason: reason}, http.StatusOK, nil
	}
	gw := host.householdGateway(id.Name)
	specs := gw.OfferedSpecs(host.Mode == runtimedb.ButlerModeQAOnly)
	tools := make([]string, 0, len(specs))
	for _, t := range specs {
		tools = append(tools, t.Name)
	}
	return butlerStatus{
		Configured: true,
		Provider:   host.Provider.Name(),
		Model:      host.Model,
		Endpoint:   redact.String(host.Endpoint),
		Mode:       string(host.Mode),
		Tools:      tools,
	}, http.StatusOK, nil
}

// askHouseholdButler runs one family-surface session for the
// authenticated caller (ADR-W026 A1).
//
// The member the session acts for is the CALLER — the identity Keystone
// already authenticated — and there is no field in the request through
// which a caller could name somebody else. That is deliberate for G1:
// the only callers today are the Console and `wild`, where the caller is
// a human with a Wildstone credential. When a chat adapter becomes a
// caller (work package G2) the member comes from a binding row the
// adapter cannot forge, and it arrives here through the same field, so
// this handler does not change shape.
//
// The route carries the read verb, as the operator's ask does: talking
// is not a write. The one thing a family session can cause — a
// domestic-tier action — is bounded by the domestic gateway's own
// checks, which are the same ones an approved household rule passes
// through (ADR-W024 A5).
func (s *Server) askHouseholdButler(r *http.Request, id principal.Identity) (any, int, error) {
	host := s.ButlerHost
	if !host.householdConfigured() {
		return nil, http.StatusServiceUnavailable,
			fmt.Errorf("household butler not configured (needs -butler-provider and a household surface)")
	}
	message, st, err := readAskMessage(r)
	if err != nil {
		return nil, st, err
	}
	return s.runFamilyButlerSession(r.Context(), message, id.Name, "")
}

// runFamilyButlerSession runs one family-surface session for member and
// returns the ask response. Shared by the HTTP ask endpoint and the chat
// inbound endpoint (ADR-W026 A8).
//
// systemExtra is appended to the family system prompt for the audience
// the session is speaking to — the group-chat note (A7) is the only user
// of it today. It can add a constraint to what the Butler SAYS; it
// cannot add or remove a tool, because the tools come from the gateway
// and the gateway is built from the member.
func (s *Server) runFamilyButlerSession(ctx context.Context, message, member, systemExtra string) (any, int, error) {
	host := s.ButlerHost
	session := &butler.Session{
		Gateway:  host.householdGateway(member),
		ReadOnly: host.Mode == runtimedb.ButlerModeQAOnly,
	}
	if systemExtra != "" {
		session.System = butler.HouseholdSystemPrompt + "\n\n" + systemExtra
	}
	tr, err := session.Run(ctx, host.Provider, message)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("household butler session: %w", err)
	}

	// Persist the session (ADR-W017 A1, unchanged for this surface): the
	// record is principal-bound and carries the surface, so an audit can
	// separate what the household asked for from what the operator did.
	// Nothing here can carry a plan id — the family surface has no
	// propose path to produce one.
	stored := storedTranscript{Provider: tr.Audit.Provider}
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
		ActingFor:      member,
		Provider:       tr.Audit.Provider,
		Mode:           host.Mode,
		Surface:        runtimedb.ButlerSurfaceFamily,
		FinalText:      finalText,
		Capped:         tr.CappedIterations,
		TranscriptJSON: redact.String(string(transcriptJSON)),
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("butler.session", "", principal.Identity{Name: butlerPrincipal, ActingFor: member},
		fmt.Sprintf("household session %s: %d tool calls", sessionID, len(tr.Audit.Calls)))

	return askResponse{
		SessionID: sessionID,
		Mode:      string(host.Mode),
		FinalText: finalText,
		Capped:    tr.CappedIterations,
	}, http.StatusOK, nil
}

// householdFactsFrom flattens runtime-DB household event rows into the
// small shape the family gateway shows a model, newest first. It lives
// in the API package rather than in keystone/butler because that package
// must not learn the runtime DB's row type — the seam exists to keep it
// from doing so.
func householdFactsFrom(rows []runtimedb.HouseholdEvent) []butler.HouseholdFact {
	out := make([]butler.HouseholdFact, 0, len(rows))
	for _, e := range rows {
		out = append(out, butler.HouseholdFact{
			OccurredAt: e.OccurredAt,
			Type:       e.Type,
			Subject:    e.Subject,
			Entity:     e.Entity,
			Detail:     householdDetail(e.Payload),
		})
	}
	return out
}

// householdDetail renders an event payload as one short line. A payload
// is publisher-written text; it is bounded here and enveloped downstream
// like every other untrusted fact.
func householdDetail(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	const maxDetail = 300
	s := string(payload)
	if len(s) <= maxDetail {
		return s
	}
	// Cut on a rune boundary: a payload is publisher-written text and
	// may be any language, and half a rune is invalid UTF-8 rather than
	// a shortened word.
	cut := maxDetail
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// HouseholdEventSeam builds the family surface's timeline reader over
// the runtime DB (ADR-W026 A1). cmd/keystone wires it; it lives here so
// the only query shape a family session can make is written next to the
// endpoint that serves it.
//
// Every lookup is narrowed and bounded before it reaches SQL: a family
// session cannot ask for the whole timeline, because there is no value
// of HouseholdLookup that means "everything" — an absent limit becomes
// the default, and an oversized one is clamped.
func HouseholdEventSeam(db *runtimedb.DB) func(context.Context, butler.HouseholdLookup) ([]butler.HouseholdFact, error) {
	return func(_ context.Context, q butler.HouseholdLookup) ([]butler.HouseholdFact, error) {
		limit := q.Limit
		if limit <= 0 || limit > butler.MaxHouseholdFacts {
			limit = butler.MaxHouseholdFacts
		}
		rows, err := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{
			Type:       q.EventType,
			Subject:    q.Subject,
			SubjectSet: q.Subject != "",
			Limit:      limit,
			Newest:     true,
		})
		if err != nil {
			return nil, err
		}
		return householdFactsFrom(rows), nil
	}
}
