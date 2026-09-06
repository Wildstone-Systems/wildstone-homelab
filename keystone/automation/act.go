package automation

// Acting on a fired decision (ADR-W024 A5, work package H4).
//
// The engine decides; the domestic gateway acts. This file is the seam
// between them, and it is deliberately thin: render the rule's params
// against its trigger, claim the action on the household stream, hand it
// to the gateway, and record a failure if one comes back.
//
// Ordering is the interesting part. Escalation, when the rule asks for
// it, runs first — it decides the wording. The claim is then written
// BEFORE the dispatch, under a deterministic event id, so a tick retried
// after a crash finds the claim already there and does not run the
// action a second time. That makes dispatch at-most-once rather than at-least-
// once: the household may miss an action if Keystone dies at exactly the
// wrong moment, and will never get it twice. For a question that is the
// right trade — a duplicate "did you get the mail?" is a nuisance, but
// an executor that later unlocks a door must never run twice on a
// restart, and every executor inherits this ordering.
//
// So `automation.acted` means DISPATCHED, not succeeded. A failure
// follows it as `automation.action_failed`; a success leaves only the
// claim here, with the executor's own summary going to the operational
// event stream through the gateway's audit hook.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"text/template"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

const (
	// TypeActed is recorded when an action is dispatched to the gateway.
	TypeActed = "automation.acted"
	// TypeActionFailed follows TypeActed when the gateway refused the
	// call or the executor failed.
	TypeActionFailed = "automation.action_failed"
	// maxRenderedParamBytes bounds one rendered param value. Generous
	// against the schema's own bound, since a template may expand.
	maxRenderedParamBytes = 4096
)

// Actions performs a household rule's action. *domestic.Gateway
// implements it; the engine holds only this method, so it can neither
// register an executor nor reach one directly.
type Actions interface {
	Dispatch(ctx context.Context, c domestic.Call) (domestic.Result, error)
}

// paramContext is the small, explicit context a rule's action params
// render over. Deliberately flat and documented: a rule author is not a
// programmer, and every name here has to be guessable.
type paramContext struct {
	// Rule is the automation's name.
	Rule string
	// Subject is the member/device the trigger was about.
	Subject string
	// EventType, EventID, CorrelationID identify the trigger.
	EventType     string
	EventID       string
	CorrelationID string
	// At is the trigger's time, RFC3339 in UTC.
	At string
	// Time and Date are the trigger's local wall clock, in the
	// household's zone: "07:30" and "2026-09-04".
	Time string
	Date string
	// Payload is the trigger's own payload, so a rule can say
	// {{ .Payload.zone }}.
	Payload map[string]any
}

// RenderParams resolves a rule's action params against its trigger.
// missingkey=error is deliberate: {{ .Payload.typo }} must fail the
// action loudly, not send a message with "<no value>" in it.
func RenderParams(params map[string]string, ev runtimedb.HouseholdEvent, rule string, loc *time.Location) (map[string]string, error) {
	if len(params) == 0 {
		return nil, nil
	}
	if loc == nil {
		loc = time.UTC
	}
	local := ev.OccurredAt.In(loc)
	ctx := paramContext{
		Rule: rule, Subject: ev.Subject, EventType: ev.Type, EventID: ev.EventID,
		CorrelationID: ev.CorrelationID,
		At:            ev.OccurredAt.UTC().Format(time.RFC3339),
		Time:          local.Format("15:04"),
		Date:          local.Format("2006-01-02"),
		Payload:       map[string]any{},
	}
	if len(ev.Payload) > 0 {
		if err := json.Unmarshal(ev.Payload, &ctx.Payload); err != nil {
			// A payload that is not an object cannot happen through the
			// intake, which validates it. Say so rather than rendering
			// half the params.
			return nil, fmt.Errorf("trigger payload is not a JSON object: %w", err)
		}
	}

	out := make(map[string]string, len(params))
	for name, raw := range params {
		t, err := template.New(name).Option("missingkey=error").Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", name, err)
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, ctx); err != nil {
			return nil, fmt.Errorf("param %q: %w", name, err)
		}
		if buf.Len() > maxRenderedParamBytes {
			return nil, fmt.Errorf("param %q rendered to %d bytes, over the %d-byte bound", name, buf.Len(), maxRenderedParamBytes)
		}
		out[name] = buf.String()
	}
	return out, nil
}

