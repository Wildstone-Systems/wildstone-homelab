package automation

// Drafting a rule (ADR-W022 H7): the one path a proposed household rule
// takes before anyone approves it, whoever wrote it.
//
// A rule reaches the Statefile the same way every resource does — as a
// plan a human approves — so the interesting question is not how the
// Butler writes one but how anybody checks one BEFORE proposing it.
// ReplayDraft is that check: parse, validate, policy, and run it against
// real history with no side effects. The API endpoint and the Butler's
// dry-run tool are the same call, so a rule the Butler tried is a rule
// the operator can try identically, and neither sees a different verdict
// from the other.

import (
	"context"
	"fmt"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// DefaultDraftWindow is how far back a draft replays when the caller
// does not say. A week covers the weekly rhythms most household rules
// are about, without pulling a year of history through a summary.
const DefaultDraftWindow = 7 * 24 * time.Hour

// DraftReplay is what a dry run found.
type DraftReplay struct {
	Rule string `json:"rule"`
	// Enabled is the draft's own disabled flag, inverted. A disabled
	// rule still replays — the question a dry run answers is what it
	// WOULD do, which is the point of asking before turning it on.
	Enabled   bool       `json:"enabled"`
	From      time.Time  `json:"from"`
	To        time.Time  `json:"to"`
	Evaluated int        `json:"evaluated"`
	Fired     int        `json:"fired"`
	Decisions []Decision `json:"decisions"`
	// Violations are what a proposal of this rule would be refused for.
	// A dry run is not a proposal, so they are reported rather than
	// fatal: seeing the rule's behaviour AND why it would be rejected in
	// one answer is more useful than one error at a time.
	Violations []policy.Violation `json:"violations,omitempty"`
}

// ParseDraft decodes and validates one Automation document.
func ParseDraft(doc string) (resource.Resource, error) {
	parsed, err := resource.Unmarshal([]byte(doc))
	if err != nil {
		return resource.Resource{}, fmt.Errorf("parsing rule: %w", err)
	}
	if parsed.Kind != resource.KindAutomation {
		return resource.Resource{}, fmt.Errorf("rule must be kind Automation, got %s", parsed.Kind)
	}
	if err := parsed.Validate(); err != nil {
		return resource.Resource{}, fmt.Errorf("invalid rule: %w", err)
	}
	return parsed, nil
}

// ReplayDraft validates a rule, policy-checks it as principal/role would
// propose it, and replays it over [from, to]. Nothing is written: not the
// Statefile, not the household stream, not the engine's cursor.
func (e *Engine) ReplayDraft(ctx context.Context, rule resource.Resource, pol *policy.Engine, principal, role string, from, to time.Time, limit int) (DraftReplay, error) {
	if rule.Automation == nil {
		return DraftReplay{}, fmt.Errorf("automation: not a rule")
	}
	out := DraftReplay{Rule: rule.Metadata.Name, Enabled: !rule.Automation.Disabled, From: from, To: to}

	if pol != nil {
		current, err := e.State.Load()
		if err != nil {
			return out, err
		}
		out.Violations, _ = pol.Validate(policy.Change{
			Proposed: []resource.Resource{rule}, Current: current, Principal: principal, Role: role,
		})
	}

	// Replay as if enabled, on a copy: a dry run must never mutate the
	// rule it was handed, and a disabled rule's whole point is asking
	// what it would do.
	spec := *rule.Automation
	spec.Disabled = false
	replayed := rule
	replayed.Automation = &spec

	decisions, err := e.Replay(ctx, []resource.Resource{replayed}, from, to, limit)
	if err != nil {
		return out, err
	}
	out.Decisions = decisions
	out.Evaluated = len(decisions)
	for _, d := range decisions {
		if d.Fired {
			out.Fired++
		}
	}
	return out, nil
}

// Summary renders a dry run as the few lines a person (or a model about
// to propose the rule) actually needs: how often it would have fired,
// and why it did or did not each time.
func (r DraftReplay) Summary(max int) string {
	s := fmt.Sprintf("Rule %q replayed over %s to %s: %d matching event(s), %d would have fired.\n",
		r.Rule, r.From.Format(time.RFC3339), r.To.Format(time.RFC3339), r.Evaluated, r.Fired)
	if !r.Enabled {
		s += "(The draft is disabled; this is what it would do once enabled.)\n"
	}
	for _, v := range r.Violations {
		s += "POLICY WOULD REJECT THIS RULE: " + v.String() + "\n"
	}
	if r.Evaluated == 0 {
		s += "No event in the window matched its trigger. Either nothing like this happened, " +
			"or the trigger's event type or subject does not match what publishers actually send.\n"
		return s
	}
	shown := r.Decisions
	if max > 0 && len(shown) > max {
		shown = shown[len(shown)-max:]
	}
	for _, d := range shown {
		verdict := "suppressed"
		if d.Fired {
			verdict = "FIRED"
		}
		s += fmt.Sprintf("  %s  %s  %s: %s\n",
			d.At.Format(time.RFC3339), verdict, subjectLabel(d.Subject), d.Reason)
	}
	if len(shown) < len(r.Decisions) {
		s += fmt.Sprintf("  (%d earlier evaluation(s) not shown)\n", len(r.Decisions)-len(shown))
	}
	return s
}
