// Package automation is the deterministic household rule engine (ADR-W022
// A6/A7, work package H3): it follows the household event stream, matches
// each event against the approved Automation resources in the Statefile,
// evaluates their conditions in code, and records every decision —
// fired or suppressed, and why — back onto the same stream, where it can
// be read, correlated, and replayed.
//
// Nothing here calls a model, and nothing here talks to a service. The
// engine decides; a fired decision's action is handed to the domestic
// gateway through the Actions seam (act.go), and escalation to the
// Butler (H6) will hang off the same point. A rule with no escalation
// never involves a model at all.
//
// Every decision is a pure function of (rule, event, what the stream
// says) — Evaluate — so a rule can be run against history with no side
// effects (Replay) before anyone approves it, which is the only way to
// test a rule (ADR-W022 A7).
//
// This package never imports keystone/reconcile (the apply-capable
// surface) or keystone/butler (the model); structural_test.go pins it.
package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

const (
	// Source is the publishing principal name on every decision the
	// engine records — Keystone itself, in the system/ family beside
	// system/oob and system/drift. It is not an API principal: nothing
	// can mint a token for it, so nothing but the engine can write a
	// decision.
	Source = "system/automation"
	// TypeFired is recorded when every condition held: the rule would act.
	TypeFired = "automation.fired"
	// TypeSuppressed is recorded when a condition failed, with the reason.
	TypeSuppressed = "automation.suppressed"

	// DefaultMaxTriggerAge bounds how old a trigger may be and still fire.
	// After an outage the engine catches up on the stream; a "left home"
	// from six hours ago must not ask about breakfast now. Such triggers
	// are recorded as suppressed with a stale reason, not skipped: the
	// record is still the audit.
	DefaultMaxTriggerAge = time.Hour
	// DefaultBatch is how many events one Tick reads.
	DefaultBatch = 200
)

// Lookup is everything a condition may consult. Two implementations: the
// live stream (dbLookup) and the replay view (replayLookup), which sees
// the same history but keeps its own firings so a draft rule's cooldown
// is simulated rather than read from records it never wrote.
type Lookup interface {
	// Count returns how many events of eventType occurred in [from, to],
	// for the given subject when subjectSet, excluding the event with
	// seq excludeSeq (the trigger itself).
	Count(eventType string, subject string, subjectSet bool, from, to time.Time, excludeSeq int64) (int, error)
	// LastFired returns when rule last fired for subject strictly before
	// t, if ever.
	LastFired(rule, subject string, before time.Time) (time.Time, bool, error)
}

// Decision is one evaluation of one rule against one event.
type Decision struct {
	Rule      string    `json:"rule"`
	EventSeq  int64     `json:"eventSeq"`
	EventID   string    `json:"eventId"`
	EventType string    `json:"eventType"`
	Subject   string    `json:"subject,omitempty"`
	At        time.Time `json:"at"`
	Fired     bool      `json:"fired"`
	// Reason says why a suppressed decision stopped, naming the
	// condition; for a fired one it is "all conditions held".
	Reason string `json:"reason"`
	// Action and Tier are what a fired decision would do (H4).
	Action string `json:"action,omitempty"`
	Tier   string `json:"tier,omitempty"`
	// Escalates says whether the rule asks the Butler first (H6).
	Escalates bool `json:"escalates,omitempty"`
}

// Matches reports whether ev is a trigger for rule: same event type,
// and the rule's subject (when set) is the event's. A disabled rule
// matches nothing. Events the engine itself recorded never trigger a
// rule — that is the loop guard: a rule cannot fire on a firing.
func Matches(rule resource.Resource, ev runtimedb.HouseholdEvent) bool {
	a := rule.Automation
	if rule.Kind != resource.KindAutomation || a == nil || a.Disabled {
		return false
	}
	if ev.Source == Source {
		return false
	}
	if a.Trigger.EventType != ev.Type {
		return false
	}
	return a.Trigger.Subject == "" || a.Trigger.Subject == ev.Subject
}

// Evaluate runs rule's conditions, in order, against ev. loc is the
// household's wall clock for timeOfDay and weekday (nil = UTC). It
// assumes Matches held and rule passed resource validation; a malformed
// rule here is a programming error surfaced as an error, not a decision.
func Evaluate(rule resource.Resource, ev runtimedb.HouseholdEvent, look Lookup, loc *time.Location) (Decision, error) {
	a := rule.Automation
	if loc == nil {
		loc = time.UTC
	}
	d := Decision{
		Rule: rule.Metadata.Name, EventSeq: ev.Seq, EventID: ev.EventID, EventType: ev.Type,
		Subject: ev.Subject, At: ev.OccurredAt,
		Action: a.Action.Tool, Tier: a.Action.Tier, Escalates: a.Escalation != nil,
	}
	for i, c := range a.Conditions {
		ok, detail, err := evalCondition(rule.Metadata.Name, c, ev, look, loc)
		if err != nil {
			return d, fmt.Errorf("automation %s: conditions[%d] %s: %w", rule.Metadata.Name, i, c.Kind, err)
		}
		if !ok {
			d.Fired = false
			d.Reason = fmt.Sprintf("conditions[%d] %s: %s", i, c.Kind, detail)
			return d, nil
		}
	}
	d.Fired = true
	d.Reason = "all conditions held"
	return d, nil
}

