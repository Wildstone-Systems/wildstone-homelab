package resource

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// AutomationSpec is the spec for kind Automation (ADR-W022 A1): a
// household rule, approved once through the plan pipeline like any other
// resource and then run by the deterministic automation engine without
// asking again. It is the ONE schema both a human and the Butler author
// into, and the one the engine executes — there is no second execution
// path (ADR-W022 H7).
//
// A rule reads, in order: WHEN this event arrives (Trigger), IF these
// checks hold (Conditions, all of them, evaluated in code), gather THIS
// much context (Context), optionally ask the Butler to phrase or decide
// (Escalation), then DO this (Action) through the domestic gateway.
// Everything but Escalation is deterministic, and a rule with no
// Escalation never involves a model at all.
//
// Nothing here is executed by this package or by the reconciler. The
// resource is Keystone-owned: it lives in the Statefile, is applied at
// no node, and is present exactly when it is desired. The engine
// (ADR-W022 H3) reads it from there.
type AutomationSpec struct {
	// Description is the human sentence for the plan review and the
	// Console: what this rule is for.
	Description string `yaml:"description"`
	// Disabled pauses the rule without deleting it. The zero value is
	// enabled, so a rule that says nothing runs.
	Disabled bool `yaml:"disabled,omitempty"`
	// Trigger names the household event that starts an evaluation.
	Trigger AutomationTrigger `yaml:"trigger"`
	// Conditions must ALL hold for the rule to proceed. Most evaluations
	// stop here, cheaply, with an automation.suppressed event.
	Conditions []AutomationCondition `yaml:"conditions,omitempty"`
	// Context names what the escalation packet may contain — small,
	// targeted retrievals, never a history dump (ADR-W022 A6).
	Context []AutomationContext `yaml:"context,omitempty"`
	// Escalation, when present, opens a Butler session with the trigger
	// event and the Context results; the Butler's output is routed to
	// Action. Absent, Action runs with its fixed Params.
	Escalation *AutomationEscalation `yaml:"escalation,omitempty"`
	// Action is the typed operation that runs, through the domestic
	// gateway (ADR-W022 A5).
	Action AutomationAction `yaml:"action"`
}

// AutomationTrigger matches an incoming household event.
type AutomationTrigger struct {
	// EventType is the canonical type, e.g. person.left_home.
	EventType string `yaml:"eventType"`
	// Subject, when set, restricts the trigger to events about this
	// member/device/household id. Empty matches any subject.
	Subject string `yaml:"subject,omitempty"`
}

// AutomationCondition is one deterministic check. Kind selects which
// fields apply; the closed set is automationConditionKinds.
type AutomationCondition struct {
	// Kind is one of: eventAbsent, eventPresent, timeOfDay, weekday,
	// cooldown.
	Kind string `yaml:"kind"`
	// EventType (eventAbsent, eventPresent): the household event type
	// looked for.
	EventType string `yaml:"eventType,omitempty"`
	// Window (eventAbsent, eventPresent, cooldown): how far back from the
	// trigger's occurred_at to look, as a Go duration ("6h", "20h",
	// "72h"). At most MaxAutomationWindow.
	Window string `yaml:"window,omitempty"`
	// SameSubject (eventAbsent, eventPresent): only count events about
	// the trigger's subject.
	SameSubject bool `yaml:"sameSubject,omitempty"`
	// After / Before (timeOfDay): local wall-clock bounds as "HH:MM". At
	// least one is required; both may be set.
	After  string `yaml:"after,omitempty"`
	Before string `yaml:"before,omitempty"`
	// Days (weekday): the days the rule may fire, from mon..sun.
	Days []string `yaml:"days,omitempty"`
}

// AutomationContext is one targeted retrieval for the escalation packet.
type AutomationContext struct {
	// Kind is one of: recentEvents.
	Kind string `yaml:"kind"`
	// EventType (recentEvents): the household event type to fetch.
	EventType string `yaml:"eventType,omitempty"`
	// SameSubject (recentEvents): only events about the trigger's subject.
	SameSubject bool `yaml:"sameSubject,omitempty"`
	// Limit (recentEvents): how many, newest first. 1..MaxAutomationContextLimit.
	Limit int `yaml:"limit,omitempty"`
}

// AutomationEscalation asks the Butler to phrase or decide.
type AutomationEscalation struct {
	// Prompt is the instruction the Butler receives alongside the packet.
	// Bounded; it is authored text, not a place for a document.
	Prompt string `yaml:"prompt"`
}

