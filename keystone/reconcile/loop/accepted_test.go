package loop

import (
	"testing"
)

// acceptedHarness is harness with the Statefile wired to the runtime DB's
// accepted revision, the way cmd/keystone wires it.
func acceptedHarness(t *testing.T) (*Loop, *okApplier, func(name string) string) {
	t.Helper()
	l, state, db, app := harness(t)
	state.Accepted = db.StatefileHead
	// commit adds a volume and commits it, returning the new HEAD without
	// accepting it.
	commit := func(name string) string {
		t.Helper()
		if err := state.Put(volume(name, "n1")); err != nil {
			t.Fatal(err)
		}
		if err := state.Commit("test", "", "add "+name); err != nil {
			t.Fatal(err)
		}
		head, err := state.Head()
		if err != nil {
			t.Fatal(err)
		}
		return head
	}
	return l, app, commit
}

// Only the accepted revision reaches a node: an uncommitted file and a
// committed-but-unaccepted commit are both inert (contract §7, immutable
// intent).
func TestTickConvergesOnlyToTheAcceptedRevision(t *testing.T) {
	l, app, commit := acceptedHarness(t)
	first := commit("data")
	if err := l.DB.SetStatefileHead(first); err != nil {
		t.Fatal(err)
	}
	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if out.Revision != first || !out.Repaired {
		t.Fatalf("first tick = %+v; want a repair at %s", out, first)
	}
	settled := app.ops

	// Committed by hand, not yet accepted by ingestion.
	commit("scratch")
	// And an uncommitted file on top.
	if err := l.State.Put(volume("uncommitted", "n1")); err != nil {
		t.Fatal(err)
	}
	out, err = l.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if out.Revision != first || out.Repaired {
		t.Fatalf("tick after unaccepted edits = %+v; want a quiet pass at %s", out, first)
	}
	obs, err := l.DB.ListObserved()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if o.Name != "data" {
			t.Fatalf("an unaccepted resource reached the observed set: %+v (ops %d -> %d)", obs, settled, app.ops)
		}
	}
}

// A repository with history but no accepted revision pauses the loop
// rather than converging to an empty tree, which would delete everything.
func TestTickWaitsWhenNothingIsAccepted(t *testing.T) {
	l, app, commit := acceptedHarness(t)
	commit("data")
	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if out.Revision != "" || out.Repaired || app.ops != 0 {
		t.Fatalf("tick with nothing accepted = %+v, ops %d; want a pause", out, app.ops)
	}
}
