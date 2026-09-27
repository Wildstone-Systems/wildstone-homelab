package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

// While Hermes does not answer its health probe the bridge makes no call
// Keystone would count as a sign of life, neither a heartbeat nor a
// deliveries poll, so the runtime goes offline and turns park instead of
// waiting on a run that cannot start (#389). When Hermes answers again,
// both resume.
func TestBridgeGoesQuietWhileHermesIsDown(t *testing.T) {
	old := healthProbeDownInterval
	healthProbeDownInterval = 20 * time.Millisecond
	t.Cleanup(func() { healthProbeDownInterval = old })

	var hermesUp atomic.Bool
	var beats, polls atomic.Int32
	ks := &fakeClient{
		heartbeatFn: func(ctx context.Context, hb agentapi.Heartbeat) error {
			beats.Add(1)
			return nil
		},
		deliveriesFn: func(ctx context.Context, after string, wait time.Duration) ([]agentapi.Delivery, error) {
			polls.Add(1)
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Millisecond):
			}
			return nil, nil
		},
	}
	hc := &fakeHermes{healthFn: func(ctx context.Context) error {
		if hermesUp.Load() {
			return nil
		}
		return errors.New("connection refused")
	}}
	br, logBuf := testBridge(t, ks, hc)
	br.health = newHealthGate()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{}, 2)
	// The first probe runs before the delivery loop starts, as it does
	// in main: heartbeatLoop probes before it does anything else.
	br.probeHermes(ctx)
	go func() { br.heartbeatLoop(ctx); done <- struct{}{} }()
	go func() { br.deliveryLoop(ctx); done <- struct{}{} }()
	stop := func() { cancel(); <-done; <-done }
	defer func() {
		if ctx.Err() == nil {
			stop()
		}
	}()

	time.Sleep(150 * time.Millisecond)
	if n := beats.Load(); n != 0 {
		t.Errorf("heartbeats while hermes is down = %d, want 0", n)
	}
	if n := polls.Load(); n != 0 {
		t.Errorf("deliveries polls while hermes is down = %d, want 0", n)
	}

	hermesUp.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for (beats.Load() == 0 || polls.Load() == 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if beats.Load() == 0 || polls.Load() == 0 {
		t.Fatalf("after hermes recovered: heartbeats=%d polls=%d, want both resumed", beats.Load(), polls.Load())
	}
	stop() // the loops write the log; stop them before reading it
	for _, want := range []string{"hermes is not answering", "hermes is answering again"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Errorf("log is missing %q:\n%s", want, logBuf.String())
		}
	}
}

// A Hermes with no /health endpoint at all is not a Hermes that is down:
// the bridge carries on as it did before the check existed.
func TestHermesWithoutAHealthEndpointCountsAsUp(t *testing.T) {
	hs := httptest.NewServer(http.NotFoundHandler())
	defer hs.Close()

	br, logBuf := testBridge(t, &fakeClient{}, nil)
	br.hermes = &hermesClient{Base: hs.URL}
	br.health = newHealthGate()

	if !br.probeHermes(t.Context()) {
		t.Fatal("probe of a Hermes without /health = down, want up")
	}
	br.probeHermes(t.Context())
	if n := strings.Count(logBuf.String(), "no /health endpoint"); n != 1 {
		t.Errorf("\"no /health endpoint\" logged %d times, want once:\n%s", n, logBuf.String())
	}
}
