package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// Slice 11 (#212), disclosure half: a Connection bounds what may be
// SAID over it, not only who may address it (design §8.3, §8.4).

// TestGroupDestinationIsRefusedForARecognisedPerson — §8.3's inbound
// rule, asserted as a refusal WITH a redirect and, the part that makes
// it a boundary, before any inference.
//
// A recognised Person is the whole point. A stranger in a room is
// refused by admission and proves nothing about this rule; the case
// that matters is somebody the Agent knows perfectly well, asking from
// a place where it cannot know who else is reading.
func TestGroupDestinationIsRefusedForARecognisedPerson(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(answerTurns("whatever you like"), runtimedb.ButlerModeFull)
	h.agentAt(t, "family", "telegram")
	h.person("sam")
	h.bindAccount(t, "family", "sam", "telegram", "12345")

	// The control: the SAME person, the SAME bot, the SAME question in
	// a direct chat is answered. Without it every assertion below would
	// hold against a deployment where nothing works at all.
	if _, out := h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345",
		"text": "what is for dinner?", "messageId": "d-1",
	}); !out.Bound {
		t.Fatalf("the control was not admitted: %+v", out)
	}
	direct := h.providerCalls(t)
	if direct == 0 {
		t.Fatal("the control never reached the model, so the refusal below proves nothing")
	}

	_, out := h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "conversation": "room-7",
		"group": true, "addressed": true,
		"text": "what is for dinner?", "messageId": "g-1",
	})
	if !out.Bound {
		t.Fatalf("the room message was not even recognised: %+v", out)
	}
	if n := h.providerCalls(t); n != direct {
		t.Fatalf("a room message reached the model %d time(s) (design §8.3)", n-direct)
	}

	// Refused WITH somewhere to go. #212 asks for the redirect by name,
	// and a bare refusal teaches a household member their bot is broken.
	queued := h.poll(t, runtimedb.RoleConcierge, "")
	last := queued[len(queued)-1]
	if last.Target != "room-7" {
		t.Fatalf("the redirect did not go to the room: %+v", last)
	}
	if !strings.Contains(last.Body, "one-to-one") || !strings.Contains(last.Body, "directly") {
		t.Fatalf("the room was not redirected to a one-to-one connection: %q", last.Body)
	}
	// And it discloses nothing while refusing — not what was asked, not
	// who asked, not what the house knows.
	if strings.Contains(last.Body, "dinner") || strings.Contains(last.Body, "sam") {
		t.Fatalf("the refusal itself disclosed something: %q", last.Body)
	}
}

// TestNoPersonalOrHouseholdMemoryReachesAGroupDestination — the
// OUTBOUND half, and a separate rule rather than a consequence of the
// inbound one.
//
// This is the shape of F-156-08, which was live on the deployment. The
// inbound refusal works because the adapter TELLS it `group: true`;
// this path is handed a target string by a household rule, and a
// target string does not say whether it is one person or forty. If the
// only guard were the inbound one, a rule would happily deliver what
// the house knows into a room.
func TestNoPersonalOrHouseholdMemoryReachesAGroupDestination(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
	at := h.agentAt(t, "family", "telegram")
	h.person("sam")
	binding := h.bindAccount(t, "family", "sam", "telegram", "12345")

	// The control first: a push to sam's DIRECT conversation is
	// delivered, so the refusal below is about the destination and not
	// about pushes being broken.
	if _, err := h.s.QueueChatPush(context.Background(), at, binding.Account, binding.ID,
		"the heating comes on at six", "corr-direct", ""); err != nil {
		t.Fatalf("a push to a direct chat was refused: %v", err)
	}

	// The room becomes known the only way it can: somebody addressed
	// the Agent from it. That message was refused, and the fact that
	// the conversation IS a room is what the refusal leaves behind.
	h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "conversation": "room-7",
		"group": true, "addressed": true, "text": "hello", "messageId": "g-1",
	})
	if !h.s.DB.IsChatRoom(at.Seq, "room-7") {
		t.Fatal("a group message did not leave the room on record, so nothing downstream can know")
	}

	// Now the house tries to say something into it.
	_, err := h.s.QueueChatPush(context.Background(), at, "room-7", binding.ID,
		"sam, your prescription is ready for collection", "corr-1", "")
	if err == nil {
		t.Fatal("the house sent what it knows into a room (design §8.3; F-156-08)")
	}
	if !strings.Contains(err.Error(), "room") {
		t.Fatalf("the refusal does not say why: %v", err)
	}

	// REFUSED AT THE QUEUE, not dropped at delivery. A row that reached
	// the queue and was quietly discarded later looks, to everything
	// that reads the queue, exactly like a message that was sent.
	//
	// The ONE thing the house says into a room is the redirect, and it
	// is a reply to somebody who just spoke rather than something the
	// house decided to say. It carries nothing: no name, no fact, not
	// even what was asked. Everything the house INITIATES — kind
	// "push" — is what this rule is about.
	for _, q := range h.poll(t, runtimedb.RoleConcierge, "") {
		if q.Target != "room-7" {
			continue
		}
		if q.Kind == "push" {
			t.Fatalf("the house initiated a message into a room: %+v", q)
		}
		if !strings.Contains(q.Body, "one-to-one") {
			t.Fatalf("something other than the redirect was queued to a room: %+v", q)
		}
		if strings.Contains(q.Body, "prescription") || strings.Contains(q.Body, "sam") {
			t.Fatalf("the personal content reached the room: %+v", q)
		}
	}
	// And the direct push from the control is still there, so the
	// refusal above removed exactly one message and not the feature.
	var direct bool
	for _, q := range h.poll(t, runtimedb.RoleConcierge, "") {
		if q.Kind == "push" && strings.Contains(q.Body, "heating") {
			direct = true
		}
	}
	if !direct {
		t.Fatal("the direct push went missing too; the guard is refusing more than rooms")
	}
}

