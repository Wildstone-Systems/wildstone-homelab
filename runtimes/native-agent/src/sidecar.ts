// The native agent run: one Wildstone job, driven by the OpenAI Agents
// SDK.
//
// This process owns the reasoning loop and nothing else. It holds no
// Wildstone tool: each tool here is a stub that sends the call to
// Keystone and waits for the answer, so authorization, execution and
// audit all happen on the Keystone side. Timeouts, budgets and
// cancellation are enforced there too; what this side adds is reporting
// what it sees (turns, malformed calls) and ending in exactly one
// outcome message.

import {
  Agent,
  MaxTurnsExceededError,
  ModelBehaviorError,
  run,
  tool,
  setTracingDisabled,
  type AgentInputItem,
  type Model,
  type ModelRequest,
  type ModelResponse,
  type Tool,
} from '@openai/agents-core';

import {
  TOOL_ASK_USER,
  TOOL_REPORT_BLOCKED,
  type ErrorKind,
  type Outbound,
  type StartMessage,
  type ToolResultMessage,
  type ToolSpec,
} from './protocol.js';

// Tracing would export conversation content to OpenAI's trace service.
// A household's conversations stay in the household.
setTracingDisabled(true);

export type Send = (m: Outbound) => void;

const MAX_ERROR_TEXT = 2048;

function bound(s: string, n: number): string {
  return s.length <= n ? s : s.slice(0, n) + '…';
}

/**
 * Carries tool calls to Keystone and their results back, matched by
 * call id.
 */
export class ToolBridge {
  private pending = new Map<string, (r: ToolResultMessage) => void>();
  private seq = 0;

  constructor(private readonly send: Send, private readonly signal: AbortSignal) {}

  call(callId: string | undefined, name: string, args: string): Promise<string> {
    const id = callId && !this.pending.has(callId) ? callId : `wildstone-${++this.seq}`;
    return new Promise<string>((resolve, reject) => {
      if (this.signal.aborted) {
        reject(new Error('cancelled'));
        return;
      }
      const onAbort = () => {
        this.pending.delete(id);
        reject(new Error('cancelled'));
      };
      this.signal.addEventListener('abort', onAbort, { once: true });
      this.pending.set(id, (r) => {
        this.signal.removeEventListener('abort', onAbort);
        // An error result is still an observation: the model reads why
        // the call failed and decides what to do next.
        resolve(r.output ?? '');
      });
      this.send({ type: 'tool_call', call_id: id, name, arguments: args });
    });
  }

  /** Delivers a result. Returns false for a call id nobody is waiting on. */
  resolve(r: ToolResultMessage): boolean {
    const done = this.pending.get(r.call_id);
    if (!done) return false;
    this.pending.delete(r.call_id);
    done(r);
    return true;
  }
}

/**
 * Holds each model attempt until Keystone says proceed. Keystone checks
 * its own boundaries (authority, privacy, fencing) in between; if it
 * refuses, it stops this process and the promise never resolves.
 */
export class ModelGate {
  private waiting: (() => void) | undefined;

  constructor(private readonly send: Send, private readonly signal: AbortSignal) {}

  ask(turn: number, attempt: number): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      if (this.signal.aborted) {
        reject(new Error('cancelled'));
        return;
      }
      const onAbort = () => {
        this.waiting = undefined;
        reject(new Error('cancelled'));
      };
      this.signal.addEventListener('abort', onAbort, { once: true });
      this.waiting = () => {
        this.signal.removeEventListener('abort', onAbort);
        resolve();
      };
      this.send({ type: 'turn', turn, attempt });
    });
  }

  /** Keystone's proceed. Returns false when nothing was waiting. */
  proceed(): boolean {
    const go = this.waiting;
    if (!go) return false;
    this.waiting = undefined;
    go();
    return true;
  }
}

/** How many times one model call is retried, each attempt gated. */
export const MODEL_RETRIES = 1;

function retryable(err: unknown): boolean {
  const status = (err as { status?: unknown } | null)?.status;
  // No status: a network failure or a timeout. 429 and 5xx: the provider
  // is busy or broken. Anything else is the request's fault and would
  // fail the same way again.
  return typeof status !== 'number' || status === 429 || status >= 500;
}

/**
 * Wraps the real model. Every attempt waits for Keystone's go-ahead and
 * reports how it ended; each malformed tool call the SDK would otherwise
 * handle silently is reported too.
 */
export class ReportingModel implements Model {
  private turns = 0;

  constructor(
    private readonly inner: Model,
    private readonly send: Send,
    private readonly known: Set<string>,
    private readonly gate: ModelGate,
    private readonly signal: AbortSignal,
    private readonly backoffMs = 1000,
  ) {}

  async getResponse(request: ModelRequest): Promise<ModelResponse> {
    const turn = ++this.turns;
    for (let attempt = 1; ; attempt++) {
      await this.gate.ask(turn, attempt);
      let response: ModelResponse;
      try {
        response = await this.inner.getResponse(request);
      } catch (err) {
        this.send({ type: 'model_done', is_error: true });
        if (this.signal.aborted || attempt > MODEL_RETRIES || !retryable(err)) throw err;
        await new Promise((r) => setTimeout(r, this.backoffMs * attempt));
        continue;
      }
      const calls: string[] = [];
      for (const item of response.output) {
        if (item.type !== 'function_call') continue;
        calls.push(item.name);
        const reason = invalidReason(item.name, item.arguments, this.known);
        if (reason) {
          this.send({ type: 'invalid_action', call_id: item.callId, name: item.name, reason });
        }
      }
      this.send({ type: 'model_done', tool_calls: calls.sort() });
      return response;
    }
  }

