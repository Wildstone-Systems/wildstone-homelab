package agentgateway

import (
	"context"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime/legacy"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/operation"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

type researchSurface struct {
	*counting
	run func(context.Context) error
}

func (s *researchSurface) Execute(ctx context.Context, _ string, _ map[string]any, _ bool) (butler.ToolOutcome, error) {
	return butler.ToolOutcome{Content: "synthetic retained result"}, s.run(ctx)
}

func TestResearchBudgetParksThroughLegacySliceAndCanonicalGateway(t *testing.T) {
	f := newFixture(t)
	task, h := f.openTask(t, "alfred", "mika")
	ctx := context.Background()
	r := butlerstore.Research{Ledger: f.ledger, Authorize: func(context.Context, string, string) (runtimedb.ResearchLimits, error) {
		return runtimedb.ResearchLimits{Searches: 1, Fetches: 2}, nil
	}}
	original := f.g.Gateways
	calls := 0
	f.g.Gateways = butlerlive.GatewayFunc(func(b runtimedb.Butler, turn butlerlive.Turn) (butler.Toolset, error) {
		surface, err := original.For(b, turn)
		if err != nil {
			return nil, err
		}
		return &researchSurface{counting: surface.(*counting), run: func(ctx context.Context) error {
			id := operation.From(ctx)
			attempts, err := f.db.ListButlerTaskAttempts(id.TaskID)
			if err != nil {
				return err
			}
			var seq int64
			for _, a := range attempts {
				if a.Step == id.Step && a.State == runtimedb.ButlerAttemptDispatched {
					seq = a.Seq
				}
			}
			_, err = r.Attempt(ctx, id.TaskID, seq, "search", func(context.Context) error { calls++; return nil })
			return err
		}}, nil
	})
	runner := legacy.Runner{Client: InProcess(f.g), Handle: h, PermissionLabel: "alfred", Audience: string(runtimedb.ButlerAudiencePrivate), Actor: "mika"}
	slice, _, err := runner.BeginSlice(ctx, "synthetic research")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		result, err := slice.Call(ctx, "describe_capabilities", map[string]any{"n": i})
		if err != nil {
			t.Fatalf("slice %d: %v", i, err)
		}
		if result.Parked != (i == 1) {
			t.Fatalf("slice %d result=%+v", i, result)
		}
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d", calls)
	}
	waits, err := f.db.ListButlerTaskWaits(task)
	if err != nil || len(waits) != 1 || waits[0].Reason != runtimedb.ButlerWaitBudgetExhausted {
		t.Fatalf("waits=%+v %v", waits, err)
	}
	detail := waits[0].Detail
	if err = f.g.Wait(ctx, agentapi.WorkWait{Handle: h, Reason: agentapi.WorkWaitBudgetExhausted, Detail: "runtime acknowledgement"}); err != nil {
		t.Fatal(err)
	}
	waits, err = f.db.ListButlerTaskWaits(task)
	if err != nil || len(waits) != 1 || waits[0].Detail != detail {
		t.Fatalf("wait overwritten: %+v %v", waits, err)
	}
	used, _, _, err := f.ledger.AggregateToolCalls(task)
	if err != nil || used != 2 {
		t.Fatalf("platform calls=%d %v", used, err)
	}
	// An acknowledged wait remains waiting, and the runtime cannot extend it.
	current, err := f.db.GetButlerTask(task)
	if err != nil || current.State != runtimedb.ButlerTaskWaiting {
		t.Fatalf("task=%+v %v", current, err)
	}
	// A human may continue after the admission transaction but before Slice's
	// acknowledgement arrives. The late acknowledgement must not park it again.
	budget, err := f.db.GetResearchBudget(task)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.ContinueResearch(task, "mika", budget.Revision, runtimedb.ResearchLimits{Searches: 2, Fetches: 2}); err != nil {
		t.Fatal(err)
	}
	if err = f.g.Wait(ctx, agentapi.WorkWait{Handle: h, Reason: agentapi.WorkWaitBudgetExhausted, Detail: runtimedb.ErrResearchBudgetExhausted.Error()}); err != nil {
		t.Fatal(err)
	}
	current, err = f.db.GetButlerTask(task)
	if err != nil || current.State != runtimedb.ButlerTaskRunning {
		t.Fatalf("late acknowledgement undid continuation: %+v %v", current, err)
	}

}
