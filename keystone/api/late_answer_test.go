package api

import (
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// An answer settled after its turn returned goes back to where the
// turn's reply went (#389): into the conversation, and to the chat that
// was told it was still being worked on.
func TestALateAnswerReachesTheConversationAndItsChat(t *testing.T) {
	h := newHarness(t)
	if err := h.s.DB.RecordButlerMember("mika", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.AcquireCoordinator("keystone-test"); err != nil {
		t.Fatal(err)
	}
	at := h.agentAt(t, "family", "telegram")
	b, err := h.s.DB.GetButler(at.Butler)
	if err != nil {
		t.Fatal(err)
	}
	sid := insertCandidateSession(t, h, at, "mika", "is n1 healthy?")
	taskID, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: b.Surface, AgentClass: b.AgentClass, Audience: runtimedb.ButlerAudiencePrivate,
		OriginConnection: at.Name(), OriginConnectionRef: at.Seq,
		Objective: "is n1 healthy?", ObjectiveKind: runtimedb.ObjectiveInformational,
		Retention: runtimedb.ButlerRetentionConversation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.SetButlerTaskSession(taskID, sid); err != nil {
		t.Fatal(err)
	}
	// The turn's own reply: the still-working notice, to chat 4242.
	if seq := h.s.queueChatReply(at, "4242", "I'm still working on this", 0, "", "turn-1", sid); seq == 0 {
		t.Fatal("queuing the turn's reply failed")
	}

	if err := h.s.DeliverLateAnswer(taskID, "n1 is fine"); err != nil {
		t.Fatalf("DeliverLateAnswer: %v", err)
	}

	sess, err := h.s.DB.GetButlerSession(sid)
	if err != nil {
		t.Fatal(err)
	}
	last := sess.Turns[len(sess.Turns)-1]
	if last.Role != runtimedb.ButlerRoleAssistant || last.Text != "n1 is fine" {
		t.Fatalf("last turn = %+v, want the late answer", last)
	}
	out, err := h.s.DB.ChatOutboundAfter(at, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1].Target != "4242" || out[1].Body != "n1 is fine" {
		t.Fatalf("chat outbound = %+v, want the late answer sent to 4242 after the notice", out)
	}

	// Settling the same task again sends nothing new to the chat.
	if err := h.s.DeliverLateAnswer(taskID, "n1 is fine"); err != nil {
		t.Fatalf("second DeliverLateAnswer: %v", err)
	}
	if again, _ := h.s.DB.ChatOutboundAfter(at, 0, 10); len(again) != 2 {
		t.Fatalf("chat outbound after a repeat = %d messages, want 2", len(again))
	}
}

// A conversation that never replied into a chat (the portal, the
// Console) gets the answer as a turn and nothing is queued.
func TestALateAnswerForAScreenConversationIsATurnOnly(t *testing.T) {
	h := newHarness(t)
	if err := h.s.DB.RecordButlerMember("mika", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.AcquireCoordinator("keystone-test"); err != nil {
		t.Fatal(err)
	}
	at := h.agentAt(t, "family", "telegram")
	b, err := h.s.DB.GetButler(at.Butler)
	if err != nil {
		t.Fatal(err)
	}
	sid := insertCandidateSession(t, h, at, "mika", "is n1 healthy?")
	taskID, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: b.Surface, AgentClass: b.AgentClass, Audience: runtimedb.ButlerAudiencePrivate,
		Objective: "is n1 healthy?", ObjectiveKind: runtimedb.ObjectiveInformational,
		Retention: runtimedb.ButlerRetentionConversation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.SetButlerTaskSession(taskID, sid); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DeliverLateAnswer(taskID, "n1 is fine"); err != nil {
		t.Fatalf("DeliverLateAnswer: %v", err)
	}
	sess, err := h.s.DB.GetButlerSession(sid)
	if err != nil {
		t.Fatal(err)
	}
	if last := sess.Turns[len(sess.Turns)-1]; last.Text != "n1 is fine" {
		t.Fatalf("last turn = %+v, want the late answer", last)
	}
	if out, _ := h.s.DB.ChatOutboundAfter(at, 0, 10); len(out) != 0 {
		t.Fatalf("chat outbound = %+v, want nothing for a screen conversation", out)
	}
}

// Only a turn whose Agent answers through an external runtime, with its
// task still open, tells a web chat to wait for an answer. A native
// turn's reply is its answer.
func TestOnlyAnExternalRuntimesOpenTaskIsPending(t *testing.T) {
	h := newHarness(t)
	if err := h.s.DB.RecordButlerMember("mika", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	at := h.agentAt(t, "family", "telegram")
	b, err := h.s.DB.GetButler(at.Butler)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: b.Surface, AgentClass: b.AgentClass, Audience: runtimedb.ButlerAudiencePrivate,
		Objective: "is n1 healthy?", ObjectiveKind: runtimedb.ObjectiveInformational,
		Retention: runtimedb.ButlerRetentionConversation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.TransitionButlerTask(taskID, runtimedb.ButlerTaskRunning, "test", "mika"); err != nil {
		t.Fatal(err)
	}
	if h.s.answerPending(b.Name, taskID) {
		t.Fatal("a native Agent's running task reported pending")
	}
	if err := h.s.DB.SetButlerRuntime(b.Seq, runtimedb.ButlerRuntime{Type: agentapi.RuntimeHermes}, "mika"); err != nil {
		t.Fatal(err)
	}
	if !h.s.answerPending(b.Name, taskID) {
		t.Fatal("an external runtime's running task is not pending")
	}
	if err := h.s.DB.TransitionButlerTask(taskID, runtimedb.ButlerTaskFailed, "test", "mika"); err != nil {
		t.Fatal(err)
	}
	if h.s.answerPending(b.Name, taskID) {
		t.Fatal("a finished task reported pending")
	}
}

// A late answer that arrives while its turn is still being recorded
// waits for the conversation link and the chat reply, then follows the
// reply to the chat, rather than being lost or shown on screen only.
func TestALateAnswerWaitsForItsTurnToBeRecorded(t *testing.T) {
	h := newHarness(t)
	h.s.lateAnswerPoll = 10 * time.Millisecond
	if err := h.s.DB.RecordButlerMember("mika", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.AcquireCoordinator("keystone-test"); err != nil {
		t.Fatal(err)
	}
	at := h.agentAt(t, "family", "telegram")
	b, err := h.s.DB.GetButler(at.Butler)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: b.Surface, AgentClass: b.AgentClass, Audience: runtimedb.ButlerAudiencePrivate,
		OriginConnection: at.Name(), OriginConnectionRef: at.Seq,
		Objective: "is n1 healthy?", ObjectiveKind: runtimedb.ObjectiveInformational,
		Retention: runtimedb.ButlerRetentionConversation,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The answer arrives before the turn is linked to its conversation.
	if err := h.s.DeliverLateAnswer(taskID, "n1 is fine"); err != nil {
		t.Fatalf("DeliverLateAnswer: %v", err)
	}
	// Then the turn is recorded: linked, and its reply queued.
	sid := insertCandidateSession(t, h, at, "mika", "is n1 healthy?")
	if err := h.s.DB.SetButlerTaskSession(taskID, sid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if out, _ := h.s.DB.ChatOutboundAfter(at, 0, 10); len(out) != 0 {
		t.Fatalf("the answer went out before the turn's own reply: %+v", out)
	}
	if seq := h.s.queueChatReply(at, "4242", "I'm still working on this", 0, "", "turn-1", sid); seq == 0 {
		t.Fatal("queuing the turn's reply failed")
	}

	deadline := time.Now().Add(3 * time.Second)
	var out []runtimedb.ChatOutbound
	for time.Now().Before(deadline) {
		if out, _ = h.s.DB.ChatOutboundAfter(at, 0, 10); len(out) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(out) != 2 || out[1].Target != "4242" || out[1].Body != "n1 is fine" {
		t.Fatalf("chat outbound = %+v, want the late answer after the turn's reply", out)
	}
	sess, err := h.s.DB.GetButlerSession(sid)
	if err != nil {
		t.Fatal(err)
	}
	if last := sess.Turns[len(sess.Turns)-1]; last.Text != "n1 is fine" {
		t.Fatalf("last turn = %+v, want the late answer", last)
	}
}

// The page's baseline stops at the reply to the newest question, so a
// late answer already in the conversation is still drawn.
func TestSeenStopsAtTheReplyToTheNewestQuestion(t *testing.T) {
	msgs := []assistantMessage{
		{Role: "user", Text: "hi"}, {Role: "assistant", Text: "hello"},
		{Role: "user", Text: "is n1 healthy?"}, {Role: "assistant", Text: "still working"},
		{Role: "assistant", Text: "n1 is fine"},
	}
	if got := seenThroughLastAsk(msgs); got != 4 {
		t.Fatalf("seen = %d, want 4", got)
	}
	if got := seenThroughLastAsk(msgs[:4]); got != 4 {
		t.Fatalf("seen without the late answer = %d, want 4", got)
	}
}
