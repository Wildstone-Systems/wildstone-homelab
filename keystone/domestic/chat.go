package domestic

// The chat.send executor (ADR-W026 A8, work package G6): a household
// rule reaching a member over the chat app they already use.
//
// It is the ntfy notifier's sibling, and deliberately the same shape —
// declared by a package, dispatched through this gateway, tier-checked,
// timed out and audited. What differs is only the road: ntfy pushes to a
// topic, this queues a message an adapter delivers, so a rule's question
// and a conversation's answer travel the same path and fail the same way
// (ADR-W026 A8).
//
// This package still learns no vendor and no database: both capabilities
// arrive as injected functions, so keystone/domestic imports neither
// keystone/runtimedb nor any chat SDK.

import (
	"context"
	"fmt"
	"strings"
)

// ChatRecipient is where a member can be reached on one transport.
type ChatRecipient struct {
	Transport string
	// Target is the conversation to send to — the member's direct chat.
	Target string
	// BindingID is the binding this came from, for the audit record.
	BindingID int64
}

// ChatSender is the "<capability>.send" executor. One is registered per
// installed chat capability that declares the tool, so a rule names the
// capability it means: on a deployment running both Telegram and Matrix
// for the household, `chat-telegram-family.send` and
// `chat-matrix-family.send` are different recipients, not one ambiguous
// one.
type ChatSender struct {
	// Capability is the capability this executor belongs to, e.g.
	// chat-telegram-family.
	Capability string
	// Transport is the vendor behind it, e.g. telegram.
	Transport string
	// Lookup resolves a household member to their bound conversation on
	// this transport. A member with no binding is not an error the rule
	// can fix — it is a person who has not been introduced to the bot —
	// so the refusal says exactly that.
	Lookup func(ctx context.Context, member string) (ChatRecipient, error)
	// Ask queues the message and, when the call carries a correlation
	// id, records the question so the member's reply can close the loop
	// as household.answered (ADR-W024 H4). It returns the queued seq.
	Ask func(ctx context.Context, r ChatRecipient, body, correlationID, rule, subject string) (int64, error)
}

// Name implements Executor.
func (c *ChatSender) Name() string { return c.Capability + ".send" }

// Execute implements Executor.
func (c *ChatSender) Execute(ctx context.Context, call Call) (Result, error) {
	if c.Lookup == nil || c.Ask == nil {
		return Result{}, &RefusalError{Tool: call.Tool, Reason: "chat delivery is not wired on this control plane"}
	}
	body := strings.TrimSpace(call.Params["message"])
	if body == "" {
		return Result{}, &RefusalError{Tool: call.Tool, Reason: "message is required"}
	}
	// Who to reach: the param when the rule names one, otherwise the
	// member the trigger was about. A rule that asks "did you get the
	// mail?" means the person who came home, and saying so twice in
	// every rule is how the two drift apart.
	member := strings.TrimSpace(call.Params["member"])
	if member == "" {
		member = call.Subject
	}
	if member == "" {
		return Result{}, &RefusalError{Tool: call.Tool, Reason: "no member to send to: the rule names none and its trigger has no subject"}
	}
	recipient, err := c.Lookup(ctx, member)
	if err != nil {
		return Result{}, &RefusalError{Tool: call.Tool, Reason: fmt.Sprintf(
			"%s has no %s account bound: introduce them with `wild chat code -surface family -identity %s`",
			member, c.Transport, member)}
	}
	seq, err := c.Ask(ctx, recipient, body, call.CorrelationID, call.Rule, member)
	if err != nil {
		return Result{}, err
	}
	summary := fmt.Sprintf("queued a %s message for %s", c.Transport, member)
	return Result{
		Summary: summary,
		Detail: map[string]string{
			"transport": c.Transport,
			"member":    member,
			"seq":       fmt.Sprintf("%d", seq),
			// Whether an answer can close the loop is a property of the
			// call, and worth recording: a question with no correlation
			// id is a statement.
			"awaitingAnswer": fmt.Sprintf("%t", call.CorrelationID != ""),
		},
	}, nil
}
