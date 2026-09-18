package upkeep

// The daily pass: work out where everything stands, and tell the
// household what changed.
//
// WHAT COUNTS AS A CHANGE is the whole design of this file. A commitment
// that has been overdue for three weeks is overdue every morning, and a
// household told so every morning stops reading its notifications — which
// is the failure ADR-W034's own costs section names as the one a schema
// cannot fix. So the pass announces TRANSITIONS, against a cache of what
// was last said, and says nothing at all on a quiet day.

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/commitment"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
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
