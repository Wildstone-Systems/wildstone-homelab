package api

import (
	"sync"
	"time"
)

// anonBucket is the shared key for every request that has not
// authenticated: one strict global budget for probes and token guessing.
const anonBucket = "\x00anonymous"

// limiter is a per-key token bucket. Authenticated principals get a
// generous interactive budget; the anonymous bucket is deliberately tight.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time // injectable for tests
}

type bucket struct {
	tokens float64
	last   time.Time
}

const (
	authBurst  = 50
	authPerSec = 25
	anonBurst  = 5
	anonPerSec = 1
	// A chat message can cost a model call, so a chat account gets a
	// much tighter budget than the adapter's own credential does
	// (ADR-W026 A6). It is per ACCOUNT, not per adapter: one member
	// cannot exhaust the household's provider budget, and a wedged
	// adapter re-sending one conversation cannot loop.
	chatBurst  = 8
	chatPerSec = 0.2
)

func newLimiter() *limiter {
	return &limiter{buckets: map[string]*bucket{}, now: time.Now}
}

// allowChat consumes one token from a chat account's own bucket. The
// key is "<transport>:<account>", and the namespace is kept apart from
// the principal buckets so an adapter's credential and the accounts it
// relays for cannot spend each other's budget.
func (l *limiter) allowChat(account string) bool {
	return l.take("\x00chat:"+account, chatBurst, chatPerSec)
}

// allow consumes one token from key's bucket, reporting whether the
// request may proceed.
func (l *limiter) allow(key string) bool {
	burst, perSec := float64(authBurst), float64(authPerSec)
	if key == anonBucket {
		burst, perSec = anonBurst, anonPerSec
	}
	return l.take(key, burst, perSec)
}

// take is the shared token-bucket step.
func (l *limiter) take(key string, burst, perSec float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * perSec
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
