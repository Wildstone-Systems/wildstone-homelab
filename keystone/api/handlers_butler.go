package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
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
	// Butler names which specialist to ask (ADR-W031 A11). The Console
	// holder owns every Butler (A10), so naming one is a choice they
	// already hold rather than an escalation.
	//
	// Empty is answered by the ONLY Butler this caller may talk to on
	// this surface, and refused when there is more than one. Picking for
	// them would be picking which model sees what they typed.
	Butler string `json:"butler,omitempty"`
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
	message, named, st, err := readAskMessage(r)
	if err != nil {
		return nil, st, err
	}
	if s.butlersConfigured() {
		// The person the caller acts for, as below — a turn is had by a
		// Person (ADR-W033 B10), and this branch used the credential's
		// own name while the one below it did not.
		return s.askNamedButler(r.Context(), message, named, actingMember(id), actingAsOwner(id), nil)
	}
	// The unconfigured check lives in the session runner, with the one
	// snapshot it takes — checking here as well would read the backend
	// twice for one request, which is the thing ADR-W017 A9's snapshot
	// exists to prevent.
	// actingMember, not id.Name. The turn is had by the PERSON the
	// caller is acting for — a Console session names one — and what it
	// writes says so: its memory, its work and its notices all name a
	// Person since 0043. Passing the principal's own name here is how
	// `root-admin` ended up owning household memory (#240).
	return s.runAdminButlerSession(r.Context(), message, actingMember(id), butlerlive.ConsoleConnection, nil)
}

