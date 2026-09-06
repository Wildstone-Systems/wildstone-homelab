package api

import (
	"fmt"
	"testing"
	"time"
)

// The bucket map is bounded. Principal buckets are one per credential,
// but chat buckets are keyed by account ids an ADAPTER reports — so
// without a sweep, a buggy or hostile adapter naming a fresh account
// every message grows this map until the control plane dies
// (ADR-W026 A6).
func TestLimiterSweepsIdleBuckets(t *testing.T) {
	now := time.Now()
	l := newLimiter()
	l.now = func() time.Time { return now }

	for i := 0; i < maxBuckets+50; i++ {
		l.allowChat(fmt.Sprintf("telegram:flood-%d", i))
		// Every bucket is created a moment apart, so the earliest ones
		// age past the idle window while the newest do not.
		now = now.Add(time.Second)
	}
	if len(l.buckets) > maxBuckets {
		t.Fatalf("buckets = %d, want the map held at or below %d", len(l.buckets), maxBuckets)
	}

	// A live account keeps its budget across a sweep: the sweep drops
	// buckets that are idle, and an idle bucket is a full one.
	key := "telegram:sam"
	for i := 0; i < chatBurst; i++ {
		if !l.allowChat(key) {
			t.Fatalf("a live account was limited after %d of %d messages", i, chatBurst)
		}
	}
	if l.allowChat(key) {
		t.Fatalf("the burst was not enforced for a live account")
	}
}

// A key from a transport is bounded before it becomes a map key: it is
// somebody else's text.
func TestLimiterBoundsChatKeys(t *testing.T) {
	l := newLimiter()
	huge := make([]byte, 64<<10)
	for i := range huge {
		huge[i] = 'a'
	}
	l.allowChat("telegram:" + string(huge))
	for key := range l.buckets {
		if len(key) > maxChatKeyBytes+len("\x00chat:telegram:") {
			t.Fatalf("an unbounded key reached the bucket map: %d bytes", len(key))
		}
	}
}

// Chat accounts and principals draw on separate budgets: an adapter's
// own credential cannot be exhausted by the accounts it relays for, and
// one account cannot spend another's.
func TestLimiterNamespacesAreSeparate(t *testing.T) {
	l := newLimiter()
	for i := 0; i < chatBurst; i++ {
		l.allowChat("telegram:flooder")
	}
	if l.allowChat("telegram:flooder") {
		t.Fatalf("the flooding account was not limited")
	}
	if !l.allowChat("telegram:someone-else") {
		t.Fatalf("one account's flood limited another")
	}
	if !l.allow("chat-telegram-family") {
		t.Fatalf("an account's flood limited the adapter's own credential")
	}
}
