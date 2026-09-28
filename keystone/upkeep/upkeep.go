// Package upkeep is the daily pass over the household's commitments
// (ADR-W035 A15, work package K4): it reads what the household declared,
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
	"github.com/chickenrat17/wildstone-homelab/keystone/householdtz"
	"log/slog"
	"slices"
	"sort"
	"strings"
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
	// Zone, when set, is the household's live timezone and wins over
	// Location, so an approved change applies without a restart.
	Zone *householdtz.Zone
	Log  *slog.Logger
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
	if l := e.Zone.Location(); l != nil {
		return l
	}
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
	Name        string `json:"name"`
	Description string `json:"description"`
	Owner       string `json:"owner,omitempty"`
	Scope       string `json:"scope,omitempty"`
	// PartOfDay is when in the day a routine belongs, empty for the
	// things that belong nowhere in particular.
	PartOfDay string `json:"partOfDay,omitempty"`
	// Every is the interval of the FIRST due rule — the one a page
	// renders and the one homeEditCommitment replaces. Carried so an
	// edit form can open on what the routine says rather than on a
	// guess the household then has to correct.
	Every       string   `json:"every,omitempty"`
	MayComplete []string `json:"mayComplete,omitempty"`
	Notify      []string `json:"notify,omitempty"`
	Disabled    bool     `json:"disabled,omitempty"`
	// Rollover is what happens when a cycle closes. A surface needs it
	// to know whether closing this one opens another, and therefore
	// whether the evidence it is about to record will have to measure
	// anything.
	Rollover string `json:"rollover,omitempty"`
	// RequiresBooking is whether an appointment is part of answering
	// this. A surface needs it to know whether "booked" is an answer on
	// offer or a verb this commitment has no use for.
	RequiresBooking bool                         `json:"requiresBooking,omitempty"`
	Occurrence      commitment.Occurrence        `json:"occurrence"`
	Booking         *runtimedb.CommitmentBooking `json:"booking,omitempty"`
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
		Owner: e.currentPerson(res.Metadata.Owner), Scope: spec.Scope, PartOfDay: spec.PartOfDay,
		Every:       firstDueEvery(spec),
		MayComplete: e.currentPeople(spec.MayComplete), Notify: e.currentPeople(spec.Notify), Disabled: spec.Disabled,
		Rollover: spec.Rollover, RequiresBooking: spec.RequiresBooking,
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

	// A LIVE BOOKING IS READ FOR EVERY COMMITMENT, and fed to the engine
	// only for the ones that require one.
	//
	// Those are two different questions and this used to answer only
	// the second. `requiresBooking` means "an appointment is part of
	// answering this" — the dentist. A booking on a commitment that
	// does NOT require one means something simpler and just as real:
	// this cycle is happening on a different day. The bins go out on a
	// Thursday and this week, for the holiday, on the Friday.
	//
	// Reading it for everything is what lets a surface move one cycle
	// without touching the schedule. Feeding it to the engine only when
	// it is required is what keeps that from changing anybody's state:
	// a chore does not become "booked" because somebody shifted it, and
	// an overdue one does not stop being overdue.
	var live *runtimedb.CommitmentBooking
	b, berr := e.DB.LatestCommitmentBooking(res.Metadata.Name)
	switch {
	case berr == nil && b.Live():
		// A booking made for a cycle that has since closed is not
		// this cycle's appointment. The ledger keeps it; the open
		// cycle does not inherit it. That is also exactly what makes
		// a one-week shift a one-week shift.
		if len(in.History) == 0 || b.BookedAt.After(in.History[len(in.History)-1].At) {
			live = &b
			if spec.RequiresBooking {
				in.Booking = &commitment.Booking{At: b.At, BookedAt: b.BookedAt, By: b.BookedBy}
			}
		}
	case berr == nil:
		// Cancelled. Deliberately not passed on: the household has
		// no appointment, which is the whole question.
	case berr == runtimedb.ErrNotFound:
	default:
		return commitment.Input{}, nil, fmt.Errorf("upkeep: commitment %q booking: %w", res.Metadata.Name, berr)
	}
	return in, live, nil
}

