package butler

import (
	"context"
	"fmt"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
)

// defaultMaxIterations bounds the agent loop's tool-call round trips
// (ADR-W006 A5.3/B3: "no infinite tool loops").
const defaultMaxIterations = 8

// ToolCallRecord is one audited tool call: what the model asked for, and
// the (already redacted/enveloped/truncated) result it got back.
type ToolCallRecord struct {
	Name   string
	Input  map[string]any
	Result string
	Error  string
	Plan   *plan.Plan
	// ResourceYAML carries an accepted propose_change's canonical
	// resource document (ADR-W017 A3) for the host to submit as a real
	// proposed plan record after the session.
	ResourceYAML string
	Rejected     bool
}

// Audit is the session audit record ADR-W006 A5.4 requires: provider/
// model, every tool call + result, every plan produced. No apply/approval
// outcome is recorded here because this package never applies anything —
// approval and apply happen outside the session, over the returned plans.
type Audit struct {
	Provider string
	Calls    []ToolCallRecord
	// Plans collects every plan.Plan produced during the session (from
	// get_plan and propose_change calls), in call order.
	Plans []*plan.Plan
}

// Transcript is what Session.Run returns: the model's final assistant
// text plus the full session audit.
type Transcript struct {
	FinalText string
	Audit     Audit
	// CappedIterations is true if the loop stopped because
	// MaxIterations was reached with the model still requesting tool
	// calls, rather than because the model produced a final answer.
	CappedIterations bool
}

// Session drives a Provider through the Butler's tool gateway (ADR-W006
// A5): the agent loop. Its only mutation surface is whatever ToolGateway
// exposes — currently propose_change, which itself never applies anything
// (see doc.go). Session holds no apply capability of its own.
type Session struct {
	Gateway *ToolGateway
	// System overrides SystemPrompt if non-empty.
	System string
	// MaxIterations bounds tool-call round trips. <= 0 uses
	// defaultMaxIterations.
	MaxIterations int
	// ReadOnly restricts the tool set offered to the model to read tools
	// only, omitting propose_change (ADR-W006 A4: QAOnly mode for a model
	// that failed the tool-reliability probe).
	ReadOnly bool
}

func (s *Session) maxIterations() int {
	if s.MaxIterations > 0 {
		return s.MaxIterations
	}
	return defaultMaxIterations
}

func (s *Session) systemPrompt() string {
	if s.System != "" {
		return s.System
	}
	return SystemPrompt
}

func (s *Session) toolSpecs() []provider.Tool {
	if s.ReadOnly {
		return s.Gateway.ReadOnlyToolSpecs()
	}
	return s.Gateway.ToolSpecs()
}

// dispatch routes a model-requested tool call through the gateway, applying
// the session's read-only constraint at the execution boundary. A ReadOnly
// session refuses mutating tools even if the model names one that was not in
// its offered set — the enforcement the spec set (offered tools) only
// advertises.
func (s *Session) dispatch(ctx context.Context, name string, input map[string]any) (ToolOutcome, error) {
	if s.ReadOnly {
		return s.Gateway.DispatchReadOnly(ctx, name, input)
	}
	return s.Gateway.Dispatch(ctx, name, input)
}

// Run drives p through the agent loop for one userMessage: send the
// system prompt + tool set + conversation, execute any tool calls the
// model requests through the gateway (which redacts, envelopes, and
// bounds every result — ToolGateway.Dispatch), feed results back, and
// repeat until the model stops requesting tools or MaxIterations is hit.
func (s *Session) Run(ctx context.Context, p provider.Provider, userMessage string) (*Transcript, error) {
	tr := &Transcript{Audit: Audit{Provider: p.Name()}}
	messages := []provider.Message{{Role: "user", Text: userMessage}}
	tools := s.toolSpecs()

	for i := 0; i < s.maxIterations(); i++ {
		resp, err := p.Chat(ctx, provider.ChatRequest{
			System:   s.systemPrompt(),
			Messages: messages,
			Tools:    tools,
		})
		if err != nil {
			return tr, fmt.Errorf("butler session: chat: %w", err)
		}

		if len(resp.ToolCalls) == 0 {
			tr.FinalText = resp.Text
			return tr, nil
		}

		messages = append(messages, provider.Message{
			Role:      "assistant",
			Text:      resp.Text,
			ToolCalls: resp.ToolCalls,
		})

		results := make([]provider.ToolResult, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			rec := ToolCallRecord{Name: tc.Name, Input: tc.Input}
			outcome, err := s.dispatch(ctx, tc.Name, tc.Input)
			content := outcome.Content
			if err != nil {
				rec.Error = err.Error()
				content = fmt.Sprintf("error: %v", err)
			} else {
				rec.Result = outcome.Content
				rec.Plan = outcome.Plan
				rec.ResourceYAML = outcome.ResourceYAML
				rec.Rejected = outcome.Rejected
				if outcome.Plan != nil {
					tr.Audit.Plans = append(tr.Audit.Plans, outcome.Plan)
				}
			}
			tr.Audit.Calls = append(tr.Audit.Calls, rec)
			results = append(results, provider.ToolResult{CallID: tc.ID, Content: content})
		}

		messages = append(messages, provider.Message{Role: "tool", ToolResults: results})
	}

	tr.CappedIterations = true
	tr.FinalText = "(reached the maximum number of tool-call iterations without a final answer)"
	return tr, nil
}