// TestCrossConnectionFallbackRequiresExplicitConfiguration — §8.3:
// "Cross-Connection delivery fallback occurs only when explicitly
// configured — never as a default."
//
// THE DEFAULT USED TO BE THE FALLBACK. ChatRecipientFor walked every
// live binding on a transport and returned the first match, and
// ListChatBindings orders newest first — so a household with two
// Member-Agent bots on Telegram delivered a rule's message to whichever
// Connection had been bound most recently. Nobody configured that,
// nobody could see it, and which Agent spoke for the house depended on
// the order of a list.
func TestCrossConnectionFallbackRequiresExplicitConfiguration(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
	h.person("sam")
	first := h.agentAt(t, "family", "telegram")
	h.bindAccount(t, "family", "sam", "telegram", "12345")

	// One Connection, one candidate: delivery works and names it. The
	// control, because a rule that refuses everything would pass every
	// assertion below.
	target, _, connection, err := h.s.ChatRecipientFor("telegram", "sam")
	if err != nil {
		t.Fatalf("one connection should need no configuration: %v", err)
	}
	if connection != first.Seq || target == "" {
		t.Fatalf("recipient = %q on connection %d, want %d", target, connection, first.Seq)
	}

	// A second Member Agent, on the same transport, that sam is also
	// admitted to. Both could carry it, and nothing says which should.
	second := h.secondFamilyAgent(t, "joan", "telegram", "12345")
	if _, _, _, err := h.s.ChatRecipientFor("telegram", "sam"); err == nil {
		t.Fatal("the house chose between two connections with nothing configured (design §8.3)")
	} else {
		// The refusal NAMES the choices. A caller told only "there is
		// more than one" has been refused with no way forward — the
		// same answer ErrAmbiguousConnection already gives elsewhere.
		if !strings.Contains(err.Error(), first.Butler) || !strings.Contains(err.Error(), second.Butler) {
			t.Fatalf("the refusal does not name the candidates: %v", err)
		}
	}

	// Configured explicitly, and delivery resumes — on the Connection
	// that was named, not on the one that happened to sort first.
	if _, err := h.s.DB.ExecForTest(
		`UPDATE connection_disclosure SET accepts_fallback = 1 WHERE connection = ?`,
		second.Seq); err != nil {
		t.Fatal(err)
	}
	_, _, connection, err = h.s.ChatRecipientFor("telegram", "sam")
	if err != nil {
		t.Fatalf("an explicitly configured fallback was still refused: %v", err)
	}
	if connection != second.Seq {
		t.Fatalf("delivery went to connection %d, want the configured %d", connection, second.Seq)
	}

	// MARKING BOTH IS NOT A CONFIGURATION, it is the same ambiguity
	// spelled differently, and it must not resolve to "the first one".
	if _, err := h.s.DB.ExecForTest(
		`UPDATE connection_disclosure SET accepts_fallback = 1 WHERE connection = ?`,
		first.Seq); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.s.ChatRecipientFor("telegram", "sam"); err == nil {
		t.Fatal("two connections both marked to take the overflow resolved to one of them")
	}
}

// secondFamilyAgent creates another Member Agent with its own bot on
// the same transport, and admits an account to it.
//
// Two Member Agents on one transport is not a contrivance: ADR-W033 B7
// allows an Agent many Connections and a household many Agents, and
// "one bot for the kitchen, one for the kids" is the obvious shape.
// The bug it exposes is that the delivery lookup keyed on the
// transport alone.
func (h *harness) secondFamilyAgent(t *testing.T, agent, transport, account string) runtimedb.Connection {
	t.Helper()
	if _, err := h.s.DB.CreateButler(runtimedb.Butler{
		Name: agent, Directive: "look after the house",
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember,
		Model: "kitchen-box",
	}, "root-admin"); err != nil {
		t.Fatalf("CreateButler(%s): %v", agent, err)
	}
	capability := chatCapabilityPrefix + transport + "-" + agent
	if _, err := h.s.DB.AddButlerAddress(agent, transport, capability, "", "root-admin"); err != nil {
		t.Fatalf("addressing %s: %v", agent, err)
	}
	at, err := h.s.DB.ChatConnectionAt(transport, capability)
	if err != nil {
		t.Fatalf("resolving %s: %v", capability, err)
	}
	// Admitted the way anyone is: a code issued and redeemed.
	hash := "hash-second-" + agent + "-" + account
	if err := h.s.DB.InsertChatClaimCode(hash, runtimedb.ChatClaimCode{
		Connection: at.Seq, Identity: "sam", CreatedBy: "root-admin",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("issuing a code on %s: %v", agent, err)
	}
	if _, err := h.s.DB.RedeemChatClaimCode(hash, at, account, "sam", "conv-"+agent); err != nil {
		t.Fatalf("redeeming on %s: %v", agent, err)
	}
	return at
}
