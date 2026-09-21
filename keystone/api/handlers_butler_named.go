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

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
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

// actingAsOwner reports whether this caller is the Console holder
// acting on Butlers they own (ADR-W031 A10).
//
// The test is the MANAGE verb, not the endpoint. Manage is what creates
// a Butler, assigns its model and edits its access list, so a caller
// holding it can put themselves on any access list in one call — the
// exemption therefore grants no authority that was being withheld, it
// removes a step that could only ever be performed and never refused.
//
// A chat adapter can never satisfy it. Concierge and valet hold
// converse, deliberately and structurally (ADR-W026 A8), so a household
// member's message keeps going through the access list exactly as
// before. That is the half of this that must not move.
func actingAsOwner(id principal.Identity) bool {
	return principal.Allowed(id.Role, principal.VerbManage)
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
	if code, err := s.turnHasAPerson(turn.Member); err != nil {
		return nil, code, err
	}
	if note, handled, e := s.prepareSecretInput(turn.Member, turn.Text, 0); handled {
		if e != nil {
			return nil, http.StatusServiceUnavailable, e
		}
		return askResponse{FinalText: note}, http.StatusOK, nil
	}
	ans, err := s.Butlers.Serve(ctx, turn)
	if err != nil {
		// Serve returns the durable task id ALONGSIDE its error, and
		// that is not an accident: by the time a provider fails, tools
		// may already have run and some of them changed the house.
		// Whatever work exists is kept before the error goes back.
		preserved := s.preserveFailedTurn(turn, prior, ans)
		switch {
		case errors.Is(err, runtimedb.ErrNoButlerAccess),
			errors.Is(err, runtimedb.ErrNoButlerForAddress),
			errors.Is(err, runtimedb.ErrNotFound):
			// Indistinguishable on purpose, following the store's own note:
			// "that is not yours" confirms the Butler exists and roughly
			// what it is for.
			return preserved, http.StatusNotFound, fmt.Errorf("no butler answers there for you")
		case errors.Is(err, runtimedb.ErrButlerUnreachable):
			// A6. 503 rather than 502, and the difference is the whole
			// decision: the Butler is real, it is not answering, and it
			// will answer when its OWN model returns. Nothing retries this
			// onto another one.
			return preserved, http.StatusServiceUnavailable, err
		case errors.Is(err, runtimedb.ErrNotAdmitted):
			return preserved, http.StatusForbidden, err
		default:
			return preserved, http.StatusBadGateway, fmt.Errorf("butler session: %w", err)
		}
	}

	// Proposals go through the ONE pipeline (ADR-W017 A3), as principal
	// "butler" acting-for the person who asked. The Butler's name is not
	// the acting identity: it is a scope on the work, and the RBAC row
	// that bounds a Butler proposal is the one that has always bounded
	// it.
	butlerID := principal.Identity{Name: butlerPrincipal, Role: runtimedb.RoleButler, ActingFor: turn.Member}
	planIDs, proposalErrs, persistedCalls := s.submitSessionProposalsWithCalls(butlerID, ans.Calls)
	if finish, ok := s.Butlers.(interface {
		CompleteProposalTask(string, []string, []butler.ToolCallRecord) (bool, error)
	}); ok && len(planIDs) > 0 && len(proposalErrs) == 0 {
		completed, err := finish.CompleteProposalTask(ans.TaskID, planIDs, persistedCalls)
		if err != nil {
			proposalErrs = append(proposalErrs, "proposal completion could not be recorded: "+err.Error())
		} else if completed {
			completedTask, readErr := s.DB.GetButlerTask(ans.TaskID)
			if readErr != nil {
				return nil, http.StatusInternalServerError, readErr
			}
			ans.FinalText = completedTask.ResultSummary
		}
	}

	if preserve, ok := s.Butlers.(interface {
		PreserveProposalTask(string, []string, []butler.ToolCallRecord) error
	}); ok && len(planIDs) > 0 && len(proposalErrs) > 0 {
		if err := preserve.PreserveProposalTask(ans.TaskID, planIDs, persistedCalls); err != nil {
			proposalErrs = append(proposalErrs, "proposal evidence could not be recorded: "+err.Error())
		}
	}
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
		Principal: butlerPrincipal,
		ActingFor: turn.Member,
		Provider:  ans.Provider,
		Mode:      mode,
		Surface:   ans.Surface,
		// The RESOLVED Butler, not the requested one. The turn may name
		// none at all (the chat path lets the address decide), and a
		// session filed under a name the caller supplied would be a
		// conversation attributed to a Butler that did not hold it.
		Butler:               ans.Butler,
		InboundConnection:    turn.Connection.Name,
		InboundConnectionRef: turn.Connection.Ref,
		FinalText:            finalText,
		Capped:               ans.Capped,
		TranscriptJSON:       redact.String(string(transcriptJSON)),
		Turns: []runtimedb.ButlerTurn{
			{Role: runtimedb.ButlerRoleUser, Text: redact.String(turn.Text), TaskID: ans.TaskID},
			{
				Role: runtimedb.ButlerRoleAssistant, Text: finalText, Capped: ans.Capped,
				TranscriptJSON: redact.String(string(transcriptJSON)), PlanIDs: planIDs,
			},
		},
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if err = s.DB.LinkPlanSessionPrivacy(sessionID, planIDs); err != nil {
		return askResponse{SessionID: sessionID}, http.StatusInternalServerError, err
	}
	// Link the durable task to the conversation it belongs to. The
	// ordering forces this to happen here rather than at creation: the
	// session id only exists once the turn is recorded, and the task had
	// to exist before the work could start.
	//
	// The turn already committed its task handle and privacy provenance.
	// Keep that handle on failure, while withholding the answer until this
	// secondary task/session bookkeeping succeeds.
	if ans.TaskID != "" && sessionID != "" {
		if lerr := s.DB.SetButlerTaskSession(ans.TaskID, sessionID); lerr != nil {
			s.Log.Error("linking butler task to its conversation", "task", ans.TaskID,
				"session", sessionID, "err", lerr)
			return askResponse{SessionID: sessionID, Mode: string(mode), PlanIDs: planIDs, ProposalErrors: proposalErrs}, http.StatusInternalServerError, fmt.Errorf("could not preserve conversation privacy provenance")
		}
	}
	// Inbound provenance was committed with the turn. Built-in synchronous
	// replies also leave here; external chat records outbound when queued.
	if turn.Connection.Ref == 0 {
		s.noteBuiltInSessionReply(sessionID, turn.Connection.Name)
	}
	// The activity line names the Butler and the model. "Which model saw
	// this material" is the question ADR-W031 A2 exists to answer by
	// name, and an answer that lives only in the process's memory is not
	// one anybody can ask later.
	s.event("butler.session", "", principal.Identity{Name: butlerPrincipal, ActingFor: turn.Member},
		fmt.Sprintf("session %s as %s on %s (%s): %d tool calls, %d proposals, task %s",
			sessionID, ans.Butler, ans.Model, ans.ModelKind, len(ans.Calls), len(planIDs), ans.TaskID))

	return askResponse{
		SessionID:      sessionID,
		Mode:           string(mode),
		FinalText:      finalText,
		Capped:         ans.Capped,
		PlanIDs:        planIDs,
		ProposalErrors: proposalErrs,
	}, http.StatusOK, nil
}