// Evidence is where A8 stops being a comment, and where it stops being
// stricter than the household needs.
//
// A RECURRING COUNTER CYCLE CANNOT BE CLOSED WITH NO NUMBER AT ALL. The
// position a meter stood at is what the next cycle is measured from, so
// a completion holding nothing closes this cycle and opens one that can
// never be concluded — and no endpoint deletes a completion, so the
// household cannot take it back.
//
// BUT A ROUGH NUMBER IS NOT NO NUMBER, and an earlier version of this
// refused them alike. The point of the tracker is that the oil gets
// changed at about five thousand miles, not that the odometer at the
// moment of service is known exactly. Refusing a stale reading meant a
// `vehicle.serviced` event could not close the cycle at all, so the
// commitment sat overdue forever — and a household nagged forever about
// something it has already done stops reading its notifications, which
// is the one failure this design says a schema cannot fix.
//
// The error runs the safe way, which is what makes this sound. A stale
// reading is BEHIND where the meter actually stood, so the next cycle is
// measured from too low a number and comes due EARLY. Nagging sooner is
// a nuisance; nagging never is a missed oil change.
//
// So: a number typed by somebody standing at the truck is best, a stale
// reading is accepted and recorded as approximate, and nothing at all is
// still refused. The second return names the meters whose position was
// guessed, so the record says so rather than implying a precision it
// does not have.
//
// It lives HERE, in one function, because two things close cycles: a
// member tapping Done and a household event a commitment named in
// `satisfiedBy`. The same rule has to govern both.
func Evidence(st Standing, given map[string]float64) (map[string]float64, []string, error) {
	positions, approx, missing := evidence(st, given)
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf(
			"what did %s say? nobody has ever read it, so closing this would leave the next one unmeasurable",
			strings.Join(missing, " and "))
	}
	return positions, approx, nil
}

// Missing names the meters a completion cannot supply any number for.
// These BLOCK the write.
//
// IT EXISTS SO THAT A SURFACE CAN ASK THE SAME QUESTION THE WRITE PATH
// WILL. A card that worked this out for itself got it wrong twice over:
// it offered Done where Evidence refused, so every tap returned 400, and
// it offered only a reading where a reading could not help, so there was
// no way forward at all.
func Missing(st Standing, given map[string]float64) []string {
	_, _, missing := evidence(st, given)
	return missing
}

// Wanted names the meters whose number would be taken from a reading too
// old to rely on. These do NOT block the write — the card asks for them
// so the household can do better than a guess, and accepts the guess
// when nobody answers.
func Wanted(st Standing, given map[string]float64) []string {
	_, approx, _ := evidence(st, given)
	return approx
}

// evidence returns the positions a completion would carry, the meters
// among them whose position came from a reading too old to rely on, and
// the meters it could supply nothing for at all.
func evidence(st Standing, given map[string]float64) (positions map[string]float64, approx, missing []string) {
	// ONLY THE METERS THIS COMMITMENT MEASURES AGAINST. `given` is
	// whatever a client sent, and the card now collects numbers per
	// METER rather than per commitment — so a page holding one truck's
	// odometer and one water filter could reasonably hand the same map
	// to every answer on it.
	//
	// `positions` is documented as what the NEXT cycle is measured
	// from, and a tyre rotation carrying the water filter's gallons is
	// a record that means something it does not. Copying the map
	// wholesale let a client decide what a completion claims.
	positions = map[string]float64{}
	if st.Rollover == resource.RolloverOnce {
		// Nothing is measured from a one-off's completion, because
		// nothing follows it — so it records nothing either.
		return positions, nil, nil
	}
	for _, rule := range st.Occurrence.Rules {
		// The clock's position is the completion's own `at`, which is
		// always known. Only counters need answering.
		if rule.Calendar || rule.Meter == resource.MeterClock {
			continue
		}
		if v, ok := given[rule.Meter]; ok {
			// Somebody typed it. Nothing beats that.
			positions[rule.Meter] = v
			continue
		}
		if !rule.Known {
			if !slices.Contains(missing, rule.Meter) {
				missing = append(missing, rule.Meter)
			}
			continue
		}
		positions[rule.Meter] = rule.Position
		if rule.Stale && !slices.Contains(approx, rule.Meter) {
			approx = append(approx, rule.Meter)
		}
	}
	return positions, approx, missing
}

// firstDueEvery is the interval of the rule a page renders: the first,
// which is the one homeEditCommitment replaces.
func firstDueEvery(spec *resource.CommitmentSpec) string {
	if spec == nil || len(spec.Due) == 0 {
		return ""
	}
	return spec.Due[0].Every
}

// currentPerson rewrites `household/<person>` to the handle that Person
// carries now (#372, ADR-W035 A21).
//
// A commitment names a person as the household typed them: by the handle
// they had then, or by their person_id. Everything downstream compares
// and displays the current handle, so the reference is brought up to date
// once, here, where every standing is built. A handle that belonged to
// somebody who has since been renamed still reaches them, because 0093
// keeps every handle a Person has carried. A reference naming nobody on
// record is left as written, and so matches nobody.
func (e *Engine) currentPerson(ref string) string {
	rest, ok := strings.CutPrefix(ref, "household/")
	if !ok || rest == "" || e.DB == nil {
		return ref
	}
	id, err := e.DB.ResolvePersonRef(rest)
	if err != nil || id == "" {
		return ref
	}
	handle, err := e.DB.PersonHandle(id)
	if err != nil {
		return ref
	}
	return "household/" + handle
}

// currentPeople is currentPerson over a list, on a copy: the spec is the
// household's declaration and is not rewritten.
func (e *Engine) currentPeople(refs []string) []string {
	if refs == nil {
		return nil
	}
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = e.currentPerson(r)
	}
	return out
}
