// A model that plays back a script instead of calling a provider. It
// exists so tests — here and in Keystone — can drive the real SDK loop
// through a real sidecar process without a network or a model. main.ts
// only accepts it when WILDSTONE_AGENT_ALLOW_SCRIPTED_MODEL=1.

import { Usage, type Model, type ModelRequest, type ModelResponse } from '@openai/agents-core';

export type ScriptStep = {
  // Reply with text (a final answer).
  text?: string;
  // Reply with tool calls; arguments is sent verbatim, so a test can
  // send malformed JSON.
  tool_calls?: { name: string; arguments: string }[];
  // Wait this long first (to test timeouts).
  sleep_ms?: number;
  // Fail the model call with this message.
  throw?: string;
};

export class ScriptedModel implements Model {
  private i = 0;
  private calls = 0;

  constructor(private readonly steps: ScriptStep[]) {
    if (!Array.isArray(steps) || steps.length === 0) throw new Error('scripted model needs at least one step');
  }

  async getResponse(request: ModelRequest): Promise<ModelResponse> {
    // Past the end, the last step repeats: a model that is stuck.
    const step = this.steps[Math.min(this.i++, this.steps.length - 1)]!;
    if (step.sleep_ms) {
      await new Promise<void>((resolve, reject) => {
        const t = setTimeout(resolve, step.sleep_ms);
        request.signal?.addEventListener('abort', () => {
          clearTimeout(t);
          reject(new Error('aborted'));
        });
      });
    }
    if (step.throw) throw new Error(step.throw);
    const output: ModelResponse['output'] = [];
    for (const c of step.tool_calls ?? []) {
      output.push({ type: 'function_call', callId: `call-${++this.calls}`, name: c.name, arguments: c.arguments, status: 'completed' });
    }
    if (step.text !== undefined) {
      output.push({ type: 'message', role: 'assistant', status: 'completed', content: [{ type: 'output_text', text: step.text }] });
    }
    return { usage: new Usage(), output };
  }

  getStreamedResponse(): never {
    throw new Error('scripted model does not stream');
  }
}
