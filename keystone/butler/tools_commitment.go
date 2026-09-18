package butler

// Commitments, for an agent (ADR-W035 A13, K7): two read tools.
//
// The ceiling is the point. An agent may DRAFT a commitment — turn "track
// the truck's oil every five thousand miles" into the document the engine
// already understands — and it may READ where things stand, so it can
// answer "what's coming up on the truck". It cannot mark one satisfied,
// waive a cycle, or move a threshold. Those are writes by a person or a
// plan, and there is no tool here that performs any of them.
//
// That is not an omission to be fixed later. A household whose agent can
// close its own commitments has a tracker that agrees with itself and
// nothing else, and the value of the whole design is that the record
// reflects what actually happened.
//
// The family surface does hold one commitment-adjacent WRITE —
// record_meter_reading, in tools_reading.go — and the line between them
// is A13's own. This file said "deliberately no third tool", which was
// stricter than A13: the clause is "record a reading it was NOT GIVEN",
// and a number a person said out loud was never on the forbidden list.
// Writing down what somebody told you is transcription. Deciding what it
// means is the engine's, and stays there.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

var commitmentSchemaSpec = provider.Tool{
	Name: "get_commitment_schema",
	Description: "Describe how to write a household Commitment and Meter on THIS deployment: " +
		"the closed vocabularies for rollover modes, scopes and intervals, and the meters that " +
		"actually exist here. Read this before drafting either, so a proposal is not refused " +
		"for using a word this deployment does not have.",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
}

func handleGetCommitmentSchema(_ context.Context, gw *ToolGateway, _ map[string]any) (ToolOutcome, error) {
	var b strings.Builder
	b.WriteString(`A Meter is a number the household keeps that only counts up. A Commitment is
a thing the household owes itself, due when a meter reaches a number.
Both are resources a human approves; neither acts on anything by itself.

  kind: Meter
  metadata: { name: <kebab-name>, owner: household | household/<member> | capability/<name> }
  spec:
    description: <what this number is, in a sentence>
    unit: <lowercase words: miles, hours, gallons>
    staleAfter: <Go duration, optional — how old a reading may get before
                 it can no longer support "not due yet">
    estimate: <number>/<d|w|mo|yr>, optional — display only, never moves a state

  kind: Commitment
  metadata: { name: <kebab-name>, owner: household | household/<member> }
  spec:
    description: <what is owed, in a sentence the household will be shown>
    rollover: `)
	b.WriteString(strings.Join(resource.CommitmentRollovers(), " | "))
	b.WriteString(`
    anchor: <YYYY-MM-DD — when tracking starts>
    anchorAt: { <counter meter>: <what it read at the anchor> }   # optional
    requiresBooking: <true when being due is not enough and an
                      appointment has to exist — a checkup, not a filter>
    scope: `)
	b.WriteString(strings.Join(resource.CommitmentScopes(), " | "))
	b.WriteString(`   # default household
    mayComplete: [household/<member> | group/<name>]   # who may answer it
    notify: [household/<member> | group/<name>]        # who hears about it
    due:
      - meter: clock | <meter name>
        every: <interval>          # 5000 for a counter; 90d, 6mo, 1yr for the clock
        lead: <interval>           # how early it starts asking
    satisfiedBy: { eventType: <namespace.name>, subject: ..., entity: ... }   # optional

Rules that are refused rather than warned about:

  - The clock is measured in time and a counter in numbers. "every: 6mo"
    on an odometer is refused, and so is "every: 5000" on the clock.
  - An interval is one word. A month is "mo"; "m" is minutes.
  - Several meters are allowed and the earliest to trip decides.
  - Only rollover "once" may name an absolute due point (at / atValue),
    because a fixed point cannot repeat.
  - There is no default rollover. Pick one deliberately: "once" finishes,
    "fromCompletion" measures the next one from where the meter stood when
    the work was done, "fromSchedule" keeps the original date whatever
    happened.

Three fields answer three different questions, and using one for all
three is the mistake this schema exists to prevent. metadata.owner is who
it is ABOUT. mayComplete is who may answer it. notify is who hears. A
child's checkup is about the child, answered by a parent, and the child
is not who should be nagged.

`)
	if gw == nil || gw.Store == nil {
		b.WriteString("This deployment's meters could not be read, so draft against the clock only.\n")
		return ToolOutcome{Content: b.String()}, nil
	}
	desired, err := gw.Store.Load()
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("reading desired state: %w", err)
	}
	var meters []string
	for _, res := range desired {
		if res.Kind == resource.KindMeter && res.Meter != nil {
			meters = append(meters, fmt.Sprintf("  %s (%s) — %s",
				res.Metadata.Name, res.Meter.Unit, res.Meter.Description))
		}
	}
	sort.Strings(meters)
	b.WriteString("Meters on this deployment:\n\n  clock — the calendar, built in, takes no readings\n")
	if len(meters) == 0 {
		b.WriteString("\nNo counter meters are declared here yet. A commitment measured in miles\nneeds its Meter proposed first, in the same plan or before it.\n")
	} else {
		b.WriteString(strings.Join(meters, "\n"))
		b.WriteString("\n")
	}
	return ToolOutcome{Content: b.String()}, nil
}

var commitmentStandingSpec = provider.Tool{
	Name: "get_commitments",
	Description: "Where the household's commitments stand right now: what is due, what is " +
		"overdue, what is booked, and which numbers nobody can currently read. Use this to " +
		"answer questions about what is coming up. It records nothing and changes nothing.",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
}

// CommitmentStandings is what the gateway needs to answer the question.
// An interface so keystone/butler does not depend on the pass's timer,
// and so the family surface can be handed a narrowed view later without
// this file learning about surfaces.
type CommitmentStandings interface {
	Describe() ([]CommitmentLine, error)
}

// CommitmentLine is one commitment, already reduced to the sentence a
// model should see. Reduced HERE rather than in the prompt, because a
// model handed the engine's whole occurrence — anchors, positions, lead
// thresholds — will do arithmetic with it, and the arithmetic is the one
// thing that is already settled.
type CommitmentLine struct {
	Name    string
	State   string
	Reason  string
	WaitsOn string
}

func handleGetCommitments(_ context.Context, gw *ToolGateway, _ map[string]any) (ToolOutcome, error) {
	if gw == nil || gw.Commitments == nil {
		return ToolOutcome{Content: "This deployment is not tracking any commitments."}, nil
	}
	lines, err := gw.Commitments.Describe()
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("reading commitments: %w", err)
	}
	if len(lines) == 0 {
		return ToolOutcome{Content: "Nothing is being tracked yet."}, nil
	}
	var b strings.Builder
	b.WriteString("Where the household's commitments stand:\n\n")
	for _, l := range lines {
		b.WriteString(fmt.Sprintf("  %-28s %-12s %s\n", l.Name, l.State, l.Reason))
	}
	b.WriteString(`
"can't tell" means a meter has not been read recently enough to say. It is
not the same as "not due": the honest answer is to ask somebody for the
number, not to reassure them.
`)
	return ToolOutcome{Content: b.String()}, nil
}
