package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Hermes's health gates the bridge's own signs of life (#389).
//
// Keystone counts every authenticated call from the runtime as proof it
// is there: the heartbeat, and the deliveries long-poll too. A bridge that
// kept making them while Hermes was down showed its Agent `connected`
// for as long as Hermes stayed down, and every turn waited out its full
// reply bound for a run that could never start. So the bridge makes
// neither call while Hermes's /health fails. Keystone's heartbeat sweep
// then marks the runtime offline, turns park at once with "not
// connected", and the bridge's first call after Hermes recovers is the
// arrival that re-offers them.

// healthProbeDownInterval is how often a down Hermes is probed, so
// recovery is noticed well inside heartbeatInterval.
var healthProbeDownInterval = 5 * time.Second

// errNoHealthEndpoint is what a health prober returns when Hermes has no
// /health at all. That says nothing about whether Hermes is up, so it
// does not hold the bridge back.
var errNoHealthEndpoint = errors.New("hermes: no /health endpoint")

// healthGate is whether Hermes last answered its health probe, and a
// way to wait until it does.
type healthGate struct {
	mu sync.Mutex
	up bool
	// known is false until the first probe has answered.
	known bool
	// ready is closed while up and replaced when Hermes goes down.
	ready chan struct{}
}

// newHealthGate starts closed, with nothing known. The heartbeat and
// delivery loops start together, so a gate that started open would let
// the first deliveries poll reach Keystone before the first probe had
// said whether Hermes was there, and Keystone would count that poll as
// the runtime arriving.
func newHealthGate() *healthGate {
	return &healthGate{ready: make(chan struct{})}
}

// set records a probe's result and reports whether it changed, which
// the first result always does.
func (g *healthGate) set(up bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.known && g.up == up {
		return false
	}
	g.known = true
	if g.up == up {
		// The first probe found Hermes down: the gate is closed already.
		return true
	}
	g.up = up
	if up {
		close(g.ready)
	} else {
		g.ready = make(chan struct{})
	}
	return true
}

func (g *healthGate) isUp() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.up
}

// wait returns once Hermes is up, or ctx is done.
func (g *healthGate) wait(ctx context.Context) error {
	g.mu.Lock()
	ready := g.ready
	g.mu.Unlock()
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// probeHermes checks Hermes's health once, records it, and logs a change.
func (b *bridge) probeHermes(ctx context.Context) bool {
	if b.health == nil {
		return true
	}
	pctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()
	err := b.hermes.Health(pctx)
	if errors.Is(err, errNoHealthEndpoint) {
		if !b.noHealthLogged.Swap(true) {
			b.log.Warn("hermes has no /health endpoint; reporting the runtime as up without checking it")
		}
		err = nil
	}
	if ctx.Err() != nil {
		return b.health.isUp()
	}
	up := err == nil
	if b.health.set(up) {
		if up {
			b.log.Info("hermes is answering; heartbeats and deliveries running")
		} else {
			b.log.Warn("hermes is not answering; pausing heartbeats and deliveries so Wildstone marks this runtime offline",
				"err", err)
		}
	}
	return up
}

// healthProbeTimeout bounds one probe. A Hermes that takes longer than
// this to say it is alive is not in a state to take a turn.
const healthProbeTimeout = 10 * time.Second
