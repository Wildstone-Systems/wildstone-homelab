package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// The G6 loop, end to end: a rule asks a household member a question
// over chat, they answer in their own words, and the answer lands on the
// household timeline correlated to the rule that asked (ADR-W024 H4,
// ADR-W026 A8).
func TestChatQuestionAndAnswerCloseTheLoop(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler([]*provider.ChatResponse{{Text: "should not be reached"}}, runtimedb.ButlerModeFull)
	binding := h.bindAccount(t, "family", "sam", "telegram", "12345")

	// A rule asks, through the same executor cmd/keystone registers.
	sender := &domestic.ChatSender{
		Capability: "chat-telegram-family",
		Transport:  "telegram",
		Lookup: func(_ context.Context, member string) (domestic.ChatRecipient, error) {
			target, id, connection, err := h.s.ChatRecipientFor("telegram", member)
			if err != nil {
				return domestic.ChatRecipient{}, err
			}
			return domestic.ChatRecipient{
				Transport: "telegram", Target: target, BindingID: id, Connection: connection,
			}, nil
		},
		Ask: func(ctx context.Context, r domestic.ChatRecipient, body, correlationID, rule, subject string) (int64, error) {
			at, aerr := h.s.DB.ChatConnectionBySeq(r.Connection)
			if aerr != nil {
				return 0, aerr
			}
			return h.s.AskOverChat(ctx, at, r.Target, r.BindingID, body, correlationID, rule, subject)
		},
	}
	if _, err := sender.Execute(context.Background(), domestic.Call{
		Tool: sender.Name(), Tier: "domestic", Rule: "mail-check", Subject: "sam",
		CorrelationID: "corr-1", TriggerType: "person.arrived_home",
		Params: map[string]string{"message": "did the mail arrive today?"},
	}); err != nil {
		t.Fatalf("rule asking over chat: %v", err)
	}

	// The question is on the family queue, addressed to the bound
	// account, as a push rather than a reply.
	queued := h.poll(t, runtimedb.RoleConcierge, "")
	if len(queued) != 1 || queued[0].Target != binding.Account || queued[0].Kind != "push" {
		t.Fatalf("queued = %+v", queued)
	}
	if queued[0].CorrelationID != "corr-1" {
		t.Fatalf("the question lost its correlation id: %+v", queued[0])
	}

	// Sam answers in their own words.
	resp, out := h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "conversation": "12345",
		"text": "yes, it's on the table", "messageId": "m-1",
	})
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(out.Reason, "answer") {
		t.Fatalf("status = %d, out = %+v", resp.StatusCode, out)
	}

	// It is a FACT on the household timeline, carrying the rule's own
	// correlation id — the same record an ntfy button produces.
	var events []runtimedb.HouseholdEvent
	if resp := h.do(runtimedb.RoleOperator, "GET", "/api/v1/household/events?after=0", nil, &events); resp.StatusCode != http.StatusOK {
		t.Fatalf("list household events: %d", resp.StatusCode)
	}
	if len(events) != 1 {
		t.Fatalf("household events = %+v, want the answer", events)
	}
	answer := events[0]
	if answer.Type != domestic.AnsweredEventType || answer.CorrelationID != "corr-1" {
		t.Fatalf("answer event = %+v", answer)
	}
	if answer.Subject != "sam" || answer.Entity != "mail-check" {
		t.Fatalf("answer is not attributed to the member and the rule: %+v", answer)
	}
	var payload map[string]string
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["answer"] != "yes, it's on the table" || payload["via"] != "telegram" {
		t.Fatalf("payload = %+v", payload)
	}

	// No Butler session ran: an answer is a fact, so the loop closes
	// with the provider switched off.
	var sessions []runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions", nil, &sessions)
	if len(sessions) != 0 {
		t.Fatalf("answering a question opened a Butler session: %+v", sessions)
	}

	// The acknowledgement went back to the same conversation.
	acks := h.poll(t, runtimedb.RoleConcierge, "")
	if len(acks) != 2 || !strings.Contains(acks[1].Body, "noted") {
		t.Fatalf("acknowledgement = %+v", acks)
	}

	// The question is closed: the NEXT message is an ordinary one and
	// opens a session rather than being recorded as a second answer.
	h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "text": "what's for dinner?", "messageId": "m-2",
	})
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/household/events?after=0", nil, &events)
	if len(events) != 1 {
		t.Fatalf("a second message was recorded as another answer: %+v", events)
	}
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions", nil, &sessions)
	if len(sessions) != 1 {
		t.Fatalf("the follow-up did not open a session: %+v", sessions)
	}
}

// A rule can only reach a member through a binding a human created, and
// only on the family surface — an admin binding is not a household
// notification target (ADR-W026 A3).
func TestChatRecipientRequiresAFamilyBinding(t *testing.T) {
	h := newHarness(t)
	h.bindAccount(t, "admin", "mika", "telegram", "555")

	// A rule notifies the household, never an Owner Agent's private
	// bot. mika is admitted to alfred, an Owner Agent, so resolving
	// them as a notification target must fail — and it is now the
	// AGENT's class that says so rather than a surface on the binding.
	if _, _, _, err := h.s.ChatRecipientFor("telegram", "mika"); err == nil {
		t.Fatalf("a rule resolved an owner agent's account as a notification target")
	}
	if _, _, _, err := h.s.ChatRecipientFor("telegram", "sam"); err == nil {
		t.Fatalf("a rule resolved an unbound member")
	}

	binding := h.bindAccount(t, "family", "sam", "telegram", "12345")
	target, id, _, err := h.s.ChatRecipientFor("telegram", "sam")
	if err != nil || target != "12345" || id != binding.ID {
		t.Fatalf("recipient = %q/%d, err = %v", target, id, err)
	}

	// Revoking takes the member out of reach on the next call.
	if err := h.s.DB.RevokeChatBinding(binding.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, _, err := h.s.ChatRecipientFor("telegram", "sam"); err == nil {
		t.Fatalf("a revoked binding is still a notification target")
	}
}

// A statement carries no correlation id, so nothing is left waiting: the
// member's next message is an ordinary one.
func TestChatStatementDoesNotOpenAQuestion(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler([]*provider.ChatResponse{{Text: "hello back"}}, runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "sam", "telegram", "12345")

	if _, err := h.s.AskOverChat(context.Background(), h.agentAt(t, "family", "telegram"), "12345", 1,
		"the dishwasher finished", "", "dishwasher", "sam"); err != nil {
		t.Fatalf("AskOverChat: %v", err)
	}
	h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "text": "thanks", "messageId": "m-1",
	})

	var events []runtimedb.HouseholdEvent
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/household/events?after=0", nil, &events)
	if len(events) != 0 {
		t.Fatalf("a reply to a statement was recorded as an answer: %+v", events)
	}
	var sessions []runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions", nil, &sessions)
	if len(sessions) != 1 {
		t.Fatalf("the reply did not reach the Butler: %+v", sessions)
	}
}
