package api

// The chat contract (ADR-W026 A8, work package G3): one inbound
// endpoint, one outbound queue, and nothing vendor-shaped in either.
//
// An adapter's whole job is protocol translation. It posts what it
// heard — transport, account, conversation, text — and polls for what to
// say. It does not choose a surface (its credential does), it does not
// assert an identity (a binding does), and it never sees Keystone state.
//
// The surface is derived from the caller's ROLE, not from the request.
// That is the difference between "two bots by convention" and two bots
// by construction: a stolen family-adapter token cannot open an
// operator session, because the concierge role has no spelling for one.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/household"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// chatFallbackWindow is how long a dedupe key built from message TEXT
// stands for one message (see chatDedupe). Long enough to absorb a
// transport's redelivery, short enough that saying "yes" twice in an
// afternoon is two answers rather than one.
const chatFallbackWindow = 10 * time.Minute

// adminGroupReply is what the operator surface says in a group chat: it
// answers in a direct message and nowhere else (ADR-W026 A7).
const adminGroupReply = "I answer operator questions in a direct message only, never in a group."

// groupRedirectReply is what EVERY Agent now says to a recognised
// Person who addresses it from a room (design §8.3).
//
// IT IS A REDIRECT AND NOT A BARE REFUSAL, because #212 asks for one
// and because the alternative teaches people the bot is broken. The
// household member did nothing wrong; they are in the wrong place, and
// the right place is one message away.
//
// It says nothing about WHY beyond the audience. "I can't see who else
// is here" is true, is the actual reason, and does not describe the
// household's admission model to a room that may contain people who
// are not in it.
const groupRedirectReply = "I only answer in a one-to-one chat — in a group I can't tell who else is reading. " +
	"Message me directly and I'll pick this up there."

type inboundMessage struct {
	Transport string `json:"transport"`
	Account   string `json:"account"`
	Text      string `json:"text"`
	// Conversation is where a reply goes. For a direct message it is the
	// account's own chat; for a group it is the group.
	Conversation string `json:"conversation"`
	// Group marks a shared audience (ADR-W026 A7).
	Group bool `json:"group,omitempty"`
	// Addressed is the adapter's word that a group message was FOR this
	// bot — a mention, or a reply to it (ADR-W033 B9.1). A direct
	// message is addressed by being direct.
	Addressed bool `json:"addressed,omitempty"`
	// MessageID is the transport's own id for this message. It is the
	// dedupe key's source, so a transport that redelivers — Telegram
	// after a lost ack, a websocket reconnect — does not produce two
	// answers.
	MessageID string `json:"messageId,omitempty"`
	// DisplayName is what the transport calls this account, recorded on
	// a binding for the operator's list and used nowhere else.
	DisplayName string `json:"displayName,omitempty"`
	// Surface may be named ONLY by a caller that holds read (a human
	// with the Console or wild, testing the contract). An adapter's
	// surface comes from its role and naming a different one is refused.
	Surface string `json:"surface,omitempty"`
	// Butler may likewise be named ONLY by a read-holding human, and for
	// the same reason: they have no bot to arrive through, so there is
	// no address to resolve one from. An adapter that sets it is
	// refused — its own credential IS the address (ADR-W031 A7), and a
	// bot that could name a Butler could name somebody else's.
	Butler string `json:"butler,omitempty"`
	// Connection picks between that Butler's several bots (B7), by the
	// address's external id, for the same read-holding human. An
	// adapter never sends it either: its credential already IS the
	// connection.
	Connection string `json:"connection,omitempty"`
}

