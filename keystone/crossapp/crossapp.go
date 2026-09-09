// Package crossapp runs a household routine across connected
// applications (ADR-W030 A4/A6, work package P12).
//
// The routines it runs are USER-AUTHORED documents, not behaviour built
// into Wildstone. "When someone leaves, check the calendar and put
// anything we're out of on the shopping list" is a thing a household
// wrote down; nothing here knows what a departure or a breakfast is.
// That distinction is the point of A6: native automation where
// appropriate, Wildstone for cross-app coordination, and no
// special-purpose logic pretending to be a feature.
//
// Three properties the runner holds, each of which is a way these
// scenarios go wrong in a real house:
//
//   It completes with the model disabled. Every step is deterministic —
//   a query, a comparison, an action. A model may phrase the message a
//   routine sends, and if there is no model the routine still runs and
//   the message is the author's own words.
//
//   It deduplicates repeated events. A phone that reports leaving three
//   times in a minute is normal, and a shopping list with milk on it
//   three times is not.
//
//   It pauses on an uncertain effect. When an action's outcome cannot be
//   established, the run stops and says so rather than retrying: the
//   whole reason A4 forbids blind retries is that "did that work?" and
//   "do it again" are different questions.

package crossapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Step kinds a routine may use. The set is closed: a routine is a small
// declarative document a person can read, not a program.
const (
	// StepQuery reads from a connection.
	StepQuery = "query"
	// StepAct performs a declared action on a connection.
	StepAct = "act"
	// StepNotify delivers a message. Its text is the author's; a model
	// may rephrase it when one is available, and the routine runs
	// identically when none is.
	StepNotify = "notify"
)

// Routine is a user-authored cross-app routine.
type Routine struct {
	Name string `json:"name" yaml:"name"`
	// Trigger is the household event type this routine responds to.
	Trigger string `json:"trigger" yaml:"trigger"`
	// DedupeWindow collapses repeated triggers. A phone reporting a
	// departure three times in a minute is one departure.
	DedupeWindow time.Duration `json:"dedupeWindow" yaml:"dedupeWindow"`
	Steps        []Step        `json:"steps" yaml:"steps"`
}

// Step is one thing a routine does.
type Step struct {
	Kind       string `json:"kind" yaml:"kind"`
	Connection string `json:"connection,omitempty" yaml:"connection,omitempty"`
	Capability string `json:"capability,omitempty" yaml:"capability,omitempty"`
	Request    string `json:"request,omitempty" yaml:"request,omitempty"`
	// As names the step's result so a later step can refer to it.
	As string `json:"as,omitempty" yaml:"as,omitempty"`
	// When is an optional guard: the step runs only if the named earlier
	// result was not empty. Deliberately the only conditional — a
	// routine with expressions in it is a program, and a program is not
	// something a household reviews.
	When string `json:"when,omitempty" yaml:"when,omitempty"`
	// Text is a notify step's message, in the author's words.
	Text string `json:"text,omitempty" yaml:"text,omitempty"`
	// Optional marks a step whose unavailability does not stop the run —
	// "check the calendar if you can" rather than "check the calendar".
	Optional bool `json:"optional,omitempty" yaml:"optional,omitempty"`
}

// Outcome states for a completed run.
const (
	// RunCompleted means every required step finished.
	RunCompleted = "completed"
	// RunSkipped means the trigger was a duplicate.
	RunSkipped = "skipped"
	// RunWaiting means a source was unavailable, or an action's outcome
	// could not be established. Either way the run stopped without
	// concluding, which is the correct answer to a question the house
	// could not answer.
	RunWaiting = "waiting"
)

