package butlerstore

import (
	"context"
	"errors"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// Research coordinates upstream attempts inside an already admitted ledger tool
// call. It does not register tools, bypass Slice/Invoke budgets, select providers,
// implement caching, or publish results. Every retry/fallback uses Attempt again.
// Both callbacks must recheck current Person/Agent/surface/grants and provenance;
// no request parameter or stored checkpoint grants authority.
type Research struct {
	Ledger    *Ledger
	Authorize func(context.Context, string, string) (runtimedb.ResearchLimits, error)
	// AuthorizeContinuation authenticates an explicit human request and authorizes
	// the exact totals/revision. It returns a trusted actor, never a model string.
	AuthorizeContinuation func(context.Context, string, int64, runtimedb.ResearchLimits) (string, error)
}

func (r *Research) check(ctx context.Context, task, kind string) (runtimedb.ResearchLimits, error) {
	if r.Ledger == nil || r.Authorize == nil {
		return runtimedb.ResearchLimits{}, ErrAuthorityWithdrawn
	}
	if err := ctx.Err(); err != nil {
		return runtimedb.ResearchLimits{}, err
	}
	if err := r.Ledger.Fence(); err != nil {
		return runtimedb.ResearchLimits{}, err
	}
	if err := r.Ledger.Authorize(task); err != nil {
		return runtimedb.ResearchLimits{}, err
	}
	return r.Authorize(ctx, task, kind)
}

// Attempt reserves before calling dispatch exactly once. Validation, queueing,
// cache lookup and policy checks belong before this dispatch boundary. There is
// no refund: failures and cancellation after reservation consume one unit.
func (r *Research) Attempt(ctx context.Context, task string, ledgerAttempt int64, kind string, dispatch func(context.Context) error) (runtimedb.ResearchBudget, error) {
	if dispatch == nil {
		return runtimedb.ResearchBudget{}, runtimedb.ErrResearchConflict
	}
	limits, err := r.check(ctx, task, kind)
	if err != nil {
		return runtimedb.ResearchBudget{}, err
	}
	seq, budget, err := r.Ledger.db.ReserveResearchAttempt(ctx, task, ledgerAttempt, kind, limits)
	if err != nil {
		return budget, err
	}
	// A cancellation between reservation and dispatch is conservatively charged,
	// but must still avoid I/O. A cancelled caller cannot suppress settlement.
	err = ctx.Err()
	if err == nil {
		err = dispatch(ctx)
	}
	state := "succeeded"
	if err != nil {
		state = "failed"
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		state = "cancelled"
	}
	settle := r.Ledger.db.FinishResearchAttempt(task, seq, state)
	return budget, errors.Join(err, settle)
}

// ReadRetained authorizes verified cache/page reads without an upstream spend.
// It must run inside the existing bounded tool call. The callback can only read
// already retained data; a cache miss must return to Attempt for admission.
func (r *Research) ReadRetained(ctx context.Context, task string, read func(context.Context) error) error {
	if read == nil {
		return runtimedb.ErrResearchConflict
	}
	if _, err := r.check(ctx, task, "read"); err != nil {
		return err
	}
	return read(ctx)
}

func (r *Research) Checkpoint(ctx context.Context, task string, epoch int64, c runtimedb.ResearchCheckpoint) error {
	if _, err := r.check(ctx, task, "checkpoint"); err != nil {
		return err
	}
	return r.Ledger.db.SaveResearchCheckpoint(task, epoch, c)
}

func (r *Research) Continue(ctx context.Context, task string, revision int64, limits runtimedb.ResearchLimits) error {
	if r.AuthorizeContinuation == nil {
		return ErrAuthorityWithdrawn
	}
	if _, err := r.check(ctx, task, "continue"); err != nil {
		return err
	}
	actor, err := r.AuthorizeContinuation(ctx, task, revision, limits)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.Ledger.db.ContinueResearch(task, actor, revision, limits)
}
