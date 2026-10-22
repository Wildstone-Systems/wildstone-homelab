package domestic

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeExecutor records what it was asked to do.
type fakeExecutor struct {
	name  string
	calls []Call
	err   error
	block time.Duration
}

func (f *fakeExecutor) Name() string { return f.name }
func (f *fakeExecutor) Execute(ctx context.Context, c Call) (Result, error) {
	f.calls = append(f.calls, c)
	if f.block > 0 {
		select {
		case <-time.After(f.block):
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	if f.err != nil {
		return Result{}, f.err
	}
	return Result{Summary: "did " + c.Tool}, nil
}

func call(tool, tier string) Call {
	return Call{Tool: tool, Tier: tier, Rule: "breakfast-check", Subject: "mika",
		Params: map[string]string{"topic": "household"}}
}

func gateway(t *testing.T, e *fakeExecutor) *Gateway {
	t.Helper()
	g := &Gateway{Authority: permittedExecution{}, Declared: DeclaredFromPackages([]AgentToolSource{
		{Capability: "notifications", Tools: map[string]string{"notify": "domestic"}},
		{Capability: "pantry", Tools: map[string]string{"inventory": "read"}},
	})}
	if e != nil {
		g.Register(e)
	}
	return g
}

func TestDispatchChecks(t *testing.T) {
	exec := &fakeExecutor{name: "notifications.notify"}
	g := gateway(t, exec)

	res, err := g.Dispatch(context.Background(), call("notifications.notify", "domestic"))
	if err != nil || res.Summary != "did notifications.notify" {
		t.Fatalf("allowed call: %+v, %v", res, err)
	}
	if len(exec.calls) != 1 || exec.calls[0].Subject != "mika" {
		t.Fatalf("executor saw %+v", exec.calls)
	}

	// Every refusal is a *RefusalError, and none of them reach the
	// executor. The tier check is the load-bearing one: it is a second
	// copy of the policy rule, so a rule that got past policy some other
	// way still cannot run an infrastructure action.
	for name, c := range map[string]Call{
		"infrastructure tier":                 call("notifications.notify", "high"),
		"empty tier":                          call("notifications.notify", ""),
		"medium tier":                         call("notifications.notify", "medium"),
		"undeclared tool":                     call("notifications.broadcast", "domestic"),
		"undeclared capability":               call("doors.unlock", "domestic"),
		"tier disagrees with the declaration": call("notifications.notify", "read"),
		"not capability-qualified":            call("notify", "domestic"),
	} {
		before := len(exec.calls)
		_, err := g.Dispatch(context.Background(), c)
		var refusal *RefusalError
		if !errors.As(err, &refusal) {
			t.Errorf("%s: err = %v, want a RefusalError", name, err)
		}
		if len(exec.calls) != before {
			t.Errorf("%s: reached the executor", name)
		}
	}

	// A declared tool with no executor registered on this control plane
	// is a refusal too, and says which.
	g2 := gateway(t, nil)
	_, err = g2.Dispatch(context.Background(), call("notifications.notify", "domestic"))
	if err == nil || !strings.Contains(err.Error(), "no executor is registered") {
		t.Fatalf("unregistered = %v", err)
	}
}

func TestDispatchWithoutCatalogStillChecksTier(t *testing.T) {
	// nil Declared: a deployment with no catalog can still run what it
	// registered, but the tier gate does not lift.
	exec := &fakeExecutor{name: "notifications.notify"}
	g := &Gateway{Authority: permittedExecution{}}
	g.Register(exec)
	if _, err := g.Dispatch(context.Background(), call("notifications.notify", "domestic")); err != nil {
		t.Fatalf("undeclared but registered: %v", err)
	}
	if _, err := g.Dispatch(context.Background(), call("notifications.notify", "high")); err == nil {
		t.Fatal("nil Declared lifted the tier check")
	}
}

func TestDispatchBoundsAndAudits(t *testing.T) {
	exec := &fakeExecutor{name: "notifications.notify", block: time.Second}
	g := gateway(t, exec)
	g.Timeout = 20 * time.Millisecond
	type audited struct {
		call Call
		err  error
	}
	var log []audited
	g.Audit = func(c Call, _ Result, err error) { log = append(log, audited{c, err}) }

	if _, err := g.Dispatch(context.Background(), call("notifications.notify", "domestic")); err == nil {
		t.Fatal("a call past the timeout returned no error")
	}
	// A refusal is audited too: what a rule tried to do is as worth
	// recording as what it did.
	g.Dispatch(context.Background(), call("notifications.notify", "high")) //nolint:errcheck
	if len(log) != 2 || log[0].err == nil || log[1].err == nil {
		t.Fatalf("audit log = %+v", log)
	}
	if g.Tools()[0] != "notifications.notify" || len(g.Tools()) != 1 {
		t.Fatalf("tools = %v", g.Tools())
	}
}

// These unit fixtures isolate tool validation from execution authority. The
// legacy/API and runtime-store tests exercise the real lease and stop policy.
type permittedExecution struct{}

func (permittedExecution) CheckHouseholdExecution() error { return nil }
func TestMissingExecutionAuthorityFailsClosed(t *testing.T) {
	g := &Gateway{}
	e := &fakeExecutor{name: "notifications.notify"}
	g.Register(e)
	if _, err := g.Dispatch(context.Background(), call("notifications.notify", TierDomestic)); err == nil {
		t.Fatal("missing authority allowed effect")
	}
	if len(e.calls) != 0 {
		t.Fatal("unfenced executor reached")
	}
}
