package butlerstore

// The stalled-task notice (ADR-W030 A4's P4 minimum, A10).
//
// When a task stalls the person who asked for it should hear about it
// once, on the surface they were already talking on, and not at three in
// the morning. Those three constraints are the whole of this file:
//
//   - Once. The dedupe key is the task, so a task that stalls, is
//     retried, and stalls again reuses the existing queued message
//     rather than sending a second. That is the outbound queue's own
//     mechanism, not a new one.
//   - The originating direct surface. The notice goes to the binding the
//     task's conversation arrived on — never to a group, never to a
//     broadcast. Broader routing, relevance and urgent exceptions are
//     P15's; sending less than that is safe, and sending more before the
//     policy exists is not.
//   - Not during quiet hours. A non-urgent notice raised inside the
//     household's window is HELD, not dropped: it is queued now with a
//     hold that expires when the window does, so it survives a restart
//     and arrives in the morning.

import (
	"fmt"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// Notifier implements butler.StallNotifier over the existing outbound
// chat queue.
type Notifier struct {
	Ledger *Ledger
	// Binding resolves the direct surface a task's owner was talking on.
	// nil, or a lookup that finds nothing, means this deployment has no
	// direct surface for that person: the stall is still recorded, and
	// nobody is messaged, which is better than messaging the wrong
	// place.
	Binding func(taskID string) (runtimedb.ChatOutbound, bool, error)
}

// NotifyStalled queues one deduplicated stall notice for the task's
// owner, held through quiet hours.
func (n *Notifier) NotifyStalled(taskID, text string) error {
	if n == nil || n.Ledger == nil || n.Binding == nil {
		return nil
	}
	msg, ok, err := n.Binding(taskID)
	if err != nil {
		return fmt.Errorf("butlerstore: resolving the stall notice target: %w", err)
	}
	if !ok {
		return nil
	}
	settings, _, err := n.Ledger.db.GetButlerSettings()
	if err != nil {
		return fmt.Errorf("butlerstore: reading butler settings: %w", err)
	}
	now := n.Ledger.now()

	msg.Body = text
	msg.Kind = "push"
	// One notice per task, forever. A stalled task that stalls again is
	// the same fact, and the household does not need it twice.
	msg.DedupeKey = "butler-task-stalled:" + taskID
	msg.CorrelationID = taskID
	if settings.InQuietHours(now) {
		msg.HoldUntil = settings.QuietHoursEnd(now)
	}
	_, err = n.Ledger.db.EnqueueChatOutbound(msg)
	if err != nil && err != runtimedb.ErrDuplicateOutbound {
		return fmt.Errorf("butlerstore: queuing the stall notice: %w", err)
	}
	return nil
}
