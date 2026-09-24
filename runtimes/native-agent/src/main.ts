// Entry point: `node dist/src/main.js` runs one job over stdin/stdout;
// `node dist/src/main.js --health` loads the SDK, prints a ready message
// and exits without calling a model.

import { createRequire } from 'node:module';
import { createInterface } from 'node:readline';

import type { Model } from '@openai/agents-core';
import { OpenAIChatCompletionsModel, OpenAIResponsesModel } from '@openai/agents-openai';
import OpenAI from 'openai';

import { PROTOCOL_VERSION, type Inbound, type ModelSpec, type Outbound, type StartMessage } from './protocol.js';
import { ScriptedModel, type ScriptStep } from './scripted.js';
import { ToolBridge, runJob } from './sidecar.js';

// stdout is the protocol. Anything a library logs goes to stderr, where
// Keystone keeps a short tail for diagnostics.
for (const level of ['log', 'info', 'warn', 'debug'] as const) {
  console[level] = (...args: unknown[]) => console.error(...args);
}

const API_KEY_ENV = 'WILDSTONE_AGENT_API_KEY';

// Stamped in by scripts/bundle.mjs: a bundled sidecar has no
// node_modules to read the SDK's version from.
declare const __AGENTS_SDK_VERSION__: string | undefined;

function engineVersion(): string {
  if (typeof __AGENTS_SDK_VERSION__ === 'string') return __AGENTS_SDK_VERSION__;
  try {
    const require = createRequire(import.meta.url);
    const pkgPath = require.resolve('@openai/agents-core').replace(/dist[\\/].*$/, 'package.json');
    return (require(pkgPath) as { version?: string }).version ?? 'unknown';
  } catch {
    return 'unknown';
  }
}

const send = (m: Outbound): void => {
  process.stdout.write(JSON.stringify(m) + '\n');
};

function ready(): Outbound {
  return { type: 'ready', protocol: PROTOCOL_VERSION, engine: 'openai-agents-sdk', engine_version: engineVersion() };
}

function buildModel(spec: ModelSpec, env: NodeJS.ProcessEnv): Model {
  if (spec.provider === 'scripted') {
    if (env.WILDSTONE_AGENT_ALLOW_SCRIPTED_MODEL !== '1') throw new Error('the scripted model is only available in tests');
    return new ScriptedModel(spec.script as ScriptStep[]);
  }
  if (spec.provider !== 'openai') throw new Error(`unknown model provider ${String(spec.provider)}`);
  if (!spec.name) throw new Error('model name is required');
  const client = new OpenAI({
    // Local OpenAI-compatible servers (Ollama) need no key; the client
    // still insists on a value.
    apiKey: env[API_KEY_ENV] || 'unused',
    baseURL: spec.base_url || undefined,
    // Bounded: Keystone's turn timeout is the real ceiling.
    maxRetries: 2,
  });
  return spec.api === 'responses'
    ? new OpenAIResponsesModel(client, spec.name)
    : new OpenAIChatCompletionsModel(client, spec.name);
}

async function main(): Promise<number> {
  if (process.argv.includes('--health')) {
    send(ready());
    return 0;
  }

  const abort = new AbortController();
  const bridge = new ToolBridge(send, abort.signal);
  let started: Promise<void> | undefined;
  let heartbeat: NodeJS.Timeout | undefined;

  const lines = createInterface({ input: process.stdin, crlfDelay: Infinity });
  const finished = new Promise<void>((resolve) => {
    lines.on('line', (line) => {
      let m: Inbound;
      try {
        m = JSON.parse(line) as Inbound;
      } catch {
        send({ type: 'error', kind: 'protocol', message: 'unreadable message from keystone' });
        abort.abort();
        resolve();
        return;
      }
      switch (m.type) {
        case 'start': {
          if (started) return;
          if (m.protocol !== PROTOCOL_VERSION) {
            send({ type: 'error', kind: 'protocol', message: `keystone speaks protocol ${m.protocol}, sidecar speaks ${PROTOCOL_VERSION}` });
            resolve();
            return;
          }
          send(ready());
          heartbeat = setInterval(() => send({ type: 'heartbeat' }), Math.max(250, m.heartbeat_ms || 5000));
          started = start(m, bridge, abort.signal).finally(resolve);
          return;
        }
        case 'tool_result':
          bridge.resolve(m);
          return;
        case 'cancel':
          abort.abort();
          return;
      }
    });
    // Keystone went away: nothing is listening for an outcome.
    lines.on('close', () => {
      abort.abort();
      if (!started) resolve();
    });
  });

  await finished;
  if (heartbeat) clearInterval(heartbeat);
  return 0;
}

async function start(m: StartMessage, bridge: ToolBridge, signal: AbortSignal): Promise<void> {
  let model: Model;
  try {
    model = buildModel(m.model, process.env);
  } catch (err) {
    send({ type: 'error', kind: 'internal', message: err instanceof Error ? err.message : String(err) });
    return;
  }
  await runJob(m, model, send, bridge, signal);
}

main().then(
  (code) => {
    // Exit only once the outcome has been flushed to Keystone.
    process.stdout.write('', () => process.exit(code));
  },
  (err) => {
    console.error(err);
    process.exit(1);
  },
);
