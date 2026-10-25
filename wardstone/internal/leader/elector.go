// Package leader is Wardstone's half of cluster leadership (W050 slice 3b,
// safety contract #180 §2): Lease election, asking Keystone for an epoch,
// and the epoch barrier that must open on every server before a new term
// writes anything.
//
// The Lease only says who should ask Keystone for the next epoch. It is
// not the fence: the API server's admission policy is, and a term whose
// barrier never opens never writes.
package leader

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi"
)

// LeaseClient is the part of kubeapi.Client the elector uses.
type LeaseClient interface {
	GetLease(ctx context.Context, ns, name string) (*kubeapi.Lease, error)
	CreateLease(ctx context.Context, l *kubeapi.Lease) (*kubeapi.Lease, error)
	UpdateLease(ctx context.Context, l *kubeapi.Lease) (*kubeapi.Lease, error)
}

// Where the Lease lives (deploy/ansible/templates/runtime-rbac.yaml.j2).
const (
	LeaseNamespace = "wildstone-system"
	LeaseName      = "wardstone-leader"
)

// Elector runs Lease election for one identity.
type Elector struct {
	Client LeaseClient
	// Identity is this Wardstone's API username, which is also what the
	// authority record names as leader.
	Identity string

	// LeaseDuration is how long others wait after the last renewal they
	// saw before taking over. RenewDeadline is how long a leader keeps
	// leading without a successful renewal; it must be shorter, so a
	// leader stops before anyone else may start. RetryPeriod is how often
	// both sides try. FailureBackoff is the pause after a term that ended
	// in an error, so a stuck barrier doesn't spin through epochs.
	LeaseDuration  time.Duration
	RenewDeadline  time.Duration
	RetryPeriod    time.Duration
	FailureBackoff time.Duration

	// The last record seen and when, by this node's own clock. Expiry is
	// judged from when this node saw a change, never from the holder's
	// timestamps, so clock skew between nodes doesn't matter.
	observed   *kubeapi.Lease
	observedAt time.Time
}

// Defaults, as in Kubernetes' own controllers.
const (
	DefaultLeaseDuration  = 15 * time.Second
	DefaultRenewDeadline  = 10 * time.Second
	DefaultRetryPeriod    = 2 * time.Second
	DefaultFailureBackoff = 30 * time.Second
)

func (e *Elector) defaults() error {
	if e.Client == nil || e.Identity == "" {
		return errors.New("leader: an elector needs a client and an identity")
	}
	if e.LeaseDuration == 0 {
		e.LeaseDuration = DefaultLeaseDuration
	}
	if e.RenewDeadline == 0 {
		e.RenewDeadline = DefaultRenewDeadline
	}
	if e.RetryPeriod == 0 {
		e.RetryPeriod = DefaultRetryPeriod
	}
	if e.FailureBackoff == 0 {
		e.FailureBackoff = DefaultFailureBackoff
	}
	if e.RenewDeadline >= e.LeaseDuration || e.RetryPeriod >= e.RenewDeadline {
		return fmt.Errorf("leader: need retry (%s) < renew deadline (%s) < lease duration (%s)", e.RetryPeriod, e.RenewDeadline, e.LeaseDuration)
	}
	return nil
}

// Run campaigns for the Lease until ctx ends. Each time this node gains
// it, lead runs with a context that is cancelled the moment leadership is
// lost; Run waits for lead to return before campaigning again, so two
// terms on one node never overlap. When lead returns by itself the Lease
// is released for someone else.
func (e *Elector) Run(ctx context.Context, lead func(context.Context) error) error {
	if err := e.defaults(); err != nil {
		return err
	}
	for {
		if err := e.acquire(ctx); err != nil {
			return err
		}
		termCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- lead(termCtx) }()
		leadErr, returned := e.hold(termCtx, done)
		cancel()
		if !returned {
			leadErr = <-done
		}
		if ctx.Err() != nil {
			e.release()
			return ctx.Err()
		}
		pause := e.RetryPeriod
		if returned {
			e.release()
			if leadErr != nil {
				pause = e.FailureBackoff
			}
		}
		if !sleep(ctx, pause) {
			return ctx.Err()
		}
	}
}

