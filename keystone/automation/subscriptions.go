package automation

// Event subscriptions (ADR-W039 §3.2 Events & subscriptions, §3.6;
// Phase 4, #341): a pure matcher plus a small evaluator over the same
// household event stream automation.Engine ticks, run AFTER it — the
// caller (cmd/keystone) enforces the ordering by ticking this engine
// second in the same loop, so a subscription never sees a raw trigger
// before the deterministic handlers have had their turn at it.
//
// A subscription is not a rule: it has no Statefile document, records
// no fired/suppressed decision, and a match is not something anyone
// reviews after the fact. What it does instead is spawn a bounded Task
// for its Agent — bounded to the subscription's OWN approved version,
// the routineprincipal.go shape: authority the Task carries is re-
// verified live against the subscription row at every use
// (agentgateway's own capability-narrowing check), never trusted from
// what was true the moment it matched — and record a `subscription`
// delivery so that Agent's runtime discovers it on its next long-poll.
//
// This file is what keeps structural_test.go's own guarantee honest for
// a SECOND engine in this package: it imports keystone/butlerstore (to
// open the bounded Task and issue its handle), which structural_test.go
// does not forbid — but never keystone/reconcile or keystone/butler, the
// two lines this package as a whole must not cross.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// eventTypeMatch is the "event_type" match kind's own parameters — the
// only match kind Wildstone ships. The vocabulary is open to growth:
// MatchesSubscription treats an unrecognised kind as "matches nothing"
// rather than an error, so a subscription a later release cannot
// evaluate yet is inert, not a crash.
type eventTypeMatch struct {
	EventType string `json:"eventType"`
	// Subject narrows to one subject when SameSubject is set; empty
	// (with SameSubject true) never narrows, which is deliberately
	// indistinguishable from SameSubject being unset — a subscription
	// naming no subject to compare against matches every subject, the
	// same as not asking for one.
	Subject     string `json:"subject,omitempty"`
	SameSubject bool   `json:"sameSubject,omitempty"`
}

// MatchesSubscription is pure: no lookup, no clock, no side effect — the
// same shape Matches() is for a rule. A subscription that is not
// currently enabled and approved matches nothing, however its match
// document reads; an event the deterministic engine itself recorded
// never matches, which is the same loop guard Matches uses for the same
// reason (a subscription must not fire on the engine's own bookkeeping).
func MatchesSubscription(sub runtimedb.AgentSubscription, ev runtimedb.HouseholdEvent) bool {
	if !sub.Enabled || sub.ApprovedVersion == 0 {
		return false
	}
	if ev.Source == Source {
		return false
	}
	switch sub.MatchKind {
	case "event_type":
		var m eventTypeMatch
		if err := json.Unmarshal([]byte(sub.MatchJSON), &m); err != nil || m.EventType == "" {
			return false
		}
		if m.EventType != ev.Type {
			return false
		}
		if m.SameSubject && m.Subject != "" && m.Subject != ev.Subject {
			return false
		}
		return true
	default:
		return false
	}
}

// subscriptionGrants is what a matched Task's GrantsJSON carries: which
// subscription it originated from, so agentgateway can re-check its
// LIVE capability set — never a snapshot — at every tool call the Task
// makes (ADR-W039 §3.6 "may only narrow").
type subscriptionGrants struct {
	Subscription int64 `json:"subscription"`
}

// SubscriptionGrantsJSON renders one subscription's origin marker.
// Exported so agentgateway's own narrowing check (which cannot import
// this package — the ADR's boundary runs the other way, engine to
// gateway, never back) can parse the identical shape without the two
// packages agreeing on a JSON layout by convention alone; the constant
// key name is defined once, here.
func SubscriptionGrantsJSON(subscriptionID int64) string {
	body, _ := json.Marshal(subscriptionGrants{Subscription: subscriptionID})
	return string(body)
}

