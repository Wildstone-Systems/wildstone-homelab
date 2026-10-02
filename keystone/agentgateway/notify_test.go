package agentgateway

// Notify's own positive evidence (ADR-W039 §3.2 Notifications, §4 item
// 8; Phase 4, #341). TestNotifyRefusesATaskWithNoConnection in
// gateway_test.go is the refusal half; this is the success half, and
// the room refusal, both exercised over the same in-process Gateway the
// rest of this package's tests use.

import (
	"context"
	"errors"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func (f *fixture) openTaskOnConnection(t *testing.T, butlerName, owner, transport, externalID string) (string, agentapi.Handle, int64) {
	t.Helper()
	connSeq, err := f.db.AddButlerAddress(butlerName, transport, externalID, "", "mika")
	if err != nil {
		t.Fatalf("AddButlerAddress: %v", err)
	}
	b, err := f.db.GetButler(butlerName)
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.db.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: owner, Owner: owner,
		Surface: b.Surface, AgentClass: b.AgentClass, Audience: runtimedb.ButlerAudiencePrivate,
		OriginConnection: transport + ":" + externalID, OriginConnectionRef: connSeq, OriginConversation: "synthetic-conversation",
		Objective: "answer " + owner, ObjectiveKind: runtimedb.ObjectiveInformational,
		Retention: runtimedb.ButlerRetentionConversation,
	})
	if err != nil {
		t.Fatalf("InsertButlerTask: %v", err)
	}
	if err := f.db.SetTaskButler(id, butlerName); err != nil {
		t.Fatalf("SetTaskButler: %v", err)
	}
	h, err := f.ledger.IssueHandle(id)
	if err != nil {
		t.Fatalf("IssueHandle: %v", err)
	}
	return id, h, connSeq
}

func TestNotifyQueuesOntoTheTasksOwnConnection(t *testing.T) {
	f := newFixture(t)
	_, h, connSeq := f.openTaskOnConnection(t, "joan", "sam", "telegram", "chat-joan")
	if err := f.g.Notify(context.Background(), agentapi.NotifyRequest{Handle: h, Text: "hi from joan"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	queued, err := f.db.ChatOutboundAfter(runtimedb.Connection{Seq: connSeq}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Body != "hi from joan" {
		t.Fatalf("queued = %+v", queued)
	}

	// Same DedupeKey collapses a repeat, the stall notifier's own
	// contract (§4 item 8's own "dedupe").
	if err := f.g.Notify(context.Background(), agentapi.NotifyRequest{Handle: h, Text: "hi from joan", DedupeKey: "same"}); err != nil {
		t.Fatalf("first deduped Notify: %v", err)
	}
	if err := f.g.Notify(context.Background(), agentapi.NotifyRequest{Handle: h, Text: "hi from joan", DedupeKey: "same"}); err != nil {
		t.Fatalf("second deduped Notify: %v", err)
	}
	queued, err = f.db.ChatOutboundAfter(runtimedb.Connection{Seq: connSeq}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 2 { // the first untagged notify + one deduped pair
		t.Fatalf("queued after a repeated dedupe key = %d, want 2", len(queued))
	}
}

func TestNotifyRefusesARoom(t *testing.T) {
	f := newFixture(t)
	_, h, connSeq := f.openTaskOnConnection(t, "joan", "sam", "telegram", "the-group")
	if err := f.db.NoteChatRoom(connSeq, "the-group"); err != nil {
		t.Fatal(err)
	}
	err := f.g.Notify(context.Background(), agentapi.NotifyRequest{Handle: h, Text: "should not arrive"})
	if err == nil {
		t.Fatal("Notify into a room was accepted")
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Notify into a room = %v, want ErrRefused", err)
	}
}
