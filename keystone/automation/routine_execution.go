package automation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/domestic"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// RoutineActions is the domestic path plus a live compatibility probe. A
// missing capability blocks before the at-most-once effect claim.
type RoutineActions interface {
	Actions
	Compatible(tool, tier string) bool
	RoutineDestination(context.Context, string, string) (runtimedb.MemoryDestination, error)
}

var errRoutineRecipientRevoked = errors.New("routine recipient is no longer admitted")

// errRoutineNoRecipient is an event that names nobody for a routine that
// sends to "whoever the event is about": a meal logged on an unlinked
// profile, a kitchen timer set by nobody in particular. That run has
// nobody to tell, so it stops; the routine is not paused, because the
// next event may well name somebody.
var errRoutineNoRecipient = errors.New("this event names nobody to tell")

// TickReviewed consumes the household stream independently of the legacy
// Statefile cursor. Only exact Owner-approved Plan snapshots are considered.
// The first tick adopts existing history, so enabling the executor does not
// unexpectedly fire old triggers.
func (e *Engine) TickReviewed(ctx context.Context) error {
	if e.DB == nil || e.ReviewedActions == nil {
		return fmt.Errorf("reviewed routine executor is not wired")
	}
	if err := e.DB.CheckHouseholdExecution(); err != nil {
		return err
	}
	cursor, initialized, err := e.DB.RoutineExecutionCursor()
	if err != nil {
		return err
	}
	if !initialized {
		latest, err := e.DB.QueryHouseholdEvents(runtimedb.HouseholdQuery{Newest: true, Limit: 1})
		if err != nil {
			return err
		}
		if len(latest) != 0 {
			return e.DB.SetRoutineExecutionCursor(latest[0].Seq)
		}
		if err := e.DB.SetRoutineExecutionCursor(0); err != nil {
			return err
		}
	}
	batch := e.Batch
	if batch <= 0 {
		batch = DefaultBatch
	}
	events, err := e.DB.HouseholdEventsAfter(cursor, batch)
	if err != nil {
		return err
	}
	versions, err := e.DB.ApprovedRoutineVersions()
	if err != nil {
		return err
	}
	if err := e.retryCapabilityBlocked(ctx, versions); err != nil {
		return err
	}
	maxAge := e.MaxTriggerAge
	if maxAge <= 0 {
		maxAge = DefaultMaxTriggerAge
	}
	look := dbLookup{db: e.DB}
	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, version := range versions {
			rule, err := ParseDraft(version.Document)
			if err != nil || rule.Metadata.Name != version.Name {
				// An invalid reviewed snapshot is never handed to the legacy
				// executor. It needs a corrected Plan, not a guessed rule.
				e.log().Error("reviewed routine document is invalid", "routine", version.Name, "err", err)
				continue
			}
			if !Matches(rule, ev) || e.now().Sub(ev.OccurredAt) > maxAge {
				continue
			}
			decision, err := Evaluate(rule, ev, look, e.loc())
			if err != nil {
				return err
			}
			if !decision.Fired {
				continue
			}
			run, _, err := e.DB.ClaimRoutineRun(version.PlanID, ev.Seq)
			if err != nil {
				if errors.Is(err, runtimedb.ErrRoutineDenied) {
					continue
				}
				return err
			}
			if run.State == "pending" {
				if err := e.executeReviewed(ctx, version, run, ev, decision, rule); err != nil {
					return err
				}
			}
		}
		if err := e.DB.SetRoutineExecutionCursor(ev.Seq); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) retryCapabilityBlocked(ctx context.Context, versions []runtimedb.RoutineExecutionVersion) error {
	blocked, err := e.DB.CapabilityBlockedRoutineRuns(100)
	if err != nil {
		return err
	}
	byPlan := make(map[string]runtimedb.RoutineExecutionVersion, len(versions))
	for _, version := range versions {
		byPlan[version.PlanID] = version
	}
	for _, run := range blocked {
		if err := ctx.Err(); err != nil {
			return err
		}
		version, ok := byPlan[run.PlanID]
		if !ok {
			continue
		}
		rule, err := ParseDraft(version.Document)
		if err != nil || rule.Metadata.Name != version.Name {
			continue
		}
		ev, err := e.DB.RoutineTriggerEvent(run.TriggerEvent)
		if err != nil {
			return err
		}
		decision, err := Evaluate(rule, ev, dbLookup{db: e.DB}, e.loc())
		if err != nil {
			return err
		}
		if !decision.Fired || !e.ReviewedActions.Compatible(decision.Action, decision.Tier) {
			continue
		}
		if err := e.executeReviewed(ctx, version, run, ev, decision, rule); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) executeReviewed(ctx context.Context, version runtimedb.RoutineExecutionVersion,
	run runtimedb.RoutineRun, ev runtimedb.HouseholdEvent, decision Decision, rule resource.Resource) error {
	params, err := RenderParams(rule.Automation.Action.Params, ev, decision.Rule, e.loc())
	if err != nil {
		return e.DB.SetRoutineRunState(run.ID, "failed", "render: "+err.Error())
	}
	destination, err := e.routineDestination(ctx, version, decision, params)
	if err != nil {
		return e.blockRoutineDelivery(version, run.ID, err)
	}
	var principal *runtimedb.RoutinePrincipal
	var memoryIDs []int64
	if rule.Automation.Escalation != nil {
		// Inference cannot silently use the deterministic fallback wording.
		if _, err := e.DB.ResolveRoutineButler(version.Name); err != nil || e.ReviewedEscalator == nil || version.AssignedAgent == "" {
			return e.DB.SetRoutineRunState(run.ID, "blocked", "inference requires the assigned available Agent")
		}
		if len(rule.Automation.Context) != 0 {
			// The older recentEvents context has no disclosure label. It must
			// not be copied into a reviewed routine's model packet.
			return e.DB.SetRoutineRunState(run.ID, "blocked", "reviewed event context has no disclosure label")
		}
		if err := e.DB.CheckRoutineRunBoundary(run.ID); err != nil {
			return e.DB.SetRoutineRunState(run.ID, "stopped", "stopped before model round: "+err.Error())
		}
		principal, err = e.DB.BindRoutinePrincipal(version.PlanID)
		if err != nil {
			return e.DB.SetRoutineRunState(run.ID, "blocked", "assigned Agent cannot bind this approved version")
		}
		memories, err := e.DB.RetrieveRoutineMemory(principal, destination, 20)
		if err != nil {
			return e.DB.SetRoutineRunState(run.ID, "blocked", "reviewed memory cannot be disclosed: "+err.Error())
		}
		facts := make([]Fact, 0, len(memories))
		for _, m := range memories {
			facts = append(facts, Fact{Label: fmt.Sprintf("memory %d", m.Seq), Content: m.Content})
			memoryIDs = append(memoryIDs, m.Seq)
		}
		into := rule.Automation.EscalationParam()
		answer, err := e.ReviewedEscalator(ctx, version.AssignedAgent, EscalationRequest{
			Rule: decision.Rule, Instruction: rule.Automation.Escalation.Prompt,
			Fallback: params[into], Facts: facts,
		})
		if err != nil || sanitizeEscalated(answer) == "" {
			return e.DB.SetRoutineRunState(run.ID, "failed", "assigned Agent did not complete inference")
		}
		if err := e.DB.CheckRoutineRunBoundary(run.ID); err != nil {
			return e.DB.SetRoutineRunState(run.ID, "stopped", "stopped after model round: "+err.Error())
		}
		params[into] = sanitizeEscalated(answer)
	}
	if _, err := e.routineDestination(ctx, version, decision, params); err != nil {
		return e.blockRoutineDelivery(version, run.ID, err)
	}
	if !e.ReviewedActions.Compatible(decision.Action, decision.Tier) {
		return e.DB.SetRoutineRunState(run.ID, "blocked", "required capability is unavailable: "+decision.Action)
	}
	claimed, err := e.DB.ClaimRoutineEffect(run.ID)
	if errors.Is(err, runtimedb.ErrRoutineDenied) {
		return e.DB.SetRoutineRunState(run.ID, "stopped", "authorization changed before dispatch")
	}
	if err != nil || !claimed {
		return err
	}
	if err := e.DB.CheckRoutineRunBoundary(run.ID); err != nil {
		return e.DB.SetRoutineRunState(run.ID, "stopped", "stopped before dispatch: "+err.Error())
	}
	call := domestic.Call{Tool: decision.Action, Tier: decision.Tier, Params: params,
		Rule: decision.Rule, Subject: decision.Subject,
		CorrelationID: correlationOf(ev), TriggerType: decision.EventType}
	ctx = domestic.WithExecutionCheck(ctx, func() error {
		if err := e.DB.CheckRoutineRunBoundary(run.ID); err != nil {
			return err
		}
		dest, err := e.routineDestination(ctx, version, decision, params)
		if err != nil {
			return err
		}
		for _, id := range memoryIDs {
			if err := e.DB.CheckRoutineMemory(principal, id, dest); err != nil {
				return err
			}
		}
		return nil
	})
	result, dispatchErr := e.ReviewedActions.Dispatch(ctx, call)
	if dispatchErr != nil {
		if errors.Is(dispatchErr, errRoutineRecipientRevoked) {
			if err := e.DB.PauseRoutineRecipient(version.Name); err != nil {
				return err
			}
		}
		// The call may have crossed the external boundary before an error.
		// Keep the claim and outcome uncertain; never repeat it blindly.
		if err := e.DB.RecordRoutineEffect(run.ID, "uncertain", dispatchErr.Error()); err != nil {
			return err
		}
		return e.DB.SetRoutineRunState(run.ID, "uncertain", "dispatch outcome could not be verified")
	}
	outcome := "uncertain"
	if result.EffectConfirmed {
		outcome = "confirmed"
	} else if result.EffectPending {
		outcome = "pending"
	}
	// The record says what was done, never household content the
	// executor handed back (domestic.Result.AuditSummary).
	effect := result.Summary
	if result.AuditSummary != "" {
		effect = result.AuditSummary
	}
	if err := e.DB.RecordRoutineEffect(run.ID, outcome, effect); err != nil {
		return err
	}
	if err := e.DB.CheckRoutineRunBoundary(run.ID); err != nil {
		return e.DB.SetRoutineRunState(run.ID, "stopped", "stopped after effect: "+err.Error())
	}
	if outcome == "uncertain" {
		return e.DB.SetRoutineRunState(run.ID, "uncertain", "executor gave no effect receipt")
	}
	return e.DB.SetRoutineRunState(run.ID, "completed", "effect "+outcome)
}

func (e *Engine) blockRoutineDelivery(version runtimedb.RoutineExecutionVersion, runID int64, err error) error {
	if errors.Is(err, errRoutineNoRecipient) {
		return e.DB.SetRoutineRunState(runID, "stopped", "delivery: "+err.Error())
	}
	if errors.Is(err, errRoutineRecipientRevoked) {
		if pauseErr := e.DB.PauseRoutineRecipient(version.Name); pauseErr != nil {
			return pauseErr
		}
	}
	return e.DB.SetRoutineRunState(runID, "blocked", "delivery: "+err.Error())
}

func (e *Engine) routineDestination(ctx context.Context, version runtimedb.RoutineExecutionVersion, d Decision, params map[string]string) (runtimedb.MemoryDestination, error) {
	if err := e.checkRoutineOutput(version, d, params); err != nil {
		return runtimedb.MemoryDestination{}, err
	}
	member := strings.TrimSpace(params["member"])
	if member == "" {
		member = d.Subject
	}
	destination, err := e.ReviewedActions.RoutineDestination(ctx, d.Action, member)
	if err != nil {
		return runtimedb.MemoryDestination{}, err
	}
	person, err := e.DB.GetButlerMember(member)
	if err != nil || !person.Active() {
		return runtimedb.MemoryDestination{}, errRoutineRecipientRevoked
	}
	destination.RecipientPerson = person.PersonID
	return destination, nil
}

func (e *Engine) checkRoutineOutput(version runtimedb.RoutineExecutionVersion, d Decision, params map[string]string) error {
	// An ntfy topic has no enumerable subscriber set, so it cannot prove
	// that a revoked Person will not still receive the message. Reviewed
	// routines need a destination with a live, resolvable Person identity.
	if strings.HasPrefix(d.Action, "chat-") && strings.HasSuffix(d.Action, ".send") {
		member := strings.TrimSpace(params["member"])
		if member == "" {
			member = d.Subject
		}
		if member == "" {
			return errRoutineNoRecipient
		}
		person, err := e.DB.GetButlerMember(member)
		if err != nil || !person.Active() {
			return errRoutineRecipientRevoked
		}
		people, err := e.DB.ResolveAudience(version.OutputAudience, "")
		if err != nil {
			return err
		}
		for _, allowed := range people {
			if allowed == person.PersonID {
				return nil
			}
		}
		return fmt.Errorf("recipient is outside the approved output audience")
	}
	return errors.New("the reviewed delivery target cannot be resolved")
}

// RunReviewed polls while this process holds the coordinator lease.
func (e *Engine) RunReviewed(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	recovered := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !recovered {
				if err := e.DB.RecoverClaimedRoutineEffects(); err != nil {
					e.log().Error("reviewed routine recovery held", "err", err)
					continue
				}
				recovered = true
			}
			if err := e.TickReviewed(ctx); err != nil {
				e.log().Error("reviewed routine tick", "err", err)
			}
		}
	}
}
