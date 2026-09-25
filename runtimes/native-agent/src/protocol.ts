// The sidecar protocol: newline-delimited JSON on stdin (from Keystone)
// and stdout (to Keystone), one message per line.
//
// A turn message is a request: before every model attempt, retries
// included, the sidecar sends one and waits for Keystone's proceed.
// model_done reports how the attempt ended.
//
// This file mirrors keystone/agentruntime/native/protocol.go. Bump
// PROTOCOL_VERSION on both sides together when a message changes shape.

export const PROTOCOL_VERSION = 2;

// Control tools added to every agent. Keystone refuses Wildstone tools
// that would shadow them.
export const TOOL_ASK_USER = 'ask_user';
export const TOOL_REPORT_BLOCKED = 'report_blocked';

export type ToolSpec = {
  name: string;
  description: string;
  parameters: Record<string, unknown>;
};

export type ModelSpec = {
  provider: 'openai' | 'scripted';
  base_url?: string;
  name?: string;
  api?: 'chat_completions' | 'responses';
  script?: unknown;
};

export type StartMessage = {
  type: 'start';
  protocol: number;
  job_id: string;
  instructions: string;
  input: string;
  // Opaque to Keystone: whatever a previous run of this sidecar returned.
  history?: unknown[];
  // Earlier turns as plain text, when there is no history.
  conversation?: { role: 'user' | 'assistant'; text: string }[];
  tools?: ToolSpec[];
  model: ModelSpec;
  max_turns: number;
  heartbeat_ms: number;
};

export type ToolResultMessage = {
  type: 'tool_result';
  call_id: string;
  output?: string;
  is_error?: boolean;
};

export type CancelMessage = { type: 'cancel' };

export type ProceedMessage = { type: 'proceed' };

export type Inbound = StartMessage | ToolResultMessage | CancelMessage | ProceedMessage;

export type ErrorKind =
  | 'max_turns'
  | 'model_behavior'
  | 'model'
  | 'cancelled'
  | 'protocol'
  | 'internal';

export type Outbound =
  | {
      type: 'ready';
      protocol: number;
      engine: string;
      engine_version: string;
    }
  | { type: 'heartbeat' }
  | { type: 'turn'; turn: number; attempt: number }
  | { type: 'model_done'; is_error?: boolean; tool_calls?: string[] }
  | { type: 'tool_call'; call_id: string; name: string; arguments: string }
  | { type: 'invalid_action'; call_id: string; name: string; reason: string }
  | { type: 'final'; output: string; history: unknown[] }
  | { type: 'waiting_for_user'; question: string; history: unknown[] }
  | { type: 'blocked'; reason: string; history: unknown[] }
  | { type: 'error'; kind: ErrorKind; message: string };