func evalCondition(rule string, c resource.AutomationCondition, ev runtimedb.HouseholdEvent, look Lookup, loc *time.Location) (bool, string, error) {
	switch c.Kind {
	case "eventAbsent", "eventPresent":
		window, err := time.ParseDuration(c.Window)
		if err != nil {
			return false, "", err
		}
		subject := ""
		if c.SameSubject {
			subject = ev.Subject
		}
		n, err := look.Count(c.EventType, subject, c.SameSubject, ev.OccurredAt.Add(-window), ev.OccurredAt, ev.Seq)
		if err != nil {
			return false, "", err
		}
		who := ""
		if c.SameSubject {
			who = " for " + subjectLabel(ev.Subject)
		}
		if c.Kind == "eventAbsent" {
			return n == 0, fmt.Sprintf("%d %s%s in the last %s", n, c.EventType, who, c.Window), nil
		}
		return n > 0, fmt.Sprintf("no %s%s in the last %s", c.EventType, who, c.Window), nil
	case "timeOfDay":
		local := ev.OccurredAt.In(loc)
		now := local.Hour()*60 + local.Minute()
		after, before := -1, -1
		if c.After != "" {
			after = clockMinutes(c.After)
		}
		if c.Before != "" {
			before = clockMinutes(c.Before)
		}
		var ok bool
		switch {
		case after >= 0 && before >= 0 && after > before:
			// An overnight window: after 22:00 or before 06:00.
			ok = now >= after || now < before
		default:
			ok = (after < 0 || now >= after) && (before < 0 || now < before)
		}
		return ok, fmt.Sprintf("%s local is outside %s", local.Format("15:04"), clockRange(c.After, c.Before)), nil
	case "weekday":
		day := strings.ToLower(ev.OccurredAt.In(loc).Weekday().String()[:3])
		for _, d := range c.Days {
			if d == day {
				return true, "", nil
			}
		}
		return false, fmt.Sprintf("%s is not one of %s", day, strings.Join(c.Days, ",")), nil
	case "cooldown":
		window, err := time.ParseDuration(c.Window)
		if err != nil {
			return false, "", err
		}
		last, ok, err := look.LastFired(rule, ev.Subject, ev.OccurredAt)
		if err != nil {
			return false, "", err
		}
		if !ok {
			return true, "", nil
		}
		since := ev.OccurredAt.Sub(last)
		if since < window {
			return false, fmt.Sprintf("fired %s ago for %s, within the %s cooldown", since.Round(time.Minute), subjectLabel(ev.Subject), c.Window), nil
		}
		return true, "", nil
	default:
		return false, "", fmt.Errorf("unknown condition kind %q", c.Kind)
	}
}

func clockMinutes(hhmm string) int {
	h, _ := strconv.Atoi(hhmm[:2])
	m, _ := strconv.Atoi(hhmm[3:])
	return h*60 + m
}

func clockRange(after, before string) string {
	switch {
	case after != "" && before != "":
		return after + "–" + before
	case after != "":
		return "after " + after
	default:
		return "before " + before
	}
}

func subjectLabel(s string) string {
	if s == "" {
		return "no subject"
	}
	return s
}

// ---------------------------------------------------------------- lookups

// dbLookup answers conditions from the live stream.
type dbLookup struct{ db *runtimedb.DB }

