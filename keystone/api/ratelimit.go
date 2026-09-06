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

// maxChatKeyBytes bounds a chat bucket key. The account half is text an
// ADAPTER supplies, so it is somebody else's input reaching a map key:
// bounded here, and swept below.
const maxChatKeyBytes = 128

// maxBuckets is when the sweep runs. Principal buckets are a bounded set
// — one per credential — but chat buckets are keyed by transport account
// ids the adapter reports, so without a sweep a buggy or hostile adapter
// naming a new account each time would grow this map until the control
// plane died. The bucket only matters while it is being refilled, so a
// bucket nothing has touched for maxBucketIdle is indistinguishable from
// one that never existed.
const (
	maxBuckets    = 4096
	maxBucketIdle = 10 * time.Minute
)

// allowChat consumes one token from a chat account's own bucket. The
// key is "<transport>:<account>", and the namespace is kept apart from
// the principal buckets so an adapter's credential and the accounts it
// relays for cannot spend each other's budget.
func (l *limiter) allowChat(account string) bool {
	if len(account) > maxChatKeyBytes {
		account = account[:maxChatKeyBytes]
	}
	return l.take("\x00chat:"+account, chatBurst, chatPerSec)
}

// sweep drops buckets that have gone idle long enough to be full again.
// Called with the lock held, only when the map has grown past
// maxBuckets, so the ordinary path stays two map operations.
func (l *limiter) sweep(now time.Time) {
	for key, b := range l.buckets {
		if now.Sub(b.last) > maxBucketIdle {
			delete(l.buckets, key)
		}
	}
	// Every bucket is live and the map is still over the mark: the
	// deployment is genuinely busy, or something is flooding it. Either
	// way, do not grow without bound — drop the idlest half of what is
	// left rather than letting the map become the failure.
	if len(l.buckets) > maxBuckets {
		cutoff := now.Add(-maxBucketIdle / 2)
		for key, b := range l.buckets {
			if b.last.Before(cutoff) {
				delete(l.buckets, key)
			}
		}
	}
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
		if len(l.buckets) >= maxBuckets {
			l.sweep(now)
		}
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
