package main

// The adapter's own flood guard (ADR-W033 B9.1).
//
// A bot's handle is public, so anyone can message it, and a stranger is
// owed nothing — not a reply, and not Keystone's attention either. This
// guard decides, before any request leaves the adapter, whether a
// message is worth sending on at all:
//
//   - every account has its own small budget, so one sender cannot
//     monopolise the bot;
//   - an account Keystone has said it does not know is remembered for a
//     while and dropped here, so a stranger's flood costs one request,
//     not one per message;
//   - accounts not yet seen to be bound share ONE budget per bot, so a
//     thousand throwaway accounts are one budget, not a thousand;
//   - something shaped like an invite key has its own, smaller budget —
//     it is the one thing a stranger may legitimately send.
//
// Everything is in memory and bounded: the tables are capped, and a
// dropped message is counted, not logged one by one.

import (
	"log/slog"
	"sync"
	"time"
)

const (
	// Any one account: a short burst, then one message every 15s.
	accountBurst = 6
	accountEvery = 15 * time.Second
	// An account Keystone called unknown is dropped here this long.
	unknownFor = 10 * time.Minute
	// Invite-key attempts, per account and for the whole bot.
	redeemBurst    = 3
	redeemEvery    = 2 * time.Minute
	redeemAllBurst = 10
	redeemAllEvery = 6 * time.Second
	// Accounts not yet seen bound share this, per bot.
	newcomerBurst = 20
	newcomerEvery = 3 * time.Second
	// A bound account stays "known" this long after it was last seen.
	knownFor = 24 * time.Hour
	// No table grows past this.
	guardMaxEntries = 10000
)

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func (b *tokenBucket) take(now time.Time, burst int, every time.Duration) bool {
	if b.last.IsZero() {
		b.tokens, b.last = float64(burst), now
	}
	b.tokens += float64(now.Sub(b.last)) / float64(every)
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type guard struct {
	now func() time.Time
	log *slog.Logger

	mu        sync.Mutex
	accounts  map[string]*tokenBucket
	redeems   map[string]*tokenBucket
	redeemAll tokenBucket
	newcomers tokenBucket
	unknown   map[string]time.Time // until
	known     map[string]time.Time // until
	dropped   int
	reported  time.Time
}

func newGuard(log *slog.Logger) *guard {
	return &guard{log: log, accounts: map[string]*tokenBucket{}, redeems: map[string]*tokenBucket{},
		unknown: map[string]time.Time{}, known: map[string]time.Time{}}
}

func (g *guard) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// admit reports whether a message from account is sent on to Keystone.
// key is whether it is an invite-key attempt.
func (g *guard) admit(account string, key bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	ok := g.decide(now, account, key)
	if !ok {
		g.dropped++
	}
	if g.dropped > 0 && now.Sub(g.reported) >= time.Minute {
		if g.log != nil {
			g.log.Warn("dropped messages from accounts over their limit or unknown to the household",
				"count", g.dropped, "note", "nothing was sent to Keystone or said back")
		}
		g.dropped, g.reported = 0, now
	}
	return ok
}

func (g *guard) decide(now time.Time, account string, key bool) bool {
	if key {
		return bucketFor(g.redeems, account, now).take(now, redeemBurst, redeemEvery) &&
			g.redeemAll.take(now, redeemAllBurst, redeemAllEvery)
	}
	if until, ok := g.unknown[account]; ok && now.Before(until) {
		return false
	}
	if !bucketFor(g.accounts, account, now).take(now, accountBurst, accountEvery) {
		return false
	}
	if until, ok := g.known[account]; ok && now.Before(until) {
		return true
	}
	return g.newcomers.take(now, newcomerBurst, newcomerEvery)
}

// markUnknown remembers Keystone's word that an account is nobody the
// household has introduced.
func (g *guard) markUnknown(account string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	delete(g.known, account)
	prune(g.unknown, now)
	g.unknown[account] = now.Add(unknownFor)
}

// markKnown remembers that an account is bound: it skips the newcomer
// budget, though never its own.
func (g *guard) markKnown(account string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	delete(g.unknown, account)
	prune(g.known, now)
	g.known[account] = now.Add(knownFor)
}

func bucketFor(m map[string]*tokenBucket, account string, now time.Time) *tokenBucket {
	b, ok := m[account]
	if !ok {
		if len(m) >= guardMaxEntries {
			// Full of live senders: a flood of accounts. Forget them all
			// rather than grow; the shared newcomer budget still holds.
			for k := range m {
				delete(m, k)
			}
		}
		b = &tokenBucket{}
		m[account] = b
	}
	return b
}

func prune(m map[string]time.Time, now time.Time) {
	if len(m) < guardMaxEntries {
		return
	}
	for k, until := range m {
		if !now.Before(until) {
			delete(m, k)
		}
	}
	if len(m) >= guardMaxEntries {
		for k := range m {
			delete(m, k)
		}
	}
}
