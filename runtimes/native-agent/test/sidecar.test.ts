import assert from 'node:assert/strict';
import { test } from 'node:test';

import { PROTOCOL_VERSION, type Outbound, type StartMessage } from '../src/protocol.js';
import { ScriptedModel, type ScriptStep } from '../src/scripted.js';
import { ModelGate, ToolBridge, runJob } from '../src/sidecar.js';

const lookup = {
  name: 'lookup',
  description: 'Look something up.',
  parameters: { type: 'object', properties: { q: { type: 'string' } } },
};

function startMsg(over: Partial<StartMessage> = {}): StartMessage {
  return {
    type: 'start',
    protocol: PROTOCOL_VERSION,
    job_id: 'j1',
    instructions: 'Be helpful.',
    input: 'hello',
    tools: [lookup],
    model: { provider: 'scripted' },
    max_turns: 5,
    heartbeat_ms: 1000,
    ...over,
  };
}

/**
 * Runs a job against a scripted model, answering every tool call the
 * way `answer` says, and returns everything the sidecar sent.
 */
async function drive(
  steps: ScriptStep[],
  over: Partial<StartMessage> = {},
  answer: (name: string, args: string) => string = () => 'ok',
  abort = new AbortController(),
): Promise<Outbound[]> {
  const sent: Outbound[] = [];
  let bridge!: ToolBridge;
  let gate!: ModelGate;
  const send = (m: Outbound) => {
    sent.push(m);
    if (m.type === 'tool_call') {
      queueMicrotask(() => bridge.resolve({ type: 'tool_result', call_id: m.call_id, output: answer(m.name, m.arguments) }));
    }
    // Keystone lets every attempt through (its refusals are tested on
    // the Go side, where they are decided).
    if (m.type === 'turn') queueMicrotask(() => gate.proceed());
  };
  bridge = new ToolBridge(send, abort.signal);
  gate = new ModelGate(send, abort.signal);
  await runJob(startMsg(over), new ScriptedModel(steps), send, bridge, gate, abort.signal, 1);
  return sent;
}

const outcome = (sent: Outbound[]) => sent[sent.length - 1]!;

test('a tool call goes to keystone and its result reaches the model', async () => {
  const sent = await drive([{ tool_calls: [{ name: 'lookup', arguments: '{"q":"x"}' }] }, { text: 'found it' }], {}, () => 'x=1');
  const call = sent.find((m) => m.type === 'tool_call');
  assert.deepEqual(call && { name: call.name, args: call.arguments }, { name: 'lookup', args: '{"q":"x"}' });
  const out = outcome(sent);
  assert.equal(out.type, 'final');
  assert.equal(out.type === 'final' && out.output, 'found it');
  assert.ok(out.type === 'final' && JSON.stringify(out.history).includes('x=1'));
  assert.equal(sent.filter((m) => m.type === 'turn').length, 2);
});

test('ask_user ends the run waiting for the person', async () => {
  const sent = await drive([{ tool_calls: [{ name: 'ask_user', arguments: '{"question":"Which room?"}' }] }]);
  const out = outcome(sent);
  assert.equal(out.type, 'waiting_for_user');
  assert.equal(out.type === 'waiting_for_user' && out.question, 'Which room?');
  assert.ok(!sent.some((m) => m.type === 'tool_call'), 'control tools never reach keystone');
});

test('report_blocked ends the run blocked', async () => {
  const sent = await drive([{ tool_calls: [{ name: 'report_blocked', arguments: '{"reason":"no access"}' }] }]);
  const out = outcome(sent);
  assert.equal(out.type, 'blocked');
  assert.equal(out.type === 'blocked' && out.reason, 'no access');
});

test('malformed arguments are reported to keystone, and the model can recover', async () => {
  const sent = await drive([{ tool_calls: [{ name: 'lookup', arguments: 'not json' }] }, { text: 'done' }]);
  const bad = sent.find((m) => m.type === 'invalid_action');
  assert.equal(bad?.type === 'invalid_action' && bad.reason, 'arguments are not valid JSON');
  assert.ok(!sent.some((m) => m.type === 'tool_call'), 'a malformed call is never dispatched');
  assert.equal(outcome(sent).type, 'final');
});

