package agentgateway

// Notify implements agentapi.Client (ADR-W039 §3.2 Notifications, §4
// item 8; Phase 4, #341): outbound text through the task's OWN
// Connection, and never any other way — a notification reaches a Person
// only through a Wildstone Connection, the same rule QueueChatPush
// enforces for a household rule's own push (api/handlers_chat_message.go)
// and the stall notifier enforces for its one notice
// (butlerstore/notify.go): the originating direct surface, never a
// room, never a broadcast, held through quiet hours by the outbound
// queue's own dedupe-and-hold machinery.
//
// This is the same queue those two write to (runtimedb's chat_outbound),
// reached directly because agentgateway cannot import keystone/api (the
// structural boundary Phase 2 pins) and QueueChatPush lives there. A
// runtime composes the text and nothing else — admission, disclosure,
// quiet hours and the room refusal are all decided here, from the
// handle, never from anything the request could name.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/redact"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func (g *Gateway) Notify(ctx context.Context, req agentapi.NotifyRequest) error {
	s, err := g.open(ctx, req.Handle)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return fmt.Errorf("%w: notify needs text", ErrRefused)
	}
	// No Connection means no notify path — a routine or subscription
	// task admitted with none has nobody to reach this way, and there
	// is no fallback: "no Connection" and "some Connection nobody
	// checked" must never be indistinguishable in this call's behaviour.
	if s.hc.OriginConnectionRef == 0 {
		return fmt.Errorf(
			"%w: this task has no connection to notify through, and a runtime never reaches a person any other way (ADR-W039 §3.2)",
			ErrRefused)
	}
	conn, err := g.DB.ChatConnectionBySeq(s.hc.OriginConnectionRef)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRefused, err)
	}
	// NO HOUSEHOLD OR PERSONAL CONTENT INTO A ROOM (design §8.3), the
	// same check QueueChatPush makes at queue time, so the refusal is
	// visible where the decision was made.
	if g.DB.IsChatRoom(conn.Seq, conn.ExternalID) {
		return fmt.Errorf(
			"%w: %s is a room, and the house does not send what it knows into a room (design §8.3)",
			ErrRefused, conn.ExternalID)
	}
	dedupe := req.DedupeKey
	if dedupe != "" {
		// A runtime's own key collapses repeats of the SAME notice, the
		// stall notifier's own contract — scoped to this Agent so two
		// Agents choosing the same key cannot collide.
		dedupe = fmt.Sprintf("agent-notify:%s:%s", s.hc.Butler.Name, dedupe)
	} else {
		dedupe = fmt.Sprintf("agent-notify:%s:%d", s.hc.Task.ID, g.now().UnixNano())
	}
	// No Operation: chat_outbound_effects (0072) is the ledger's own
	// tool-step evidence trail, populated only when a real step backs
	// the send, and Notify has no step — it is not a tool dispatch, and
	// setting one would fabricate evidence of a call that never
	// happened. PrivacyTask instead links the message to this Task for
	// the disclosure check alone, at read time, the same as any other
	// chat effect this Task's own tool calls produce.
	outbound, err := runtimedb.WithChatOutboundPrivacy(ctx, runtimedb.ChatOutbound{
		PrivacyTask: s.hc.Task.Seq,
		Connection:  conn.Seq, Target: conn.ExternalID,
		Body: redact.String(text), Kind: "push", CorrelationID: s.hc.Task.ID, DedupeKey: dedupe,
	})
	if err != nil {
		return err
	}
	if _, err := g.DB.EnqueueChatOutbound(outbound); err != nil && !errors.Is(err, runtimedb.ErrDuplicateOutbound) {
		return fmt.Errorf("agentgateway: queuing a notification: %w", err)
	}
	return nil
}
