package domestic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AnsweredEventType is the household event a reply button publishes
// (ADR-W024 H4). It closes the loop the notification opened: the
// question, the answer, and the rule that asked are one correlated
// thread on the household stream.
const AnsweredEventType = "household.answered"

// maxReplyOptions is how many answer buttons one notification may
// carry. ntfy itself allows three, and a question with more than three
// answers is a form, not a notification.
const maxReplyOptions = 3

// Notifier is the notifications.notify executor: it publishes a message
// to ntfy, optionally with answer buttons that post the answer back to
// the household intake.
//
// The buttons are the whole point of H4. Without them a rule can only
// tell the household something; with them it can ask, and the answer
// becomes a fact on the same stream that triggered the question — no
// chat surface, no polling, no second channel.
type Notifier struct {
	Authority HouseholdExecutionAuthority
	// BaseURL is the ntfy server Keystone publishes to, e.g.
	// http://127.0.0.1:8080 or the container address on the wildstone
	// network. Required.
	BaseURL string
	// Token is an optional ntfy access token, sent as a bearer.
	Token string
	// ReplyURL is the household intake URL a reply button posts to, as
	// the PHONE can reach it — not Keystone's loopback address. Empty
	// means no buttons are attached: a rule can still notify, and a
	// question with nowhere to send its answer is not silently turned
	// into one.
	ReplyURL string
	// MintReply mints a short-lived, emit-only token for one
	// notification's buttons. Required for buttons; see the note in
	// Execute about what that token can and cannot do.
	MintReply func(ctx context.Context) (string, error)

	HTTPClient *http.Client
}

// Name implements Executor.
func (n *Notifier) Name() string { return "notifications.notify" }

func (n *Notifier) client() *http.Client {
	if n.HTTPClient != nil {
		return n.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// ntfyAction is one action button in ntfy's publish JSON.
type ntfyAction struct {
	Action  string            `json:"action"`
	Label   string            `json:"label"`
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Clear   bool              `json:"clear,omitempty"`
}

type ntfyMessage struct {
	Topic    string       `json:"topic"`
	Message  string       `json:"message"`
	Title    string       `json:"title,omitempty"`
	Priority int          `json:"priority,omitempty"`
	Tags     []string     `json:"tags,omitempty"`
	Actions  []ntfyAction `json:"actions,omitempty"`
}

// Execute publishes one notification.
//
// Params:
//
//	topic    (required) the ntfy topic to publish to
//	message  (required) the body
//	title    (optional) the notification title
//	priority (optional) ntfy priority 1..5
//	tags     (optional) comma-separated ntfy tags
//	replies  (optional) comma-separated answer labels; each becomes a
//	         button posting household.answered with that answer
//
// On the reply token: it is minted per notification, expires, and holds
// the publisher role — one verb, emit. It travels to a household device
// inside the notification, which is a real exposure and is why it is
// nothing more than that: whoever holds it can claim the household
// answered something, and can do nothing else. It cannot read state,
// propose, or approve.
func (n *Notifier) Execute(ctx context.Context, c Call) (Result, error) {
	if err := checkExecution(n.Authority); err != nil {
		return Result{}, err
	}
	if n.BaseURL == "" {
		return Result{}, fmt.Errorf("no ntfy base URL configured")
	}
	topic := strings.TrimSpace(c.Params["topic"])
	message := c.Params["message"]
	if topic == "" {
		return Result{}, fmt.Errorf("param %q is required", "topic")
	}
	if strings.TrimSpace(message) == "" {
		return Result{}, fmt.Errorf("param %q is required", "message")
	}

	msg := ntfyMessage{Topic: topic, Message: message, Title: c.Params["title"]}
	if p := c.Params["priority"]; p != "" {
		var v int
		if _, err := fmt.Sscanf(p, "%d", &v); err != nil || v < 1 || v > 5 {
			return Result{}, fmt.Errorf("param %q must be 1..5, got %q", "priority", p)
		}
		msg.Priority = v
	}
	if tags := splitList(c.Params["tags"]); len(tags) > 0 {
		msg.Tags = tags
	}

	replies := splitList(c.Params["replies"])
	switch {
	case len(replies) == 0:
	case n.ReplyURL == "" || n.MintReply == nil:
		// A question whose answer has nowhere to go is not a question.
		// Refusing is louder than quietly sending a message with no
		// buttons and waiting forever for a reply.
		return Result{}, fmt.Errorf("this rule asks for replies but no household reply URL is configured (-household-reply-url)")
	case len(replies) > maxReplyOptions:
		return Result{}, fmt.Errorf("param %q has %d options; at most %d are supported", "replies", len(replies), maxReplyOptions)
	default:
		token, err := n.MintReply(ctx)
		if err != nil {
			return Result{}, fmt.Errorf("minting a reply token: %w", err)
		}
		for _, answer := range replies {
			body, err := json.Marshal(map[string]any{
				"eventId":       replyEventID(c, answer),
				"occurredAt":    time.Now().UTC().Format(time.RFC3339Nano),
				"type":          AnsweredEventType,
				"subject":       c.Subject,
				"entity":        c.Rule,
				"correlationId": c.CorrelationID,
				"payload":       map[string]string{"answer": answer, "rule": c.Rule},
			})
			if err != nil {
				return Result{}, err
			}
			msg.Actions = append(msg.Actions, ntfyAction{
				Action: "http", Label: answer, URL: n.ReplyURL, Method: http.MethodPost,
				Headers: map[string]string{
					"Authorization": "Bearer " + token,
					"Content-Type":  "application/json",
				},
				Body:  string(body),
				Clear: true,
			})
		}
	}

	raw, err := json.Marshal(msg)
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(n.BaseURL, "/"), bytes.NewReader(raw))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	if err := checkExecution(n.Authority); err != nil {
		return Result{}, err
	}
	resp, err := n.client().Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("publishing to ntfy: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return Result{}, fmt.Errorf("ntfy returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	detail := map[string]string{"topic": topic}
	summary := "notified " + topic
	if len(msg.Actions) > 0 {
		labels := make([]string, 0, len(msg.Actions))
		for _, a := range msg.Actions {
			labels = append(labels, a.Label)
		}
		detail["replies"] = strings.Join(labels, ", ")
		summary = fmt.Sprintf("asked %s (%s)", topic, strings.Join(labels, "/"))
	}
	return Result{Summary: summary, Detail: detail}, nil
}

// replyEventID is deterministic per (rule, correlation, answer), so a
// household member tapping the same button twice records one answer.
// Tapping a DIFFERENT button records a second, later event — a change of
// mind is a fact too, and the stream keeps both in order.
func replyEventID(c Call, answer string) string {
	base := c.CorrelationID
	if base == "" {
		base = c.Rule
	}
	return "reply:" + base + ":" + strings.ToLower(strings.ReplaceAll(answer, " ", "-"))
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