// preserveFailedTurn keeps the work a failed turn has already done, and
// returns a handle the next turn can resume it through.
//
// Serve hands back the durable task id alongside its error on purpose.
// A provider can fail after several tool calls have completed, and some
// of those calls changed the house — a door unlocked, a light switched,
// a message sent. Returning the error and dropping the answer leaves
// that task attached to no conversation at all, and then the next
// message on the thread (an adapter retry, or the person simply saying
// "continue") opens a FRESH task and runs those same calls a second
// time. Unlocking a door twice is not the same as unlocking it once.
//
// Recording the turn is what fixes that: it gives the task a
// conversation to belong to, and ButlerTaskForSession is how the next
// turn finds a task still in flight and resumes it instead of starting
// over. The transcript keeps whichever calls did complete, so what
// happened before the failure is answerable afterwards rather than only
// inferable from the house.
//
// A failure to preserve is logged, never returned. The caller is already
// on its way back with the provider's error, and replacing that with a
// bookkeeping error would hide the thing that actually went wrong.
func (s *Server) preserveFailedTurn(turn butlerlive.Turn, prior *turnContext, ans butlerlive.Answer) any {
	// No task means the turn failed before any work could start —
	// an unresolved Butler, a refused admission, a client that would
	// not build. There is nothing to keep.
	if ans.TaskID == "" {
		return nil
	}
	// An accepted propose_change is submitted even though the turn
	// failed, for the same reason as on the success path: a proposal is
	// not an action. It lands as a plan record in state `proposed` and
	// a human approve verb is still the only way forward (ADR-W017 A3).
	//
	// The alternative is losing it. ResourceYAML lives on the call and
	// not in the stored transcript, and the resume path reconstructs
	// only the tool RESULT — so a proposal carried through a failed turn
	// arrives at the next successful one with nothing to submit, and the
	// plan the person asked for disappears without anybody being told.
	butlerID := principal.Identity{Name: butlerPrincipal, Role: runtimedb.RoleButler, ActingFor: turn.Member}
	planIDs, proposalErrs, persistedCalls := s.submitSessionProposalsWithCalls(butlerID, ans.Calls)
	if finish, ok := s.Butlers.(interface {
		PreserveProposalTask(string, []string, []butler.ToolCallRecord) error
	}); ok && len(planIDs) > 0 {
		if err := finish.PreserveProposalTask(ans.TaskID, planIDs, persistedCalls); err != nil {
			s.Log.Error("preserving failed turn proposal evidence", "task", ans.TaskID, "err", err)
			proposalErrs = append(proposalErrs, "proposal evidence could not be recorded: "+err.Error())
		}
	}

	stored := storedTranscript{Provider: ans.Provider, PlanIDs: planIDs}
	for _, call := range ans.Calls {
		stored.Calls = append(stored.Calls, transcriptRecord{
			Name: call.Name, Input: call.Input, Result: call.Result,
			Error: call.Error, Rejected: call.Rejected,
		})
	}
	transcriptJSON, merr := json.Marshal(stored)
	if merr != nil {
		s.Log.Error("encoding transcript for a failed turn", "task", ans.TaskID, "err", merr)
		return nil
	}
	mode := runtimedb.ButlerModeFull
	if ans.ReadOnly {
		mode = runtimedb.ButlerModeQAOnly
	}
	// The assistant turn is deliberately absent: the Butler did not
	// answer, and writing an empty one would put words in its mouth for
	// whoever reads the thread later.
	//
	// The transcript rides on the TURN, not only on the session. A
	// follow-up appends turns to an existing session and never touches
	// the session-level fields, so a transcript set only there would
	// survive a first turn and vanish on every later one — and this
	// function would be claiming, in its own activity line, to have kept
	// calls that were not written down anywhere.
	failed := redact.String(string(transcriptJSON))
	sessionID, serr := s.recordTurn(prior, runtimedb.ButlerSession{
		Principal:            butlerPrincipal,
		ActingFor:            turn.Member,
		Provider:             ans.Provider,
		Mode:                 mode,
		Surface:              ans.Surface,
		Butler:               ans.Butler,
		InboundConnection:    turn.Connection.Name,
		InboundConnectionRef: turn.Connection.Ref,
		TranscriptJSON:       failed,
		Turns: []runtimedb.ButlerTurn{
			// planIDs ride on the turn so the Console can show the
			// proposal next to the question that caused it, the same
			// way a successful assistant turn carries its own.
			{Role: runtimedb.ButlerRoleUser, Text: redact.String(turn.Text), TaskID: ans.TaskID,
				TranscriptJSON: failed, PlanIDs: planIDs},
		},
	})
	if serr != nil || sessionID == "" {
		if serr != nil {
			s.Log.Error("recording a failed turn", "task", ans.TaskID, "err", serr)
		}
		return nil
	}
	if lerr := s.DB.LinkPlanSessionPrivacy(sessionID, planIDs); lerr != nil {
		s.Log.Error("linking Plan privacy", "err", lerr)
		return askResponse{SessionID: sessionID}
	}
	if lerr := s.DB.SetButlerTaskSession(ans.TaskID, sessionID); lerr != nil {
		s.Log.Error("linking a failed turn's task to its conversation",
			"task", ans.TaskID, "session", sessionID, "err", lerr)
		return askResponse{SessionID: sessionID, Mode: string(mode), PlanIDs: planIDs, ProposalErrors: proposalErrs}
	}
	s.event("butler.session.failed", "", principal.Identity{Name: butlerPrincipal, ActingFor: turn.Member},
		fmt.Sprintf("session %s kept task %s after a failure: %d tool calls already ran, %d proposals",
			sessionID, ans.TaskID, len(ans.Calls), len(planIDs)))
	return askResponse{
		SessionID: sessionID, Mode: string(mode),
		PlanIDs: planIDs, ProposalErrors: proposalErrs,
	}
}

