package leader

import (
	"context"
	"fmt"
)

// Issuer asks Keystone for a new epoch with this node as leader
// (keystone/clusterauthority). The request over the Wardstone protocol is
// wired in slice 3c.
type Issuer interface {
	Issue(ctx context.Context, leader string) (uint64, error)
}

// Term is one leadership term this node may write in.
type Term struct {
	Epoch uint64
}

// Leader ties election, the epoch and the barrier together.
type Leader struct {
	Elector *Elector
	Issuer  Issuer
	Barrier Barrier
	// Serve runs for as long as the term lasts: its context is cancelled
	// when leadership is lost, and it must stop writing then (contract
	// §2.5). Returning ends the term and releases the Lease; return an
	// error on an epoch rejection.
	Serve func(ctx context.Context, t Term) error
	// Incident reports a term that could not start: no epoch, or a
	// barrier that stayed shut. May be nil.
	Incident func(error)
}

// Run campaigns until ctx ends.
func (l Leader) Run(ctx context.Context) error {
	return l.Elector.Run(ctx, func(ctx context.Context) error {
		epoch, err := l.Issuer.Issue(ctx, l.Elector.Identity)
		if err != nil {
			return l.incident(ctx, fmt.Errorf("leader: no epoch from Keystone: %w", err))
		}
		if err := l.Barrier.Wait(ctx, epoch); err != nil {
			return l.incident(ctx, err)
		}
		return l.Serve(ctx, Term{Epoch: epoch})
	})
}

// incident reports err, unless the term ended because leadership did.
func (l Leader) incident(ctx context.Context, err error) error {
	if ctx.Err() == nil && l.Incident != nil {
		l.Incident(err)
	}
	return err
}
