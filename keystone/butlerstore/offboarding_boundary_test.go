package butlerstore

import (
	"context"
	"errors"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
)

func TestActiveTurnDoesNotContinueIntoAnotherModelRound(t *testing.T) {
	f := newDerivedFixture(t)
	r := newRunner(t, newCountingToolset(), f.ledger, f.taskID)
	p := &revokingProvider{
		steps: []*provider.ChatResponse{readsFor("node-a")},
		revoke: func() {
			if err := f.db.OffboardButlerMember("mika", "root-admin"); err != nil {
				t.Fatal(err)
			}
		},
	}
	r.Provider = p
	if _, err := r.Run(context.Background(), "read node-a"); !errors.Is(err, ErrAuthorityWithdrawn) {
		t.Fatalf("active turn after offboarding: %v", err)
	}
	if p.at != 1 {
		t.Fatalf("provider saw %d rounds after offboarding, want only the in-flight round", p.at)
	}
}

func TestRetryDoesNotFireAfterRevocation(t *testing.T) {
	f := newDerivedFixture(t)
	r := newRunner(t, newCountingToolset(), f.ledger, f.taskID)
	r.RetryLimit = 1
	p := &failingThenRevokingProvider{revoke: func() {
		if err := f.db.OffboardButlerMember("mika", "root-admin"); err != nil {
			t.Fatal(err)
		}
	}}
	r.Provider = p
	if _, err := r.Run(context.Background(), "read node-a"); !errors.Is(err, ErrAuthorityWithdrawn) {
		t.Fatalf("retry after offboarding: %v", err)
	}
	if p.attempts != 1 {
		t.Fatalf("provider saw %d attempts after offboarding", p.attempts)
	}
}

func TestPreparedPrivateContextDoesNotReachAPrompt(t *testing.T) {
	f := newDerivedFixture(t)
	r := newRunner(t, newCountingToolset(), f.ledger, f.taskID)
	// This history stands in for private context assembled while the
	// Person still had authority, before the provider dispatch boundary.
	r.History = []provider.Message{{Role: "user", Text: "private medical context"}}
	if err := f.db.OffboardButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	p := &revokingProvider{steps: []*provider.ChatResponse{{Text: "should never run"}}}
	r.Provider = p
	if _, err := r.Run(context.Background(), "continue"); !errors.Is(err, ErrAuthorityWithdrawn) {
		t.Fatalf("prepared prompt after offboarding: %v", err)
	}
	if p.at != 0 {
		t.Fatal("private context reached a provider call after offboarding")
	}
}