func (l dbLookup) Count(eventType, subject string, subjectSet bool, from, to time.Time, excludeSeq int64) (int, error) {
	events, err := l.db.QueryHouseholdEvents(runtimedb.HouseholdQuery{
		Type: eventType, Subject: subject, SubjectSet: subjectSet, From: from, To: to, Limit: 1000,
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range events {
		if e.Seq != excludeSeq {
			n++
		}
	}
	return n, nil
}

func (l dbLookup) LastFired(rule, subject string, before time.Time) (time.Time, bool, error) {
	events, err := l.db.QueryHouseholdEvents(runtimedb.HouseholdQuery{
		Type: TypeFired, Source: Source, Entity: rule, Subject: subject, SubjectSet: true,
		To: before.Add(-time.Nanosecond), Newest: true, Limit: 1,
	})
	if err != nil || len(events) == 0 {
		return time.Time{}, false, err
	}
	return events[0].OccurredAt, true, nil
}

// replayLookup sees the same history for event counts but keeps its own
// firings for cooldowns, so a draft rule is simulated from a clean slate
// and an existing rule's replay does not double-count against records
// the real engine wrote for the same window.
type replayLookup struct {
	dbLookup
	fired map[string][]time.Time // rule+"\x00"+subject → firing times, ascending
}

func (l *replayLookup) LastFired(rule, subject string, before time.Time) (time.Time, bool, error) {
	times := l.fired[rule+"\x00"+subject]
	for i := len(times) - 1; i >= 0; i-- {
		if times[i].Before(before) {
			return times[i], true, nil
		}
	}
	return time.Time{}, false, nil
}

func (l *replayLookup) record(d Decision) {
	if l.fired == nil {
		l.fired = map[string][]time.Time{}
	}
	key := d.Rule + "\x00" + d.Subject
	l.fired[key] = append(l.fired[key], d.At)
}

// ---------------------------------------------------------------- engine

// Engine follows the stream and records decisions. Zero values are
// usable except DB and State.
type Engine struct {
	DB    *runtimedb.DB
	State *statefile.Store
	Log   *slog.Logger
	// Location is the household's wall clock for timeOfDay and weekday
	// conditions. nil is UTC, which is almost never what a household
	// means; cmd/keystone sets it from -household-timezone.
	Location *time.Location
	// Now is the engine clock; nil means time.Now.
	Now func() time.Time
	// Actions, when set, performs a fired rule's action through the
	// domestic gateway (ADR-W022 A5). nil leaves decisions recorded and
	// unacted — which is what a control plane with no capabilities
	// installed should do, and what the engine did before H4.
	Actions Actions
	// Escalator, when set, phrases the one action param a rule's
	// escalation names (ADR-W022 A6). nil means an escalating rule acts
	// with the wording its author wrote, which is the deterministic
	// backbone doing its job rather than a degraded mode.
	Escalator Escalator
	// MaxTriggerAge: see DefaultMaxTriggerAge. Zero means the default.
	MaxTriggerAge time.Duration
	// Batch: see DefaultBatch. Zero means the default.
	Batch int

	poke     chan struct{}
	pokeOnce sync.Once
}

// TickOutcome summarizes one Tick.
type TickOutcome struct {
	// Adopted is true when a first tick over an existing stream moved
	// the cursor to its end without evaluating: history that predates
	// the engine is not a backlog of things to do now.
	Adopted bool
	// Evaluated counts (rule, event) pairs decided this tick.
	Evaluated, Fired, Suppressed int
	// Cursor is the stream position after the tick.
	Cursor int64
	// Decisions are what was recorded, in order.
	Decisions []Decision
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// Rules returns the enabled Automation resources in the Statefile.
func (e *Engine) Rules() ([]resource.Resource, error) {
	all, err := e.State.Load()
	if err != nil {
		return nil, fmt.Errorf("automation: loading statefile: %w", err)
	}
	var rules []resource.Resource
	for _, r := range all {
		if r.Kind == resource.KindAutomation && r.Automation != nil && !r.Automation.Disabled {
			rules = append(rules, r)
		}
	}
	return rules, nil
}

// Tick reads the next batch of events past the cursor, decides every
// matching rule for each, records the decisions, and advances the
// cursor. Decisions carry a deterministic event id (rule@triggerSeq), so
// a tick that is retried after a crash records nothing twice.
func (e *Engine) Tick(ctx context.Context) (TickOutcome, error) {
	var out TickOutcome
	cursor, err := e.DB.AutomationCursor()
	if err != nil {
		return out, err
	}
	batch := e.Batch
	if batch <= 0 {
		batch = DefaultBatch
	}
	if cursor == 0 {
		// First run over a stream with history: adopt its end. The
		// stream may already carry decisions from a replay-free past;
		// none of them are ours to act on now.
		latest, err := e.DB.QueryHouseholdEvents(runtimedb.HouseholdQuery{Newest: true, Limit: 1})
		if err != nil {
			return out, err
		}
		if len(latest) > 0 {
			cursor = latest[0].Seq
			if err := e.DB.SetAutomationCursor(cursor); err != nil {
				return out, err
			}
			out.Adopted, out.Cursor = true, cursor
			e.log().Info("automation: adopted stream position", "seq", cursor)
			return out, nil
		}
	}
	out.Cursor = cursor

	events, err := e.DB.HouseholdEventsAfter(cursor, batch)
	if err != nil {
		return out, err
	}
	if len(events) == 0 {
		return out, nil
	}
	rules, err := e.Rules()
	if err != nil {
		return out, err
	}
	maxAge := e.MaxTriggerAge
	if maxAge <= 0 {
		maxAge = DefaultMaxTriggerAge
	}
	look := dbLookup{db: e.DB}
	now := e.now()

	for _, ev := range events {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		for _, rule := range rules {
			if !Matches(rule, ev) {
				continue
			}
			var d Decision
			if age := now.Sub(ev.OccurredAt); age > maxAge {
				a := rule.Automation
				d = Decision{
					Rule: rule.Metadata.Name, EventSeq: ev.Seq, EventID: ev.EventID, EventType: ev.Type,
					Subject: ev.Subject, At: ev.OccurredAt, Action: a.Action.Tool, Tier: a.Action.Tier,
					Escalates: a.Escalation != nil,
					Reason:    fmt.Sprintf("stale: trigger occurred %s ago, beyond the %s bound", age.Round(time.Minute), maxAge),
				}
			} else {
				d, err = Evaluate(rule, ev, look, e.Location)
				if err != nil {
					return out, err
				}
			}
			if err := e.record(d, ev); err != nil {
				return out, err
			}
			if err := e.act(ctx, d, ev, rule); err != nil {
				return out, err
			}
			out.Evaluated++
			if d.Fired {
				out.Fired++
			} else {
				out.Suppressed++
			}
			out.Decisions = append(out.Decisions, d)
		}
		cursor = ev.Seq
		if err := e.DB.SetAutomationCursor(cursor); err != nil {
			return out, err
		}
		out.Cursor = cursor
	}
	return out, nil
}

// record writes one decision onto the household stream.
func (e *Engine) record(d Decision, trigger runtimedb.HouseholdEvent) error {
	typ := TypeSuppressed
	if d.Fired {
		typ = TypeFired
	}
	// What happened NEXT is not this record's business: a decision is a
	// decision. The follow-on automation.acted / action_failed /
	// escalation_pending events say whether anything ran (see act.go).
	payload, _ := json.Marshal(map[string]any{
		"trigger": d.EventSeq, "triggerType": d.EventType, "reason": d.Reason,
		"action": d.Action, "tier": d.Tier, "escalates": d.Escalates,
	})
	correlation := correlationOf(trigger)
	_, dup, err := e.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID:       d.Rule + "@" + strconv.FormatInt(d.EventSeq, 10),
		OccurredAt:    d.At,
		Subject:       d.Subject,
		Source:        Source,
		Type:          typ,
		Entity:        d.Rule,
		Payload:       payload,
		CorrelationID: correlation,
	})
	if err != nil {
		return fmt.Errorf("automation: recording decision: %w", err)
	}
	if dup {
		e.log().Debug("automation: decision already recorded", "rule", d.Rule, "trigger", d.EventSeq)
	}
	return nil
}

// Replay evaluates rules against the stream between from and to with no
// side effects (ADR-W022 A7). Cooldowns are simulated from the replay's
// own firings. limit bounds the events read (0 = 1000).
func (e *Engine) Replay(ctx context.Context, rules []resource.Resource, from, to time.Time, limit int) ([]Decision, error) {
	events, err := e.DB.QueryHouseholdEvents(runtimedb.HouseholdQuery{From: from, To: to, Limit: limit})
	if err != nil {
		return nil, err
	}
	look := &replayLookup{dbLookup: dbLookup{db: e.DB}}
	decisions := []Decision{}
	for _, ev := range events {
		if ctx.Err() != nil {
			return decisions, ctx.Err()
		}
		for _, rule := range rules {
			if !Matches(rule, ev) {
				continue
			}
			d, err := Evaluate(rule, ev, look, e.Location)
			if err != nil {
				return decisions, err
			}
			if d.Fired {
				look.record(d)
			}
			decisions = append(decisions, d)
		}
	}
	return decisions, nil
}

// Poke asks Run to tick now — the intake calls it after every new event.
func (e *Engine) Poke() {
	select {
	case e.pokeCh() <- struct{}{}:
	default:
	}
}

func (e *Engine) pokeCh() chan struct{} {
	e.pokeOnce.Do(func() { e.poke = make(chan struct{}, 1) })
	return e.poke
}

// Run ticks every interval and on every Poke until ctx is done.
func (e *Engine) Run(ctx context.Context, interval time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		case <-e.pokeCh():
		}
		out, err := e.Tick(ctx)
		if err != nil {
			e.log().Error("automation tick", "err", err)
			continue
		}
		if out.Evaluated > 0 {
			e.log().Info("automation tick", "evaluated", out.Evaluated, "fired", out.Fired, "suppressed", out.Suppressed, "cursor", out.Cursor)
		}
	}
}
