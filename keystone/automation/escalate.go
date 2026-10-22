package automation

// Escalation (ADR-W024 A6, work package H6): the one point where a
// household rule involves a model, and the narrowest one that is useful.
//
// The engine has already decided that the rule fires, which tool runs, at
// which tier, and for whom. What it asks the Butler for is the WORDING of
// a single action param the rule declared — and it asks only after
// gathering the small, targeted context the rule named, never a dump of
// history.
//
// The rule's own value for that param is the fallback, and it is used
// whenever the model does not answer: unconfigured, failing, truncated,
// empty. So an escalating rule still acts when the Butler is off, with
// the wording its author wrote. That is the deterministic-backbone
// principle applied to the one probabilistic step in the loop.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

const (
	// TypeEscalated records that the Butler answered and its wording was
	// used. The wording itself is not repeated here — it travels in the
	// action's own params on the automation.acted record beside it.
	TypeEscalated = "automation.escalated"
	// TypeEscalationFailed records that the Butler was asked and did not
	// answer usefully. The action still ran, with the rule's wording.
	TypeEscalationFailed = "automation.escalation_failed"
	// TypeEscalationSkipped records that a rule asked for the Butler on a
	// control plane with none configured. The action still ran, with the
	// rule's wording.
	TypeEscalationSkipped = "automation.escalation_skipped"

	// maxEscalatedBytes bounds what the engine will accept back from an
	// escalator. The Butler bounds its own output too; this is the bound
	// at the trust boundary, which is the one that has to hold.
	maxEscalatedBytes = resource.MaxAutomationParamBytes
	// maxFactEvents bounds one context provider's contribution.
	maxFactEvents = resource.MaxAutomationContextLimit
)

// Fact is one piece of gathered context: a label a household member would
// recognise, and the untrusted content behind it.
type Fact struct {
	Label   string
	Content string
}

// EscalationRequest is what the engine hands an escalator.
type EscalationRequest struct {
	// Rule is the automation's name.
	Rule string
	// Instruction is the rule author's escalation prompt.
	Instruction string
	// Fallback is the rule's own wording for the param being replaced —
	// what the household receives if this returns an error.
	Fallback string
	// Facts are the rule's context providers, already retrieved.
	Facts []Fact
}

// Escalator phrases one action param. It is deliberately not "the
// Butler": the engine holds this method and nothing else, so it can
// neither open a session with tools nor reach a provider directly, and
// keystone/automation never imports keystone/butler (structural_test.go).
type Escalator interface {
	Escalate(ctx context.Context, req EscalationRequest) (string, error)
}

// EscalatorFunc adapts a function to Escalator, so cmd/keystone can wire
// the Butler's phraser without either package learning the other's types.
type EscalatorFunc func(ctx context.Context, req EscalationRequest) (string, error)

func (f EscalatorFunc) Escalate(ctx context.Context, req EscalationRequest) (string, error) {
	return f(ctx, req)
}

// gatherFacts retrieves the rule's declared context providers (ADR-W024
// A6). Targeted retrieval, not history: each provider names one event
// type, an optional same-subject scope, and a bounded count, and the
// window ends at the trigger so a replay of the same moment sees the
// same facts.
func (e *Engine) gatherFacts(rule resource.Resource, ev runtimedb.HouseholdEvent) ([]Fact, error) {
	var facts []Fact
	for _, cx := range rule.Automation.Context {
		if cx.Kind != "recentEvents" {
			// The vocabulary is closed at validation, so this is a kind
			// added to the schema and not to the engine. Say which.
			return nil, fmt.Errorf("context provider %q is declared but not implemented", cx.Kind)
		}
		limit := cx.Limit
		if limit <= 0 || limit > maxFactEvents {
			limit = maxFactEvents
		}
		subject := ""
		if cx.SameSubject {
			subject = ev.Subject
		}
		events, err := e.DB.QueryHouseholdEvents(runtimedb.HouseholdQuery{
			Type: cx.EventType, Subject: subject, SubjectSet: cx.SameSubject,
			To: ev.OccurredAt, Newest: true, Limit: limit + 1,
		})
		if err != nil {
			return nil, err
		}
		facts = append(facts, Fact{
			Label:   factLabel(cx, ev),
			Content: renderFacts(events, ev.Seq, limit, e.loc()),
		})
	}
	return facts, nil
}

