package leader

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi/kubeapitest"
)

const (
	idA = "system:serviceaccount:wildstone-system:wardstone-node-a"
	idB = "system:serviceaccount:wildstone-system:wardstone-node-b"
)

// partitionable cuts one node off from the API server.
type partitionable struct {
	LeaseClient
	down atomic.Bool
}

var errPartitioned = errors.New("partitioned")

func (p *partitionable) GetLease(ctx context.Context, ns, name string) (*kubeapi.Lease, error) {
	if p.down.Load() {
		return nil, errPartitioned
	}
	return p.LeaseClient.GetLease(ctx, ns, name)
}

func (p *partitionable) UpdateLease(ctx context.Context, l *kubeapi.Lease) (*kubeapi.Lease, error) {
	if p.down.Load() {
		return nil, errPartitioned
	}
	return p.LeaseClient.UpdateLease(ctx, l)
}

func (p *partitionable) CreateLease(ctx context.Context, l *kubeapi.Lease) (*kubeapi.Lease, error) {
	if p.down.Load() {
		return nil, errPartitioned
	}
	return p.LeaseClient.CreateLease(ctx, l)
}

func fast(c LeaseClient, id string) *Elector {
	return &Elector{
		Client: c, Identity: id,
		LeaseDuration: 1 * time.Second, RenewDeadline: 600 * time.Millisecond,
		RetryPeriod: 50 * time.Millisecond, FailureBackoff: 400 * time.Millisecond,
	}
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestElectorRefusesUnsafeTimings(t *testing.T) {
	e := &Elector{Client: &partitionable{}, Identity: idA,
		LeaseDuration: time.Second, RenewDeadline: 2 * time.Second, RetryPeriod: 100 * time.Millisecond}
	if err := e.Run(context.Background(), func(context.Context) error { return nil }); err == nil {
		t.Fatal("a renew deadline longer than the lease was accepted")
	}
	if err := (&Elector{}).Run(context.Background(), nil); err == nil {
		t.Fatal("an elector without a client was accepted")
	}
}

func TestLeadsThenReleasesOnShutdown(t *testing.T) {
	srv := kubeapitest.New()
	defer srv.Close()
	e := fast(srv.Client(idA), idA)
	ctx, cancel := context.WithCancel(context.Background())
	var leading atomic.Bool
	ended := make(chan error, 1)
	go func() {
		ended <- e.Run(ctx, func(c context.Context) error {
			leading.Store(true)
			<-c.Done()
			leading.Store(false)
			return nil
		})
	}()
	waitFor(t, "leadership", 2*time.Second, leading.Load)
	if l := srv.Lease(LeaseNamespace, LeaseName); l == nil || l.Spec.HolderIdentity != idA {
		t.Fatalf("lease = %+v", l)
	}
	cancel()
	if err := <-ended; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if leading.Load() {
		t.Fatal("lead was still running after Run returned")
	}
	if l := srv.Lease(LeaseNamespace, LeaseName); l.Spec.HolderIdentity != "" {
		t.Fatalf("lease not released: holder %q", l.Spec.HolderIdentity)
	}
}

// The safety property: a partitioned leader stops before anyone else may
// start, so two terms never overlap in time.
func TestPartitionedLeaderStopsBeforeTheNextStarts(t *testing.T) {
	srv := kubeapitest.New()
	defer srv.Close()
	ca := &partitionable{LeaseClient: srv.Client(idA)}
	cb := &partitionable{LeaseClient: srv.Client(idB)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var aStopped, bStarted time.Time
	var aLeading atomic.Bool
	go func() {
		_ = fast(ca, idA).Run(ctx, func(c context.Context) error {
			aLeading.Store(true)
			<-c.Done()
			mu.Lock()
			if aStopped.IsZero() {
				aStopped = time.Now()
			}
			mu.Unlock()
			return nil
		})
	}()
	waitFor(t, "A to lead", 2*time.Second, aLeading.Load)

	var bLeading atomic.Bool
	go func() {
		_ = fast(cb, idB).Run(ctx, func(c context.Context) error {
			mu.Lock()
			if bStarted.IsZero() {
				bStarted = time.Now()
			}
			mu.Unlock()
			bLeading.Store(true)
			<-c.Done()
			return nil
		})
	}()
	time.Sleep(300 * time.Millisecond)
	if bLeading.Load() {
		t.Fatal("B led while A held a live lease")
	}

	ca.down.Store(true)
	waitFor(t, "B to take over", 4*time.Second, bLeading.Load)
	mu.Lock()
	defer mu.Unlock()
	if aStopped.IsZero() || !aStopped.Before(bStarted) {
		t.Fatalf("terms overlapped: A stopped %v, B started %v", aStopped, bStarted)
	}
	if l := srv.Lease(LeaseNamespace, LeaseName); l.Spec.HolderIdentity != idB || l.Spec.LeaseTransitions < 1 {
		t.Fatalf("lease = %+v", l.Spec)
	}
}

func TestAFailedTermReleasesAndBacksOff(t *testing.T) {
	srv := kubeapitest.New()
	defer srv.Close()
	e := fast(srv.Client(idA), idA)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var starts []time.Time
	go func() {
		_ = e.Run(ctx, func(context.Context) error {
			mu.Lock()
			starts = append(starts, time.Now())
			mu.Unlock()
			return errors.New("barrier stuck")
		})
	}()
	waitFor(t, "two terms", 3*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(starts) >= 2 })
	mu.Lock()
	gap := starts[1].Sub(starts[0])
	mu.Unlock()
	if gap < e.FailureBackoff {
		t.Fatalf("retried after %v, want at least the %v backoff", gap, e.FailureBackoff)
	}
}

// probe is one fake server for the barrier.
type probe struct {
	name  string
	admit func(n int, cm *kubeapi.ConfigMap) error
	mu    sync.Mutex
	calls int
	seen  *kubeapi.ConfigMap
}

func (p *probe) Server() string { return p.name }

func (p *probe) DryRunCreateConfigMap(_ context.Context, cm *kubeapi.ConfigMap) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.seen = cm
	return p.admit(p.calls, cm)
}

