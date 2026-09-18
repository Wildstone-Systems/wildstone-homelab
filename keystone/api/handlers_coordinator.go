package api

// The coordinator lease, made visible and recoverable (ADR-W030 A11).
//
// The store deliberately has no automatic takeover: no election, no
// timeout, no failback. A second Keystone that finds another holder
// refuses to dispatch and says so, and handing the database over is an
// explicit administrative act.
//
// That decision needs these two routes to be operable rather than a
// trap. Without a way to see who holds the lease, "no durable work is
// wired in this process" is a symptom with no diagnosis; without a way
// to promote, a deployment that moved host — or one whose old process
// is genuinely gone — has no route back to doing durable work at all.
//
// Promotion carries `manage` rather than `write`. Deciding which
// process owns the runtime database is administration of the deployment
// itself, not a change to anything the deployment holds.

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// coordinatorView is what the lease looks like from outside. Self is the
// question an operator is actually asking — "is it me?" — answered here
// rather than left to them to compare two opaque strings.
type coordinatorView struct {
	Holder       string    `json:"holder"`
	Epoch        int64     `json:"epoch"`
	AcquiredAt   time.Time `json:"acquiredAt"`
	HeartbeatAt  time.Time `json:"heartbeatAt"`
	PromotedFrom string    `json:"promotedFrom,omitempty"`
	// Self reports whether THIS process holds the lease it is
	// describing. A process that does not hold it serves reads and
	// dispatches no durable work.
	Self bool `json:"self"`
	// Me is this process's own id, so an operator can see what it would
	// promote TO before deciding to.
	Me string `json:"me"`
}

// getCoordinator reports the current lease. It carries `read`: knowing
// which process owns the database is part of reading the deployment's
// state, and an operator diagnosing a silent Butler needs it before
// they hold anything stronger.
func (s *Server) getCoordinator(_ *http.Request, _ principal.Identity) (any, int, error) {
	if s.CoordinatorHolder == "" {
		return nil, http.StatusServiceUnavailable,
			fmt.Errorf("this process does not know its own coordinator identity")
	}
	lease, err := s.DB.GetCoordinator()
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("no coordinator lease has ever been taken")
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return leaseView(lease, s.CoordinatorHolder), http.StatusOK, nil
}

// promoteCoordinator takes the database over for THIS process, fencing
// the previous holder's epoch.
//
// The caller cannot name who to promote. A promotion that took a holder
// id from the request would let one process hand the database to
// another that never asked for it and is not running — which is the
// same brick this route exists to prevent, reached from the other side.
func (s *Server) promoteCoordinator(_ *http.Request, id principal.Identity) (any, int, error) {
	if s.CoordinatorHolder == "" {
		return nil, http.StatusServiceUnavailable,
			fmt.Errorf("this process does not know its own coordinator identity")
	}
	lease, err := s.DB.PromoteCoordinator(s.CoordinatorHolder)
	if err != nil {
		// Already holding it is not an error worth a 500: the operator
		// asked for a state the deployment is already in.
		return nil, http.StatusConflict, err
	}
	// The record of who moved the database, in the same activity log
	// every other administrative act lands in. A promotion fences work
	// that another process may still believe it owns, so "who did this
	// and when" is the first question afterwards.
	s.event("coordinator.promoted", "", id,
		fmt.Sprintf("coordinator lease promoted to %s at epoch %d (fencing %s)",
			lease.Holder, lease.Epoch, lease.PromotedFrom))
	// The promotion took effect in the DATABASE. This process still has
	// to be restarted to pick it up, because the lease a running
	// Keystone dispatches under was captured at startup — saying so is
	// the difference between a working recovery and an operator
	// watching a promoted deployment stay silent.
	return map[string]any{
		"lease": leaseView(lease, s.CoordinatorHolder),
		"note": "the lease is now held by this deployment; restart keystone so the running process " +
			"picks it up and wires durable Butler work",
	}, http.StatusOK, nil
}

func leaseView(l runtimedb.CoordinatorLease, me string) coordinatorView {
	return coordinatorView{
		Holder:       l.Holder,
		Epoch:        l.Epoch,
		AcquiredAt:   l.AcquiredAt,
		HeartbeatAt:  l.HeartbeatAt,
		PromotedFrom: l.PromotedFrom,
		Self:         l.Holder == me,
		Me:           me,
	}
}
