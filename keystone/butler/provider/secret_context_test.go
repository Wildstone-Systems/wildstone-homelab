package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSafeMessagesOmitsSecretCommandsWithoutMutatingHistory(t *testing.T) {
	for _, scope := range []string{"personal", "household"} {
		t.Run(scope, func(t *testing.T) {
			raw := "generated-low-entropy-value"
			command := "/secret " + scope + " WiFi = " + raw
			history := []Message{{Role: "user", Text: command}, {Role: "tool", ToolResults: []ToolResult{{Content: "Previous submission: " + command}}}, {Role: "assistant", ToolCalls: []ToolCall{{Input: map[string]any{"text": command}}}}, {Role: "user", Text: "show calendar"}}
			before, _ := json.Marshal(history)
			safe := SafeMessages(history)
			encoded, _ := json.Marshal(safe)
			if strings.Contains(string(encoded), raw) {
				t.Fatal("secret command survived inference boundary")
			}
			after, _ := json.Marshal(history)
			if string(before) != string(after) {
				t.Fatal("source history mutated")
			}
			if safe[3].Text != "show calendar" {
				t.Fatal("ordinary history changed")
			}
		})
	}
}
