// Package householdtz holds the household's timezone while Keystone runs.
//
// The household's zone is a setting (ButlerSettings.Timezone), and an
// Owner can change it — directly in the Console, or by approving an
// Agent's proposal (ADR-W033 E2). Everything that reads the household's
// clock — the Butler's "it is now", the calendar tools, automations,
// upkeep, the portal — holds one *Zone rather than a *time.Location
// copied at startup, so an approved change applies to the next thing
// that asks, with no restart.
package householdtz

import (
	"sync/atomic"
	"time"
)

// Zone is the household's current timezone, safe for concurrent use.
type Zone struct {
	p atomic.Pointer[time.Location]
}

// New is a Zone starting at l (UTC when nil).
func New(l *time.Location) *Zone {
	z := &Zone{}
	z.Set(l)
	return z
}

// Set changes the zone for everything holding this Zone.
func (z *Zone) Set(l *time.Location) {
	if l == nil {
		l = time.UTC
	}
	z.p.Store(l)
}

// Location is the zone now. A nil Zone answers nil, so a holder can fall
// back to a fixed location it was given instead.
func (z *Zone) Location() *time.Location {
	if z == nil {
		return nil
	}
	return z.p.Load()
}

// Or is the zone now, or fallback when there is no Zone.
func (z *Zone) Or(fallback *time.Location) *time.Location {
	if l := z.Location(); l != nil {
		return l
	}
	return fallback
}