  getStreamedResponse(): never {
    throw new Error('the native agent runtime does not stream');
  }
}

function invalidReason(name: string, args: string, known: Set<string>): string {
  if (!known.has(name)) return 'unknown tool';
  try {
    const v: unknown = JSON.parse(args === '' ? '{}' : args);
    if (v === null || typeof v !== 'object' || Array.isArray(v)) return 'arguments are not a JSON object';
  } catch {
    return 'arguments are not valid JSON';
  }
  return '';
}

const questionSchema = {
  type: 'object',
  properties: { question: { type: 'string', description: 'One clear question for the person.' } },
  required: ['question'],
  additionalProperties: false,
} as const;

const reasonSchema = {
  type: 'object',
  properties: { reason: { type: 'string', description: 'Why you cannot continue.' } },
  required: ['reason'],
  additionalProperties: false,
} as const;

function stringField(input: unknown, key: string): string {
  if (input && typeof input === 'object' && key in input) {
    const v = (input as Record<string, unknown>)[key];
    if (typeof v === 'string') return v;
  }
  return '';
}

function wildstoneTool(spec: ToolSpec, bridge: ToolBridge): Tool {
  return tool({
    name: spec.name,
    description: spec.description,
    // Wildstone's schemas are not written for strict mode, and Keystone
    // validates every call itself anyway.
    parameters: { type: 'object', properties: {}, ...spec.parameters } as never,
    strict: false,
    execute: async (input, _ctx, details) =>
      bridge.call(details?.toolCall?.callId, spec.name, JSON.stringify(input ?? {})),
  });
}

/**
 * Runs one job to exactly one outcome message. Never throws.
 */
export async function runJob(
  start: StartMessage,
  model: Model,
  send: Send,
  bridge: ToolBridge,
  gate: ModelGate,
  signal: AbortSignal,
  backoffMs = 1000,
): Promise<void> {
  const specs = start.tools ?? [];
  const known = new Set<string>([TOOL_ASK_USER, TOOL_REPORT_BLOCKED, ...specs.map((s) => s.name)]);

  // Which control tool (if any) ended the run, and what it said.
  let control: { kind: 'ask' | 'blocked'; text: string } | undefined;
  const controlTools: Tool[] = [
    tool({
      name: TOOL_ASK_USER,
      description: 'Ask the person one question when you need information only they can give. This ends your turn.',
      parameters: questionSchema as never,
      strict: false,
      execute: async (input) => {
        control ??= { kind: 'ask', text: stringField(input, 'question') };
        return 'question sent';
      },
    }),
    tool({
      name: TOOL_REPORT_BLOCKED,
      description: 'Report that you cannot make progress, and why. This ends your turn.',
      parameters: reasonSchema as never,
      strict: false,
      execute: async (input) => {
        control ??= { kind: 'blocked', text: stringField(input, 'reason') };
        return 'reported';
      },
    }),
  ];

  const agent = new Agent({
    name: 'wildstone-native',
    instructions: start.instructions,
    model: new ReportingModel(model, send, known, gate, signal, backoffMs),
    tools: [...specs.map((s) => wildstoneTool(s, bridge)), ...controlTools],
    toolUseBehavior: { stopAtToolNames: [TOOL_ASK_USER, TOOL_REPORT_BLOCKED] },
  });

  const earlier: AgentInputItem[] =
    start.history && start.history.length > 0
      ? (start.history as AgentInputItem[])
      : (start.conversation ?? []).map((u): AgentInputItem =>
          u.role === 'assistant'
            ? { type: 'message', role: 'assistant', status: 'completed', content: [{ type: 'output_text', text: u.text }] }
            : { type: 'message', role: 'user', content: u.text },
        );
  const input: string | AgentInputItem[] =
    earlier.length > 0 ? [...earlier, { type: 'message', role: 'user', content: start.input }] : start.input;

  const fail = (kind: ErrorKind, err: unknown) =>
    send({ type: 'error', kind, message: bound(err instanceof Error ? err.message : String(err), MAX_ERROR_TEXT) });

  try {
    const result = await run(agent, input, { maxTurns: start.max_turns, signal });
    const history = result.history as unknown[];
    if (control?.kind === 'ask') {
      send({ type: 'waiting_for_user', question: control.text, history });
    } else if (control?.kind === 'blocked') {
      send({ type: 'blocked', reason: control.text, history });
    } else {
      const out = result.finalOutput;
      send({ type: 'final', output: typeof out === 'string' ? out : out == null ? '' : JSON.stringify(out), history });
    }
  } catch (err) {
    if (signal.aborted) fail('cancelled', 'cancelled');
    else if (err instanceof MaxTurnsExceededError) fail('max_turns', err);
    else if (err instanceof ModelBehaviorError) fail('model_behavior', err);
    else fail('model', err);
  }
}
