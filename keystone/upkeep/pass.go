package upkeep

// The daily pass: work out where everything stands, and tell the
// household what changed.
//
// WHAT COUNTS AS A CHANGE is the whole design of this file. A commitment
// that has been overdue for three weeks is overdue every morning, and a
// household told so every morning stops reading its notifications — which
// is the failure ADR-W035's own costs section names as the one a schema
// cannot fix. So the pass announces TRANSITIONS, against a cache of what
// was last said, and says nothing at all on a quiet day.

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/commitment"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// Change is one thing the pass decided to say.
type Change struct {
	Commitment string `json:"commitment"`
	From       string `json:"from,omitempty"`
	To         string `json:"to"`
	Type       string `json:"type"`
	Seq        int64  `json:"seq"`
	Reason     string `json:"reason"`
}

// Tick runs one pass: evaluate everything, record what changed, and
// return it. Nothing else in this package writes.
func (e *Engine) Tick() ([]Change, error) {
	// The proof first, the verdict second. An event that closes a cycle
	// has to be in the ledger before the pass reads it, or the household
	// is told something is overdue in the same breath as its own
	// timeline says it was done.
	if err := e.harvest(); err != nil {
		return nil, err
	}
	standings, err := e.Evaluate()
	if err != nil {
		return nil, err
	}
	var changes []Change
	for _, st := range standings {
		if st.Disabled {
			// A paused commitment is not evaluated AT the household. Its
			// history is kept and its cache left alone, so un-pausing
			// says whatever is true then rather than replaying what was
			// true when it was paused.
			continue
		}
		change, err := e.announce(st)
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}
	return changes, nil
}

// announce records one commitment's transition, if it is one.
func (e *Engine) announce(st Standing) (*Change, error) {
	occ := st.Occurrence
	// "I cannot tell" is reported as itself rather than as the pending it
	// technically is, because the household's answer to it is different:
	// pending needs nothing, and an unreadable meter needs somebody to go
	// and look (A11).
	state := string(occ.State)
	if len(occ.Unresolved) > 0 && occ.Trigger == "" {
		state = "unresolved"
	}

	last, err := e.DB.AnnouncedCommitmentState(st.Name)
	switch {
	case err == nil:
		// A cycle closing and reopening into the same state is news even
		// though the state did not change — the checkup was DONE, which
		// is the most interesting thing that has happened to it all year.
		if last.State == state && last.Cycle == occ.Cycle && last.Trigger == occ.Trigger {
			return nil, nil
		}
	case err == runtimedb.ErrNotFound:
		// Never announced. A commitment that starts life pending has
		// nothing to say, so the household is not told about every rule
		// it just approved; anything else is worth a first word.
		if state == string(commitment.StatePending) {
			return nil, e.DB.AnnounceCommitmentState(runtimedb.CommitmentState{
				Commitment: st.Name, State: state, Cycle: occ.Cycle, Trigger: occ.Trigger,
			})
		}
	default:
		return nil, fmt.Errorf("upkeep: reading announced state for %q: %w", st.Name, err)
	}

	typ, ok := eventTypeFor(state, last.Cycle, occ.Cycle)
	if !ok {
		// A transition with nothing to say: pending after pending on a
		// new cycle is covered by cycle_opened, and everything else that
		// reaches here is a state the timeline has no fact for.
		return nil, e.DB.AnnounceCommitmentState(runtimedb.CommitmentState{
			Commitment: st.Name, State: state, Cycle: occ.Cycle, Trigger: occ.Trigger,
		})
	}

	payload, _ := json.Marshal(map[string]any{
		"state": state, "from": last.State, "cycle": occ.Cycle,
		"trigger": occ.Trigger, "reason": occ.Reason,
		"unresolved": occ.Unresolved, "pastDue": occ.PastDue,
		"mayComplete": st.MayComplete, "notify": st.Notify,
		"scope": st.Scope, "owner": st.Owner,
	})
	seq, _, err := e.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		// Deterministic: one fact per commitment per cycle per state, so
		// a pass that runs twice — a restart, an operator poking it —
		// records one of each rather than two.
		EventID:    fmt.Sprintf("%s@%d@%s", st.Name, occ.Cycle, state),
		OccurredAt: e.now(),
		Subject:    subjectOf(st),
		Source:     Source,
		Type:       typ,
		Entity:     st.Name,
		Payload:    payload,
	})
	if err != nil {
		return nil, fmt.Errorf("upkeep: recording %s for %q: %w", typ, st.Name, err)
	}
	if err := e.DB.AnnounceCommitmentState(runtimedb.CommitmentState{
		Commitment: st.Name, State: state, Cycle: occ.Cycle, Trigger: occ.Trigger,
	}); err != nil {
		return nil, err
	}
	e.log().Info("commitment changed", "commitment", st.Name,
		"from", last.State, "to", state, "cycle", occ.Cycle)
	return &Change{
		Commitment: st.Name, From: last.State, To: state,
		Type: typ, Seq: seq, Reason: occ.Reason,
	}, nil
}

