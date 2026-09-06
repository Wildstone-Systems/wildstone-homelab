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
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

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
//   - butler    → admin, always. The admin adapter's role.
//   - read-holding humans (admin/operator) → whichever they name, so the
//     Console and `wild` can exercise the contract end to end.
func surfaceForCaller(id principal.Identity, requested string) (runtimedb.ButlerSurface, error) {
	switch id.Role {
	case runtimedb.RoleConcierge:
		if requested != "" && requested != string(runtimedb.ButlerSurfaceFamily) {
			return "", fmt.Errorf("this credential serves the family surface")
		}
		return runtimedb.ButlerSurfaceFamily, nil
	case runtimedb.RoleButler:
		if requested != "" && requested != string(runtimedb.ButlerSurfaceAdmin) {
			return "", fmt.Errorf("this credential serves the admin surface")
		}
		return runtimedb.ButlerSurfaceAdmin, nil
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
	surface, err := surfaceForCaller(id, strings.TrimSpace(msg.Surface))
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
	if !s.limiter.allowChat(transport + ":" + account) {
		return nil, http.StatusTooManyRequests, fmt.Errorf("too many messages; slow down")
	}

	binding, err := s.DB.LookupChatBinding(transport, account, surface)
	if err != nil {
		// One reply, no detail, no session row (ADR-W026 A4).
		seq := s.queueChatReply(surface, transport, conversation, unboundReply, 0, "",
			chatDedupe(transport, conversation, msg.MessageID, "unbound", text))
		return inboundResponse{Accepted: true, Bound: false, Queued: seq,
			Reason: "no binding for this account on this surface"}, http.StatusAccepted, nil
	}

	// Group rules (ADR-W026 A7). The operator surface refuses outright;
	// the family surface answers, with the audience declared to it.
	systemExtra := ""
	if msg.Group {
		if surface == runtimedb.ButlerSurfaceAdmin {
			seq := s.queueChatReply(surface, transport, conversation, adminGroupReply, binding.ID, "",
				chatDedupe(transport, conversation, msg.MessageID, "admin-group", text))
			return inboundResponse{Accepted: true, Bound: true, Queued: seq,
				Reason: "the operator surface answers in direct messages only"}, http.StatusAccepted, nil
		}
		systemExtra = butler.HouseholdGroupNote
	}

	if s.ButlerHost == nil || s.ButlerHost.Provider == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no Butler provider configured")
	}

	var (
		out any
		st  int
	)
	if surface == runtimedb.ButlerSurfaceFamily {
		out, st, err = s.runFamilyButlerSession(r.Context(), text, binding.Identity, systemExtra)
	} else {
		out, st, err = s.runAdminButlerSession(r.Context(), text, binding.Identity)
	}
	if err != nil {
		return nil, st, err
	}
	if err := s.DB.TouchChatBinding(binding.ID); err != nil {
		s.Log.Error("touching chat binding failed", "err", err)
	}

	ask, _ := out.(askResponse)
	body := chatReplyBody(ask)
	seq := s.queueChatReply(surface, transport, conversation, body, binding.ID, "",
		chatDedupe(transport, conversation, msg.MessageID, ask.SessionID, text))
	return inboundResponse{Accepted: true, Bound: true, Queued: seq}, http.StatusAccepted, nil
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

// queueChatReply enqueues one reply, returning its seq. A queue failure
// is logged rather than returned: the session already happened and is
// already recorded, and telling the adapter the whole request failed
// would have it redeliver a message that has been answered.
func (s *Server) queueChatReply(surface runtimedb.ButlerSurface, transport, target, body string, bindingID int64, correlationID, dedupe string) int64 {
	seq, err := s.DB.EnqueueChatOutbound(runtimedb.ChatOutbound{
		Surface: surface, Transport: transport, Target: target,
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
func chatDedupe(transport, conversation, messageID, discriminator, text string) string {
	if messageID != "" {
		return fmt.Sprintf("%s:%s:%s", transport, conversation, messageID)
	}
	sum := sha256.Sum256([]byte(transport + "\x00" + conversation + "\x00" + discriminator + "\x00" + text))
	return fmt.Sprintf("%s:%s:h%s", transport, conversation, hex.EncodeToString(sum[:8]))
}

// pollChatOutbound is the adapter's delivery cursor (ADR-W026 A8): the
// messages queued for ITS surface, after the cursor it holds, oldest
// first. The surface comes from the caller's role, so an adapter cannot
// read another surface's queue.
func (s *Server) pollChatOutbound(r *http.Request, id principal.Identity) (any, int, error) {
	surface, err := surfaceForCaller(id, strings.TrimSpace(r.URL.Query().Get("surface")))
	if err != nil {
		return nil, http.StatusForbidden, err
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	waitMs, _ := strconv.Atoi(r.URL.Query().Get("waitMs"))
	transport := strings.TrimSpace(r.URL.Query().Get("transport"))
	messages, err := longPoll(r, waitMs, func() ([]runtimedb.ChatOutbound, error) {
		return s.DB.ChatOutboundAfter(surface, transport, after, limit)
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
	surface, err := surfaceForCaller(id, strings.TrimSpace(r.URL.Query().Get("surface")))
	if err != nil {
		return nil, http.StatusForbidden, err
	}
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("seq must be a number")
	}
	if err := s.DB.AckChatOutbound(surface, seq); err != nil {
		return nil, http.StatusNotFound, fmt.Errorf("no queued message %d on this surface", seq)
	}
	return map[string]any{"acked": seq}, http.StatusOK, nil
}

// QueueChatPush enqueues a message nobody asked for — a household rule
// notifying over chat (ADR-W026 A8, work package G6). It is exported
// because cmd/keystone wires it into the domestic gateway as an
// executor; it is the same queue, so a push and a reply are delivered
// and retried identically.
func (s *Server) QueueChatPush(_ context.Context, surface runtimedb.ButlerSurface, transport, target, body, correlationID, dedupe string) (int64, error) {
	if dedupe == "" {
		dedupe = chatDedupe(transport, target, "", "push", body+time.Now().UTC().Format(time.RFC3339Nano))
	}
	seq, err := s.DB.EnqueueChatOutbound(runtimedb.ChatOutbound{
		Surface: surface, Transport: transport, Target: target,
		Body: redact.String(body), Kind: "push", CorrelationID: correlationID, DedupeKey: dedupe,
	})
	if err != nil && seq == 0 {
		return 0, err
	}
	return seq, nil
}