// AutomationAction is the typed operation the rule performs.
type AutomationAction struct {
	// Tool is "<capability>.<tool>", an agentTool a package declares
	// (ADR-W005 A1), e.g. notifications.notify.
	Tool string `yaml:"tool"`
	// Tier is the action's risk tier in the shared vocabulary (read, low,
	// medium, high, domestic). The schema accepts the vocabulary; policy
	// decides which tiers a household rule may carry (ADR-W022 A5).
	Tier string `yaml:"tier"`
	// Params are the tool's inputs. Values may reference the trigger's
	// fields with {{ .Subject }} and friends; the engine renders them.
	// Never a secret value: policy holds these to the same rule as a
	// container's env.
	Params map[string]string `yaml:"params,omitempty"`
}

const (
	// MaxAutomationWindow bounds every condition window. A rule that
	// looks back further than this is a report, not a rule.
	MaxAutomationWindow = 30 * 24 * time.Hour
	// MaxAutomationContextLimit bounds a recentEvents retrieval: a
	// context packet is a handful of facts, not a history.
	MaxAutomationContextLimit = 20
	// MaxAutomationPromptBytes bounds an escalation prompt.
	MaxAutomationPromptBytes = 2000
	// MaxAutomationParamBytes bounds one action param value.
	MaxAutomationParamBytes = 1024
)