// turnHasAPerson refuses a Butler turn that speaks for nobody.
//
// A turn can write memory, file work and raise a notice, and since
// migration 0043 every one of those names a Person rather than whatever
// string the caller authenticated as. A bare machine token — root-admin,
// op, an adapter's own principal — has no person behind it, so the write
// would fail on a foreign key several layers down, with an error that
// says nothing about what to do.
//
// This is the write path half of #240: the schema now refuses to record
// a principal where a person belongs, and the endpoint says so where the
// caller can act on it. It deliberately checks the resolved member
// rather than requiring ActingFor outright, so a deployment whose
// administrator IS a Person on record keeps working.
func (s *Server) turnHasAPerson(member string) (int, error) {
	if member == "" {
		return http.StatusBadRequest, fmt.Errorf("a butler turn is had by somebody; this one names nobody")
	}
	m, err := s.DB.GetButlerMember(member)
	switch {
	case errors.Is(err, runtimedb.ErrNotFound):
		return http.StatusBadRequest, fmt.Errorf(
			"%q is not a household member, and a butler turn is had by a person: "+
				"call with a token that acts for one, or admit %q first (ADR-W030 A2.1)", member, member)
	case err != nil:
		return http.StatusInternalServerError, err
	}
	// ADMITTED, not merely on record. A row exists for somebody who was
	// disabled and for somebody who was offboarded, and design §10.1
	// says a person who has lost authority may not have work done for
	// them — which starts at the prompt, not at the first tool call.
	//
	// Binding the gateway's authority is not enough on its own: it
	// makes the tools refuse, and a model answer carrying no tool call
	// completes perfectly happily. The message has already been sent to
	// the provider by then, which on a cloud route means a revoked
	// person's words left the house. Found by a review bot on #286.
	//
	// The Console door already re-derives standing per request, so this
	// is not the only guard for a Console caller — but a chat adapter
	// or any other machine principal acting for a member does not go
	// through that door, and this is the check on the path they take.
	if !m.Active() {
		return http.StatusForbidden, fmt.Errorf(
			"%q is no longer admitted to the Butler, so no work can be done for them (ADR-W030 A2.1)",
			member)
	}
	return 0, nil
}
