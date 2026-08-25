package loop

import (
	"context"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// TestPokeWakesRunImmediately pins R2a: with a long base interval, a
// Poke still gets the pending approved plan converged promptly.
func TestPokeWakesRunImmediately(t *testing.T) {
	l, state, db, _ := harness(t)
	if err := state.Put(volume("data", "n1")); err != nil {
		t.Fatal(err)
	}
	if err := state.Commit("test", "plan-1", "add volume"); err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertPlan("op", "", "add volume", "low", `{"x":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TransitionPlan(id, runtimedb.PlanApproved, "op"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go l.Run(ctx, time.Hour) // interval alone would never fire in-test

	l.Poke()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, err := db.GetPlan(id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.State == runtimedb.PlanApplied {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("poked Run never applied the plan: %+v", rec)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestFailureBackoff pins R2b: consecutive failed ticks stretch the
// delay exponentially up to the cap, and one clean tick resets it.
func TestFailureBackoff(t *testing.T) {
	l, state, db, _ := harness(t)
	l.Connected = func(string) bool { return false } // plans can't land

	container := resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindContainer,
		Metadata:   resource.Metadata{Name: "app", Owner: "capability/test", Node: "n1"},
		Container: &resource.ContainerSpec{
			Image: "reg.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	if err := state.Put(container); err != nil {
		t.Fatal(err)
	}

	base := time.Second
	if got := l.delay(base); got != base {
		t.Fatalf("fresh delay = %v, want %v", got, base)
	}
	// Each failed plan tick doubles the delay (a fresh approved plan per
	// tick, since a failed one goes terminal).
	for i, want := range []time.Duration{2 * base, 4 * base, 8 * base} {
		id, err := db.InsertPlan("op", "", "deploy", "low", `{"x":1}`)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.TransitionPlan(id, runtimedb.PlanApproved, "op"); err != nil {
			t.Fatal(err)
		}
		out, err := l.Tick(t.Context())
		if err != nil || out.Failed == "" {
			t.Fatalf("tick %d = %+v, %v; want failure", i, out, err)
		}
		if got := l.delay(base); got != want {
			t.Fatalf("delay after %d failures = %v, want %v", i+1, got, want)
		}
	}
	// The cap holds.
	l.failStreak.Store(50)
	if got := l.delay(base); got != base<<maxBackoffShift {
		t.Fatalf("capped delay = %v, want %v", got, base<<maxBackoffShift)
	}
	// A clean tick (node back online counts as a quiet drift tick — but
	// here simply reconnect and let drift repair fail-free) resets.
	l.Connected = func(string) bool { return true }
	out, err := l.Tick(t.Context())
	if err != nil || out.Failed != "" {
		t.Fatalf("recovery tick = %+v, %v", out, err)
	}
	if got := l.delay(base); got != base {
		t.Fatalf("delay after recovery = %v, want %v", got, base)
	}
}
