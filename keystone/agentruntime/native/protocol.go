package native

import (
	"encoding/json"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime"
)

// The sidecar protocol: newline-delimited JSON over the sidecar's stdin
// and stdout, one message per line. It is the whole boundary between
// Wildstone and the OpenAI Agents SDK. No SDK type crosses it — the
// conversation history travels as an opaque JSON value that only the
// sidecar reads.
//
// Keystone → sidecar: start, tool_result, cancel.
// Sidecar → Keystone: ready, heartbeat, turn, tool_call, invalid_action,
// and exactly one outcome — final, waiting_for_user, blocked or error.
//
// The TypeScript side is runtimes/native-agent/src/protocol.ts. Bump
// ProtocolVersion on both sides together when a message changes shape.
const ProtocolVersion = 1

const (
	msgStart     = "start"
	msgToolRes   = "tool_result"
	msgCancel    = "cancel"
	msgReady     = "ready"
	msgHeartbeat = "heartbeat"
	msgTurn      = "turn"
	msgToolCall  = "tool_call"
	msgInvalid   = "invalid_action"
	msgFinal     = "final"
	msgWaiting   = "waiting_for_user"
	msgBlocked   = "blocked"
	msgError     = "error"
)

// Error kinds the sidecar reports.
const (
	errKindMaxTurns = "max_turns"
)

// Control tools the sidecar adds to every agent. They let the model say
// "I need a person" instead of reasoning forever, and Wildstone tool
// names may not shadow them.
const (
	toolAskUser       = "ask_user"
	toolReportBlocked = "report_blocked"
)

type modelSpec struct {
	// Provider is "openai" (any OpenAI-compatible endpoint, including
	// Ollama) or, only when the sidecar is started with the test switch,
	// "scripted".
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url,omitempty"`
	Name     string `json:"name,omitempty"`
	// API is "chat_completions" (default) or "responses".
	API string `json:"api,omitempty"`
	// Script drives the "scripted" provider in tests.
	Script json.RawMessage `json:"script,omitempty"`
}

// message is every protocol message; Type says which fields apply.
type message struct {
	Type     string `json:"type"`
	Protocol int    `json:"protocol,omitempty"`

	// start
	JobID        string                  `json:"job_id,omitempty"`
	Instructions string                  `json:"instructions,omitempty"`
	Input        string                  `json:"input,omitempty"`
	History      json.RawMessage         `json:"history,omitempty"`
	Tools        []agentruntime.ToolSpec `json:"tools,omitempty"`
	Model        *modelSpec              `json:"model,omitempty"`
	MaxTurns     int                     `json:"max_turns,omitempty"`
	HeartbeatMS  int64                   `json:"heartbeat_ms,omitempty"`

	// tool_call, tool_result, invalid_action
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	// outcomes and ready
	Output        string `json:"output,omitempty"`
	Question      string `json:"question,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Message       string `json:"message,omitempty"`
	Turn          int    `json:"turn,omitempty"`
	Engine        string `json:"engine,omitempty"`
	EngineVersion string `json:"engine_version,omitempty"`
}
