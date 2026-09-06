package domestic

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func testSender(t *testing.T) (*ChatSender, *[]string) {
	t.Helper()
	var asked []string
	return &ChatSender{
		Capability: "chat-telegram-family",
		Transport:  "telegram",
		Lookup: func(_ context.Context, member string) (ChatRecipient, error) {
			if member != "sam" {
				return ChatRecipient{}, errors.New("no binding")
			}
			return ChatRecipient{Transport: "telegram", Target: "12345", BindingID: 4}, nil
		},
		Ask: func(_ context.Context, r ChatRecipient, body, correlationID, rule, subject string) (int64, error) {
			asked = append(asked, strings.Join([]string{r.Target, body, correlationID, rule, subject}, "|"))
			return 9, nil
		},
	}, &asked
}

// The executor answers to "<capability>.send", which is what the
// package declares and what a rule names.
func TestChatSenderName(t *testing.T) {
	s, _ := testSender(t)
	if s.Name() != "chat-telegram-family.send" {
		t.Fatalf("Name = %q", s.Name())
	}
}

// A rule that names no member sends to the person its trigger was
// about, and the correlation id travels with the message so the reply
// can close the loop (ADR-W024 H4).
func TestChatSenderUsesTheTriggerSubject(t *testing.T) {
	s, asked := testSender(t)
	res, err := s.Execute(context.Background(), Call{
		Tool: s.Name(), Tier: "domestic", Rule: "mail-check", Subject: "sam",
		CorrelationID: "corr-1",
		Params:        map[string]string{"message": "did the mail arrive?"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(*asked) != 1 || (*asked)[0] != "12345|did the mail arrive?|corr-1|mail-check|sam" {
		t.Fatalf("asked = %v", *asked)
	}
	if res.Detail["awaitingAnswer"] != "true" || res.Detail["member"] != "sam" {
		t.Fatalf("detail = %+v", res.Detail)
	}
}

// A member nobody has introduced to the bot is a refusal that says what
// to do about it, not a silent failure.
func TestChatSenderRefusesUnboundMember(t *testing.T) {
	s, asked := testSender(t)
	_, err := s.Execute(context.Background(), Call{
		Tool: s.Name(), Tier: "domestic", Rule: "mail-check", Subject: "mika",
		Params: map[string]string{"message": "did the mail arrive?"},
	})
	var refusal *RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if !strings.Contains(refusal.Reason, "wild chat code") {
		t.Fatalf("refusal does not say how to fix it: %q", refusal.Reason)
	}
	if len(*asked) != 0 {
		t.Fatalf("a message was queued for an unbound member: %v", *asked)
	}
}

// A message with nobody to send it to is refused rather than guessed at.
func TestChatSenderRefusesWithoutARecipient(t *testing.T) {
	s, _ := testSender(t)
	if _, err := s.Execute(context.Background(), Call{
		Tool: s.Name(), Tier: "domestic", Rule: "r",
		Params: map[string]string{"message": "hello"},
	}); err == nil {
		t.Fatalf("a message with no member or subject was accepted")
	}
	if _, err := s.Execute(context.Background(), Call{
		Tool: s.Name(), Tier: "domestic", Rule: "r", Subject: "sam",
		Params: map[string]string{},
	}); err == nil {
		t.Fatalf("an empty message was accepted")
	}
}

// The gateway's own checks still apply: an executor registered here
// cannot be reached above the domestic tier (ADR-W024 A5).
func TestChatSenderStillPassesTheGatewayTierCheck(t *testing.T) {
	s, asked := testSender(t)
	g := &Gateway{}
	g.Register(s)
	if _, err := g.Dispatch(context.Background(), Call{
		Tool: s.Name(), Tier: "high", Subject: "sam",
		Params: map[string]string{"message": "hello"},
	}); err == nil {
		t.Fatalf("a high-tier chat send was dispatched")
	}
	if len(*asked) != 0 {
		t.Fatalf("a refused call reached the executor: %v", *asked)
	}
}