test('an unknown tool is reported and ends the run as a model behavior error', async () => {
  const sent = await drive([{ tool_calls: [{ name: 'format_disk', arguments: '{}' }] }]);
  assert.ok(sent.some((m) => m.type === 'invalid_action' && m.reason === 'unknown tool'));
  const out = outcome(sent);
  assert.equal(out.type === 'error' && out.kind, 'model_behavior');
});

test('a model that never stops calling tools hits the turn budget', async () => {
  const sent = await drive([{ tool_calls: [{ name: 'lookup', arguments: '{"q":"again"}' }] }], { max_turns: 3 });
  const out = outcome(sent);
  assert.equal(out.type === 'error' && out.kind, 'max_turns');
});

test('a model error is reported, not thrown', async () => {
  const sent = await drive([{ throw: 'provider down' }]);
  const out = outcome(sent);
  assert.equal(out.type, 'error');
  assert.equal(out.type === 'error' && out.kind, 'model');
  assert.match(out.type === 'error' ? out.message : '', /provider down/);
});

test('cancellation stops a waiting run', async () => {
  const abort = new AbortController();
  setTimeout(() => abort.abort(), 50);
  const sent = await drive([{ sleep_ms: 10_000, text: 'too late' }], {}, undefined, abort);
  const out = outcome(sent);
  assert.equal(out.type === 'error' && out.kind, 'cancelled');
});

test('a resumed run continues from its history', async () => {
  const first = await drive([{ tool_calls: [{ name: 'ask_user', arguments: '{"question":"Which room?"}' }] }]);
  const waiting = outcome(first);
  assert.equal(waiting.type, 'waiting_for_user');
  const history = waiting.type === 'waiting_for_user' ? waiting.history : [];
  const second = await drive([{ text: 'Lights off in the kitchen.' }], { history, input: 'the kitchen' });
  const out = outcome(second);
  assert.equal(out.type, 'final');
  const h = JSON.stringify(out.type === 'final' ? out.history : []);
  assert.ok(h.includes('Which room?') && h.includes('the kitchen'), 'history carries the question and the answer');
});

test('every model attempt waits for keystone and reports how it ended', async () => {
  const sent: Outbound[] = [];
  const abort = new AbortController();
  let gate!: ModelGate;
  const send = (m: Outbound) => sent.push(m);
  gate = new ModelGate(send, abort.signal);
  const bridge = new ToolBridge(send, abort.signal);
  const done = runJob(startMsg(), new ScriptedModel([{ text: 'hi' }]), send, bridge, gate, abort.signal, 1);
  await new Promise((r) => setTimeout(r, 50));
  assert.deepEqual(sent.map((m) => m.type), ['turn'], 'no model call before proceed');
  gate.proceed();
  await done;
  assert.deepEqual(sent.map((m) => m.type), ['turn', 'model_done', 'final']);
});

test('a failed model call is retried once, and the retry is gated too', async () => {
  const sent = await drive([{ throw: 'blip' }, { text: 'recovered' }]);
  const turns = sent.filter((m) => m.type === 'turn');
  assert.deepEqual(turns.map((m) => m.type === 'turn' && [m.turn, m.attempt]), [[1, 1], [1, 2]]);
  const dones = sent.filter((m) => m.type === 'model_done');
  assert.deepEqual(dones.map((m) => m.type === 'model_done' && Boolean(m.is_error)), [true, false]);
  assert.equal(outcome(sent).type, 'final');
});

test('earlier conversation reaches the model as messages', async () => {
  const sent = await drive([{ text: 'Yes, the kitchen.' }], {
    conversation: [
      { role: 'user', text: 'turn on the lights' },
      { role: 'assistant', text: 'Which room?' },
    ],
    input: 'kitchen',
  });
  const out = outcome(sent);
  const h = JSON.stringify(out.type === 'final' ? out.history : []);
  assert.ok(h.indexOf('turn on the lights') < h.indexOf('Which room?') && h.indexOf('Which room?') < h.indexOf('kitchen'));
});
