package butler

// Recording a meter reading from the family surface (ADR-W035 A3, A13).
//
// A3 lists household chat as one of the four doors a reading may come
// through, and this is that door. Until now it was the one door with no
// hinge: the agent could describe a meter and report where a commitment
// stood, and a person who told it "truck's at 84,210" got a polite
// acknowledgement and nothing written down.
//
// A13 IS WHAT SHAPES THE TOOL, and the clause that matters is narrower
// than it first reads. The Butler "cannot mark a commitment satisfied,
// record a reading IT WAS NOT GIVEN, waive an occurrence, or change a
// due threshold." A reading it WAS given is not on that list, and the
// distinction is the whole design:
//
//   - The tool takes a number. It cannot derive, estimate, or look one
//     up — there is no path from this handler to a meter's history, an
//     estimate, or a projection, and nothing it could read if there
//     were.
//   - It writes a reading and stops. It does not close a cycle, waive
//     one, book an appointment, or touch a threshold. A household whose
//     agent can close its own commitments has a tracker that agrees
//     with itself and nothing else.
//
// So the agent is a transcriber here, not a witness. The number came
// from a person; the engine decides what it means.
//
// It is attributed to the member whose session this is, exactly as
// do_household_action is, because ADR-W001 admits no unattributed
// household write.

import (
	"context"
	"strconv"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
)

// ReadingCall is one reading a person gave the agent.
type ReadingCall struct {
	Meter string
	Value float64
	// Member is who the session is bound to, and who the reading is
	// recorded as coming from.
	Member string
}

var recordReadingSpec = provider.Tool{
	Name: "record_meter_reading",
	Description: "Write down a number somebody in the household has just told you — an odometer, " +
		"an hour meter, a gallons count. Use it ONLY for a number a person actually said in this " +
		"conversation: never a number you worked out, remembered from earlier, or estimated, and " +
		"never one you are asking them to confirm. If you are not sure you were told the number, " +
		"ask for it rather than calling this. Recording a reading does not mark anything as done — " +
		"it tells the house where the meter stands, and the house works out what that means.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"meter": map[string]any{
				"type":        "string",
				"description": "Which meter, by the name this household declared for it.",
			},
			"value": map[string]any{
				"type":        "number",
				"description": "The number the person gave you, exactly as they said it.",
			},
		},
		"required": []any{"meter", "value"},
	},
}

func handleRecordReading(ctx context.Context, gw *HouseholdGateway, input map[string]any) (ToolOutcome, error) {
	if gw.seams.RecordReading == nil {
		return ToolOutcome{Content: "Meter readings are not wired on this deployment.", Rejected: true}, nil
	}
	if gw.member == "" {
		// ADR-W001, the same backstop do_household_action carries: a
		// household write that names nobody is not a household write.
		return ToolOutcome{
			Content:  "This session is not bound to a household member, so no reading can be recorded.",
			Rejected: true,
		}, nil
	}
	meter := strings.TrimSpace(stringInput(input, "meter"))
	if meter == "" {
		return ToolOutcome{Content: "No meter named.", Rejected: true}, nil
	}
	value, ok := numberInput(input, "value")
	if !ok {
		// Deliberately a refusal rather than a default. A reading with
		// no number is the one thing this tool must never invent, and
		// zero is a real odometer value.
		return ToolOutcome{
			Content:  "No number given. Ask what the meter says rather than guessing.",
			Rejected: true,
		}, nil
	}
	if value < 0 {
		return ToolOutcome{Content: "A meter counts up from zero.", Rejected: true}, nil
	}
	said, err := gw.seams.RecordReading(ctx, ReadingCall{Meter: meter, Value: value, Member: gw.member})
	if err != nil {
		return ToolOutcome{}, err
	}
	return ToolOutcome{Content: said, EffectConfirmed: true}, nil
}

// numberInput reads a JSON number, which a provider may hand over as a
// float64 or as a string depending on the model.
//
// ParseFloat rather than Sscanf, and the difference matters here more
// than it usually would: Sscanf stops at the first thing it cannot read,
// so "84210 or so" and "84210abc" would both arrive as a confident
// 84210. A tool whose entire constraint is that it does not invent
// numbers must not quietly salvage one out of a sentence. The whole
// string parses, or the model is told to ask.
func numberInput(input map[string]any, key string) (float64, bool) {
	switch v := input[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}
