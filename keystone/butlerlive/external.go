package butlerlive

// Serving a turn through an external runtime (ADR-W039 §3.1/§3.7, #364).
//
// A Butler whose runtime is not native answers only through that
// runtime. Serve's offline branch already parks a turn when the runtime
// cannot be reached; this is the connected half: the turn is handed to
// the runtime as a DeliveryTurn, and the answer it reports back is
// accepted exactly as the native runtime's is — through a Slice, whose
// Finish runs the tool-free objective check and completes with evidence
// from the ledger. The runtime reports an answer; it never completes its
// own task, because a runtime saying it is done is not evidence that it
// is (ADR-W030 A4).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentruntime/legacy"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// externalReplyWait bounds how long a turn waits for its runtime's
// answer. Past it the person is told the work is still going, and the
// task carries on without them.
const externalReplyWait = 2 * time.Minute

// externalStillWorking is what the person sees when the runtime has not
// answered inside externalReplyWait. It is true by construction: the
// task is still open and the runtime still holds its handle.
const externalStillWorking = "I'm still working on this and it is taking longer than expected. The task is still open."

// ExternalTurn is a DeliveryTurn's payload for a turn handed to an
// external runtime: the task handle, what the person said, and the
// conversation so far. Text and History are data, never instructions to
// Wildstone; a runtime treats them as the person's words.
type ExternalTurn struct {
	Handle  string              `json:"handle"`
	Text    string              `json:"text,omitempty"`
	History []ExternalUtterance `json:"history,omitempty"`
}

// ExternalUtterance is one earlier message in the conversation.
type ExternalUtterance struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// runtimeReply is what a runtime reported for a task a turn is waiting
// on: its answer, or why it could not produce one.
//
// An answer carries the handle the runtime presented and a done channel
// the settlement result is sent on: the runtime's Complete returns only
// once the answer has been judged and recorded, so a failure there
// reaches the runtime, which leaves its delivery unacknowledged and
// tries again under the same handle.
type runtimeReply struct {
	text   string
	failed string
	handle agentapi.Handle
	done   chan error
}

// settled reports a settlement result to whoever is waiting on r.
func (r runtimeReply) settled(err error) {
	if r.done != nil {
		r.done <- err
	}
}

// externalRuntime reports whether b answers through a runtime other
// than the native one. A Butler with no runtime record is native (see
// runtimeOffline).
func (bs *Butlers) externalRuntime(b runtimedb.Butler) (agentapi.RuntimeType, bool, error) {
	rt, err := bs.DB.GetButlerRuntime(b.Seq)
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	return rt.Type, rt.Type != agentapi.RuntimeNative, nil
}

func (bs *Butlers) replyWait() time.Duration {
	if bs.ExternalReplyWait > 0 {
		return bs.ExternalReplyWait
	}
	return externalReplyWait
}

// runExternal hands one turn to b's connected runtime and waits,
// boundedly, for its answer. It returns what runNative would: the
// transcript, and an error only when the turn could not be answered.
func (bs *Butlers) runExternal(ctx context.Context, runner *legacy.Runner,
	b runtimedb.Butler, rtType agentapi.RuntimeType, t Turn,
) (*butler.Transcript, error) {
	taskID := runner.Handle.TaskID()
	s, tr, err := runner.BeginSlice(ctx, t.Text)
	if s == nil {
		return tr, err
	}

	replies := bs.awaitReply(taskID)
	defer bs.stopAwaiting(taskID, replies)

	payload, err := json.Marshal(ExternalTurn{
		Handle: string(runner.Handle), Text: t.Text, History: externalHistory(t.History),
	})
	if err != nil {
		return s.Transcript(), err
	}
	if _, err := bs.DB.RecordAgentDelivery(b.Seq, agentapi.DeliveryTurn, string(payload)); err != nil {
		return s.Transcript(), err
	}

	timer := time.NewTimer(bs.replyWait())
	defer timer.Stop()
	select {
	case r := <-replies:
		if r.failed != "" {
			// The runtime already failed the task through Work Fail;
			// there is nothing to finish, only something to say.
			tr := s.Transcript()
			tr.FinalText = fmt.Sprintf("%s's %s runtime could not finish this: %s", b.Name, rtType, r.failed)
			return tr, nil
		}
		err := s.Finish(ctx, r.text)
		r.settled(err)
		return s.Transcript(), err
	case <-timer.C:
		tr := s.Transcript()
		tr.FinalText = externalStillWorking
		return tr, nil
	case <-ctx.Done():
		// The person went away. The runtime keeps its handle and its
		// answer is still settled when it arrives (RuntimeReplied).
		return s.Transcript(), ctx.Err()
	}
}

// externalHistory is the conversation so far, as a runtime takes it.
func externalHistory(history []provider.Message) []ExternalUtterance {
	var out []ExternalUtterance
	for _, u := range utterances(history) {
		out = append(out, ExternalUtterance{Role: u.Role, Text: u.Text})
	}
	return out
}

// awaitReply registers a turn as waiting on taskID's runtime.
func (bs *Butlers) awaitReply(taskID string) chan runtimeReply {
	ch := make(chan runtimeReply, 1)
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.waiting == nil {
		bs.waiting = map[string]chan runtimeReply{}
	}
	bs.waiting[taskID] = ch
	return ch
}

