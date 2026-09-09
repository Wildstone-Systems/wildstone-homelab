package api

// Serving a conversation as a NAMED Butler (ADR-W031), on the two paths
// a person actually talks on: a chat adapter's inbound door, and the
// Console's ask endpoint.
//
// Everything below is a decision about WHICH Butler, and every one of
// them is answered without reading the message. That constraint is not
// decoration — A2 withdrew the model router precisely because anything
// that decides from content must first receive the content, so a
// selection step is the disclosure it was meant to prevent.
//
//   - On chat, the receiving bot decides. Its address comes from the
//     adapter's own Keystone credential, never from the request body.
//   - On the Console, the holder decides, by naming one. A10 gives them
//     every Butler, so naming one is a choice they already hold.
//   - Nowhere does anything infer a Butler from what was said.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// butlerAddressFor returns the address a message arriving on this
// credential was sent TO (ADR-W031 A7: "the bot token IS the address").
//
// A chat adapter's principal is minted as chat-<capability>, so the
// capability — and with it the bot — is recoverable from the credential
// that authenticated the call. Nothing in the request body contributes,
// which is the property that matters: a field an adapter fills in is a
// field a compromised adapter can fill in with another Butler's name.
//
// A human caller holding read is not a bot and has no address. They
// name a Butler instead, which the Console path does.
func butlerAddressFor(id principal.Identity) (string, bool) {
	capability, ok := strings.CutPrefix(id.Name, chatAdapterPrincipalPrefix)
	if !ok || capability == "" {
		return "", false
	}
	return capability, true
}

// butlerTargetFor decides which Butler a chat message is addressed to,
// without reading the message.
//
// An adapter gets no say. Its own credential is the address (A7), and
// naming a Butler in the body is refused rather than ignored: silently
// dropping a field an adapter set would mean a misconfigured bot
// answering as the wrong Butler and looking correct.
//
// A read-holding human — the Console holder, or `wild` exercising the
// contract end to end — has no bot to arrive through, so they name one.
// That is not an escalation: A10 gives the Console holder every Butler.
func butlerTargetFor(id principal.Identity, requested string) (address, named string, err error) {
	if addr, isAdapter := butlerAddressFor(id); isAdapter {
		if requested != "" {
			return "", "", fmt.Errorf(
				"a chat adapter cannot name a butler; the bot a message arrives at is the address (ADR-W031 A7)")
		}
		return addr, "", nil
	}
	if !principal.Allowed(id.Role, principal.VerbRead) {
		return "", "", fmt.Errorf("this credential cannot name a butler")
	}
	if requested == "" {
		return "", "", fmt.Errorf(
			"name a butler: this credential is not a chat adapter, so there is no address to resolve one from")
	}
	return "", requested, nil
}

// butlersConfigured reports whether this deployment has any Butler.
//
// It is the ONE question whose answer may send a conversation down the
// legacy -butler-provider path, and it is deliberately a fact about the
// DEPLOYMENT rather than about the message. A deployment upgrading into
// ADR-W031 has no Butlers on its first boot and must keep answering,
// or the upgrade silently goes mute. Once any Butler exists, an address
// with no Butler behind it is refused rather than quietly answered by a
// different model.
//
// A read failure is treated as "there are Butlers": erring toward
// refusing is the safe direction when the alternative is answering on a
// model nobody assigned.
func (s *Server) butlersConfigured() bool {
	if s.Butlers == nil {
		return false
	}
	any, err := s.Butlers.Any()
	if err != nil {
		s.Log.Error("counting butlers; treating the deployment as having them", "err", err)
		return true
	}
	return any
}

