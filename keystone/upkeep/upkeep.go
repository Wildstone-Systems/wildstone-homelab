// Package upkeep is the daily pass over the household's commitments
// (ADR-W034 A15, work package K4): it reads what the household declared,
// what its meters have said and what has been done about it, works out
// where each commitment stands, and records the changes on the household
// timeline.
//
// It is the bridge keystone/commitment cannot be. That package is a leaf
// on the standard library — no storage, no clock, no Statefile — so
// something has to hand it the world, and this is that something. The
// split is the point: every decision this package makes is made by a
// pure function it can be tested without, and everything here is
// gathering and recording.
//
// WHAT IT DOES NOT DO. It does not notify anybody. When a commitment
// changes state it writes a commitment.* fact to the timeline and stops;
// who hears about it, on which surface, at what hour is an Automation the
// household writes (A12). That is why this package holds no model, no
// gateway and no notion of quiet hours, and why the household still
// knows the truck is due with every model in the house switched off.
package upkeep

import (
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/commitment"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

const (
	// Source is the publishing principal on every fact this package
	// records — Keystone itself, in the system/ family beside
	// system/automation. Not an API principal: nothing can mint a token
	// for it, so nothing but this engine can claim a commitment moved.
	Source = "system/commitment"

	// The household facts a commitment produces. One per state it can
	// reach, plus the one that asks for a number instead of announcing a
	// verdict — because "I cannot tell" is a thing the household needs
	// to hear and is not a state (A11).
	TypeDueSoon     = "commitment.due_soon"
	TypeOverdue     = "commitment.overdue"
	TypeScheduled   = "commitment.scheduled"
	TypeSatisfied   = "commitment.satisfied"
	TypeWaived      = "commitment.waived"
	TypeUnresolved  = "commitment.unresolved"
	TypeCycleOpened = "commitment.cycle_opened"
)

// Statefile is what this package needs from desired state.
type Statefile interface {
	Load() ([]resource.Resource, error)
}

// Engine runs the pass.
type Engine struct {
	DB    *runtimedb.DB
	State Statefile
	// Loc is the household's clock. A calendar step lands on the same
	// local day of the month, so this is not decoration (ADR-W029: a
	// household has one timezone).
	Loc *time.Location
	Log *slog.Logger
	// Now is overridable so a test can run a year in an afternoon.
	Now func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) loc() *time.Location {
	if e.Loc != nil {
		return e.Loc
	}
	return time.UTC
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// Standing is where one commitment stands, with everything a caller
// needs to render it without asking a second question.
type Standing struct {
	Name        string                       `json:"name"`
	Description string                       `json:"description"`
	Owner       string                       `json:"owner,omitempty"`
	Scope       string                       `json:"scope,omitempty"`
	MayComplete []string                     `json:"mayComplete,omitempty"`
	Notify      []string                     `json:"notify,omitempty"`
	Disabled    bool                         `json:"disabled,omitempty"`
	Occurrence  commitment.Occurrence        `json:"occurrence"`
	Booking     *runtimedb.CommitmentBooking `json:"booking,omitempty"`
}

// Evaluate works out where every declared commitment stands, right now,
// without recording anything.
//
// Side-effect free on purpose: it is what the Console, the CLI, the
// household's front page and the pass itself all call, and a read that
// quietly wrote would make four callers into four ways to change the
// world.
func (e *Engine) Evaluate() ([]Standing, error) {
	desired, err := e.State.Load()
	if err != nil {
		return nil, fmt.Errorf("upkeep: reading desired state: %w", err)
	}
	meters, err := e.meters(desired)
	if err != nil {
		return nil, err
	}
	now := e.now()

	var out []Standing
	for _, res := range desired {
		if res.Kind != resource.KindCommitment || res.Commitment == nil {
			continue
		}
		st, err := e.standing(res, meters, now)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// meters gathers every declared meter with its readings, once per pass
// rather than once per commitment — one odometer feeds the oil, the
// tyres and the brakes, and reading it three times would be three
// chances to see three different numbers.
func (e *Engine) meters(desired []resource.Resource) (map[string]commitment.Meter, error) {
	out := map[string]commitment.Meter{}
	for _, res := range desired {
		if res.Kind != resource.KindMeter || res.Meter == nil {
			continue
		}
		staleAfter, err := resource.MeterStaleAfter(res.Meter.StaleAfter)
		if err != nil {
			return nil, fmt.Errorf("upkeep: meter %q staleAfter: %w", res.Metadata.Name, err)
		}
		perDay, err := resource.MeterEstimatePerDay(res.Meter.Estimate)
		if err != nil {
			return nil, fmt.Errorf("upkeep: meter %q estimate: %w", res.Metadata.Name, err)
		}
		stored, err := e.DB.ListMeterReadings(res.Metadata.Name, 0)
		if err != nil {
			return nil, fmt.Errorf("upkeep: meter %q readings: %w", res.Metadata.Name, err)
		}
		m := commitment.Meter{
			Name: res.Metadata.Name, Unit: res.Meter.Unit,
			StaleAfter: staleAfter, Estimate: perDay,
		}
		for _, r := range stored {
			m.Readings = append(m.Readings, commitment.Reading{
				At: r.At, Value: r.Value, By: r.RecordedBy,
				Rebase: r.Rebase, Prior: r.Prior, Reason: r.Reason,
			})
		}
		out[res.Metadata.Name] = m
	}
	return out, nil
}

func (e *Engine) standing(res resource.Resource, meters map[string]commitment.Meter, now time.Time) (Standing, error) {
	spec := res.Commitment
	st := Standing{
		Name: res.Metadata.Name, Description: spec.Description,
		Owner: res.Metadata.Owner, Scope: spec.Scope,
		MayComplete: spec.MayComplete, Notify: spec.Notify, Disabled: spec.Disabled,
	}
	in, booking, err := e.input(res, meters, now)
	if err != nil {
		return st, err
	}
	st.Booking = booking
	occ, err := commitment.Evaluate(in)
	if err != nil {
		return st, fmt.Errorf("upkeep: commitment %q: %w", res.Metadata.Name, err)
	}
	st.Occurrence = occ
	return st, nil
}

// input turns a declared commitment plus the ledger into the pure
// function's argument.
func (e *Engine) input(res resource.Resource, meters map[string]commitment.Meter, now time.Time) (commitment.Input, *runtimedb.CommitmentBooking, error) {
	spec := res.Commitment
	// A bare date means that day in the household's own timezone, not in
	// UTC, and an RFC3339 instant already names its moment. Passing the
	// location in is why `resource` can tell them apart while still not
	// knowing what time it is anywhere.
	//
	// Rebuilding BOTH in loc was the bug: noon UTC became noon Chicago,
	// five hours late, and every threshold derived from the anchor moved
	// with it — to a date that still looked entirely plausible.
	anchor, err := resource.CommitmentAnchorAt(spec.Anchor, e.loc())
	if err != nil {
		return commitment.Input{}, nil, fmt.Errorf("upkeep: commitment %q anchor: %w", res.Metadata.Name, err)
	}

	c := commitment.Commitment{
		Name: res.Metadata.Name, Anchor: anchor,
		RequiresBooking: spec.RequiresBooking,
		Rollover:        commitment.Rollover(spec.Rollover),
		AnchorAt:        spec.AnchorAt,
	}
	used := map[string]bool{}
	for _, d := range spec.Due {
		rule := commitment.DueRule{Meter: d.Meter}
		if d.Every != "" {
			iv, perr := commitment.ParseInterval(d.Every)
			if perr != nil {
				return commitment.Input{}, nil, fmt.Errorf("upkeep: commitment %q: %w", res.Metadata.Name, perr)
			}
			rule.Every = iv
		}
		if d.Lead != "" {
			iv, perr := commitment.ParseInterval(d.Lead)
			if perr != nil {
				return commitment.Input{}, nil, fmt.Errorf("upkeep: commitment %q lead: %w", res.Metadata.Name, perr)
			}
			rule.Lead = iv
		}
		if d.At != "" {
			// The same distinction as the anchor, for the same reason: an
			// absolute due point given as an instant is that instant.
			at, perr := resource.CommitmentAnchorAt(d.At, e.loc())
			if perr != nil {
				return commitment.Input{}, nil, fmt.Errorf("upkeep: commitment %q due at: %w", res.Metadata.Name, perr)
			}
			rule.At = at
		}
		rule.AtValue = d.AtValue
		c.Due = append(c.Due, rule)
		used[d.Meter] = true
	}

	in := commitment.Input{Commitment: c, Now: now, Loc: e.loc(), Meters: map[string]commitment.Meter{}}
	for name := range used {
		if name == commitment.MeterClock {
			continue
		}
		// A commitment may name a meter whose declaration has been
		// removed. The engine reports that as unresolved rather than
		// failing the whole pass, because one deleted meter must not
		// stop the household hearing about everything else.
		in.Meters[name] = meters[name]
		if _, ok := meters[name]; !ok {
			in.Meters[name] = commitment.Meter{Name: name}
		}
	}

	completions, err := e.DB.ListCommitmentCompletions(res.Metadata.Name)
	if err != nil {
		return commitment.Input{}, nil, fmt.Errorf("upkeep: commitment %q completions: %w", res.Metadata.Name, err)
	}
	for _, done := range completions {
		in.History = append(in.History, commitment.Completion{
			At: done.At, By: done.RecordedBy, Waived: done.Waived,
			Reason: done.Reason, Positions: done.Positions,
		})
	}

	var live *runtimedb.CommitmentBooking
	if spec.RequiresBooking {
		b, berr := e.DB.LatestCommitmentBooking(res.Metadata.Name)
		switch {
		case berr == nil && b.Live():
			// A booking made for a cycle that has since closed is not
			// this cycle's appointment. The ledger keeps it; the open
			// cycle does not inherit it.
			if len(in.History) == 0 || b.BookedAt.After(in.History[len(in.History)-1].At) {
				live = &b
				in.Booking = &commitment.Booking{At: b.At, BookedAt: b.BookedAt, By: b.BookedBy}
			}
		case berr == nil:
			// Cancelled. Deliberately not passed on: the household has
			// no appointment, which is the whole question.
		case berr == runtimedb.ErrNotFound:
		default:
			return commitment.Input{}, nil, fmt.Errorf("upkeep: commitment %q booking: %w", res.Metadata.Name, berr)
		}
	}
	return in, live, nil
}
