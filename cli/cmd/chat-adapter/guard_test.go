package main

import (
	"fmt"
	"testing"
	"time"
)

func testGuard() (*guard, *time.Time) {
	now := time.Unix(1_800_000_000, 0)
	g := newGuard(nil)
	g.now = func() time.Time { return now }
	return g, &now
}

// One account gets a short burst, then one message every 15 seconds.
func TestOneAccountCannotFloodTheBot(t *testing.T) {
	g, now := testGuard()
	g.markKnown("sam")
	passed := 0
	for i := 0; i < 50; i++ {
		if g.admit("sam", false) {
			passed++
		}
	}
	if passed != accountBurst {
		t.Fatalf("a burst of 50 let %d through, want %d", passed, accountBurst)
	}
	*now = now.Add(accountEvery)
	if !g.admit("sam", false) {
		t.Error("the budget did not refill")
	}
}

// Once Keystone says an account is unknown, its messages stop here —
// except an invite key, which is the one thing a stranger may send.
func TestAStrangerCostsOneRequest(t *testing.T) {
	g, now := testGuard()
	if !g.admit("stranger", false) {
		t.Fatal("the first message from a new account was dropped")
	}
	g.markUnknown("stranger")
	for i := 0; i < 5; i++ {
		if g.admit("stranger", false) {
			t.Fatal("a known stranger's message was sent on")
		}
	}
	if !g.admit("stranger", true) {
		t.Error("a stranger's invite key was dropped")
	}
	*now = now.Add(unknownFor + time.Second)
	if !g.admit("stranger", false) {
		t.Error("an account stayed blocked past the window")
	}
}

// Many throwaway accounts share one budget per bot; a bound account does
// not spend it.
func TestManyStrangersShareOneBudget(t *testing.T) {
	g, _ := testGuard()
	passed := 0
	for i := 0; i < 500; i++ {
		if g.admit(fmt.Sprintf("acct-%d", i), false) {
			passed++
		}
	}
	if passed != newcomerBurst {
		t.Fatalf("500 new accounts got %d through, want %d", passed, newcomerBurst)
	}
	g.markKnown("sam")
	if !g.admit("sam", false) {
		t.Error("a bound account was held behind the strangers' budget")
	}
}

// Invite-key attempts: a few per account, and a cap for the whole bot.
func TestKeyAttemptsAreLimited(t *testing.T) {
	g, _ := testGuard()
	passed := 0
	for i := 0; i < 10; i++ {
		if g.admit("guesser", true) {
			passed++
		}
	}
	if passed != redeemBurst {
		t.Fatalf("one account's key attempts got %d through, want %d", passed, redeemBurst)
	}
	g, _ = testGuard()
	all := 0
	for i := 0; i < 100; i++ {
		if g.admit(fmt.Sprintf("g-%d", i), true) {
			all++
		}
	}
	if all != redeemAllBurst {
		t.Errorf("key attempts across accounts got %d through, want %d", all, redeemAllBurst)
	}
}

// No table grows without bound, however many accounts appear.
func TestTheGuardStaysBounded(t *testing.T) {
	g, _ := testGuard()
	for i := 0; i < guardMaxEntries*2; i++ {
		acct := fmt.Sprintf("a-%d", i)
		g.admit(acct, false)
		g.markUnknown(acct)
	}
	if len(g.accounts) > guardMaxEntries || len(g.unknown) > guardMaxEntries {
		t.Errorf("tables grew to %d and %d", len(g.accounts), len(g.unknown))
	}
}