// runNamedButlerSession serves one turn as a named Butler and shapes it
// into the same askResponse every other conversational path returns.
//
// Everything after the run is deliberately the SHARED pipeline: the
// same proposal submission, the same redaction, the same session
// record. A named Butler is a different answer to "which model", not a
// second way of recording what happened — two audit paths would mean
// two things to keep true.
func (s *Server) runNamedButlerSession(ctx context.Context, turn butlerlive.Turn, prior *turnContext) (any, int, error) {
	ans, err := s.Butlers.Serve(ctx, turn)
	switch {
	case err == nil:
	case errors.Is(err, runtimedb.ErrNoButlerAccess),
		errors.Is(err, runtimedb.ErrNoButlerForAddress),
		errors.Is(err, runtimedb.ErrNotFound):
		// Indistinguishable on purpose, following the store's own note:
		// "that is not yours" confirms the Butler exists and roughly
		// what it is for.
		return nil, http.StatusNotFound, fmt.Errorf("no butler answers there for you")
	case errors.Is(err, runtimedb.ErrButlerUnreachable):
		// A6. 503 rather than 502, and the difference is the whole
		// decision: the Butler is real, it is not answering, and it
		// will answer when its OWN model returns. Nothing retries this
		// onto another one.
		return nil, http.StatusServiceUnavailable, err
	case errors.Is(err, runtimedb.ErrNotAdmitted):
		return nil, http.StatusForbidden, err
	default:
		return nil, http.StatusBadGateway, fmt.Errorf("butler session: %w", err)
	}

	// Proposals go through the ONE pipeline (ADR-W017 A3), as principal
	// "butler" acting-for the person who asked. The Butler's name is not
	// the acting identity: it is a scope on the work, and the RBAC row
	// that bounds a Butler proposal is the one that has always bounded
	// it.
	butlerID := principal.Identity{Name: butlerPrincipal, Role: runtimedb.RoleButler, ActingFor: turn.Member}
	planIDs, proposalErrs := s.submitSessionProposals(butlerID, ans.Calls)

	stored := storedTranscript{Provider: ans.Provider, PlanIDs: planIDs}
	for _, call := range ans.Calls {
		stored.Calls = append(stored.Calls, transcriptRecord{
			Name: call.Name, Input: call.Input, Result: call.Result,
			Error: call.Error, Rejected: call.Rejected,
		})
	}
	transcriptJSON, err := json.Marshal(stored)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("encoding transcript: %w", err)
	}
	finalText := redact.String(ans.FinalText)
	mode := runtimedb.ButlerModeFull
	if ans.ReadOnly {
		mode = runtimedb.ButlerModeQAOnly
	}
	sessionID, err := s.recordTurn(prior, runtimedb.ButlerSession{
		Principal:      butlerPrincipal,
		ActingFor:      turn.Member,
		Provider:       ans.Provider,
		Mode:           mode,
		Surface:        ans.Surface,
		FinalText:      finalText,
		Capped:         ans.Capped,
		TranscriptJSON: redact.String(string(transcriptJSON)),
		Turns: []runtimedb.ButlerTurn{
			{Role: runtimedb.ButlerRoleUser, Text: redact.String(turn.Text)},
			{
				Role: runtimedb.ButlerRoleAssistant, Text: finalText, Capped: ans.Capped,
				TranscriptJSON: redact.String(string(transcriptJSON)), PlanIDs: planIDs,
			},
		},
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// Link the durable task to the conversation it belongs to. The
	// ordering forces this to happen here rather than at creation: the
	// session id only exists once the turn is recorded, and the task had
	// to exist before the work could start.
	//
	// A failure is logged rather than returned. The answer has already
	// been produced and is about to be delivered; losing the link costs
	// the NEXT turn its resume, and dropping the reply would cost this
	// one entirely.
	if ans.TaskID != "" && sessionID != "" {
		if lerr := s.DB.SetButlerTaskSession(ans.TaskID, sessionID); lerr != nil {
			s.Log.Error("linking butler task to its conversation", "task", ans.TaskID,
				"session", sessionID, "err", lerr)
		}
	}
	// The activity line names the Butler and the model. "Which model saw
	// this material" is the question ADR-W031 A2 exists to answer by
	// name, and an answer that lives only in the process's memory is not
	// one anybody can ask later.
	s.event("butler.session", "", principal.Identity{Name: butlerPrincipal, ActingFor: turn.Member},
		fmt.Sprintf("session %s as %s on %s (%s): %d tool calls, %d proposals, task %s",
			sessionID, ans.Butler, ans.Model, ans.RouteKind, len(ans.Calls), len(planIDs), ans.TaskID))

	return askResponse{
		SessionID:      sessionID,
		Mode:           string(mode),
		FinalText:      finalText,
		Capped:         ans.Capped,
		PlanIDs:        planIDs,
		ProposalErrors: proposalErrs,
	}, http.StatusOK, nil
}