// eventTypeFor maps a state to the fact the timeline carries, and says
// when there is nothing to carry.
func eventTypeFor(state string, lastCycle, cycle int) (string, bool) {
	switch state {
	case string(commitment.StateDueSoon):
		return TypeDueSoon, true
	case string(commitment.StateOverdue):
		return TypeOverdue, true
	case string(commitment.StateScheduled):
		return TypeScheduled, true
	case string(commitment.StateSatisfied):
		return TypeSatisfied, true
	case string(commitment.StateWaived):
		return TypeWaived, true
	case "unresolved":
		return TypeUnresolved, true
	case string(commitment.StatePending):
		// Pending is only news when it is a NEW cycle: the thing was
		// done and the next one has opened. Pending after pending is the
		// definition of a quiet day.
		if lastCycle != 0 && cycle > lastCycle {
			return TypeCycleOpened, true
		}
		return "", false
	}
	return "", false
}

// subjectOf is who the fact is about, for a stream consumer filtering by
// person. The commitment's owner when it names one; the household
// otherwise.
func subjectOf(st Standing) string {
	const prefix = "household/"
	if len(st.Owner) > len(prefix) && st.Owner[:len(prefix)] == prefix {
		return st.Owner[len(prefix):]
	}
	return ""
}

// Run drives Tick on a timer until the context is done.
//
// Once a day, at a fixed hour, plus immediately on demand. A73's
// reasoning applies unchanged: nothing here needs minute precision — a
// yearly checkup with a sixty-day lead and an oil change with a
// three-hundred-mile lead are both indifferent to whether the pass ran
// at 07:00 or 07:04 — and scheduled duty belongs to Keystone rather than
// to cron on a host (ADR-W011 A4).
func (e *Engine) Run(done <-chan struct{}, poke <-chan struct{}, at time.Time) {
	for {
		wait := time.Until(at)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-done:
			timer.Stop()
			return
		case <-poke:
			timer.Stop()
		case <-timer.C:
			at = at.Add(24 * time.Hour)
		}
		if changes, err := e.Tick(); err != nil {
			e.log().Error("commitment pass failed", "error", err)
		} else if len(changes) > 0 {
			e.log().Info("commitment pass", "changes", len(changes))
		}
	}
}

// harvestedBy is who a completion an event proved is recorded as.
//
// Not the publisher and not a member: the publisher said the truck was
// serviced, which is a fact about the truck, and turning that into "this
// cycle is closed" is this engine's inference. A household reading its
// own history should be able to tell the two apart.
const harvestedBy = "system/commitment"

// maxHarvest bounds how many matching events one pass reads per
// commitment. Only the first usable one closes the cycle, so this is a
// guard against a publisher that has emitted the same verb ten thousand
// times, not a limit on anything the household would notice.
const maxHarvest = 200