// acquire blocks until this node holds the Lease or ctx ends.
func (e *Elector) acquire(ctx context.Context) error {
	for {
		if held, _ := e.try(ctx); held {
			return nil
		}
		if !sleep(ctx, e.RetryPeriod) {
			return ctx.Err()
		}
	}
}

// hold renews the Lease until lead returns (returned=true, with its
// error), leadership is lost, or ctx ends.
func (e *Elector) hold(ctx context.Context, done <-chan error) (err error, returned bool) {
	lastRenew := time.Now()
	t := time.NewTicker(e.RetryPeriod)
	defer t.Stop()
	for {
		select {
		case err := <-done:
			return err, true
		case <-ctx.Done():
			return nil, false
		case <-t.C:
		}
		rctx, cancel := context.WithTimeout(ctx, e.RenewDeadline)
		held, _ := e.try(rctx)
		cancel()
		if held {
			lastRenew = time.Now()
			continue
		}
		// Someone else holds it now, or the renew deadline passed
		// without a renewal: stop before they may start.
		if e.observed != nil && e.observed.Spec.HolderIdentity != e.Identity && e.observed.Spec.HolderIdentity != "" {
			return nil, false
		}
		if time.Since(lastRenew) >= e.RenewDeadline {
			return nil, false
		}
	}
}

// try acquires or renews once. held reports whether this node holds the
// Lease afterwards.
func (e *Elector) try(ctx context.Context) (held bool, err error) {
	now := time.Now()
	secs := int32(e.LeaseDuration / time.Second)
	if secs < 1 {
		secs = 1
	}
	l, err := e.Client.GetLease(ctx, LeaseNamespace, LeaseName)
	if kubeapi.IsNotFound(err) {
		created, err := e.Client.CreateLease(ctx, &kubeapi.Lease{
			Metadata: kubeapi.ObjectMeta{Namespace: LeaseNamespace, Name: LeaseName},
			Spec: kubeapi.LeaseSpec{
				HolderIdentity: e.Identity, LeaseDurationSeconds: secs,
				AcquireTime: &kubeapi.MicroTime{Time: now}, RenewTime: &kubeapi.MicroTime{Time: now},
			},
		})
		if err != nil {
			return false, err
		}
		e.observe(created, now)
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if e.observed == nil || l.Metadata.ResourceVersion != e.observed.Metadata.ResourceVersion {
		e.observe(l, now)
	}
	holder := l.Spec.HolderIdentity
	dur := time.Duration(l.Spec.LeaseDurationSeconds) * time.Second
	if dur <= 0 {
		dur = e.LeaseDuration
	}
	if holder != "" && holder != e.Identity && now.Before(e.observedAt.Add(dur)) {
		return false, nil
	}
	upd := *l
	upd.Spec.LeaseDurationSeconds = secs
	upd.Spec.RenewTime = &kubeapi.MicroTime{Time: now}
	if holder != e.Identity {
		upd.Spec.HolderIdentity = e.Identity
		upd.Spec.AcquireTime = &kubeapi.MicroTime{Time: now}
		upd.Spec.LeaseTransitions++
	}
	out, err := e.Client.UpdateLease(ctx, &upd)
	if err != nil {
		return false, err
	}
	e.observe(out, now)
	return true, nil
}

func (e *Elector) observe(l *kubeapi.Lease, at time.Time) {
	e.observed, e.observedAt = l, at
}

// release hands the Lease back if this node still holds it, so the next
// leader needn't wait out the full duration.
func (e *Elector) release() {
	ctx, cancel := context.WithTimeout(context.Background(), e.RetryPeriod)
	defer cancel()
	l, err := e.Client.GetLease(ctx, LeaseNamespace, LeaseName)
	if err != nil || l.Spec.HolderIdentity != e.Identity {
		return
	}
	upd := *l
	upd.Spec.HolderIdentity = ""
	upd.Spec.LeaseDurationSeconds = 1
	upd.Spec.RenewTime = &kubeapi.MicroTime{Time: time.Now()}
	if out, err := e.Client.UpdateLease(ctx, &upd); err == nil {
		e.observe(out, time.Now())
	}
}

// sleep waits d or until ctx ends; false means ctx ended.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