// automationEventTypeRegexp is the household event type shape, the same
// pattern keystone/household and keystone/pkgspec carry; a test in
// keystone/household pins all three equal. Duplicated so resource stays
// a leaf package.
var automationEventTypeRegexp = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// automationToolRegexp is "<capability>.<tool>": a capability name and
// an identifier.
var automationToolRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.[a-z][a-z0-9_]{0,63}$`)

// automationClockRegexp is "HH:MM", 24-hour.
var automationClockRegexp = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

var automationConditionKinds = map[string]bool{
	"eventAbsent": true, "eventPresent": true, "timeOfDay": true, "weekday": true, "cooldown": true,
}

var automationContextKinds = map[string]bool{
	"recentEvents": true,
}

// automationTiers is the shared risk vocabulary (keystone/pkgspec
// riskTiers, keystone/policy.Tier plus "read").
var automationTiers = map[string]bool{
	"read": true, "low": true, "medium": true, "high": true, "domestic": true,
}

var automationWeekdays = map[string]bool{
	"mon": true, "tue": true, "wed": true, "thu": true, "fri": true, "sat": true, "sun": true,
}

// AutomationOwnerAllowed reports whether an owner is one an Automation
// may carry: the household as a whole ("household"), one member
// ("household/<name>"), or a capability that ships the rule
// ("capability/<name>"). Nothing is orphaned (ADR-W004 A1); nothing is
// "system" either — a rule always belongs to someone who can be asked.
func AutomationOwnerAllowed(owner string) bool {
	if owner == "household" {
		return true
	}
	for _, prefix := range []string{"household/", "capability/"} {
		if rest, ok := strings.CutPrefix(owner, prefix); ok {
			return nameRegexp.MatchString(rest)
		}
	}
	return false
}

func validateAutomation(r *Resource) error {
	fail := func(field, msg string) error {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: field, Msg: msg}
	}
	if r.Automation == nil {
		return fail("spec", "automation spec missing")
	}
	if !AutomationOwnerAllowed(r.Metadata.Owner) {
		return fail("metadata.owner", "must be household, household/<member>, or capability/<name>")
	}
	a := r.Automation
	if strings.TrimSpace(a.Description) == "" {
		return fail("spec.description", "must not be empty: it is what a reviewer reads before approving the rule")
	}
	if err := checkAutomationText(a.Description, 500); err != "" {
		return fail("spec.description", err)
	}

	if !automationEventTypeRegexp.MatchString(a.Trigger.EventType) {
		return fail("spec.trigger.eventType", fmt.Sprintf("%q must be namespace.name in lowercase [a-z0-9_], e.g. person.left_home", a.Trigger.EventType))
	}
	if err := checkAutomationIdentifier(a.Trigger.Subject); err != "" {
		return fail("spec.trigger.subject", err)
	}

	for i, c := range a.Conditions {
		field := fmt.Sprintf("spec.conditions[%d]", i)
		if !automationConditionKinds[c.Kind] {
			return fail(field+".kind", "must be one of eventAbsent, eventPresent, timeOfDay, weekday, cooldown")
		}
		switch c.Kind {
		case "eventAbsent", "eventPresent":
			if !automationEventTypeRegexp.MatchString(c.EventType) {
				return fail(field+".eventType", fmt.Sprintf("%q must be namespace.name in lowercase [a-z0-9_]", c.EventType))
			}
			if err := checkAutomationWindow(c.Window); err != "" {
				return fail(field+".window", err)
			}
			if c.After != "" || c.Before != "" || len(c.Days) > 0 {
				return fail(field, "after/before/days apply only to timeOfDay and weekday conditions")
			}
		case "cooldown":
			if err := checkAutomationWindow(c.Window); err != "" {
				return fail(field+".window", err)
			}
			if c.EventType != "" || c.SameSubject || c.After != "" || c.Before != "" || len(c.Days) > 0 {
				return fail(field, "a cooldown condition takes only a window")
			}
		case "timeOfDay":
			if c.After == "" && c.Before == "" {
				return fail(field, "timeOfDay needs after, before, or both, as HH:MM")
			}
			for name, v := range map[string]string{"after": c.After, "before": c.Before} {
				if v != "" && !automationClockRegexp.MatchString(v) {
					return fail(field+"."+name, fmt.Sprintf("%q must be HH:MM (24-hour)", v))
				}
			}
			if c.EventType != "" || c.Window != "" || c.SameSubject || len(c.Days) > 0 {
				return fail(field, "a timeOfDay condition takes only after and before")
			}
		case "weekday":
			if len(c.Days) == 0 {
				return fail(field+".days", "must list at least one of mon, tue, wed, thu, fri, sat, sun")
			}
			seen := map[string]bool{}
			for _, d := range c.Days {
				if !automationWeekdays[d] {
					return fail(field+".days", fmt.Sprintf("%q is not one of mon, tue, wed, thu, fri, sat, sun", d))
				}
				if seen[d] {
					return fail(field+".days", fmt.Sprintf("%q listed twice", d))
				}
				seen[d] = true
			}
			if c.EventType != "" || c.Window != "" || c.SameSubject || c.After != "" || c.Before != "" {
				return fail(field, "a weekday condition takes only days")
			}
		}
	}

	for i, cx := range a.Context {
		field := fmt.Sprintf("spec.context[%d]", i)
		if !automationContextKinds[cx.Kind] {
			return fail(field+".kind", "must be recentEvents")
		}
		if !automationEventTypeRegexp.MatchString(cx.EventType) {
			return fail(field+".eventType", fmt.Sprintf("%q must be namespace.name in lowercase [a-z0-9_]", cx.EventType))
		}
		if cx.Limit < 1 || cx.Limit > MaxAutomationContextLimit {
			return fail(field+".limit", fmt.Sprintf("must be 1..%d: a context packet is a few facts, not a history", MaxAutomationContextLimit))
		}
	}

	if a.Escalation != nil {
		if strings.TrimSpace(a.Escalation.Prompt) == "" {
			return fail("spec.escalation.prompt", "must not be empty (omit escalation for a rule that never involves the Butler)")
		}
		if err := checkAutomationText(a.Escalation.Prompt, MaxAutomationPromptBytes); err != "" {
			return fail("spec.escalation.prompt", err)
		}
	}

	if !automationToolRegexp.MatchString(a.Action.Tool) {
		return fail("spec.action.tool", fmt.Sprintf("%q must be <capability>.<tool>, e.g. notifications.notify", a.Action.Tool))
	}
	if !automationTiers[a.Action.Tier] {
		return fail("spec.action.tier", "must be one of read, low, medium, high, domestic")
	}
	for k, v := range a.Action.Params {
		if err := checkAutomationIdentifier(k); err != "" || k == "" {
			return fail("spec.action.params", fmt.Sprintf("key %q: must be a short identifier", k))
		}
		if len(v) > MaxAutomationParamBytes {
			return fail("spec.action.params."+k, fmt.Sprintf("exceeds %d bytes", MaxAutomationParamBytes))
		}
		if strings.ContainsAny(v, "\x00") {
			return fail("spec.action.params."+k, "contains a NUL byte")
		}
	}
	return nil
}

// checkAutomationWindow parses a Go duration and bounds it.
func checkAutomationWindow(w string) string {
	if w == "" {
		return "required, as a duration such as 6h or 20h"
	}
	d, err := time.ParseDuration(w)
	if err != nil {
		return fmt.Sprintf("%q is not a duration (use forms like 30m, 6h, 72h)", w)
	}
	if d <= 0 {
		return "must be positive"
	}
	if d > MaxAutomationWindow {
		return fmt.Sprintf("%s exceeds the %s bound", w, MaxAutomationWindow)
	}
	return ""
}

// checkAutomationIdentifier bounds an optional identifier-shaped field:
// a subject, a param key.
func checkAutomationIdentifier(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > 256 {
		return "exceeds 256 bytes"
	}
	if i := strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }); i >= 0 {
		return fmt.Sprintf("control or space character at offset %d", i)
	}
	return ""
}

// checkAutomationText bounds a free-text field and refuses control
// characters other than newlines.
func checkAutomationText(s string, max int) string {
	if len(s) > max {
		return fmt.Sprintf("exceeds %d bytes", max)
	}
	if i := strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }); i >= 0 {
		return fmt.Sprintf("control character at offset %d", i)
	}
	return ""
}