// askOneButler answers as ONE named Butler, on whichever surface that
// Butler lives.
//
// It exists because the two ask endpoints are doors, and a door sets a
// ceiling: /butler/ask pins RequireSurface to admin and
// /household/butler/ask to family, so asking a household Butler through
// the operator's door is refused — deliberately shaped as "no butler
// answers there for you" so a family adapter cannot map the operator's
// roster by probing names.
//
// That shaping is right for a credential pinned to a surface and wrong
// for the Console holder, who holds both (A10) and is naming a Butler
// rather than arriving through a bot. So this route names the Butler in
// the path and leaves RequireSurface empty: the ceiling comes from the
// Butler's own record, which is where it was always decided.
func (s *Server) askOneButler(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Message string `json:"message"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if strings.TrimSpace(req.Message) == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("a question needs a message")
	}
	if !s.butlersConfigured() {
		return nil, http.StatusServiceUnavailable,
			fmt.Errorf("no butler is configured on this deployment")
	}
	return s.runNamedButlerSession(r.Context(), butlerlive.Turn{
		Butler: name,
		// The person the caller acts for, not the credential's own
		// name — see the note on askButler above (#240).
		Member:     actingMember(id),
		Text:       req.Message,
		AsOwner:    actingAsOwner(id),
		Connection: butlerlive.ConnectionScope{Name: butlerlive.ConsoleConnection},
	}, nil)
}

// askNamedButler answers on the operator surface as a named Butler,
// resolving the name when the caller left it out.
func (s *Server) askNamedButler(ctx context.Context, message, named, member string, asOwner bool, prior *turnContext) (any, int, error) {
	if named == "" {
		b, err := s.Butlers.Sole(runtimedb.ButlerSurfaceAdmin, member, asOwner)
		if err != nil {
			// Ambiguity is a 400 and lists the names: the caller can fix
			// it, and telling them how is the difference between a
			// usable endpoint and one they have to read the source of.
			if errors.Is(err, butlerlive.ErrAmbiguousButler) {
				return nil, http.StatusBadRequest, err
			}
			return nil, http.StatusNotFound, fmt.Errorf("no butler answers to you here")
		}
		named = b.Name
	}
	turn := butlerlive.Turn{
		Butler:  named,
		Member:  member,
		Text:    message,
		AsOwner: asOwner,
		// The operator surface's ceiling. A Console caller holds both
		// surfaces, but this ENDPOINT is the operator one, and an
		// endpoint that would answer as a family Butler is an endpoint
		// that merges the two surfaces ADR-W026 A1 separated.
		RequireSurface: runtimedb.ButlerSurfaceAdmin,
		Connection:     butlerlive.ConnectionScope{Name: butlerlive.ConsoleConnection},
	}
	if prior != nil {
		turn.SessionID, turn.History = prior.SessionID, prior.History
	}
	return s.runNamedButlerSession(ctx, turn, prior)
}

// turnContext carries an existing conversation into a session run
// (ADR-W027). A nil *turnContext is a first question, which opens a new
// session; a non-nil one appends to the session it names.
type turnContext struct {
	SessionID string
	History   []provider.Message
}

// historyFrom replays a stored conversation as provider messages. Only
// the user and assistant TEXT crosses over — see butler.Session.History
// for why the tool traffic deliberately does not.
func historyFrom(turns []runtimedb.ButlerTurn) []provider.Message {
	out := make([]provider.Message, 0, len(turns))
	for _, t := range turns {
		if t.Text == "" {
			continue
		}
		out = append(out, provider.Message{Role: t.Role, Text: t.Text})
	}
	return out
}

// continueButlerSession appends a follow-up to a conversation that
// already exists (ADR-W027). It is one endpoint rather than one per
// surface, and that is not a step back from ADR-W026 A1's separation:
// the surface comes from the STORED session, so it is decided by the
// endpoint the conversation started on and cannot be chosen by this
// request. A caller cannot move a household question onto the
// operator's Butler by continuing it here.
//
// Only the person the session was recorded for may continue it. An
// operator can still READ anyone's session — that is what the audit is
// for — but reading a conversation and speaking into it are different
// acts, and the second one is not an audit.
func (s *Server) continueButlerSession(r *http.Request, id principal.Identity) (any, int, error) {
	message, named, st, err := readAskMessage(r)
	if err != nil {
		return nil, st, err
	}
	sess, err := s.DB.GetButlerSession(r.PathValue("id"))
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("butler session not found")
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// Against the PERSON, not the credential. A session is Person +
	// Agent + session_id (ADR-W033 B10), and it is recorded acting-for
	// the person the caller spoke as — so comparing the principal's own
	// name here would refuse a Console holder their own conversation
	// and hand a shared credential everybody else's.
	if sess.ActingFor != actingMember(id) {
		return nil, http.StatusForbidden,
			fmt.Errorf("session %s belongs to %s; a conversation can only be continued by the person it was recorded for", sess.ID, sess.ActingFor)
	}
	prior := &turnContext{SessionID: sess.ID, History: historyFrom(sess.Turns)}
	if s.butlersConfigured() {
		// A follow-up cannot move to a different Butler. butlerlive
		// refuses it at the task, which is where the guarantee belongs —
		// the task snapshotted which Butler it runs as, and honouring a
		// new name here would put one Butler's model in front of
		// material another gathered (ADR-W031 A4).
		//
		// The session's OWN stamp decides which Butler continues it.
		// Before the stamp existed the name had to come from the caller
		// or be resolved from the surface, and resolving it stopped
		// working the moment a surface had two Butlers: Sole() answers
		// ambiguous, and a conversation somebody was in the middle of
		// fails with a message about there being more than one. The
		// session already knows; ask it.
		//
		// A caller naming a DIFFERENT Butler is refused here rather than
		// quietly ignored, because "I sent this to nutrition" and "this
		// went to health" must not both be true of one message.
		if sess.Butler != "" {
			if named != "" && named != sess.Butler {
				return nil, http.StatusConflict, fmt.Errorf(
					"session %s belongs to %s, so it cannot be continued as %s — start a new conversation with %s instead",
					sess.ID, sess.Butler, named, named)
			}
			named = sess.Butler
		}
		if sess.Surface == runtimedb.ButlerSurfaceFamily {
			return s.askNamedFamilyButler(r.Context(), message, named, sess.ActingFor, "", false, actingAsOwner(id), prior,
				butlerlive.ConnectionScope{Name: butlerlive.ConsoleConnection})
		}
		return s.askNamedButler(r.Context(), message, named, sess.ActingFor, actingAsOwner(id), prior)
	}
	if sess.Surface == runtimedb.ButlerSurfaceFamily {
		return s.runFamilySession(r.Context(), message, sess.ActingFor, "", butlerlive.ConsoleConnection, false, prior)
	}
	return s.runAdminButlerSession(r.Context(), message, sess.ActingFor, butlerlive.ConsoleConnection, prior)
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
func (s *Server) runAdminButlerSession(ctx context.Context, message, actingFor, connection string, prior *turnContext) (any, int, error) {
	if note, handled, e := s.prepareSecretInput(actingFor, message, 0); handled {
		if e != nil {
			return nil, http.StatusServiceUnavailable, e
		}
		return askResponse{FinalText: note}, http.StatusOK, nil
	}
	askProv, askGw, askMode, _, _ := s.ButlerHost.Current()
	if askProv == nil || askGw == nil {
		return nil, http.StatusServiceUnavailable,
			fmt.Errorf("butler provider not configured (start keystone with -butler-provider)")
	}
	// The same rule as the named path: a turn is had by a person. See
	// turnHasAPerson.
	//
	// After the configuration check, deliberately. A deployment with no
	// Butler at all has nothing to say about who is asking, and
	// answering "you are not a member" there would send an operator
	// looking for the wrong problem.
	if code, err := s.turnHasAPerson(actingFor); err != nil {
		return nil, code, err
	}
	// A COPY, because askGw is the host's gateway and is shared by
	// every request in flight: binding this person to it would be both
	// a data race and one caller's identity answering for another's.
	// Scoped already exists for exactly that reason, and its authorizer
	// is replaced immediately below.
	gateway := askGw.Scoped(askGw.Authorizer)
	s.bindPersonAuthority(gateway, actingFor, connection)
	session := &butler.Session{
		Gateway:  gateway,
		ReadOnly: askMode == runtimedb.ButlerModeQAOnly,
	}
	if prior != nil {
		session.History = prior.History
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
	planIDs, proposalErrs := s.submitSessionProposals(butlerID, tr.Audit.Calls)

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
	turns := []runtimedb.ButlerTurn{
		// The question is stored too, which it never was before
		// ADR-W027. It goes through redact for the same reason the
		// answer does: it is operator-supplied text that can carry
		// secret-shaped content, and it is about to be persisted.
		{Role: runtimedb.ButlerRoleUser, Text: redact.String(message)},
		{
			Role: runtimedb.ButlerRoleAssistant, Text: finalText, Capped: tr.CappedIterations,
			TranscriptJSON: redact.String(string(transcriptJSON)), PlanIDs: planIDs,
		},
	}
	sessionID, err := s.recordTurn(prior, runtimedb.ButlerSession{
		Principal:         butlerPrincipal,
		ActingFor:         actingFor,
		Provider:          tr.Audit.Provider,
		Mode:              askMode,
		Surface:           runtimedb.ButlerSurfaceAdmin,
		InboundConnection: connection,
		FinalText:         finalText,
		Capped:            tr.CappedIterations,
		TranscriptJSON:    redact.String(string(transcriptJSON)),
		Turns:             turns,
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.noteBuiltInSessionReply(sessionID, connection)
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
// submitSessionProposals puts every accepted proposal a session
// produced through the ONE proposal pipeline (ADR-W017 A3), and returns
// the plan ids alongside the refusals.
//
// Refusals are returned rather than swallowed. A session that tried to
// propose something and was blocked by standing policy is a different
// event from one that proposed nothing, and reporting them identically
// hides the case worth looking at.
//
// It is shared by the legacy single-provider path and the named-Butler
// one so that "what a conversation is allowed to cause" is one
// implementation. The Butler a session ran as does not appear here: it
// scopes the WORK, and the RBAC row that bounds a Butler proposal —
// read and propose, never approve — is the same one either way.
func (s *Server) submitSessionProposals(butlerID principal.Identity, calls []butler.ToolCallRecord) (planIDs, proposalErrs []string) {
	planIDs, proposalErrs, _ = s.submitSessionProposalsWithCalls(butlerID, calls)
	return
}
func (s *Server) submitSessionProposalsWithCalls(butlerID principal.Identity, calls []butler.ToolCallRecord) (planIDs, proposalErrs []string, persisted []butler.ToolCallRecord) {

	for _, call := range calls {
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
			persisted = append(persisted, call)
		}
	}
	return planIDs, proposalErrs, persisted
}

// recordTurn opens a new session or appends to the one prior names,
// and returns the session id either way. Both surfaces persist through
// here so that "a follow-up lands in the conversation it answers" is one
// decision rather than two implementations of it.
func (s *Server) recordTurn(prior *turnContext, sess runtimedb.ButlerSession) (string, error) {
	if prior == nil {
		return s.DB.InsertButlerSession(sess)
	}
	if err := s.DB.AppendButlerTurnsFrom(prior.SessionID, sess.Turns, sess.InboundConnection, sess.InboundConnectionRef); err != nil {
		return "", err
	}
	return prior.SessionID, nil
}

func (s *Server) listButlerSessions(r *http.Request, _ principal.Identity) (any, int, error) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	surface := runtimedb.ButlerSurface(r.URL.Query().Get("surface"))
	if surface != "" && !runtimedb.ValidButlerSurface(surface) {
		return nil, http.StatusBadRequest, fmt.Errorf("unknown surface %q (admin|family)", surface)
	}
	// A Butler filter narrows to the sessions stamped with that name.
	// It is not defaulted from the surface: showing every household
	// Butler's conversations under one Butler's name is the one thing
	// this list must not do.
	sessions, err := s.DB.ListButlerSessions(limit, runtimedb.ButlerSessionFilter{
		Surface: surface, Butler: r.URL.Query().Get("butler"),
	})
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
func readAskMessage(r *http.Request) (message, butlerName string, status int, err error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAskBytes+1))
	if err != nil {
		return "", "", http.StatusBadRequest, fmt.Errorf("reading body: %w", err)
	}
	if len(body) > maxAskBytes {
		return "", "", http.StatusRequestEntityTooLarge, fmt.Errorf("message exceeds %d bytes", maxAskBytes)
	}
	var req askRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", "", http.StatusBadRequest, fmt.Errorf("parsing ask: %w", err)
	}
	if req.Message == "" {
		return "", "", http.StatusBadRequest, fmt.Errorf("message required")
	}
	return req.Message, strings.TrimSpace(req.Butler), http.StatusOK, nil
}
