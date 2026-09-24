package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

func testBridge(t *testing.T, ks *fakeClient, hc *fakeHermes) (*bridge, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	return &bridge{keystone: ks, hermes: hc, runs: newRunRegistry(), log: log, pollWait: time.Second, version: "test"}, &logBuf
}

func turnDelivery(id, taskID string) agentapi.Delivery {
	payload, _ := json.Marshal(map[string]string{"handle": taskID + ".secret"})
	return agentapi.Delivery{ID: id, Kind: agentapi.DeliveryTurn, Handle: agentapi.Handle(taskID + ".secret"), Payload: payload}
}

func TestHandleTurnCompletesAndAcksInOrder(t *testing.T) {
	var order []string
	ks := &fakeClient{
		manifestFn: func(ctx context.Context, h agentapi.Handle) (agentapi.Manifest, error) {
			order = append(order, "manifest")
			return agentapi.Manifest{SystemPrompt: "be helpful"}, nil
		},
		startFn: func(ctx context.Context, w agentapi.WorkStart) error {
			order = append(order, "start")
			return nil
		},
		completeFn: func(ctx context.Context, w agentapi.WorkComplete) error {
			order = append(order, "complete")
			return nil
		},
		ackFn: func(ctx context.Context, id string) error {
			order = append(order, "ack")
			return nil
		},
	}
	hc := &fakeHermes{
		startRunFn: func(ctx context.Context, req hermesRunRequest, idempotencyKey string) (string, error) {
			order = append(order, "run-start")
			if idempotencyKey != "d1" {
				t.Errorf("idempotency key = %q, want the delivery id", idempotencyKey)
			}
			if req.System != "be helpful" {
				t.Errorf("run request system = %q, want the manifest's system prompt", req.System)
			}
			return "run-1", nil
		},
		waitForRunFn: func(ctx context.Context, runID string) (runOutcome, error) {
			order = append(order, "run-wait")
			return runOutcome{Terminal: "completed"}, nil
		},
	}
	br, _ := testBridge(t, ks, hc)

	ok := br.handleDelivery(context.Background(), turnDelivery("d1", "task1"))
	if !ok {
		t.Fatal("handleDelivery returned false, want true")
	}
	want := []string{"manifest", "start", "run-start", "run-wait", "complete", "ack"}
	if len(order) != len(want) {
		t.Fatalf("call order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("call order = %v, want %v", order, want)
		}
	}
	if len(ks.ackCalls) != 1 || ks.ackCalls[0] != "d1" {
		t.Fatalf("ackCalls = %v, want [d1]", ks.ackCalls)
	}
}

func TestHandleTurnHermesUnreachableDoesNotAck(t *testing.T) {
	ks := &fakeClient{}
	hc := &fakeHermes{
		startRunFn: func(ctx context.Context, req hermesRunRequest, idempotencyKey string) (string, error) {
			return "", errors.New("connection refused")
		},
	}
	br, _ := testBridge(t, ks, hc)

	ok := br.handleDelivery(context.Background(), turnDelivery("d1", "task1"))
	if ok {
		t.Fatal("handleDelivery returned true on a hermes failure, want false")
	}
	if len(ks.ackCalls) != 0 {
		t.Fatalf("ackCalls = %v, want none", ks.ackCalls)
	}
	if len(ks.waitCalls) != 1 || ks.waitCalls[0].Reason != agentapi.WorkWaitModelUnavailable {
		t.Fatalf("waitCalls = %v, want one WorkWaitModelUnavailable", ks.waitCalls)
	}
}

func TestHandleTurnRunFailureFailsTask(t *testing.T) {
	ks := &fakeClient{}
	hc := &fakeHermes{
		waitForRunFn: func(ctx context.Context, runID string) (runOutcome, error) {
			return runOutcome{Terminal: "failed", Detail: "boom"}, nil
		},
	}
	br, _ := testBridge(t, ks, hc)

	ok := br.handleDelivery(context.Background(), turnDelivery("d1", "task1"))
	if !ok {
		t.Fatal("handleDelivery returned false, want true (the delivery itself was settled)")
	}
	if len(ks.failCalls) != 1 {
		t.Fatalf("failCalls = %v, want one", ks.failCalls)
	}
	if ks.failCalls[0].Reason != "failed: boom" {
		t.Fatalf("fail reason = %q, want %q", ks.failCalls[0].Reason, "failed: boom")
	}
	if len(ks.completeCalls) != 0 {
		t.Fatalf("completeCalls = %v, want none", ks.completeCalls)
	}
	if len(ks.ackCalls) != 1 {
		t.Fatalf("ackCalls = %v, want one", ks.ackCalls)
	}
}

