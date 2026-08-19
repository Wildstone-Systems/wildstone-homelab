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
)

func newLimiter() *limiter {
	return &limiter{buckets: map[string]*bucket{}, now: time.Now}
}

// allow consumes one token from key's bucket, reporting whether the
// request may proceed.
func (l *limiter) allow(key string) bool {
	burst, perSec := float64(authBurst), float64(authPerSec)
	if key == anonBucket {
		burst, perSec = anonBurst, anonPerSec
	}
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
