package provider

import "context"

// Verdict is the result of Probe: whether a model reliably tool-calls
// (Full) or should be restricted to read-only Q&A (QAOnly), per ADR-W006
// A4.
type Verdict int

const (
	// Full means the model correctly called the expected tool in the
	// scripted diagnostic — it can be trusted with the full read+propose
	// tool surface.
	Full Verdict = iota
	// QAOnly means the model did not reliably call the expected tool — it
	// gets read tools only, no propose_change.
	QAOnly
)

func (v Verdict) String() string {
	switch v {
	case Full:
		return "Full"
	case QAOnly:
		return "QAOnly"
	default:
		return "Unknown"
	}
}

// probeToolName is the name of the single diagnostic tool offered to the
// model during Probe.
const probeToolName = "get_health"

// probeTool is the single tool available during the scripted diagnostic
// eval: a get_health-like tool for a fixture service.
var probeTool = Tool{
	Name:        probeToolName,
	Description: "Fetch the health status of a named service. Returns healthy/unhealthy and details.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"service": map[string]any{
				"type":        "string",
				"description": "Name of the service to check.",
			},
		},
		"required": []string{"service"},
	},
}

// Probe runs a scripted, self-contained diagnostic eval against p: a
// fixture service is unhealthy, and the model is told to call the tool
// that fetches its health. If the model responds with a correct tool call
// (name == "get_health"), Probe returns Full; otherwise (plain text,
// rambling, wrong tool name, or an error) it returns QAOnly.
//
// Per ADR-W006 A4, this validates the model at setup time so a model that
// can't reliably tool-call is restricted to read-only Q&A mode rather than
// treated as a degraded operator.
func Probe(ctx context.Context, p Provider) (Verdict, error) {
	req := ChatRequest{
		System: "You are a diagnostic assistant for the Wildstone homelab. " +
			"When asked to diagnose a service, you must use the available tool " +
			"to fetch its health status rather than guessing.",
		Messages: []Message{
			{
				Role: "user",
				Text: "The fixture service \"widget-api\" is reporting as unhealthy. " +
					"Diagnose it by calling the tool that fetches its health.",
			},
		},
		Tools:     []Tool{probeTool},
		MaxTokens: 512,
	}

	resp, err := p.Chat(ctx, req)
	if err != nil {
		return QAOnly, err
	}

	for _, tc := range resp.ToolCalls {
		if tc.Name == probeToolName {
			return Full, nil
		}
	}
	return QAOnly, nil
}