func TestHandleEscalationAcksWithoutFailAndLogsOnce(t *testing.T) {
	ks := &fakeClient{}
	hc := &fakeHermes{}
	br, logBuf := testBridge(t, ks, hc)

	d := agentapi.Delivery{ID: "e1", Kind: agentapi.DeliveryEscalation, Payload: json.RawMessage(`{"rule":"r","instruction":"i","fallback":"f"}`)}
	if !br.handleDelivery(context.Background(), d) {
		t.Fatal("handleDelivery returned false for an escalation, want true (no-op ack)")
	}
	if !br.handleDelivery(context.Background(), agentapi.Delivery{ID: "e2", Kind: agentapi.DeliveryEscalation}) {
		t.Fatal("handleDelivery returned false for a second escalation, want true")
	}
	if len(ks.failCalls) != 0 {
		t.Fatalf("failCalls = %v, want none: an escalation delivery must never fail a task", ks.failCalls)
	}
	if len(ks.startCalls) != 0 || len(ks.completeCalls) != 0 {
		t.Fatalf("an escalation delivery must never touch Work: start=%v complete=%v", ks.startCalls, ks.completeCalls)
	}
	if got := len(ks.ackCalls); got != 2 {
		t.Fatalf("ackCalls = %v, want 2 acks (e1, e2)", ks.ackCalls)
	}
	// "log once": the warning line should appear exactly once even
	// though two escalations were handled.
	count := bytes.Count(logBuf.Bytes(), []byte("no answer path yet"))
	if count != 1 {
		t.Fatalf("escalation warning logged %d times, want exactly 1", count)
	}
}

func TestHandleDeliveryUnknownKindAcksWithoutAction(t *testing.T) {
	ks := &fakeClient{}
	br, _ := testBridge(t, ks, &fakeHermes{})
	d := agentapi.Delivery{ID: "p1", Kind: agentapi.DeliveryProposalOutcome, Payload: json.RawMessage(`{}`)}
	if !br.handleDelivery(context.Background(), d) {
		t.Fatal("handleDelivery returned false for an unhandled kind, want true")
	}
	if len(ks.startCalls) != 0 {
		t.Fatalf("startCalls = %v, want none", ks.startCalls)
	}
}

func TestHandleTurnMissingHandleAcksRatherThanLoopsForever(t *testing.T) {
	ks := &fakeClient{}
	br, _ := testBridge(t, ks, &fakeHermes{})
	d := agentapi.Delivery{ID: "d1", Kind: agentapi.DeliveryTurn} // no Handle
	if !br.handleDelivery(context.Background(), d) {
		t.Fatal("handleDelivery returned false for a handle-less turn, want true (acked to avoid an infinite retry)")
	}
	if len(ks.ackCalls) != 1 {
		t.Fatalf("ackCalls = %v, want one", ks.ackCalls)
	}
}

// TestDeliveryLoopStopsAdvancingOnFirstFailure drives the loop for
// exactly one poll: the first delivery in the batch cannot settle
// (Hermes is down), and the second, later delivery must not be acked
// either — the loop must not race ahead of the one it could not
// complete, or that delivery would never be retried once `after` moved
// past it.
func TestDeliveryLoopStopsAdvancingOnFirstFailure(t *testing.T) {
	polled := 0
	ks := &fakeClient{
		deliveriesFn: func(ctx context.Context, after string, wait time.Duration) ([]agentapi.Delivery, error) {
			polled++
			if polled == 1 {
				return []agentapi.Delivery{turnDelivery("1", "taskA"), turnDelivery("2", "taskB")}, nil
			}
			// Every poll after the first blocks until the test is done
			// with it, so the loop never spins or sleeps for real.
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	hc := &fakeHermes{
		startRunFn: func(ctx context.Context, req hermesRunRequest, idempotencyKey string) (string, error) {
			return "", errors.New("hermes down")
		},
	}
	br, _ := testBridge(t, ks, hc)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { br.deliveryLoop(ctx); close(done) }()
	// Give the first poll's synchronous processing a moment to run
	// before tearing the loop down.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deliveryLoop did not stop after its context was cancelled")
	}

	if len(ks.ackCalls) != 0 {
		t.Fatalf("ackCalls = %v, want none: both deliveries should remain unacked", ks.ackCalls)
	}
}

func TestSleepBackoffDoubles(t *testing.T) {
	d := sleepBackoff(context.Background(), time.Millisecond)
	if d != 2*time.Millisecond {
		t.Fatalf("first backoff = %v, want 2ms", d)
	}
}
