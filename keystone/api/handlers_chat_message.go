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

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
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

// unboundReply is the ONLY thing an unbound account is told (ADR-W026
// A4). It names no member, confirms no account, and describes nothing
// about the deployment: someone messaging a bot they found should learn
// only that there is a process, not that they guessed right.
const unboundReply = "I can only talk to people the household has introduced me to. " +
	"Ask whoever runs this home lab for a claim code, then send it to me on its own."

// adminGroupReply is what the operator surface says in a group chat: it
// answers in a direct message and nowhere else (ADR-W026 A7).
const adminGroupReply = "I answer operator questions in a direct message only, never in a group."

type inboundMessage struct {
	Transport string `json:"transport"`
	Account   string `json:"account"`
	Text      string `json:"text"`
	// Conversation is where a reply goes. For a direct message it is the
	// account's own chat; for a group it is the group.
	Conversation string `json:"conversation"`
	// Group marks a shared audience (ADR-W026 A7).
	Group bool `json:"group,omitempty"`
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

// surfaceForCaller decides which Butler this caller may reach.
//
//   - concierge → family, always. It has no other spelling.
//   - valet     → admin, always. The admin adapter's own role, which is
//     NOT the butler role every external agent already holds (ADR-W024
//     A5): a chat verb there would have handed each of them this queue.
//   - read-holding humans (admin/operator) → whichever they name, so the
//     Console and `wild` can exercise the contract end to end.
func surfaceForCaller(id principal.Identity, requested string) (runtimedb.ButlerSurface, error) {
	if surface, isAdapter := adapterSurface(id); isAdapter {
		if requested != "" && requested != string(surface) {
			return "", fmt.Errorf("this credential serves the %s surface", surface)
		}
		return surface, nil
	}
	if !principal.Allowed(id.Role, principal.VerbRead) {
		return "", fmt.Errorf("this credential cannot choose a surface")
	}
	surface := runtimedb.ButlerSurface(requested)
	if !runtimedb.ValidButlerSurface(surface) {
		return "", fmt.Errorf("surface must be admin or family")
	}
	return surface, nil
}

// adapterSurface reports the surface a CHAT ADAPTER's credential is
// pinned to, and whether the caller is one at all.
//
// The distinction matters wherever a check exists to stop a bot acting
// on the wrong surface: a human with the Console or `wild` is not a bot
// pinned to one, and holding them to an adapter's constraint would only
// stop an operator redeeming a code on behalf of somebody standing next
// to them.
func adapterSurface(id principal.Identity) (runtimedb.ButlerSurface, bool) {
	switch id.Role {
	case runtimedb.RoleConcierge:
		return runtimedb.ButlerSurfaceFamily, true
	case runtimedb.RoleValet:
		return runtimedb.ButlerSurfaceAdmin, true
	}
	return "", false
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

	// Claim this message before anything acts on it (ADR-W026 A8). The
	// outbound dedupe key protects the REPLY; everything before it — the
	// provider call, the session record, a domestic action that notifies
	// a person — happens first and would happen twice on a redelivery.
	// This is the automation engine's claim-before-dispatch shape
	// (ADR-W024 H4), and the same deliberate trade: at-most-once, since
	// a duplicated action is worse than a missed one.
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

	binding, err := s.DB.LookupChatBinding(at, account)
	if err != nil {
		// One reply, no detail, no session row (ADR-W026 A4).
		seq := s.queueChatReply(at, conversation, unboundReply, 0, "",
			chatDedupe(at, conversation, msg.MessageID, "unbound", text))
		s.settleInbound(claimKey, seq, &settled)
		return inboundResponse{Accepted: true, Bound: false, Queued: seq,
			Reason: "no admission for this account on this connection"}, http.StatusAccepted, nil
	}

	// Group rules (ADR-W026 A7). The operator surface refuses outright;
	// the family surface answers, with the audience declared to it.
	systemExtra := ""
	if msg.Group {
		if at.AdmitsOwnersOnly() {
			seq := s.queueChatReply(at, conversation, adminGroupReply, binding.ID, "",
				chatDedupe(at, conversation, msg.MessageID, "admin-group", text))
			s.settleInbound(claimKey, seq, &settled)
			return inboundResponse{Accepted: true, Bound: true, Queued: seq,
				Reason: "the operator surface answers in direct messages only"}, http.StatusAccepted, nil
		}
		systemExtra = butler.HouseholdGroupNote
	}

	// Is this an ANSWER to something a household rule asked (ADR-W024
	// H4, ADR-W026 A8)? If so it is a fact, not a conversation: it goes
	// onto the household timeline correlated to the rule that asked, and
	// gets a short acknowledgement. No model is involved, so the loop a
	// rule opens still closes with the Butler switched off — the
	// deterministic-backbone principle, applied to the one place chat
	// could quietly have broken it.
	if !at.AdmitsOwnersOnly() {
		if body, answered := s.recordChatAnswer(binding, conversation, text); answered {
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
	if body, reason, handled := s.runChatCommand(chatCommandCtx{
		At:           at,
		Conversation: conversation,
		Identity:     binding.Identity,
		Binding:      binding,
		Group:        msg.Group,
	}, text); handled {
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
	prior := s.chatThread(at, conversation, binding.Identity)

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
			Transport:      transport,
			Address:        address,
			Butler:         named,
			Member:         binding.Identity,
			Text:           text,
			Group:          msg.Group,
			SystemExtra:    systemExtra,
			RequireSurface: at.Surface,
		}
		if prior != nil {
			turn.SessionID, turn.History = prior.SessionID, prior.History
		}
		out, st, err = s.runNamedButlerSession(r.Context(), turn, prior)
	} else if !at.AdmitsOwnersOnly() {
		out, st, err = s.runFamilySession(r.Context(), text, binding.Identity, systemExtra, msg.Group, prior)
	} else {
		out, st, err = s.runAdminButlerSession(r.Context(), text, binding.Identity, prior)
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

// chatIdleWindow is how long a chat conversation stays open. A message
// after this starts a new session rather than silently resuming one the
// person has forgotten — and a Butler answering in the context of
// yesterday's question is harder to understand than one that starts
// clean.
const chatIdleWindow = 30 * time.Minute

// chatThread is the conversation this message continues, or nil for a
// new one.
//
// The stored session is checked against the CALLER before it is
// continued. A pointer row is keyed by transport, conversation and
// surface, but the session it names carries its own acting_for and
// surface, and those are the authority — a thread pointer that has
// drifted (a binding revoked and reissued to somebody else on the same
// chat id) must not hand a stranger the previous person's conversation.
func (s *Server) chatThread(at runtimedb.Connection, conversation, identity string) *turnContext {
	id, ok, err := s.DB.ChatThreadSession(at, conversation, identity)
	if err != nil {
		s.Log.Error("reading chat thread failed", "err", err)
		return nil
	}
	if !ok {
		return nil
	}
	sess, err := s.DB.GetButlerSession(id)
	if err != nil {
		// Including ErrNotFound: a pointer to a session that is gone is
		// a new conversation, not a failure.
		return nil
	}
	if sess.ActingFor != identity || sess.Surface != at.Surface {
		s.Log.Warn("chat thread pointed at another principal's session; starting fresh",
			"session", id, "expected", identity, "got", sess.ActingFor)
		return nil
	}
	// Age is measured from the conversation's newest turn, on ANY
	// surface. Measuring it from the pointer would call a thread dead
	// that was alive two minutes ago in the Console — the same
	// conversation, carried on somewhere else.
	if last := sess.LastTurnAt(); last.IsZero() || time.Since(last) > chatIdleWindow {
		return nil
	}
	return &turnContext{SessionID: sess.ID, History: historyFrom(sess.Turns)}
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
		// The direct conversation, not the account. On Telegram they are
		// the same value and either would work; on Matrix the account is
		// an MXID and only a room can be sent to, so addressing the
		// account would queue a message that can never be delivered
		// (ADR-W026 A8). A binding with no recorded conversation falls
		// back to the account: right where they coincide, and honest
		// where they do not — the send fails visibly rather than going
		// quietly nowhere.
		if b.Conversation != "" {
			return b.Conversation, b.ID, b.Connection, nil
		}
		return b.Account, b.ID, b.Connection, nil
	}
	return "", 0, 0, fmt.Errorf("no live %s connection admits %q to a member agent", transport, member)
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
		Surface:       at.Surface,
		Transport:     at.Transport,
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
	if dedupe == "" {
		dedupe = chatDedupe(at, target, "", "push", body+time.Now().UTC().Format(time.RFC3339Nano))
	}
	seq, err := s.DB.EnqueueChatOutbound(runtimedb.ChatOutbound{
		Connection: at.Seq, Target: target, BindingID: bindingID,
		Body: redact.String(body), Kind: "push", CorrelationID: correlationID, DedupeKey: dedupe,
	})
	if err != nil && seq == 0 {
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
func (s *Server) recordChatAnswer(binding runtimedb.ChatBinding, conversation, text string) (string, bool) {
	question, err := s.DB.ClaimChatAnswer(binding.Transport, conversation, runtimedb.ChatQuestionWindow)
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
		at, err := s.DB.ChatConnectionByExternalID(capability)
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
	at, err := s.DB.ChatConnectionForButler(butler, transport)
	if errors.Is(err, runtimedb.ErrNoConnection) {
		return runtimedb.Connection{}, fmt.Errorf("%s has no %s connection", butler, transport)
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
		at, err := s.DB.ChatConnectionByExternalID(capability)
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
	return s.DB.ChatConnectionForButler(butler, transport)
}
