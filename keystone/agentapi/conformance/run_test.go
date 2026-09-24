package conformance_test

// A minimal, wholly in-memory agentapi.Client/DeliveryClient — not
// keystone/agentgateway's, not keystone/agentapi/httpclient's — proving
// this suite runs against ANY implementation of the contract, which is
// the whole point of the package (ADR-W039 §8 Phase 5, #343). It is
// deliberately small: just enough of each family to make every named
// check exercise something real, never a stub that always answers ok.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi/conformance"
)

// fakeClient is one isolated in-memory Agent. Its zero value is not
// usable; use newFakeClient.
type fakeClient struct {
	mu         sync.Mutex
	nextTask   int
	tasks      map[string]*fakeTask
	memory     []agentapi.MemoryHit
	applied    bool // whether the one mutating tool's effect ever "ran"
	proposals  int
	deliverID  int
	deliveries []agentapi.Delivery
}

type fakeTask struct {
	secret string
	state  string // pending, running, waiting, completed
}

func newFakeClient() *fakeClient {
	return &fakeClient{tasks: map[string]*fakeTask{}}
}

var _ agentapi.Client = (*fakeClient)(nil)
var _ agentapi.DeliveryClient = (*fakeClient)(nil)

// openTask mints a fresh handle for a fresh pending task — this fake's
// own version of a Target.Handle closure.
func (f *fakeClient) openTask(t *testing.T) agentapi.Handle {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextTask++
	id := fmt.Sprintf("fake-task-%d", f.nextTask)
	f.tasks[id] = &fakeTask{secret: "s3cr3t", state: "pending"}
	return agentapi.Handle(id + ".s3cr3t")
}

// resolve is every method's first step: split the handle, find the
// task, check the secret. A wrong or unknown handle answers the same
// generic refusal either way, and never echoes the secret it was given.
func (f *fakeClient) resolve(h agentapi.Handle) (*fakeTask, error) {
	id := h.TaskID()
	task, ok := f.tasks[id]
	if !ok {
		return nil, errors.New("fakeClient: no such task")
	}
	_, secret, _ := cutHandle(h)
	if secret != task.secret {
		return nil, errors.New("fakeClient: the handle was refused")
	}
	return task, nil
}

func cutHandle(h agentapi.Handle) (id, secret string, ok bool) {
	s := string(h)
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func (f *fakeClient) Manifest(ctx context.Context, h agentapi.Handle) (agentapi.Manifest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.resolve(h); err != nil {
		return agentapi.Manifest{}, err
	}
	return agentapi.Manifest{
		AgentClass: agentapi.AgentClassOwner,
		Tools: []agentapi.ToolSpec{
			{Name: "peek", Description: "read something", InputSchema: json.RawMessage(`{"type":"object"}`), ReadOnlyHint: true},
			{Name: "propose_change", Description: "propose a change", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}, nil
}

func (f *fakeClient) Invoke(ctx context.Context, req agentapi.InvokeRequest) (agentapi.InvokeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.resolve(req.Handle); err != nil {
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: "the handle was refused"}, nil
	}
	switch req.Tool {
	case "peek":
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeOK, Content: "all clear"}, nil
	case "propose_change":
		f.proposals++
		// applied is deliberately never set true here: propose_change
		// only ever files, it never applies.
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeOK, Proposal: &agentapi.ProposalRef{ID: fmt.Sprintf("p-%d", f.proposals)}}, nil
	default:
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: "unknown tool"}, nil
	}
}

func (f *fakeClient) Recall(ctx context.Context, req agentapi.RecallRequest) ([]agentapi.MemoryHit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.resolve(req.Handle); err != nil {
		return nil, err
	}
	var out []agentapi.MemoryHit
	for _, hit := range f.memory {
		if req.Text == "" || strings.Contains(hit.Content, req.Text) {
			out = append(out, hit)
		}
	}
	return out, nil
}

func (f *fakeClient) Remember(ctx context.Context, req agentapi.RememberRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Scope == agentapi.MemoryScopeHousehold {
		return errors.New("fakeClient: remember cannot create household memory")
	}
	if _, err := f.resolve(req.Handle); err != nil {
		return err
	}
	scope := req.Scope
	if scope == "" {
		scope = agentapi.MemoryScopePersonal
	}
	kind := req.Kind
	if kind == "" {
		kind = agentapi.MemoryFact
	}
	f.memory = append(f.memory, agentapi.MemoryHit{
		Content: req.Content, Kind: kind, Scope: scope, Subject: req.Subject, ObservedAt: time.Now(),
	})
	return nil
}

func (f *fakeClient) Checkpoint(ctx context.Context, c agentapi.Checkpoint) (agentapi.CheckpointReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.resolve(c.Handle); err != nil {
		return agentapi.CheckpointReceipt{}, err
	}
	return agentapi.CheckpointReceipt{Step: 1, Attempt: 1, Epoch: 1}, nil
}

