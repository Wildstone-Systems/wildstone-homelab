package provider

import (
	"encoding/json"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/redact"
)

// SafeMessages copies history before inference, including resumed tool results.
// Secret intake happens earlier; this also protects older conversation history.
func SafeMessages(messages []Message) []Message {
	out := make([]Message, len(messages))
	for i, m := range messages {
		out[i] = m
		out[i].Text = redact.SecretText(m.Text)
		out[i].ToolResults = append([]ToolResult(nil), m.ToolResults...)
		for j, r := range out[i].ToolResults {
			out[i].ToolResults[j].Content = redact.SecretText(r.Content)
		}
		out[i].ToolCalls = append([]ToolCall(nil), m.ToolCalls...)
		for j, c := range out[i].ToolCalls {
			b, e := json.Marshal(c.Input)
			if e != nil || redact.SecretSubmission(string(b)) {
				out[i].ToolCalls[j].Input = map[string]any{"omitted": "credential input"}
			}
		}
	}
	return out
}