func factLabel(cx resource.AutomationContext, ev runtimedb.HouseholdEvent) string {
	label := fmt.Sprintf("The most recent %s events", cx.EventType)
	if cx.SameSubject {
		label += " for " + subjectLabel(ev.Subject)
	}
	return label
}

// renderFacts turns events into lines a model can read, newest first,
// excluding the trigger itself. Times are the household's wall clock: a
// model asked to write "you usually eat around seven" needs the hour a
// person would recognise, not a UTC instant.
func renderFacts(events []runtimedb.HouseholdEvent, excludeSeq int64, limit int, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	var b strings.Builder
	n := 0
	for _, ev := range events {
		if ev.Seq == excludeSeq || n >= limit {
			continue
		}
		n++
		fmt.Fprintf(&b, "%s", ev.OccurredAt.In(loc).Format("Mon 2 Jan 15:04"))
		if len(ev.Payload) > 0 {
			fmt.Fprintf(&b, "  %s", string(ev.Payload))
		}
		b.WriteString("\n")
	}
	if n == 0 {
		return "(none recorded)"
	}
	return strings.TrimRight(b.String(), "\n")
}

// escalate asks for wording and returns it, or "" to use the fallback.
// It records the outcome either way: which branch was taken is exactly
// what someone reading a surprising notification needs to know.
func (e *Engine) escalate(ctx context.Context, d Decision, ev runtimedb.HouseholdEvent, rule resource.Resource, into, fallback string) (string, error) {
	if e.Escalator == nil {
		return "", e.recordFollowUp(d, ev, TypeEscalationSkipped, "esc", map[string]any{
			"reason": "no escalator is configured; the rule's own wording was used",
			"into":   into,
		})
	}
	facts, err := e.gatherFacts(rule, ev)
	if err == nil {
		var text string
		text, err = e.Escalator.Escalate(ctx, EscalationRequest{
			Rule:        d.Rule,
			Instruction: rule.Automation.Escalation.Prompt,
			Fallback:    fallback,
			Facts:       facts,
		})
		if err == nil {
			// Bound and clean at the trust boundary. The Butler bounds
			// its own output; this is the bound that has to hold, because
			// an Escalator is a seam anything could be wired into.
			if clean := sanitizeEscalated(text); clean != "" {
				return clean, e.recordFollowUp(d, ev, TypeEscalated, "esc", map[string]any{
					"into": into, "facts": len(facts), "chars": len(clean),
				})
			}
			err = fmt.Errorf("the escalator returned nothing usable")
		}
	}
	e.log().Warn("household escalation failed", "rule", d.Rule, "err", err)
	return "", e.recordFollowUp(d, ev, TypeEscalationFailed, "esc", map[string]any{
		"reason": err.Error(),
		"into":   into,
		"detail": "the rule's own wording was used instead",
	})
}

// sanitizeEscalated reduces an escalator's answer to one bounded line.
func sanitizeEscalated(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxEscalatedBytes {
		s = strings.TrimSpace(s[:maxEscalatedBytes])
	}
	return s
}

// factsJSON is a compact record of what a packet carried, for the audit
// trail. Content is deliberately omitted: it is already on the household
// stream as the events it was gathered from.
func factsJSON(facts []Fact) string {
	labels := make([]string, 0, len(facts))
	for _, f := range facts {
		labels = append(labels, f.Label)
	}
	out, _ := json.Marshal(labels)
	return string(out)
}