// StepResult records what one step did, for the trace and for the
// person reading it afterwards.
type StepResult struct {
	Kind       string `json:"kind"`
	Connection string `json:"connection,omitempty"`
	Capability string `json:"capability,omitempty"`
	// ResultType is the integration contract's answer: ok, empty,
	// unavailable, or stale. Carried through unchanged, because
	// flattening it is exactly how an outage becomes an empty list.
	ResultType string          `json:"resultType,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	Skipped    bool            `json:"skipped,omitempty"`
	Message    string          `json:"message,omitempty"`
	ObservedAt time.Time       `json:"observedAt,omitzero"`
}

// Run is the result of one routine run.
type Run struct {
	Routine string       `json:"routine"`
	State   string       `json:"state"`
	Reason  string       `json:"reason,omitempty"`
	Steps   []StepResult `json:"steps"`
	// Messages are what the routine would deliver, in order.
	Messages []string `json:"messages,omitempty"`
}

// Integrations is the seam onto the integration-work contract. The
// runner asks a connection a question and gets one of the four result
// types back; it never talks to an application.
type Integrations interface {
	// Ask queues work and waits for the answer within the deadline. It
	// returns the contract's result type, so the runner can tell an
	// empty answer from an outage.
	Ask(ctx context.Context, connection, kind, capability, request string, deadline time.Duration) (resultType string, data json.RawMessage, observedAt time.Time, err error)
}

// Dedupe records whether a trigger has already been handled.
type Dedupe interface {
	// Seen reports whether this key was handled inside the window, and
	// records it when it was not.
	Seen(routine, key string, window time.Duration) (bool, error)
}

// Phraser optionally rewords a routine's message. A nil Phraser — or a
// model that is off — leaves the author's own text, which is why these
// scenarios complete with the model disabled.
type Phraser interface {
	Phrase(ctx context.Context, text string, context map[string]json.RawMessage) (string, error)
}

// Runner executes routines.
type Runner struct {
	Integrations Integrations
	Dedupe       Dedupe
	// Phraser is optional. Nil means the author's words are used.
	Phraser Phraser
	// QueryDeadline bounds one integration read. Zero uses five seconds,
	// the interactive end-to-end budget.
	QueryDeadline time.Duration
}

func (r *Runner) deadline() time.Duration {
	if r.QueryDeadline > 0 {
		return r.QueryDeadline
	}
	return 5 * time.Second
}

// Trigger is one household event that may start a routine.
type Trigger struct {
	Type string
	// Key identifies THIS occurrence for deduplication — an event id, or
	// subject plus a coarse timestamp. Two triggers with the same key
	// inside the window are one departure, not two.
	Key     string
	Subject string
	Payload map[string]any
}

// Execute runs one routine against one trigger.
func (r *Runner) Execute(ctx context.Context, routine Routine, trigger Trigger) (Run, error) {
	run := Run{Routine: routine.Name}
	if routine.Trigger != "" && trigger.Type != routine.Trigger {
		run.State = RunSkipped
		run.Reason = fmt.Sprintf("this routine listens for %s, not %s", routine.Trigger, trigger.Type)
		return run, nil
	}
	if r.Dedupe != nil && routine.DedupeWindow > 0 && trigger.Key != "" {
		seen, err := r.Dedupe.Seen(routine.Name, trigger.Key, routine.DedupeWindow)
		if err != nil {
			return run, err
		}
		if seen {
			// A phone reporting a departure three times is one
			// departure, and a shopping list with milk on it three times
			// is the failure this prevents.
			run.State = RunSkipped
			run.Reason = "this already happened just now"
			return run, nil
		}
	}

	results := map[string]json.RawMessage{}
	empties := map[string]bool{}
	for _, step := range routine.Steps {
		if step.When != "" && empties[step.When] {
			run.Steps = append(run.Steps, StepResult{
				Kind: step.Kind, Connection: step.Connection, Capability: step.Capability,
				Skipped: true, Message: fmt.Sprintf("%s had nothing in it", step.When),
			})
			continue
		}
		switch step.Kind {
		case StepQuery, StepAct:
			kind := "query"
			if step.Kind == StepAct {
				kind = "action"
			}
			resultType, data, observedAt, err := r.Integrations.Ask(ctx,
				step.Connection, kind, step.Capability, step.Request, r.deadline())
			result := StepResult{
				Kind: step.Kind, Connection: step.Connection, Capability: step.Capability,
				ResultType: resultType, Data: data, ObservedAt: observedAt,
			}
			if err != nil {
				// The outcome is UNKNOWN, which is different from
				// failed. An action that may or may not have happened is
				// never retried automatically (A4).
				result.Message = err.Error()
				run.Steps = append(run.Steps, result)
				run.State = RunWaiting
				if step.Kind == StepAct {
					run.Reason = fmt.Sprintf(
						"%s on %s may or may not have happened, so this is paused rather than retried: %v",
						step.Capability, step.Connection, err)
				} else {
					run.Reason = fmt.Sprintf("could not read %s from %s: %v",
						step.Capability, step.Connection, err)
				}
				return run, nil
			}
			run.Steps = append(run.Steps, result)
			switch resultType {
			case "unavailable":
				if step.Optional {
					// "Check the calendar if you can" — the run goes on
					// and says what it could not see.
					empties[step.As] = true
					continue
				}
				run.State = RunWaiting
				run.Reason = fmt.Sprintf("%s on %s is unavailable, so this is waiting rather than guessing",
					step.Capability, step.Connection)
				return run, nil
			case "empty":
				empties[step.As] = true
			case "stale":
				// A stale reading is usable, and the routine's message
				// has to say so rather than implying it is current.
				run.Messages = append(run.Messages, fmt.Sprintf(
					"(%s on %s is as of %s, not now)", step.Capability, step.Connection,
					observedAt.Format(time.RFC3339)))
			}
			if step.As != "" {
				results[step.As] = data
			}

		case StepNotify:
			text := step.Text
			if r.Phraser != nil {
				// The model is optional decoration on a deterministic
				// step: if it fails or is off, the author's words go
				// out unchanged.
				if phrased, err := r.Phraser.Phrase(ctx, text, results); err == nil && strings.TrimSpace(phrased) != "" {
					text = phrased
				}
			}
			run.Messages = append(run.Messages, text)
			run.Steps = append(run.Steps, StepResult{Kind: StepNotify, Message: text})

		default:
			return run, fmt.Errorf("crossapp: routine %q has an unknown step kind %q", routine.Name, step.Kind)
		}
	}
	run.State = RunCompleted
	return run, nil
}