// act runs a fired decision's action, if there is anything to run.
func (e *Engine) act(ctx context.Context, d Decision, ev runtimedb.HouseholdEvent, rule resource.Resource) error {
	if !d.Fired {
		return nil
	}
	if e.Actions == nil {
		return nil // decisions recorded, nothing wired to act on them
	}

	params, rerr := RenderParams(rule.Automation.Action.Params, ev, d.Rule, e.Location)
	if rerr != nil {
		return e.recordFollowUp(d, ev, TypeActionFailed, "err", map[string]any{
			"tool": d.Action, "tier": d.Tier, "error": rerr.Error(), "stage": "render",
		})
	}

	// Escalation replaces the wording of ONE declared param and nothing
	// else (ADR-W024 A6). It runs before the claim below because it
	// decides what will be dispatched; a crash in between costs a second
	// model call on the retry, never a second action.
	if d.Escalates {
		into := rule.Automation.EscalationParam()
		phrased, eerr := e.escalate(ctx, d, ev, rule, into, params[into])
		if eerr != nil {
			return eerr
		}
		if phrased != "" {
			params[into] = phrased
		}
	}

	// The claim, before the call: see the ordering note at the top.
	claimed, err := e.appendFollowUp(d, ev, TypeActed, "act", map[string]any{
		"tool": d.Action, "tier": d.Tier, "params": params,
	})
	if err != nil {
		return err
	}
	if !claimed {
		e.log().Debug("automation: action already dispatched", "rule", d.Rule, "trigger", d.EventSeq)
		return nil
	}

	_, aerr := e.Actions.Dispatch(ctx, domestic.Call{
		Tool: d.Action, Tier: d.Tier, Params: params,
		Rule: d.Rule, Subject: d.Subject,
		CorrelationID: correlationOf(ev), TriggerType: d.EventType,
	})
	if aerr != nil {
		e.log().Warn("automation action failed", "rule", d.Rule, "tool", d.Action, "err", aerr)
		return e.recordFollowUp(d, ev, TypeActionFailed, "err", map[string]any{
			"tool": d.Action, "tier": d.Tier, "error": aerr.Error(), "stage": "dispatch",
		})
	}
	return nil
}

// appendFollowUp writes one event that follows a decision, under the
// deterministic id "<rule>@<seq>:<suffix>". It reports whether the write
// was new — a duplicate means this step already happened.
func (e *Engine) appendFollowUp(d Decision, ev runtimedb.HouseholdEvent, typ, suffix string, payload map[string]any) (bool, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	_, dup, err := e.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID:       d.Rule + "@" + strconv.FormatInt(d.EventSeq, 10) + ":" + suffix,
		OccurredAt:    e.now(),
		Subject:       d.Subject,
		Source:        Source,
		Type:          typ,
		Entity:        d.Rule,
		Payload:       raw,
		CorrelationID: correlationOf(ev),
	})
	if err != nil {
		return false, fmt.Errorf("automation: recording %s: %w", typ, err)
	}
	return !dup, nil
}

func (e *Engine) recordFollowUp(d Decision, ev runtimedb.HouseholdEvent, typ, suffix string, payload map[string]any) error {
	_, err := e.appendFollowUp(d, ev, typ, suffix, payload)
	return err
}

// correlationOf is the thread an action joins: the trigger's own
// correlation id when it has one, else the trigger's event id — the same
// rule the decision record follows, so a question, its answer, and the
// rule that asked all carry one id.
func correlationOf(ev runtimedb.HouseholdEvent) string {
	if ev.CorrelationID != "" {
		return ev.CorrelationID
	}
	return ev.EventID
}