var errDenied = &kubeapi.StatusError{Code: 403, Reason: "Forbidden", Message: "denied request"}

func TestBarrierOpensOnlyWhenEveryServerAdmits(t *testing.T) {
	quick := &probe{name: "https://192.0.2.10:6443", admit: func(int, *kubeapi.ConfigMap) error { return nil }}
	slow := &probe{name: "https://192.0.2.11:6443", admit: func(n int, _ *kubeapi.ConfigMap) error {
		if n < 4 {
			return errDenied // its cache hasn't caught up yet
		}
		return nil
	}}
	b := Barrier{Servers: []Prober{quick, slow}, Timeout: 2 * time.Second, Interval: 10 * time.Millisecond}
	if err := b.Wait(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if quick.calls != 1 || slow.calls != 4 {
		t.Fatalf("calls: quick %d slow %d", quick.calls, slow.calls)
	}
	cm := slow.seen
	if cm.Metadata.Namespace != ProbeNamespace || cm.Metadata.Name != "" || !strings.HasPrefix(cm.Metadata.GenerateName, "wildstone-") ||
		cm.Metadata.Annotations[epochAnnotation] != "7" {
		t.Fatalf("probe = %+v", cm.Metadata)
	}
}

func TestBarrierThatStaysShutNamesTheServer(t *testing.T) {
	ok := &probe{name: "https://192.0.2.10:6443", admit: func(int, *kubeapi.ConfigMap) error { return nil }}
	frozen := &probe{name: "https://192.0.2.11:6443", admit: func(int, *kubeapi.ConfigMap) error { return errDenied }}
	b := Barrier{Servers: []Prober{ok, frozen}, Timeout: 100 * time.Millisecond, Interval: 10 * time.Millisecond}
	err := b.Wait(context.Background(), 8)
	var stuck *StuckError
	if !errors.As(err, &stuck) || len(stuck.Servers) != 1 || stuck.Servers[0] != frozen.name || stuck.Epoch != 8 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "restart k3s") {
		t.Fatalf("error doesn't say what to do: %v", err)
	}
	if err := (Barrier{}).Wait(context.Background(), 1); err == nil {
		t.Fatal("a barrier with no servers opened")
	}
	if err := (Barrier{Servers: []Prober{ok}}).Wait(context.Background(), 0); err == nil {
		t.Fatal("a barrier opened for epoch 0")
	}
}

type issuer struct {
	epoch uint64
	err   error
	got   atomic.Value
}

func (i *issuer) Issue(_ context.Context, leader string) (uint64, error) {
	i.got.Store(leader)
	return i.epoch, i.err
}

func TestTermServesOnlyAfterEpochAndBarrier(t *testing.T) {
	srv := kubeapitest.New()
	defer srv.Close()
	is := &issuer{epoch: 5}
	served := make(chan Term, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := Leader{
		Elector: fast(srv.Client(idA), idA),
		Issuer:  is,
		Barrier: Barrier{Servers: []Prober{&probe{name: "s", admit: func(int, *kubeapi.ConfigMap) error { return nil }}}},
		Serve: func(c context.Context, term Term) error {
			served <- term
			<-c.Done()
			return nil
		},
	}
	go func() { _ = l.Run(ctx) }()
	select {
	case term := <-served:
		if term.Epoch != 5 || is.got.Load() != idA {
			t.Fatalf("term %+v, issued for %v", term, is.got.Load())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("never served")
	}
}

func TestTermNeverServesBehindAShutBarrier(t *testing.T) {
	srv := kubeapitest.New()
	defer srv.Close()
	var incidents atomic.Int32
	var served atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := Leader{
		Elector: fast(srv.Client(idA), idA),
		Issuer:  &issuer{epoch: 9},
		Barrier: Barrier{
			Servers: []Prober{&probe{name: "frozen", admit: func(int, *kubeapi.ConfigMap) error { return errDenied }}},
			Timeout: 50 * time.Millisecond, Interval: 10 * time.Millisecond,
		},
		Serve:    func(context.Context, Term) error { served.Store(true); return nil },
		Incident: func(error) { incidents.Add(1) },
	}
	go func() { _ = l.Run(ctx) }()
	waitFor(t, "an incident", 3*time.Second, func() bool { return incidents.Load() > 0 })
	if served.Load() {
		t.Fatal("a term served behind a shut barrier")
	}
	// The failed term released the Lease for someone else.
	waitFor(t, "release", 2*time.Second, func() bool {
		ls := srv.Lease(LeaseNamespace, LeaseName)
		return ls != nil && ls.Spec.HolderIdentity == ""
	})
}

func TestTermNeverServesWithoutAnEpoch(t *testing.T) {
	srv := kubeapitest.New()
	defer srv.Close()
	var incidents atomic.Int32
	var served atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := Leader{
		Elector:  fast(srv.Client(idA), idA),
		Issuer:   &issuer{err: errors.New("keystone unreachable")},
		Barrier:  Barrier{Servers: []Prober{&probe{name: "s", admit: func(int, *kubeapi.ConfigMap) error { return nil }}}},
		Serve:    func(context.Context, Term) error { served.Store(true); return nil },
		Incident: func(error) { incidents.Add(1) },
	}
	go func() { _ = l.Run(ctx) }()
	waitFor(t, "an incident", 3*time.Second, func() bool { return incidents.Load() > 0 })
	if served.Load() {
		t.Fatal("a term served without an epoch")
	}
}