// harvest closes the cycles that household events already prove.
//
// A commitment may name the event that satisfies it — `vehicle.serviced`
// for the truck's oil — and before this the field validated, generated a
// Console type and did nothing else (Codex, #244). A household that
// declared it watched the commitment go overdue with the proof sitting
// in its own timeline.
//
// THE EVENT IS EVIDENCE, NOT AN INSTRUCTION. It closes the cycle only
// when the completion it implies can carry the numbers the next cycle
// needs — the same rule a member tapping Done is held to, in the same
// function. A `vehicle.serviced` that arrives while the odometer is
// three months stale leaves the commitment open and asking for the
// number, which is correct: the household knows the truck was serviced
// and still cannot say what the next oil change is measured from.
//
// It is also evaluated AT THE EVENT'S MOMENT rather than now. The
// package is pure and takes the time it is asked about precisely so
// this is possible, and "what did the odometer say when the truck was
// serviced" is the question a completion records.
func (e *Engine) harvest() error {
	desired, err := e.State.Load()
	if err != nil {
		return fmt.Errorf("upkeep: reading desired state: %w", err)
	}
	meters, err := e.meters(desired)
	if err != nil {
		return err
	}
	now := e.now()
	for _, res := range desired {
		if res.Kind != resource.KindCommitment || res.Commitment == nil {
			continue
		}
		by := res.Commitment.SatisfiedBy
		if by == nil || res.Commitment.Disabled {
			continue
		}
		st, err := e.standing(res, meters, now)
		if err != nil {
			return err
		}
		// ALREADY ANSWERED IS ALREADY ANSWERED, whichever door it came
		// through. A cycle holding a completion at or after the moment
		// it opened has been closed; harvesting into it would close it
		// twice.
		//
		// This is the rule that stops two doors recording one visit as
		// two. A parent taps Done at 14:00, the clinic's publisher emits
		// the same fact at 14:05, and the completion that closed cycle 1
		// also OPENED cycle 2 at 14:00 — so the event landed inside the
		// new cycle's window and closed that too. One checkup, two
		// completions, and the commitment skipped a year. It would have
		// happened on every visit in any household using both doors.
		//
		// No grace window, because there is no honest number for one:
		// two timestamps cannot say whether they describe the same
		// visit. The completion that opened a cycle is the evidence that
		// the cycle's own question was already answered.
		answered, err := e.answeredIn(res.Metadata.Name, st.Occurrence.OpenedAt)
		if err != nil {
			return err
		}
		if answered {
			continue
		}
		events, err := e.DB.QueryHouseholdEvents(runtimedb.HouseholdQuery{
			Type: by.EventType, Subject: by.Subject, SubjectSet: by.Subject != "",
			Entity: by.Entity,
			// From the moment this cycle opened: an event that closed the
			// LAST one must not close this one too, and a service done
			// before the commitment existed is not evidence about it.
			From: st.Occurrence.OpenedAt, To: now, Limit: maxHarvest,
		})
		if err != nil {
			return fmt.Errorf("upkeep: commitment %q satisfiedBy: %w", res.Metadata.Name, err)
		}
		for _, ev := range events {
			asOf, err := e.standing(res, meters, ev.OccurredAt)
			if err != nil {
				return err
			}
			positions, approx, eerr := Evidence(asOf, nil)
			if eerr != nil {
				// The proof arrived and the numbers did not. The
				// commitment stays open, and its own unresolved item is
				// already asking for exactly what is missing.
				if e.Log != nil {
					e.Log.Info("upkeep: an event could not close a cycle yet",
						"commitment", res.Metadata.Name, "event", ev.EventID, "why", eerr)
				}
				continue
			}
			_, duplicate, err := e.DB.RecordCommitmentCompletion(runtimedb.CommitmentCompletion{
				Commitment: res.Metadata.Name, At: ev.OccurredAt, RecordedBy: harvestedBy,
				Reason: "recorded from " + ev.Type, Positions: positions,
				Approximate: approx, ExternalID: ev.EventID,
			})
			if err != nil {
				return fmt.Errorf("upkeep: commitment %q satisfiedBy: %w", res.Metadata.Name, err)
			}
			if duplicate {
				// This one already closed a cycle. Keep looking: the
				// event that closes THIS cycle may be further down the
				// stream, and stopping here would hide it.
				continue
			}
			// One event closes one cycle. Whether the next one is
			// already closed too is the next pass's question, asked of a
			// ledger that now contains this.
			break
		}
	}
	return nil
}

// answeredIn reports whether a completion already sits at or after the
// moment the open cycle began.
//
// `at or after` rather than `after`: a fromCompletion cycle OPENS at the
// position its predecessor's completion recorded, so that completion's
// own timestamp is the cycle's opening moment. Excluding it would leave
// every freshly opened cycle looking unanswered to the very event that
// answered the one before.
//
// It also covers the one-off, whose cycle never reopens: an event
// arriving after the dentist has been to has nothing left to close.
func (e *Engine) answeredIn(name string, openedAt time.Time) (bool, error) {
	done, err := e.DB.ListCommitmentCompletions(name)
	if err != nil {
		return false, fmt.Errorf("upkeep: commitment %q completions: %w", name, err)
	}
	for _, c := range done {
		if !c.At.Before(openedAt) {
			return true, nil
		}
	}
	return false, nil
}
