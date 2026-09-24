package main

import (
	"context"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

// fakeClient is an in-memory agentapi.Client + agentapi.DeliveryClient
// for tests that want to drive the bridge without a real Keystone HTTP
// server. Every method records that it was called; behaviour is
// injected per test through the *Fn fields, defaulting to a harmless
// zero-value success.
type fakeClient struct {
	manifestFn   func(ctx context.Context, h agentapi.Handle) (agentapi.Manifest, error)
	invokeFn     func(ctx context.Context, req agentapi.InvokeRequest) (agentapi.InvokeResult, error)
	startFn      func(ctx context.Context, w agentapi.WorkStart) error
	waitFn       func(ctx context.Context, w agentapi.WorkWait) error
	completeFn   func(ctx context.Context, w agentapi.WorkComplete) error
	failFn       func(ctx context.Context, w agentapi.WorkFail) error
	heartbeatFn  func(ctx context.Context, hb agentapi.Heartbeat) error
	deliveriesFn func(ctx context.Context, after string, wait time.Duration) ([]agentapi.Delivery, error)
	ackFn        func(ctx context.Context, id string) error

	waitCalls     []agentapi.WorkWait
	startCalls    []agentapi.WorkStart
	completeCalls []agentapi.WorkComplete
	failCalls     []agentapi.WorkFail
	ackCalls      []string
}

var _ keystoneClient = (*fakeClient)(nil)

func (f *fakeClient) Manifest(ctx context.Context, h agentapi.Handle) (agentapi.Manifest, error) {
	if f.manifestFn != nil {
		return f.manifestFn(ctx, h)
	}
	return agentapi.Manifest{}, nil
}

func (f *fakeClient) Invoke(ctx context.Context, req agentapi.InvokeRequest) (agentapi.InvokeResult, error) {
	if f.invokeFn != nil {
		return f.invokeFn(ctx, req)
	}
	return agentapi.InvokeResult{Outcome: agentapi.OutcomeOK}, nil
}

func (f *fakeClient) Recall(ctx context.Context, req agentapi.RecallRequest) ([]agentapi.MemoryHit, error) {
	return nil, nil
}

func (f *fakeClient) Remember(ctx context.Context, req agentapi.RememberRequest) error { return nil }

func (f *fakeClient) Checkpoint(ctx context.Context, c agentapi.Checkpoint) (agentapi.CheckpointReceipt, error) {
	return agentapi.CheckpointReceipt{}, nil
}

func (f *fakeClient) Start(ctx context.Context, w agentapi.WorkStart) error {
	f.startCalls = append(f.startCalls, w)
	if f.startFn != nil {
		return f.startFn(ctx, w)
	}
	return nil
}

func (f *fakeClient) Wait(ctx context.Context, w agentapi.WorkWait) error {
	f.waitCalls = append(f.waitCalls, w)
	if f.waitFn != nil {
		return f.waitFn(ctx, w)
	}
	return nil
}

func (f *fakeClient) Complete(ctx context.Context, w agentapi.WorkComplete) error {
	f.completeCalls = append(f.completeCalls, w)
	if f.completeFn != nil {
		return f.completeFn(ctx, w)
	}
	return nil
}

func (f *fakeClient) Fail(ctx context.Context, w agentapi.WorkFail) error {
	f.failCalls = append(f.failCalls, w)
	if f.failFn != nil {
		return f.failFn(ctx, w)
	}
	return nil
}

func (f *fakeClient) Propose(ctx context.Context, req agentapi.ProposeRequest) (agentapi.ProposalRef, error) {
	return agentapi.ProposalRef{}, nil
}

func (f *fakeClient) Notify(ctx context.Context, req agentapi.NotifyRequest) error { return nil }

func (f *fakeClient) Heartbeat(ctx context.Context, hb agentapi.Heartbeat) error {
	if f.heartbeatFn != nil {
		return f.heartbeatFn(ctx, hb)
	}
	return nil
}

func (f *fakeClient) Deliveries(ctx context.Context, after string, wait time.Duration) ([]agentapi.Delivery, error) {
	if f.deliveriesFn != nil {
		return f.deliveriesFn(ctx, after, wait)
	}
	return nil, nil
}

func (f *fakeClient) Ack(ctx context.Context, id string) error {
	f.ackCalls = append(f.ackCalls, id)
	if f.ackFn != nil {
		return f.ackFn(ctx, id)
	}
	return nil
}

// fakeHermes is an in-memory hermesRunner for delivery-loop tests.
type fakeHermes struct {
	startRunFn   func(ctx context.Context, req hermesRunRequest, idempotencyKey string) (string, error)
	waitForRunFn func(ctx context.Context, runID string) (runOutcome, error)
}

var _ hermesRunner = (*fakeHermes)(nil)

func (f *fakeHermes) StartRun(ctx context.Context, req hermesRunRequest, idempotencyKey string) (string, error) {
	if f.startRunFn != nil {
		return f.startRunFn(ctx, req, idempotencyKey)
	}
	return "run-1", nil
}

func (f *fakeHermes) WaitForRun(ctx context.Context, runID string) (runOutcome, error) {
	if f.waitForRunFn != nil {
		return f.waitForRunFn(ctx, runID)
	}
	return runOutcome{Terminal: "completed"}, nil
}
