package api

import (
	"fmt"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// DeliverLateAnswer shows the person an answer that was settled after
// its turn had already returned (#389). butlerlive calls it through
// Butlers.LateAnswer.
//
// An external runtime has two minutes to answer inside the turn. Past
// that the person is told the work is still going; a turn parked for an
// offline runtime is told it will resume. Either way the conversation
// already has its reply for that turn, and the real answer arrives later
// with nobody waiting for it. Without this it completed the task and
// went nowhere.
//
// It goes where a reply for that turn would have gone:
//   - into the conversation itself, as an assistant turn, so the portal
//     and the Console show it when the conversation is opened;
//   - and, when the conversation's reply went out through a chat
//     Connection, as a new message to that same chat, under the same
//     privacy check a live reply passes (handleChatMessage).
//
// The answer can arrive while the turn that stood in for it is still
// being recorded: its conversation not yet linked to the task, or its
// chat reply not yet queued. Delivering then would lose it, or send it
// to the screen only, so it waits (lateAnswerReady) and delivers once
// that bookkeeping is done, in the background, for a bounded time. If
// the chat reply never appears (it failed to queue), the answer still
// goes into the conversation.
func (s *Server) DeliverLateAnswer(taskID, text string) error {
	ready, err := s.lateAnswerReady(taskID)
	if err != nil {
		return err
	}
	if ready {
		return s.deliverLateAnswer(taskID, text)
	}
	go func() {
		wait := s.lateAnswerPoll
		if wait <= 0 {
			wait = time.Second
		}
		for i := 0; i < lateAnswerTries; i++ {
			time.Sleep(wait)
			ready, err := s.lateAnswerReady(taskID)
			if err != nil {
				s.Log.Error("waiting to show a late answer", "task", taskID, "err", err)
				return
			}
			if ready {
				break
			}
		}
		if err := s.deliverLateAnswer(taskID, text); err != nil {
			s.Log.Error("showing a late answer in its conversation", "task", taskID, "err", err)
		}
	}()
	return nil
}

// lateAnswerTries bounds how long a late answer waits for its turn's
// bookkeeping: a minute at the default poll, far longer than recording
// a turn takes.
const lateAnswerTries = 60

// lateAnswerReady reports whether the turn a late answer belongs to has
// been recorded: its task is linked to a conversation, and, for a turn
// that arrived through a chat Connection, its reply has been queued, so
// the answer can follow it to the same chat.
func (s *Server) lateAnswerReady(taskID string) (bool, error) {
	task, err := s.DB.GetButlerTask(taskID)
	if err != nil {
		return false, err
	}
	if task.SessionID == "" {
		return false, nil
	}
	if task.OriginConnectionRef == 0 {
		return true, nil
	}
	_, ok, err := s.DB.ChatReplyRouteForSession(task.SessionID)
	return ok, err
}

// deliverLateAnswer does the delivery DeliverLateAnswer describes.
func (s *Server) deliverLateAnswer(taskID, text string) error {
	task, err := s.DB.GetButlerTask(taskID)
	if err != nil {
		return err
	}
	if task.SessionID == "" {
		return fmt.Errorf("task %s has no conversation to answer into", taskID)
	}
	body := redact.String(text)
	if err := s.DB.AppendButlerTurns(task.SessionID, []runtimedb.ButlerTurn{
		{Role: runtimedb.ButlerRoleAssistant, Text: body},
	}); err != nil {
		return fmt.Errorf("adding the answer to %s: %w", task.SessionID, err)
	}

	route, ok, err := s.DB.ChatReplyRouteForSession(task.SessionID)
	if err != nil || !ok {
		return err
	}
	at, err := s.DB.ChatConnectionBySeq(route.Connection)
	if err != nil {
		return fmt.Errorf("finding the chat %s answered through: %w", task.SessionID, err)
	}
	if err := s.DB.CheckSessionPrivacy(task.SessionID, runtimedb.ModelLocal,
		runtimedb.MemoryDestination{ConnectionRef: at.Seq}); err != nil {
		body = "I can't send that answer here because of its privacy settings. Please continue on your household screen."
	}
	// Keyed on the task: one late answer per task, however many times
	// the settlement that produced it is retried.
	if seq := s.queueChatReply(at, route.Target, body, route.BindingID, taskID,
		"late-answer:"+taskID, task.SessionID); seq == 0 {
		return fmt.Errorf("queuing the answer for %s to its chat failed", task.SessionID)
	}
	return nil
}

// answerPending reports whether a turn's reply stands in for an answer
// still to come: its Agent answers through an external runtime, and the
// task is still running (the turn stopped waiting) or waiting for that
// runtime to reconnect. Only then will DeliverLateAnswer add one.
func (s *Server) answerPending(agent, taskID string) bool {
	if taskID == "" || agent == "" {
		return false
	}
	b, err := s.DB.GetButler(agent)
	if err != nil {
		return false
	}
	rt, err := s.DB.GetButlerRuntime(b.Seq)
	if err != nil || rt.Type == agentapi.RuntimeNative {
		return false
	}
	task, err := s.DB.GetButlerTask(taskID)
	if err != nil {
		return false
	}
	switch task.State {
	case runtimedb.ButlerTaskRunning:
		return true
	case runtimedb.ButlerTaskWaiting:
		waits, err := s.DB.ListButlerTaskWaits(taskID)
		if err != nil || len(waits) == 0 {
			return false
		}
		last := waits[len(waits)-1]
		return last.Reason == runtimedb.ButlerWaitRuntimeOffline && last.ResolvedAt.IsZero()
	}
	return false
}