// SubscriptionOrigin parses a Task's GrantsJSON for a subscription
// marker SubscriptionGrantsJSON wrote. ok is false for a Task that was
// not spawned by a subscription at all — the ordinary case, which must
// never be treated as "subscription 0".
func SubscriptionOrigin(grantsJSON string) (id int64, ok bool) {
	if grantsJSON == "" {
		return 0, false
	}
	var g subscriptionGrants
	if err := json.Unmarshal([]byte(grantsJSON), &g); err != nil || g.Subscription == 0 {
		return 0, false
	}
	return g.Subscription, true
}

// SubscriptionEngine ticks the Events family (ADR-W039 §3.6): every
// enabled, approved subscription, across every Agent, against the
// household stream from its OWN cursor. Zero value is usable except DB
// and Ledger.
type SubscriptionEngine struct {
	DB     *runtimedb.DB
	Ledger *butlerstore.Ledger
	Log    *slog.Logger
	// Batch bounds how many events one subscription's tick reads past
	// its cursor. Zero means DefaultBatch (the same constant Engine
	// uses).
	Batch int
}

func (e *SubscriptionEngine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

func (e *SubscriptionEngine) batch() int {
	if e.Batch > 0 {
		return e.Batch
	}
	return DefaultBatch
}

// SubscriptionTickOutcome summarizes one Tick.
type SubscriptionTickOutcome struct {
	// Matched counts subscription/event pairs that spawned a Task.
	Matched int
	// Errored counts subscriptions whose tick failed; the engine
	// continues with the others rather than letting one bad row stall
	// the whole family (the deterministic engine's own crash-only
	// discipline does not apply here — an unreachable database is
	// still fatal, a malformed single subscription is not).
	Errored int
}

// Tick advances every enabled subscription's cursor against the
// household stream, opening a bounded Task and recording a
// `subscription` delivery for each match. Call it after
// automation.Engine.Tick in the same loop (ADR-W039 §3.6 "after the
// deterministic engine").
func (e *SubscriptionEngine) Tick(ctx context.Context) (SubscriptionTickOutcome, error) {
	var out SubscriptionTickOutcome
	subs, err := e.DB.ListEnabledAgentSubscriptions()
	if err != nil {
		return out, fmt.Errorf("automation: listing enabled subscriptions: %w", err)
	}
	for _, sub := range subs {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		n, err := e.tickOne(sub)
		if err != nil {
			out.Errored++
			e.log().Error("subscription tick failed", "subscription", sub.ID, "butler", sub.Butler, "err", err)
			continue
		}
		out.Matched += n
	}
	return out, nil
}

// tickOne is one subscription's own cursor-based tick — the same
// "adopt the stream's current end on first run rather than replaying
// history" rule automation.Engine.Tick applies, so a subscription
// approved today does not fire retroactively on years of events it
// never watched.
func (e *SubscriptionEngine) tickOne(sub runtimedb.AgentSubscription) (int, error) {
	cursor := sub.Cursor
	if cursor == 0 {
		latest, err := e.DB.QueryHouseholdEvents(runtimedb.HouseholdQuery{Newest: true, Limit: 1})
		if err != nil {
			return 0, err
		}
		if len(latest) == 0 {
			return 0, nil
		}
		if err := e.DB.SetAgentSubscriptionCursor(sub.ID, latest[0].Seq); err != nil {
			return 0, err
		}
		return 0, nil
	}
	events, err := e.DB.HouseholdEventsAfter(cursor, e.batch())
	if err != nil {
		return 0, err
	}
	matched := 0
	for _, ev := range events {
		if MatchesSubscription(sub, ev) {
			if err := e.spawn(sub, ev); err != nil {
				return matched, err
			}
			matched++
		}
		cursor = ev.Seq
		if err := e.DB.SetAgentSubscriptionCursor(sub.ID, cursor); err != nil {
			return matched, err
		}
	}
	return matched, nil
}

// spawn opens the bounded Task a match creates and records its
// delivery — the two writes ADR-W039 §3.6 asks for, done together so a
// Task with no delivery (a runtime that can never learn about it) does
// not outlive a crash between them: on retry, the SAME event is not
// re-read (the cursor only advances after this returns without error),
// so a Task from a half-finished spawn plus a fresh delivery from the
// retry is the worst case, never a Task with no delivery at all.
// primaryOwner is the household's own admitted Owner — the Person a
// subscription-spawned Task runs as author/owner of, when no specific
// person is present at all (ADR-W039 §3.6). ListButlerMembers orders by
// handle, so which Owner a multi-Owner household picks is stable rather
// than incidental to map iteration.
func (e *SubscriptionEngine) primaryOwner() (string, error) {
	members, err := e.DB.ListButlerMembers()
	if err != nil {
		return "", err
	}
	for _, m := range members {
		if m.Role == runtimedb.RoleOwner && m.Admitted && m.DisabledAt.IsZero() && m.OffboardedAt.IsZero() {
			return m.Member, nil
		}
	}
	return "", fmt.Errorf("no admitted owner exists to run subscription-spawned work as")
}

func (e *SubscriptionEngine) spawn(sub runtimedb.AgentSubscription, ev runtimedb.HouseholdEvent) error {
	if e.Ledger == nil {
		return fmt.Errorf("automation: the subscription engine has no ledger to open a task on")
	}
	b, err := e.DB.GetButlerBySeq(sub.Butler)
	if err != nil {
		return fmt.Errorf("automation: subscription %d names an agent that no longer exists: %w", sub.ID, err)
	}
	audience := runtimedb.ButlerTaskAudience(sub.Audience)
	if audience == "" {
		audience = runtimedb.ButlerAudiencePrivate
	}
	// A subscription's own approved_by (routineprincipal.go's shape:
	// possessing the approved version, not any person present now, is
	// what a matched Task's AUTHORITY traces back to) is an Actor
	// column — it is who clicked approve, which this codebase's own
	// Plan-approval path allows to be an administrator credential, not
	// only a registered Person — so it is not always a Person
	// InsertButlerTask's author/owner (Human columns) can resolve. The
	// household's own admitted Owner is: exactly what "unattended,
	// standing authority" means when no specific person is present.
	who, err := e.primaryOwner()
	if err != nil {
		return fmt.Errorf("automation: subscription %d: %w", sub.ID, err)
	}
	taskID, err := e.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: who, Owner: who,
		Surface: b.Surface, AgentClass: b.AgentClass, Audience: audience,
		Objective:          fmt.Sprintf("subscription %d: a %s event matched (%s)", sub.ID, ev.Type, ev.EventID),
		CompletionCriteria: "answer or act within this subscription's approved capability set, then complete",
		ObjectiveKind:      runtimedb.ObjectiveEffectful,
		GrantsJSON:         SubscriptionGrantsJSON(sub.ID),
		Retention:          runtimedb.ButlerRetentionManual,
	})
	if err != nil {
		return fmt.Errorf("automation: opening a task for subscription %d: %w", sub.ID, err)
	}
	if err := e.DB.SetTaskButler(taskID, b.Name); err != nil {
		return fmt.Errorf("automation: binding subscription %d's task to %s: %w", sub.ID, b.Name, err)
	}
	handle, err := e.Ledger.IssueHandle(taskID)
	if err != nil {
		return fmt.Errorf("automation: issuing a handle for subscription %d's task: %w", sub.ID, err)
	}
	payload, err := json.Marshal(map[string]any{
		"handle": string(handle), "subscription": sub.ID, "task": taskID,
		"eventType": ev.Type, "eventId": ev.EventID,
	})
	if err != nil {
		return err
	}
	if _, err := e.DB.RecordSubscriptionDelivery(sub.Butler, strconv.FormatInt(ev.Seq, 10), string(payload)); err != nil {
		return fmt.Errorf("automation: recording subscription %d's delivery: %w", sub.ID, err)
	}
	return nil
}