// inboundResponse tells the adapter what happened without telling it
// anything about the household. Queued is the seq of the reply it should
// expect on its outbound cursor.
type inboundResponse struct {
	Accepted bool   `json:"accepted"`
	Bound    bool   `json:"bound"`
	Queued   int64  `json:"queued,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// receiveChatMessage is the adapter's inbound door (ADR-W026 A8).
//
// It answers 202 with the queue position of the reply rather than the
// reply itself: the outbound queue is the single delivery path, so a
// rule's push and a conversation's answer cannot arrive by different
// routes with different retry behaviour.
func (s *Server) receiveChatMessage(r *http.Request, id principal.Identity) (any, int, error) {
	var msg inboundMessage
	if st, err := decodeChatBody(r, &msg); err != nil {
		return nil, st, err
	}
	transport := strings.TrimSpace(msg.Transport)
	account := strings.TrimSpace(msg.Account)
	if transport == "" || account == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("transport and account are required")
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("text is required")
	}
	if len(text) > maxAskBytes {
		// Bounded before anything else looks at it (ADR-W026 A6).
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("message exceeds %d bytes", maxAskBytes)
	}
	at, err := s.connectionForCaller(id, msg)
	if err != nil {
		return nil, http.StatusForbidden, err
	}
	conversation := strings.TrimSpace(msg.Conversation)
	if conversation == "" {
		conversation = account
	}

	// Rate limits are per ACCOUNT, applied before the binding lookup, so
	// an unbound stranger cannot spend the household's provider budget
	// or flood the outbound queue with guidance replies.
	// The bucket key carries the CONNECTION for the same reason the
	// dedupe key does: on Telegram one person's account id is identical
	// across every bot, so without it eight messages to one Agent would
	// spend another Agent's budget and answer a real request with a 429.
	if !s.limiter.allowChat(fmt.Sprintf("%d:%s", at.Seq, account)) {
		return nil, http.StatusTooManyRequests, fmt.Errorf("too many messages; slow down")
	}

	// THE ROOM GOES ON RECORD BEFORE ANY OTHER ANSWER, and this is the
	// earliest point it can: it needs the Connection, and the rate
	// limiter above bounds how often an account can cause the write.
	//
	// WHY SO EARLY. This is the ONLY moment anything in Wildstone is
	// told that a conversation is a room -- the adapter says so, and
	// nothing downstream can work it out from a target string. Every
	// return below (unaddressed, unbound, already-claimed, the
	// redirect) is a way of declining to answer, and a first version of
	// this recorded the room after three of them. A review of #290
	// caught it, and the case is not exotic: MOST group traffic is not
	// addressed to the bot, so the unaddressed return is the branch a
	// room is overwhelmingly likely to take first. Until something is
	// addressed to it, chat_rooms would stay empty and a household rule
	// could deliver into a room nobody had recorded.
	//
	// The conversation that was a DIRECT chat yesterday is the case
	// that matters most: a transport can turn one into a group keeping
	// the same id, a binding still names it, and ChatRecipientFor would
	// hand it to a rule as a person's private destination.
	//
	// IT FAILS CLOSED. Logging and carrying on would answer the person
	// while leaving the room unrecorded, which is the state the whole
	// outbound guard depends on not existing. A 500 asks the adapter to
	// redeliver, and redelivery is safe here: nothing has been claimed,
	// no session exists, and noting a room is idempotent.
	if msg.Group {
		if rerr := s.DB.NoteChatRoom(at.Seq, conversation); rerr != nil {
			s.Log.Error("noting a chat room", "err", rerr)
			return nil, http.StatusInternalServerError,
				fmt.Errorf("could not record that this conversation is a room, so nothing was answered")
		}
	}

	// In a group, only what is addressed to the bot is the bot's to act
	// on (ADR-W033 B9.1). The adapter drops the rest; this is the
	// backstop for one that does not, and it answers nothing.
	//
	// AFTER the room is recorded, deliberately. See above.
	if msg.Group && !msg.Addressed {
		return inboundResponse{Accepted: false, Reason: "a group message not addressed to this bot"},
			http.StatusAccepted, nil
	}

	// Claim this message before anything acts on it (ADR-W026 A8). The
	// outbound dedupe key protects the REPLY; everything before it — the
	// provider call, the session record, a domestic action that notifies
	// a person — happens first and would happen twice on a redelivery.
	// This is the automation engine's claim-before-dispatch shape
	// (ADR-W024 H4), and the same deliberate trade: at-most-once, since
	// a duplicated action is worse than a missed one.
	// Who is this? Asked BEFORE the message is claimed, so a stranger's
	// message costs a lookup and nothing else: no claim row, no session,
	// no reply (ADR-W033 B9.1). What the household keeps is a note that
	// somebody tried — at most one per account per ten minutes, and a
	// bot being flooded by many accounts stops being noted at all past
	// its own budget, so a flood cannot grow the database either.
	binding, err := s.DB.LookupChatBinding(at, account)
	if err != nil {
		// TWO DIFFERENT EVENTS, ONE IDENTICAL SILENCE.
		//
		// A stranger and a household member their Connection turns
		// away are both refused before inference, and outwardly they
		// must look the same: telling the second one why would tell
		// the first one that the account they guessed exists.
		//
		// INWARDLY they are not the same event at all, and recording
		// both as chat.unknown_account was a false audit record -- it
		// says nobody introduced the account, about somebody who is on
		// record, whose binding is sitting right there, and who was
		// turned away by a demotion or a narrowing somebody made on
		// purpose. An operator reading that goes looking for a
		// revocation that never happened, which is the exact failure
		// ErrNotAdmitted was introduced to end. A review of #289
		// caught it discarding the sentinel at the one place operators
		// actually look.
		//
		// The refusal carries its reason, which the sentinel's wrapper
		// already spells out: which ceiling, and what they would have
		// to change.
		switch {
		case errors.Is(err, runtimedb.ErrNotAdmitted):
			// Not rate-limited the way a stranger's note is. This one
			// is bounded by the household's own size: it needs a live
			// binding for an admitted Person, so it cannot be produced
			// by anybody who is not already on record.
			s.recordEvent("chat.admission_refused", at.ExternalID,
				"a household member's message was refused by the connection; nothing was said back",
				map[string]string{
					"account": account, "agent": at.Butler, "reason": err.Error(),
				})
		default:
			if s.limiter.take(fmt.Sprintf("\x00unknown:%d", at.Seq), unknownBurst, unknownPerSec) &&
				s.limiter.take(fmt.Sprintf("\x00unknownnote:%d:%s", at.Seq, account), 1, 1.0/600) {
				s.recordEvent("chat.unknown_account", at.ExternalID,
					"a message from an account nobody introduced; nothing was said back",
					map[string]string{"account": account, "agent": at.Butler})
			}
		}
		return inboundResponse{Accepted: true, Bound: false,
			Reason: "no admission for this account on this connection"}, http.StatusAccepted, nil
	}

	claimKey := chatDedupe(at, conversation, msg.MessageID, "inbound", text)
	claimed, priorSeq, err := s.DB.ClaimChatInbound(claimKey, at.Surface)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if !claimed {
		// Already handled: point the adapter at the answer it was given
		// rather than producing a second one.
		return inboundResponse{Accepted: true, Bound: true, Queued: priorSeq,
			Reason: "already handled; the reply is on the queue"}, http.StatusAccepted, nil
	}
	// A session that FAILS produced no reply and no side effects worth
	// protecting, so the claim goes back and a retry can work.
	settled := false
	defer func() {
		if !settled {
			if rerr := s.DB.ReleaseChatInbound(claimKey); rerr != nil {
				s.Log.Error("releasing inbound chat claim", "err", rerr)
			}
		}
	}()

	// GROUP AND MULTI-RECIPIENT DESTINATIONS FAIL CLOSED (design §8.3).
	//
	// This replaces ADR-W026 A7's split, where the operator surface
	// refused and the family surface answered with an audience note in
	// its prompt. The design supersedes it, and says why: "Person
	// admission authenticates the SPEAKER, not everyone who will
	// receive the reply... 'The sender is known' is not 'the audience
	// is known'."
	//
	// The note it replaces was an instruction to a model to be
	// discreet. It was a good instruction and it is not a boundary: a
	// recognised Person could pull their own Personal memory into a
	// room of unadmitted participants simply by asking for it, and
	// every disclosure rule below would have been enforced against the
	// wrong set of people. Refusal is the MVP answer until audience
	// admission exists to make it safe, which is why §8.3 keeps group
	// support as a seam rather than a feature.
	//
	// BEFORE THE ROOM IS RECORDED AND BEFORE ANY SESSION. No provider
	// call, no context assembled, no memory read -- the refusal is the
	// whole of the turn.
	if msg.Group {
		// The room is already on record -- noted above, before every
		// return that could have skipped it.
		body, reason := groupRedirectReply, "this agent answers in a one-to-one chat only (design §8.3)"
		if at.AdmitsOwnersOnly() {
			// An Owner Agent keeps its own wording. It said this
			// before the design did, and "operator questions" is more
			// use to the person reading it than the general answer.
			body, reason = adminGroupReply, "the operator surface answers in direct messages only"
		}
		seq := s.queueChatReply(at, conversation, body, binding.ID, "",
			chatDedupe(at, conversation, msg.MessageID, "group-redirect", text))
		s.settleInbound(claimKey, seq, &settled)
		return inboundResponse{Accepted: true, Bound: true, Queued: seq,
			Reason: reason}, http.StatusAccepted, nil
	}

	// Is this an ANSWER to something a household rule asked (ADR-W024
	// H4, ADR-W026 A8)? If so it is a fact, not a conversation: it goes
	// onto the household timeline correlated to the rule that asked, and
	// gets a short acknowledgement. No model is involved, so the loop a
	// rule opens still closes with the Butler switched off — the
	// deterministic-backbone principle, applied to the one place chat
	// could quietly have broken it.
	if !at.AdmitsOwnersOnly() {
		if body, answered := s.recordChatAnswer(at, binding, conversation, text); answered {
			seq := s.queueChatReply(at, conversation, body, binding.ID, "",
				chatDedupe(at, conversation, msg.MessageID, "answer", text))
			s.settleInbound(claimKey, seq, &settled)
			s.touchBinding(binding, conversation, msg.Group)
			return inboundResponse{Accepted: true, Bound: true, Queued: seq,
				Reason: "recorded as the answer to a household question"}, http.StatusAccepted, nil
		}
	}

	// Slash commands are answered here, deterministically: no model
	// runs and nothing is proposed. An exact question deserves an exact
	// answer, and these keep working when the Butler is off or the
	// startup probe put it in qa-only.
	commandCtx := chatCommandCtx{
		At:           at,
		Conversation: conversation,
		Identity:     binding.Identity,
		Binding:      binding,
		Group:        msg.Group,
	}
	if body, reason, handled := s.runChatCommand(commandCtx, text); handled {
		seq := s.queueChatReply(at, conversation, body, binding.ID, "",
			chatDedupe(at, conversation, msg.MessageID, "command", text))
		s.settleInbound(claimKey, seq, &settled)
		s.touchBinding(binding, conversation, msg.Group)
		return inboundResponse{Accepted: true, Bound: true, Queued: seq,
			Reason: reason}, http.StatusAccepted, nil
	}

	// The session runners take their own snapshot of the backend and
	// answer 503 when none is configured (ADR-W017 A9); reading it here
	// as well would be a second read of shared state for one request.

	// A conversation continues rather than restarting (ADR-W027 A11).
	// Without this every message opened a new session, so asking "why?"
	// after an answer reached a Butler that had never heard the first
	// question — the multi-turn work landed for the Console and left
	// this path passing nil.
	prior, continuationReply, cerr := s.resolveChatContinuation(commandCtx)
	if cerr != nil {
		return nil, http.StatusInternalServerError, cerr
	}
	if continuationReply != "" {
		seq := s.queueChatReply(at, conversation, continuationReply, binding.ID, "",
			chatDedupe(at, conversation, msg.MessageID, "session-choice", text))
		s.settleInbound(claimKey, seq, &settled)
		s.touchBinding(binding, conversation, msg.Group)
		return inboundResponse{Accepted: true, Bound: true, Queued: seq,
			Reason: "conversation continuation needs a safe choice"}, http.StatusAccepted, nil
	}

	var (
		out any
		st  int
	)
	// Which Butler answers is decided HERE, from the address the message
	// arrived at, and the legacy single-provider path is taken only when
	// this deployment has created no Butlers at all (ADR-W031 A2).
	//
	// The fallback reads deployment state and never the message. That
	// distinction is the whole of A2: a per-message fallback would have
	// to look at something about the message to decide, and the looking
	// is the disclosure. "Has anyone made a Butler yet" is answerable
	// before a single byte of what was said is examined.
	if s.butlersConfigured() {
		address, named, terr := butlerTargetFor(id, strings.TrimSpace(msg.Butler))
		if terr != nil {
			return nil, http.StatusForbidden, terr
		}
		turn := butlerlive.Turn{
			Transport: transport,
			Address:   address,
			// The Connection the message arrived through, named so the
			// authority seam has one to carry (§10.1). No Permits: a
			// chat Connection's ceiling is its Agent's surface, which
			// RequireSurface below already enforces.
			Connection: butlerlive.ConnectionScope{Name: at.Name(), Ref: at.Seq},
			Butler:     named,
			Member:     binding.Identity,
			Text:       text,
			Group:      msg.Group,
			// Nothing to add. The one caller that set this was the
			// group-audience note, and a group never reaches here any
			// more (design §8.3).
			SystemExtra:    "",
			RequireSurface: at.Surface,
		}
		if prior != nil {
			turn.SessionID, turn.History = prior.SessionID, prior.History
		}
		out, st, err = s.runNamedButlerSession(r.Context(), turn, prior)
	} else if !at.AdmitsOwnersOnly() {
		out, st, err = s.runFamilySession(r.Context(), text, binding.Identity, "", at.Name(), msg.Group, prior)
	} else {
		out, st, err = s.runAdminButlerSession(r.Context(), text, binding.Identity, at.Name(), prior)
	}
	if err != nil {
		// A turn can fail after tools have already run. When it did,
		// runNamedButlerSession kept the durable task and gave us the
		// session holding it — so point the thread at that session
		// before the error goes back. Without this the pointer stays
		// where it was, the next message opens a fresh task, and the
		// tool calls that already completed run a second time.
		if ask, ok := out.(askResponse); ok && ask.SessionID != "" {
			if perr := s.DB.SetChatThreadSession(
				at, conversation, binding.Identity, ask.SessionID); perr != nil {
				s.Log.Error("recording chat thread for a failed turn",
					"session", ask.SessionID, "err", perr)
			}
			// The same turn may also have tried to propose something.
			// In the Console the person sees the outcome in their plan
			// list; here the reply IS the surface, so returning only an
			// error leaves them with nothing.
			//
			// BOTH outcomes are told, and the refusal matters more than
			// the success. A plan that was accepted sits in the queue
			// and can be approved late; a proposal that was REFUSED is
			// gone. ResourceYAML lives on the tool call and never
			// reaches the stored transcript, and the resume path
			// reconstructs only the tool result — so the next turn has
			// nothing left to submit, and a refusal nobody mentioned is
			// a change the person asked for that silently never
			// happened (ADR-W017 A3).
			//
			// The claim is deliberately NOT settled, because this is a
			// notice and not the answer: the question is still
			// unanswered and the next attempt at it — a redelivery, or
			// the person asking again — must not be suppressed as a
			// duplicate. The thread pointer set just above means that
			// attempt resumes the session holding the work rather than
			// proposing it a second time. Note that the bundled adapter
			// does not itself redeliver a failed relay (it advances its
			// cursor first, then apologises), which is why this notice
			// is queued whether or not anything comes back — see #232.
			//
			// Deduplicated on the message AND the outcome, so a
			// redelivery of this question announces once while a later
			// question that ends the same way is still announced.
			if len(ask.PlanIDs) > 0 || len(ask.ProposalErrors) > 0 {
				s.queueChatReply(at, conversation,
					chatProposalNotice(ask.PlanIDs, ask.ProposalErrors), binding.ID, "",
					chatProposalDedupe(
						chatDedupe(at, conversation, msg.MessageID, "proposal", text),
						ask.PlanIDs, ask.ProposalErrors))
			}
		}
		return nil, st, err
	}
	s.touchBinding(binding, conversation, msg.Group)

	ask, _ := out.(askResponse)
	if ask.SessionID != "" {
		if err := s.DB.NoteButlerSessionConnection(ask.SessionID, "outbound", at.Name(), at.Seq); err != nil {
			s.Log.Error("recording session outbound connection", "session", ask.SessionID, "err", err)
		}
		if err := s.DB.SetChatThreadSession(
			at, conversation, binding.Identity, ask.SessionID); err != nil {
			// The answer already happened and is about to be sent. A
			// thread pointer that failed to save costs the NEXT message
			// its context; dropping this reply would cost this one
			// entirely.
			s.Log.Error("recording chat thread failed", "err", err)
		}
	}
	body := chatReplyBody(ask)
	seq := s.queueChatReply(at, conversation, body, binding.ID, "",
		chatDedupe(at, conversation, msg.MessageID, ask.SessionID, text))
	s.settleInbound(claimKey, seq, &settled)
	return inboundResponse{Accepted: true, Bound: true, Queued: seq}, http.StatusAccepted, nil
}

// chatThread is the conversation this message continues, or nil for a
// new one.
//
// The stored session is checked against the CALLER before it is
// continued. A pointer row is keyed by transport, conversation and
// surface, but the session it names carries its own acting_for and
// surface, and those are the authority — a thread pointer that has
// drifted (a binding revoked and reissued to somebody else on the same
// chat id) must not hand a stranger the previous person's conversation.
func (s *Server) chatThread(at runtimedb.Connection, conversation, identity string) (*turnContext, bool) {
	id, ok, err := s.DB.ChatThreadSession(at, conversation, identity)
	if err != nil {
		s.Log.Error("reading chat thread failed", "err", err)
		return nil, false
	}
	if !ok {
		return nil, false
	}
	if err := s.DB.ButlerSessionActive(id); err != nil {
		return nil, false
	}
	sess, allowed, err := s.loadChatContinuation(id, at)
	if err != nil {
		return nil, !errors.Is(err, runtimedb.ErrNotFound) && !errors.Is(err, runtimedb.ErrSessionRevoked)
	}
	if !allowed {
		return nil, true
	}
	if sess.ActingFor != identity || sess.Surface != at.Surface ||
		(sess.Butler != "" && sess.Butler != at.Butler) {
		s.Log.Warn("chat thread pointed at another principal's session; starting fresh",
			"session", id, "expected_person", identity, "got_person", sess.ActingFor,
			"expected_agent", at.Butler, "got_agent", sess.Butler)
		return nil, false
	}
	// Age is measured from the conversation's newest turn, on ANY
	// surface. Measuring it from the pointer would call a thread dead
	// that was alive two minutes ago in the Console — the same
	// conversation, carried on somewhere else.
	if last := sess.LastTurnAt(); last.IsZero() || time.Since(last) > s.sessionRecencyWindow() {
		return nil, false
	}
	return &turnContext{SessionID: sess.ID, History: historyFrom(sess.Turns)}, false
}

// chatReplyBody renders a completed session as the message a person
// reads. A proposal is named explicitly with its plan id and where to
// approve it: over chat, "I've made that change" would be a lie, and a
// silent proposal is worse than either (ADR-W026 A5).
func chatReplyBody(ask askResponse) string {
	body := strings.TrimSpace(ask.FinalText)
	if body == "" {
		body = "(no answer)"
	}
	if len(ask.PlanIDs) > 0 {
		body += fmt.Sprintf("\n\nProposed for your approval: %s. Nothing has changed yet — approve it in the Console or with `wild plan approve`.",
			strings.Join(ask.PlanIDs, ", "))
	}
	if len(ask.ProposalErrors) > 0 {
		body += "\n\nOne proposal was refused before it reached the queue."
	}
	if ask.Capped {
		body += "\n\n(I ran out of steps before finishing that one.)"
	}
	return body
}

// chatProposalNotice is what a person is told when the turn failed
// around work it had already tried to propose.
//
// It does not pretend to be an answer — the question is still
// unanswered — and it distinguishes the two outcomes, because they ask
// different things of the person. A plan that is waiting needs a
// decision. A proposal that was refused needs asking again, and saying
// so is the only chance they get: the refused change cannot be
// recovered from the session.
//
// "Ask me again" alone would be advice that does not work, which is
// worse than none. The refusal happened in submitSessionProposals,
// AFTER the gateway accepted the call — so the runner recorded that
// step as verified, and a resumed task serves it straight back from the
// ledger (butler/runner.go, toolCall) without re-running it or
// reconstructing ResourceYAML. Asking again inside this conversation
// therefore resumes the same task, replays the recorded result, and
// queues nothing, while sounding like it worked. /new clears the thread
// pointer, so the next question opens a fresh task whose propose_change
// actually dispatches. See #234 for closing the underlying gap, which
// is that a proposal refused downstream is left standing as a verified
// step.
func chatProposalNotice(planIDs, proposalErrs []string) string {
	waiting := fmt.Sprintf(
		"I didn't get to finish that, but part of it is waiting for you: %s. Nothing has changed yet — approve it in the Console or with `wild plan approve`.",
		strings.Join(planIDs, ", "))
	const refused = "I didn't get to finish that, and a change I tried to propose was refused before it reached the approval queue. " +
		"Nothing has changed, and nothing is waiting. Send /new and ask me again — carrying on in this conversation resumes the same task, which will not attempt that change a second time."
	switch {
	case len(planIDs) == 0:
		return refused
	case len(proposalErrs) == 0:
		return waiting
	default:
		return waiting + "\n\nSomething else I tried to propose was refused before it reached the queue and is not waiting anywhere. Send /new and ask for that part again."
	}
}

// chatProposalDedupe keys the notice above on the message that caused
// it AND the outcome it reported. Both halves are load-bearing, and
// each covers a case the other gets wrong.
//
// messageKey is this message's identity, from chatDedupe — which brings
// the surface, transport and conversation scoping with it, and falls
// back to bucketed text for adapters that send no message id. Without
// it the key is a pure function of the conversation and the outcome, so
// a person who follows this very notice and asks again, and hits the
// same validation refusal, would key identically to the first attempt
// and be told nothing. The dedupe row outlives the conversation, so
// that silence would be permanent — the one path the notice tells them
// to take, and it goes quiet.
//
// The outcome is here because the same message can be delivered twice
// and end differently: the first attempt refused, the resumed second
// one proposing a plan. Keying on the message alone would suppress the
// second, which is the half of the story that needs approving.
//
// Its refusals are hashed rather than spelled out because they are
// provider and validation text: they can be long, they can carry
// whatever the caller sent, and a dedupe key is not a place to put
// either.
func chatProposalDedupe(messageKey string, planIDs, proposalErrs []string) string {
	ids := append([]string(nil), planIDs...)
	sort.Strings(ids)
	refused := ""
	if len(proposalErrs) > 0 {
		errs := append([]string(nil), proposalErrs...)
		sort.Strings(errs)
		sum := sha256.Sum256([]byte(strings.Join(errs, "\x00")))
		refused = "|refused:" + hex.EncodeToString(sum[:8])
	}
	return fmt.Sprintf("proposal:%s:%s%s", messageKey, strings.Join(ids, ","), refused)
}

// queueChatReply enqueues one reply, returning its seq. A queue failure
// is logged rather than returned: the session already happened and is
// already recorded, and telling the adapter the whole request failed
// would have it redeliver a message that has been answered.
func (s *Server) queueChatReply(at runtimedb.Connection, target, body string, bindingID int64, correlationID, dedupe string) int64 {
	seq, err := s.DB.EnqueueChatOutbound(runtimedb.ChatOutbound{
		Connection: at.Seq, Target: target,
		Body: redact.String(body), Kind: "reply", BindingID: bindingID,
		CorrelationID: correlationID, DedupeKey: dedupe,
	})
	if err != nil && seq == 0 {
		s.Log.Error("queuing chat reply failed", "err", err)
		return 0
	}
	return seq
}

// chatDedupe builds a stable key for one logical reply. The transport's
// own message id is the anchor when it gives one; otherwise a hash of
// the text and conversation stands in, which collapses an identical
// message replayed within the window rather than answering it twice.
//
// The SURFACE is part of the key, and has to be. In Telegram a private
// chat's id is the user's own id — the same value for every bot they
// talk to — and message ids are a per-chat sequence, so one person
// messaging both the household bot and the operator bot can produce the
// same (transport, conversation, message id) twice. Without the surface
// here, the second bot's reply would be silently deduplicated away as a
// duplicate of the first's, and the household would watch one of their
// two bots go quiet for no visible reason.
func chatDedupe(at runtimedb.Connection, conversation, messageID, discriminator, text string) string {
	if messageID != "" {
		// Keyed on the CONNECTION rather than on a surface: two Agents
		// on one transport are two conversations, and one message id
		// arriving at each of their bots is two messages.
		return fmt.Sprintf("%d:%s:%s", at.Seq, conversation, messageID)
	}
	// No message id: the text stands in for one, bucketed by time.
	//
	// Without the bucket the key is permanent, so the first "yes" in a
	// conversation would claim that text forever and every later "yes"
	// would be silently treated as a redelivery of it — an adapter that
	// omits message ids would answer each phrase once and then go quiet.
	// Bucketing bounds that to one window: a genuine redelivery inside
	// it collapses, and the same words tomorrow are a new message.
	bucket := time.Now().UTC().Truncate(chatFallbackWindow).Unix()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%s", at.Seq, conversation, discriminator, text)))
	return fmt.Sprintf("%d:%s:h%s-%d", at.Seq, conversation,
		hex.EncodeToString(sum[:8]), bucket)
}

// pollChatOutbound is the adapter's delivery cursor (ADR-W026 A8): the
// messages queued for ITS surface, after the cursor it holds, oldest
// first. The surface comes from the caller's role, so an adapter cannot
// read another surface's queue.
func (s *Server) pollChatOutbound(r *http.Request, id principal.Identity) (any, int, error) {
	at, err := s.pollingConnection(r, id)
	if err != nil {
		return nil, http.StatusForbidden, err
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	waitMs, _ := strconv.Atoi(r.URL.Query().Get("waitMs"))
	messages, err := longPoll(r, waitMs, func() ([]runtimedb.ChatOutbound, error) {
		return s.DB.ChatOutboundAfter(at, after, limit)
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return messages, http.StatusOK, nil
}

// ackChatOutbound marks one message delivered. Acking twice is fine;
// acking something that was never queued is a 404, because that means
// the adapter and the queue disagree.
func (s *Server) ackChatOutbound(r *http.Request, id principal.Identity) (any, int, error) {
	at, err := s.pollingConnection(r, id)
	if err != nil {
		return nil, http.StatusForbidden, err
	}
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("seq must be a number")
	}
	if err := s.DB.AckChatOutbound(at, seq); err != nil {
		return nil, http.StatusNotFound, fmt.Errorf("no queued message %d on this connection", seq)
	}
	return map[string]any{"acked": seq}, http.StatusOK, nil
}

// ChatRecipientFor resolves a household member to their bound
// conversation on one transport (ADR-W026 G6). cmd/keystone wires it
// into the domestic gateway's chat executor; it lives here so the one
// rule that matters is written next to the endpoints that share it: a
// rule reaches a member only through a binding a human created, exactly
// as an inbound message does.
func (s *Server) ChatRecipientFor(transport, member string) (target string, bindingID, connection int64, err error) {
	bindings, err := s.DB.ListChatBindings(false)
	if err != nil {
		return "", 0, 0, err
	}
	// EVERY candidate, not the first one that matches.
	//
	// This loop used to return as soon as it found a match, and
	// ListChatBindings orders newest first — so a household with two
	// Member-Agent bots on one transport delivered a rule's message to
	// whichever Connection had been bound most recently. That is a
	// CROSS-CONNECTION DELIVERY FALLBACK, and §8.3 says one may
	// "occur only when explicitly configured — never as a default".
	// Nobody configured it, nobody could see it, and which Agent spoke
	// for the house depended on the order of a list.
	var candidates []runtimedb.ChatBinding
	var conns []runtimedb.Connection
	for _, b := range bindings {
		// A household rule notifies the household, never an Owner
		// Agent's private bot — which is also why the admin packages
		// declare no agentTools. The Connection says which Agent, so
		// the test is that Agent's class rather than a surface stored
		// on the binding.
		at, cerr := s.DB.ChatConnectionBySeq(b.Connection)
		if cerr != nil || at.AdmitsOwnersOnly() {
			continue
		}
		if b.Transport != transport || b.Identity != member {
			continue
		}
		candidates = append(candidates, b)
		conns = append(conns, at)
	}
	switch len(candidates) {
	case 0:
		return "", 0, 0, fmt.Errorf("no live %s connection admits %q to a member agent", transport, member)
	case 1:
		return recipientOf(candidates[0])
	}
	// More than one Connection could carry it. Exactly one may be
	// marked as the one that accepts what was meant for another
	// (§8.4's accepts_fallback; migration 0064) — and if none is, or
	// several are, the house says nothing rather than choosing.
	//
	// REFUSING IS THE POINT. A message that went to the wrong Agent's
	// bot arrives looking perfectly normal, so nobody finds out; a
	// message that was not sent is a rule that visibly did not fire.
	// This is the same answer ErrAmbiguousConnection already gives
	// when an Agent has two bots and a caller names neither.
	var chosen []runtimedb.ChatBinding
	for i, at := range conns {
		if at.Disclosure.AcceptsFallback {
			chosen = append(chosen, candidates[i])
		}
	}
	if len(chosen) == 1 {
		return recipientOf(chosen[0])
	}
	names := make([]string, 0, len(conns))
	for _, at := range conns {
		names = append(names, at.Butler+" on "+at.Name())
	}
	sort.Strings(names)
	return "", 0, 0, fmt.Errorf(
		"%d live %s connections admit %q (%s) and %d is marked to take what was meant for another -- "+
			"say which one should carry the household's messages (design §8.3)",
		len(conns), transport, member, strings.Join(names, ", "), len(chosen))
}

// recipientOf is where a binding's messages go.
//
// The direct conversation, not the account. On Telegram they are the
// same value and either would work; on Matrix the account is an MXID
// and only a room can be sent to, so addressing the account would
// queue a message that can never be delivered (ADR-W026 A8). A binding
// with no recorded conversation falls back to the account: right where
// they coincide, and honest where they do not — the send fails visibly
// rather than going quietly nowhere.
//
// A binding never records a GROUP conversation (touchBinding drops
// it), so this cannot return a room by that route. QueueChatPush
// checks anyway, because "cannot happen by this route" is not the same
// as "cannot happen".
func recipientOf(b runtimedb.ChatBinding) (string, int64, int64, error) {
	if b.Conversation != "" {
		return b.Conversation, b.ID, b.Connection, nil
	}
	return b.Account, b.ID, b.Connection, nil
}

// AskOverChat queues a rule's message and, when it carries a correlation
// id, records the question so the member's next reply closes the loop as
// household.answered (ADR-W024 H4).
func (s *Server) AskOverChat(ctx context.Context, at runtimedb.Connection, target string, bindingID int64, body, correlationID, rule, subject string) (int64, error) {
	seq, err := s.QueueChatPush(ctx, at, target, bindingID, body, correlationID, "")
	if err != nil {
		return 0, err
	}
	if correlationID == "" {
		// A statement, not a question: nothing to wait for, so nothing
		// to remember.
		return seq, nil
	}
	if _, err := s.DB.RecordChatQuestion(runtimedb.ChatQuestion{
		CorrelationID: correlationID,
		Connection:    at.Seq,
		Target:        target,
		Rule:          rule,
		Subject:       subject,
	}); err != nil {
		// The message is already queued; the household will see the
		// question and their answer will simply be an ordinary message.
		s.Log.Error("recording chat question", "rule", rule, "err", err)
	}
	return seq, nil
}

// QueueChatPush enqueues a message nobody asked for — a household rule
// notifying over chat (ADR-W026 A8, work package G6). It is exported
// because cmd/keystone wires it into the domestic gateway as an
// executor; it is the same queue, so a push and a reply are delivered
// and retried identically.
func (s *Server) QueueChatPush(_ context.Context, at runtimedb.Connection, target string, bindingID int64, body, correlationID, dedupe string) (int64, error) {
	// NO HOUSEHOLD OR PERSONAL CONTENT INTO A ROOM (design §8.3, and
	// the shape of F-156-08, which was live on the deployment).
	//
	// This is the PUSH path: a household rule's question, a
	// notification, anything the house says without being asked. Its
	// body is assembled from household facts and from what the rule
	// knows about a person, and it is the one outbound path whose
	// destination is not a reply to a message somebody just sent.
	//
	// The inbound side already refuses rooms, so no reply is ever
	// addressed to one. That makes this guard look redundant and it is
	// not: the reason the inbound refusal works is that the adapter
	// TELLS it `group: true`, and nothing tells this path anything. It
	// is handed a target string, and a target string does not say
	// whether it is one person or forty. chat_rooms is how it finds
	// out, and this is the check that reads it.
	//
	// Queue-time rather than send-time, so the refusal is visible
	// where the decision was made: a row that reached the queue and
	// was dropped at delivery looks, to everything that reads the
	// queue, exactly like a message that was sent.
	//
	// THIS CHECK IS NOT WHAT MAKES THE RULE HOLD. It reads, then
	// enqueues, and between those two an inbound group message can
	// commit a NoteChatRoom -- so a concurrent rule could still land a
	// push in a room that was known to be one before the insert. A
	// review of #290 found that window. Migration 0065 closes it with
	// a trigger on chat_outbound, which is atomic with the insert and
	// holds for every writer rather than for the ones that remember to
	// ask first.
	//
	// This stays because the trigger answers with a constraint
	// failure, and the person reading the log deserves the sentence
	// below. Defence in depth where the depth is real: the trigger is
	// the boundary, this is the explanation.
	if s.DB.IsChatRoom(at.Seq, target) {
		s.recordEvent("chat.room_delivery_refused", at.ExternalID,
			"a message the house initiated was addressed to a room, and was not sent",
			map[string]string{"agent": at.Butler, "correlation": correlationID})
		return 0, fmt.Errorf(
			"%s is a room, and the house does not send what it knows into a room (design §8.3)", target)
	}
	if dedupe == "" {
		dedupe = chatDedupe(at, target, "", "push", body+time.Now().UTC().Format(time.RFC3339Nano))
	}
	seq, err := s.DB.EnqueueChatOutbound(runtimedb.ChatOutbound{
		Connection: at.Seq, Target: target, BindingID: bindingID,
		Body: redact.String(body), Kind: "push", CorrelationID: correlationID, DedupeKey: dedupe,
	})
	if err != nil && seq == 0 {
		// 0065's trigger, reached when the destination became a room
		// between the check above and this insert. Same refusal, said
		// the same way, so a rule's author cannot tell which of the
		// two stopped it -- and does not need to.
		if strings.Contains(err.Error(), "is a room") {
			s.recordEvent("chat.room_delivery_refused", at.ExternalID,
				"a message the house initiated was addressed to a room, and was not sent",
				map[string]string{"agent": at.Butler, "correlation": correlationID})
			return 0, fmt.Errorf(
				"%s is a room, and the house does not send what it knows into a room (design §8.3)", target)
		}
		return 0, err
	}
	return seq, nil
}

// recordChatAnswer closes an open chat question with this message, if
// there is one, and returns the acknowledgement to send back.
//
// The answer is recorded as `household.answered` carrying the ORIGINAL
// correlation id, so the rule's trigger, its question and the reply are
// one thread on the timeline — the same record an ntfy answer button
// produces (ADR-W024 H4), reached by a different road.
//
// A question nobody answers expires rather than waiting forever, and a
// claimed question is claimed once: two messages arriving together
// produce one answer, not two.
func (s *Server) recordChatAnswer(at runtimedb.Connection, binding runtimedb.ChatBinding, conversation, text string) (string, bool) {
	question, err := s.DB.ClaimChatAnswer(at, conversation, runtimedb.ChatQuestionWindow)
	if err != nil {
		return "", false // no open question: an ordinary message
	}
	subject := question.Subject
	if subject == "" {
		subject = binding.Identity
	}
	payload, err := json.Marshal(map[string]string{"answer": text, "rule": question.Rule, "via": binding.Transport})
	if err != nil {
		s.Log.Error("encoding chat answer payload", "err", err)
		return "", false
	}
	receipt, err := s.householdStore().Append(household.Envelope{
		EventID:       fmt.Sprintf("chat-answer-%d", question.ID),
		OccurredAt:    time.Now().UTC(),
		Type:          domestic.AnsweredEventType,
		Subject:       subject,
		Entity:        question.Rule,
		CorrelationID: question.CorrelationID,
		Payload:       payload,
	}, chatAnswerSource)
	if err != nil {
		s.Log.Error("recording chat answer", "rule", question.Rule, "err", err)
		// The question is already claimed, so re-answering it is not
		// possible; say something true rather than pretending.
		return "I could not write that down just now, sorry.", true
	}
	s.Log.Info("chat answer recorded", "rule", question.Rule, "seq", receipt.Seq,
		"correlation", question.CorrelationID)
	if s.Automation != nil {
		// A recorded fact may itself fire a rule, exactly as one posted
		// to the intake does.
		s.Automation.Poke()
	}
	return "Thanks — noted.", true
}

// chatAnswerSource is the household-stream source for an answer that
// arrived over chat. It is a Keystone-internal source like the
// automation engine's own records: no publisher token was involved,
// because the answer came through an authenticated binding rather than
// through the intake.
const chatAnswerSource = "chat"

// settleInbound records the reply against the claim and marks it kept,
// so the deferred release does not hand back a claim whose work
// actually happened.
func (s *Server) settleInbound(claimKey string, seq int64, settled *bool) {
	*settled = true
	if err := s.DB.RecordChatInboundReply(claimKey, seq); err != nil {
		s.Log.Error("recording inbound chat reply", "err", err)
	}
}

// touchBinding stamps last use and, for a DIRECT message, refreshes
// where a rule should reach this person later (ADR-W026 A8).
//
// Only a direct message updates it: the room a group conversation
// happens in is not the place to send somebody a private question, and
// treating it as one is how a rule's "did you get the mail?" ends up in
// front of the whole household.
func (s *Server) touchBinding(binding runtimedb.ChatBinding, conversation string, group bool) {
	direct := conversation
	if group {
		direct = ""
	}
	if err := s.DB.TouchChatBinding(binding.ID, direct); err != nil {
		s.Log.Error("touching chat binding failed", "err", err)
	}
}

// connectionForCaller decides which Connection a message arrived at,
// without reading the message (ADR-W033 B7).
//
// A chat adapter IS a Connection: its credential carries the capability
// it was minted for, a live address holds that capability, and that
// address names one Agent. It gets no say — naming a butler or a
// surface in the body is refused rather than ignored, because silently
// dropping a field a misconfigured bot set would have it answering as
// the wrong Agent and looking correct.
//
// A read-holding human — the Console, or `wild` exercising the contract
// — has no bot to arrive through, so they name the Agent. That is not
// an escalation: ADR-W031 A10 gives the Console holder every Butler.
//
// This replaced surfaceForCaller, and the difference is the point. A
// surface is a class of Agents: resolving a message to `admin` said
// which KIND of Agent should answer, and left which one to whatever
// the roster happened to return.
func (s *Server) connectionForCaller(id principal.Identity, msg inboundMessage) (runtimedb.Connection, error) {
	if capability, isAdapter := butlerAddressFor(id); isAdapter {
		if strings.TrimSpace(msg.Butler) != "" || strings.TrimSpace(msg.Surface) != "" {
			return runtimedb.Connection{}, fmt.Errorf(
				"a chat adapter cannot name a butler or a surface; the bot a message arrives at is the connection (ADR-W031 A7)")
		}
		at, err := s.adapterConnection(capability)
		if errors.Is(err, runtimedb.ErrNoConnection) {
			return runtimedb.Connection{}, fmt.Errorf(
				"no butler answers at %s: give one that address before its bot is used", capability)
		}
		return at, err
	}
	if !principal.Allowed(id.Role, principal.VerbRead) {
		return runtimedb.Connection{}, fmt.Errorf("this credential cannot choose a connection")
	}
	butler := strings.TrimSpace(msg.Butler)
	transport := strings.TrimSpace(msg.Transport)
	if butler == "" {
		return runtimedb.Connection{}, fmt.Errorf(
			"name a butler: this credential is not a chat adapter, so there is no connection to resolve one from")
	}
	// transport and connection both narrow, and both are optional: an
	// Agent with one bot needs neither.
	at, err := s.DB.ChatConnectionForButler(butler, transport, strings.TrimSpace(msg.Connection))
	if errors.Is(err, runtimedb.ErrNoConnection) {
		return runtimedb.Connection{}, fmt.Errorf("%s has no %s connection", butler, orAny(transport))
	}
	return at, err
}

// pollingConnection is the Connection whose queue an adapter may read.
//
// An adapter's own credential decides it, so a bot cannot read another
// Agent's replies — the guarantee that used to be "an adapter cannot
// read another surface's queue", which said nothing about two Agents
// sharing one surface.
func (s *Server) pollingConnection(r *http.Request, id principal.Identity) (runtimedb.Connection, error) {
	if capability, isAdapter := butlerAddressFor(id); isAdapter {
		at, err := s.adapterConnection(capability)
		if errors.Is(err, runtimedb.ErrNoConnection) {
			return runtimedb.Connection{}, fmt.Errorf("no butler answers at %s", capability)
		}
		return at, err
	}
	if !principal.Allowed(id.Role, principal.VerbRead) {
		return runtimedb.Connection{}, fmt.Errorf("this credential cannot read a queue")
	}
	butler := strings.TrimSpace(r.URL.Query().Get("butler"))
	transport := strings.TrimSpace(r.URL.Query().Get("transport"))
	if butler == "" {
		return runtimedb.Connection{}, fmt.Errorf("name a butler: this credential is not a chat adapter")
	}
	// Both narrowings are OPTIONAL. An Agent with a single bot needs
	// neither, which is what the CLI documents; ?transport= picks
	// between transports and ?connection=<external id> between an
	// Agent's several bots on one (B7). Leaving either out used to
	// match a transport named "" — nothing — so the queue of an Agent
	// that had exactly one bot could not be read at all.
	external := strings.TrimSpace(r.URL.Query().Get("connection"))
	return s.DB.ChatConnectionForButler(butler, transport, external)
}