// stopAwaiting unregisters a turn. An answer that arrived after the turn
// stopped listening (it timed out in the same instant) is settled the
// way any other late answer is, so it is not lost with the channel.
func (bs *Butlers) stopAwaiting(taskID string, ch chan runtimeReply) {
	bs.mu.Lock()
	if bs.waiting[taskID] == ch {
		delete(bs.waiting, taskID)
	}
	bs.mu.Unlock()
	select {
	case r := <-ch:
		if r.failed == "" {
			go func() {
				err := bs.settleLateReply(context.Background(), r.handle, r.text)
				if err != nil && bs.Log != nil {
					bs.Log.Error("settling a runtime's late answer", "task", taskID, "err", err)
				}
				r.settled(err)
			}()
		}
	default:
	}
}

// deliverReply hands r to the turn waiting on taskID, reporting whether
// one was.
func (bs *Butlers) deliverReply(taskID string, r runtimeReply) bool {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	ch, ok := bs.waiting[taskID]
	if !ok {
		return false
	}
	select {
	case ch <- r:
		return true
	default:
		// Something is already waiting to be read. The first report
		// stands; a second answer is refused (RuntimeReplied) rather
		// than accepted and dropped.
		return false
	}
}

// waitingOn reports whether a turn is waiting on taskID.
func (bs *Butlers) waitingOn(taskID string) bool {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	_, ok := bs.waiting[taskID]
	return ok
}

// RuntimeReplied accepts an external runtime's final answer for the task
// on h (agentapi.WorkComplete.Reply). The turn waiting on it finishes
// the task; with none waiting — the person was told it is still going,
// or the turn was re-offered after a reconnect — it is finished here, by
// the same Slice.Finish, so the evidence rule is the same either way.
//
// Either way it returns only once the answer is settled, with the
// settlement's own error: the runtime acknowledges its delivery on a
// nil, and an answer that failed to settle must stay retryable.
func (bs *Butlers) RuntimeReplied(ctx context.Context, h agentapi.Handle, reply string) error {
	taskID := h.TaskID()
	r := runtimeReply{text: reply, handle: h, done: make(chan error, 1)}
	if bs.deliverReply(taskID, r) {
		select {
		case err := <-r.done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if bs.waitingOn(taskID) {
		return fmt.Errorf("butlerlive: an answer for %s is already being settled", taskID)
	}
	return bs.settleLateReply(ctx, h, reply)
}

// RuntimeFailed tells the turn waiting on taskID, if any, that its
// runtime failed the task (agentapi.WorkFail).
func (bs *Butlers) RuntimeFailed(taskID, reason string) {
	bs.deliverReply(taskID, runtimeReply{failed: reason})
}

// settleLateReply finishes taskID with an answer no turn is waiting for.
// It can only be done through the Agent Gateway: the runtime reached the
// task through it, and the closure path has no session to rebuild here.
//
// It settles under the runtime's own handle h rather than issuing a new
// one: a new handle would invalidate the one the runtime's delivery
// carries, so a transient failure here would leave a retry of that
// delivery refused, and the answer stranded.
//
// Once settled, the answer goes back to the conversation that asked,
// through LateAnswer. A failure there is logged rather than returned: the
// task is complete, so the runtime retrying its report could only be
// refused, and the answer is still on the task.
func (bs *Butlers) settleLateReply(ctx context.Context, h agentapi.Handle, reply string) error {
	taskID := h.TaskID()
	if bs.AgentClient == nil {
		return fmt.Errorf("butlerlive: %s has no turn waiting for its answer and no agent gateway to settle it through", taskID)
	}
	name, err := bs.DB.TaskButler(taskID)
	if err != nil {
		return err
	}
	b, err := bs.DB.GetButler(name)
	if err != nil {
		return err
	}
	task, err := bs.DB.GetButlerTask(taskID)
	if err != nil {
		return err
	}
	p, err := bs.providerFor(b)
	if err != nil {
		return fmt.Errorf("%w: %s runs on %s, which could not be reached: %v (ADR-W031 A6)",
			runtimedb.ErrButlerUnreachable, b.Name, b.Model, err)
	}
	if bs.UseAgentGateway && bs.WithPrincipal != nil {
		ctx = bs.WithPrincipal(ctx, b.Name)
	}
	runner := &legacy.Runner{
		Client:          bs.AgentClient,
		Handle:          h,
		Ledger:          bs.Ledger,
		VerifyObjective: true,
		MaxSliceRounds:  conversationSliceRounds,
		PermissionLabel: b.Name,
		Audience:        string(task.Audience),
		Actor:           task.Owner,
		StallTimeout:    2 * time.Minute,
		RetryLimit:      1,
		Notifier:        bs.Notifier,
		Now:             bs.Now,
		Log:             bs.Log,
		Provider:        p,
	}
	s, _, err := runner.BeginSlice(ctx, task.Objective)
	if s == nil {
		return err
	}
	if err := s.Finish(ctx, reply); err != nil {
		return err
	}
	if bs.Log != nil {
		bs.Log.Info("butler answered after its turn stopped waiting", "butler", b.Name, "model", b.Model,
			"task", taskID, "loop", "external")
	}
	if bs.LateAnswer != nil {
		if err := bs.LateAnswer(taskID, s.Transcript().FinalText); err != nil && bs.Log != nil {
			bs.Log.Error("showing a late answer in its conversation", "butler", b.Name, "task", taskID, "err", err)
		}
	}
	return nil
}
