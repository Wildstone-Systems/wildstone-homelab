# Native agent runtime (sidecar)

This is the reasoning loop of Wildstone's native agent. It runs on the
official [OpenAI Agents SDK](https://github.com/openai/openai-agents-js), and
Keystone starts it once per run. See ADR-W039 for the design.

- **What it owns:** the model → tool → observation loop.
- **What it does not own:** tools, authorization, audit, timeouts, budgets
  and cancellation. Keystone does all of that
  (`keystone/agentruntime/native`). Every tool call the model makes is sent
  back to Keystone over stdout, and the result comes back over stdin.

## Build and test

```sh
npm ci
npm test         # typecheck + sidecar tests
npm run build    # → dist/src/main.js
```

End-to-end with Keystone, using the real sidecar and a scripted model:

```sh
WILDSTONE_NATIVE_AGENT_SIDECAR=$PWD/dist/src/main.js \
  go -C ../../keystone test -run '^TestE2E' ./agentruntime/native
```

## Release package

```sh
npm ci && npm run bundle                       # → dist/bundle/main.mjs
../../scripts/build-native-agent.sh amd64 out  # → out/native-agent_linux_amd64.tar.gz
```

The archive holds a pinned, checksum-verified Node (`bin/node`), the bundle
(`main.mjs`) and the licenses. `install.sh` puts it in
`/usr/lib/wildstone/native-agent`. To bump Node, change the version and both
hashes at the top of `scripts/build-native-agent.sh`.

## Run

```sh
node dist/src/main.js --health   # prints a ready line, calls no model
node dist/src/main.js            # one job over stdin/stdout (Keystone does this)
```

Environment (set by Keystone; the sidecar does not inherit Keystone's own):

- `WILDSTONE_AGENT_API_KEY`: the model API key. It can be empty for a local
  server such as Ollama.
- `WILDSTONE_AGENT_ALLOW_SCRIPTED_MODEL=1`: **tests only.** Enables the
  scripted model.

## Rules

- **stdout is the protocol.** Never `console.log` to it. `main.ts` redirects
  `console.*` to stderr.
- **Change `src/protocol.ts` and `keystone/agentruntime/native/protocol.go`
  together**, and bump the protocol version on both sides.
- **Dependencies are pinned exactly.** A bump is a reviewed change. Run the
  end-to-end tests before merging it.