func (f *fakeClient) Start(ctx context.Context, w agentapi.WorkStart) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	task, err := f.resolve(w.Handle)
	if err != nil {
		return err
	}
	task.state = "running"
	return nil
}

func (f *fakeClient) Wait(ctx context.Context, w agentapi.WorkWait) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	task, err := f.resolve(w.Handle)
	if err != nil {
		return err
	}
	if !w.Reason.Valid() {
		return fmt.Errorf("fakeClient: %q is not a wait reason", w.Reason)
	}
	task.state = "waiting"
	return nil
}

func (f *fakeClient) Complete(ctx context.Context, w agentapi.WorkComplete) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	task, err := f.resolve(w.Handle)
	if err != nil {
		return err
	}
	task.state = "completed"
	return nil
}

func (f *fakeClient) Fail(ctx context.Context, w agentapi.WorkFail) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	task, err := f.resolve(w.Handle)
	if err != nil {
		return err
	}
	task.state = "failed"
	return nil
}

func (f *fakeClient) Propose(ctx context.Context, req agentapi.ProposeRequest) (agentapi.ProposalRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.resolve(req.Handle); err != nil {
		return agentapi.ProposalRef{}, err
	}
	f.proposals++
	return agentapi.ProposalRef{ID: fmt.Sprintf("p-%d", f.proposals)}, nil
}

func (f *fakeClient) Notify(ctx context.Context, req agentapi.NotifyRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.resolve(req.Handle)
	return err
}

func (f *fakeClient) Heartbeat(ctx context.Context, hb agentapi.Heartbeat) error {
	return nil
}

func (f *fakeClient) Deliveries(ctx context.Context, after string, wait time.Duration) ([]agentapi.Delivery, error) {
	deadline := time.Now().Add(wait)
	for {
		f.mu.Lock()
		var out []agentapi.Delivery
		for _, d := range f.deliveries {
			if after == "" || d.ID > after {
				out = append(out, d)
			}
		}
		f.mu.Unlock()
		if len(out) > 0 || time.Now().After(deadline) {
			if out == nil {
				out = []agentapi.Delivery{}
			}
			return out, nil
		}
		select {
		case <-ctx.Done():
			return []agentapi.Delivery{}, nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (f *fakeClient) Ack(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.deliveries[:0]
	for _, d := range f.deliveries {
		if d.ID != id {
			kept = append(kept, d)
		}
	}
	f.deliveries = kept
	return nil // acking a missing id is a no-op, i.e. idempotent
}

func (f *fakeClient) seedDelivery() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliverID++
	f.deliveries = append(f.deliveries, agentapi.Delivery{
		ID: fmt.Sprintf("d-%d", f.deliverID), Kind: agentapi.DeliveryProposalOutcome,
		Payload: json.RawMessage(`{"outcome":"approved"}`),
	})
}

// TestFakeClientIsConformant is this package's own proof that Run works
// against an implementation that is not Wildstone's: a from-scratch,
// in-memory agentapi.Client that follows the contract with none of
// keystone/agentgateway's machinery behind it.
func TestFakeClientIsConformant(t *testing.T) {
	f := newFakeClient()
	target := conformance.Target{
		Client:       f,
		Deliveries:   f,
		Handle:       func(t *testing.T) agentapi.Handle { return f.openTask(t) },
		ReadOnlyTool: "peek",
		UnknownTool:  "does-not-exist",
		ProposeTool:  "propose_change",
		NeverApplied: func(t *testing.T) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return !f.applied
		},
		RequireRecallHit: true,
		SeedDelivery:     func(t *testing.T) { f.seedDelivery() },
	}
	conformance.Run(t, target)
}

// TestCompareOfTwoIndependentFakesIsByteIdentical is Compare's own
// proof: two freshly constructed fakeClients, given the identical
// script, answer with byte-identical JSON — the property Compare exists
// to check, demonstrated on two instances of a NON-Wildstone
// implementation rather than only on agentgateway/httpclient.
func TestCompareOfTwoIndependentFakesIsByteIdentical(t *testing.T) {
	fa, fb := newFakeClient(), newFakeClient()
	a := conformance.Target{Client: fa, Handle: func(t *testing.T) agentapi.Handle { return fa.openTask(t) }}
	b := conformance.Target{Client: fb, Handle: func(t *testing.T) agentapi.Handle { return fb.openTask(t) }}

	script := func(t *testing.T, target conformance.Target) any {
		h := target.Handle(t)
		m, err := target.Client.Manifest(target.Ctx(), h)
		if err != nil {
			t.Fatalf("Manifest: %v", err)
		}
		res, err := target.Client.Invoke(target.Ctx(), agentapi.InvokeRequest{Handle: h, Tool: "peek"})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		return struct {
			Manifest agentapi.Manifest
			Invoke   agentapi.InvokeResult
		}{m, res}
	}
	conformance.Compare(t, a, b, script)
}
