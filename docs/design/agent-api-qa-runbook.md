# Agent API and Hermes runtime: a QA runbook

This is a plain-language walkthrough for testing the Agent API and the
Hermes runtime on `main`. Every command is meant to be copy-pasted one
at a time, reading the "Expected" line after each before moving on.

Runtime type `native` = Keystone-hosted; its loop is chosen per
deployment by `-agent-runtime legacy|native`: `legacy` is
`keystone/agentruntime/legacy.Runner`, `native` is the OpenAI Agents
SDK sidecar (ADR-W041). Runtime types `hermes` and `openclaw` are
external Agent API clients. Below, a "native Agent" means an Agent
whose runtime type is `native`, whichever loop drives it.

## 1. What this covers

Keystone exposes a small, well-defined API (`/api/v1/agent/...`) that
a *runtime* — the thing that actually does the reasoning — talks to
instead of touching Keystone's internals directly. The Keystone-hosted
(runtime type `native`) Agent uses that same API internally, so nothing
about it should look or behave differently. On top of that path sit the
pieces needed to attach an external runtime — specifically Hermes, a
third-party agent framework — as an alternative brain for an Agent,
running in its own container with its own credential and its own
ceiling on what it may do.

| Phase | What you can now do | Where to look |
|---|---|---|
| Foundation (Phases 0–2) | Nothing new to *do* — the native Agent's reasoning loop now runs through the same internal API an external runtime would use. It should be invisible. | `keystone/agentapi`, `keystone/agentgateway` |
| Runtime identity (Phase 1) | Every Agent has its own runtime credential you can view, rotate, or revoke. | Console → an Agent's page → the **Runtime** card |
| Talk to an Agent from outside Keystone (Phase 2c / ADR-W040) | A separate program, on a different host or container, can call an Agent's capabilities over plain HTTP with a bearer token. | `POST /api/v1/agent/manifest`, `/invoke`, etc. |
| Memory, proposals, work and events (Phases 3–4) | An external runtime can recall household memory, file a Plan for a human to approve, receive notice of household events it subscribed to, and report progress on long jobs — even while offline. | Plans screen, `agent_deliveries`, `-agent-heartbeat-ttl` |
| Execution profile & processing class (Phase 5) | Per Agent, choose whether its runtime gets a sandboxed workspace or host-administration tools, and whether it may see phone/local-only memories. | Console → Agent → **Runtime** tab |
| Hermes package (Phase 6: H1, H2, H4, H5, H7) | Install Hermes, a real third-party agent framework, as an Agent's runtime — a Console package, one container pair per Agent. Host-administration tools exist as an opt-in, Owner-approved tool family. | `packages/agent-hermes`, `cli/cmd/agent-bridge`, `keystone/domestic/hostops.go` |
| Re-render on profile change (Phase 6: H6) | Changing a packaged runtime's execution profile or processing class no longer applies in place: it re-renders the bound `agent-hermes#<instance>` capability and files exactly one reviewed Plan, applying nothing until an Owner approves it. | `keystone/api/handlers_butler_runtime.go`'s `reRenderPackagedRuntime` |
| Runtime inventory (Phase 6: H9) | An Owner can see every Agent's runtime — type, health, heartbeat, execution profile, processing class — in one place on the Butlers screen, unhealthy first, each row opening that Agent's Runtime tab. | `GET /api/v1/runtimes`, Console → Butlers → **Runtimes** |

## 2. Before you start

**Branch and checkout.**

```sh
git fetch origin main
git checkout main
git pull --ff-only
git log --oneline --grep 'ADR-W039 Phase 6/H6' -1
```
Expected: the last line names a commit whose message starts with
`feat(api): re-render a packaged runtime as a reviewed Plan`.

**Tool versions.** CI builds `main` with Go 1.25 and Node 22.
Check yours match closely enough:

```sh
go version
node --version
```
Expected: `go1.25.x` and `v22.x`. A different patch version is fine; a
different major version is not — install the right one before
continuing.

**Never use your real household database for this.** The Agent API work adds
seven database migrations (`0081` through `0087`, listed in
`keystone/runtimedb/migrations/`). The moment any `keystone` binary
built from `main` opens a data directory, those migrations run
automatically and only go forward — there is no "undo migration"
command. Do every check in this runbook against a **scratch**
Keystone: either a spare machine/VM installed fresh from
[`docs/install.md`](../install.md) (the ordinary path, just not your
real house), or the same host with a throwaway data directory and
different ports, shown in step 4 below. This mirrors the project's own
rule for testing risky changes: "no scratch test mutates the household
database" (`docs/design/slice-24-audit-rollout-plan.md`).

If you ever do end up pointing a build of `main` at your **real**
`/var/lib/wildstone/keystone` — you should not, but just in case —
stop the service and copy the database file first.
[`docs/recovery.md`](../recovery.md) is explicit that the runtime
database is *not* one of the three things Wildstone's recovery
contract keeps safe off-host; it's expected to be rebuildable, which is
exactly why nothing else backs it up for you:

```sh
sudo systemctl stop keystone
sudo cp -a /var/lib/wildstone/keystone/keystone.db /var/lib/wildstone/keystone/keystone.db.qa-backup
sudo systemctl start keystone
```

## 3. Automated checks (run these first)

These are the exact commands CI runs (`.github/workflows/ci.yml`),
module by module, from the repository root.

```sh
gofmt -l ./proto ./wardstone ./keystone ./cli ./testing/e2e
```
Expected: **no output at all**. Any file name printed means it needs
`gofmt -w` run on it.

```sh
for m in proto wardstone keystone cli testing/e2e; do (cd "$m" && go vet ./...); done
```
Expected: no output, exit code 0 for each module.

```sh
for m in proto wardstone keystone cli testing/e2e; do (cd "$m" && go test -race -timeout 30m ./...); done
```
Expected: `ok` lines for every package, no `FAIL`. **Give this a full
half hour and don't panic if it looks slow** — `keystone/runtimedb`
alone takes roughly 12 minutes under the race detector, which is why
the timeout is 30 minutes and not the usual 10. If a run ever hits
that ceiling, that's a real hang worth reporting, not "the machine was
just busy."

```sh
cd console
npm ci
npm run build
npm run render-check
npm run contrast-check
cd ..
```
Expected: `npm run build` ends with a `tsc` pass and a Vite build
summary; `render-check` and `contrast-check` each print a pass line
with no failures listed.

**Codegen drift** (a generated file must match what the code that
produced it would output today):

```sh
(cd keystone && go run ./resource/gen -out ../console/src/generated/resources.ts)
git diff --exit-code console/src/generated/
```
Expected: no diff, exit code 0. Any diff means the committed generated
file is stale — a real problem, not a QA artifact of your machine.

**Shell scripts:**

```sh
for f in scripts/*.sh; do bash -n "$f"; done
docker run --rm -v "$PWD:/mnt" koalaman/shellcheck:v0.9.0 scripts/*.sh
```
Expected: no output from `bash -n`; shellcheck prints nothing (a clean
run) or only pre-existing warnings unrelated to the Agent API work.

If every one of the above is clean, move on to manual QA. If something
here fails, don't attempt the manual scenarios — fix or report the
automated failure first, since it will make the manual results hard to
trust.

## 4. Manual QA scenarios

**Set up a scratch Keystone first**, on a spare host via
[`docs/install.md`](../install.md), or on the same machine with its
own port and data directory:

```sh
cd keystone && go build -o /tmp/keystone-qa ./cmd/keystone && cd ..
cd console && npm run build && cd ..
sudo mkdir -p /tmp/wildstone-qa-data
/tmp/keystone-qa \
  -data-dir /tmp/wildstone-qa-data \
  -api-listen 127.0.0.1:9451 \
  -listen :9450 \
  -console-dir console/dist \
  -hostops-enabled
```
Expected: it logs a "first start" line with a one-time admin token
(`wst_...`) — copy it now, it prints only once. Open
`http://127.0.0.1:9451` and sign in with it. This instance is
completely separate from anything real: different port, different
empty database.

### a. Native Agent still works exactly as before

1. In the scratch Console, create an Agent (a Butler) the ordinary way.
2. Open a chat with it and ask it something simple ("what can you do
   for me?").
3. Open its page → **Runtime** card.

Expected: it answers normally. The Runtime card shows `type: native`,
a green **connected** pill, and `credential: runtime-<name>-1`. Every
ledger row for the conversation (visible via the Agent's task/audit
view) looks exactly like it did before the Agent API work — the runtime
principal name is new, everything else about the row is not.

### b. Rotate and revoke the runtime credential

1. On the Runtime card, click **Revoke runtime**, confirm.
2. Ask the Agent something again.
3. Click **Rotate credential**, confirm.

Expected: after revoke, the pill turns **revoked** and the Agent can
no longer act — any attempt fails, since its credential is disabled.
After rotate, a new credential (`runtime-<name>-2`) is issued and the
Agent works again on the next request.

### c. Attach an external runtime by hand, with plain `curl`

No Hermes needed for this — just the wire protocol.

1. On the Agent's **Runtime** tab, set type to **Hermes**, delivery
   **pull**, save.
2. Issue a credential and copy the bearer:
   ```sh
   curl -sS -X POST http://127.0.0.1:9451/api/v1/butlers/<name>/runtime/credential \
     -H "Authorization: Bearer <your admin token>"
   ```
   Expected: a JSON body like `{"credentialRef":"runtime-<name>-1","secret":"wst_...","expiresAt":"0001-01-01T00:00:00Z"}`.
   Save the `secret` as `BEARER`.
3. Start a chat turn with this Agent in the Console (this opens a task
   and queues a delivery). Then long-poll for it:
   ```sh
   curl -sS "http://127.0.0.1:9451/api/v1/agent/deliveries?wait=5s" \
     -H "Authorization: Bearer $BEARER"
   ```
   Expected: `{"deliveries":[{"id":"1","kind":"turn","handle":"<taskid>.<secret>",...}]}`.
   Save the `handle`.
4. Fetch its manifest:
   ```sh
   curl -sS -X POST http://127.0.0.1:9451/api/v1/agent/manifest \
     -H "Authorization: Bearer $BEARER" -H "Content-Type: application/json" \
     -d '{"handle":"<handle>"}'
   ```
   Expected: `agentClass`, a `systemPrompt`, and a `tools` array — each
   tool with a `name`, `description`, `inputSchema`, and
   `readOnlyHint`.
5. Invoke a read-only tool:
   ```sh
   curl -sS -X POST http://127.0.0.1:9451/api/v1/agent/invoke \
     -H "Authorization: Bearer $BEARER" -H "Content-Type: application/json" \
     -d '{"handle":"<handle>","tool":"get_health","args":{}}'
   ```
   Expected: `{"outcome":"ok","content":"node ... ok=true ..."}`, HTTP 200.
6. Acknowledge and heartbeat:
   ```sh
   curl -sS -X POST http://127.0.0.1:9451/api/v1/agent/deliveries/1/ack \
     -H "Authorization: Bearer $BEARER"
   curl -sS -X POST http://127.0.0.1:9451/api/v1/agent/heartbeat \
     -H "Authorization: Bearer $BEARER" -H "Content-Type: application/json" \
     -d '{"at":"2026-09-25T09:00:00Z","version":"qa-curl/0.1"}'
   ```
   Expected: both `200`, empty body.
7. **Wrong Agent's bearer:** rotate a credential for a *different*
   Agent and use that bearer with the first Agent's `handle` on
   `/invoke`. Expected: `403 Forbidden` — a valid runtime credential,
   but bound to the wrong Agent for this task's handle.
8. **Revoked bearer:** revoke the runtime, then retry any call with
   the old bearer. Expected: `401 Unauthorized`.
9. **Health transitions:** the Runtime card shows `unregistered` until
   step 4's first call, then `connected`. Stop calling entirely and
   wait 90 seconds (the default `-agent-heartbeat-ttl`) — it flips to
   `offline`. Send one more heartbeat — it flips back to `connected`.

### d. Run the conformance suite against this live scratch Keystone

Using the handle from step (c.3) and the bearer from (c.2):

```sh
export KEYSTONE_RUNTIME_BEARER="$BEARER"
export KEYSTONE_CONFORMANCE_HANDLE="<handle>"
export KEYSTONE_CONFORMANCE_READONLY_TOOL="get_health"
cd keystone
go test -tags conformance_external ./agentapi/conformance/... \
  -target-url http://127.0.0.1:9451
```
Expected: `TestExternalRuntimeIsConformant` passes. Never pass the
bearer as a flag or put it in a URL — it stays in the environment
variable, exactly as the test itself insists.

### e. Proposals

1. Invoke `propose_change` (a mutating tool) instead of a read-only
   one in step (c.5), with whatever document shape that tool expects.
2. Check the Plans screen in Console.
3. Approve it as the Owner.
4. Long-poll deliveries again.

Expected: the invoke call returns `outcome: ok` with a `proposal`
reference; a new Plan appears filed by `runtime-<name>-n` acting for
your person, not the literal Agent name; after approval, the next
`/agent/deliveries` poll includes a `kind: "proposal_outcome"` entry.

### f. Subscriptions

```sh
curl -sS -X POST http://127.0.0.1:9451/api/v1/agent/propose \
  -H "Authorization: Bearer $BEARER" -H "Content-Type: application/json" \
  -d '{"handle":"<handle>","kind":"agent-subscription","document":{
        "matchKind":"event_type",
        "match":{"eventType":"<pick a real event type your household already logs>"},
        "audience":"owner",
        "capabilities":["get_health"]
      }}'
```
Approve the resulting Plan as Owner, then cause that event to happen
for real (whatever normally produces it).

Expected: a `kind: "subscription"` delivery appears on the next poll,
carrying a fresh handle for a new, bounded task — visible in the
Agent's task list, scoped to only the capabilities you approved.

### g. Execution profiles

1. On a **Member**-class native Agent's Runtime tab, set execution
   profile to **Host administration**, save.
2. On an **Owner**-class native Agent, do the same.
3. As Owner, opt a native Agent into the `host.*` tool family
   (Assign tools screen) — this only appears because you started
   Keystone with `-hostops-enabled`.
4. Ask that Agent to check `host.container_logs`, then to run
   `host.container_restart`.

Expected: (1) is refused with an explanatory error ("host
administration needs a non-native runtime or an Owner-class Agent...");
(2) succeeds; the read-only host tool (logs) answers directly; the
mutating one (restart) waits for your confirmation in Console before
anything happens.

5. **A profile change on a packaged (non-native) runtime files a Plan
   instead of applying (H6, #352):** on an Agent already backed by an
   installed `agent-hermes` package (set this up as in scenario (i)
   steps 1–3, or install it directly per hermesParams-style params),
   change its execution profile or processing class on the Runtime tab
   and save. Watch the raw response of the `PUT
   /api/v1/butlers/<name>/runtime` call (browser devtools, or repeat it
   with `curl`).

Expected: the response's runtime record already shows the new
`executionProfileRef`/processing class, but it also carries a `plan`
field naming a freshly filed Plan (`{"...":"...","plan":"<id>"}`) —
`reRenderPackagedRuntime` re-expands the bound
`agent-hermes#<instance>` capability (the `<instance>` is this Agent's
runtime-principal stem, e.g. `runtime-health-1` → `health`) and files
it through the ordinary proposal pipeline, exactly one Plan per change.
Nothing is applied yet: the Hermes/agent-bridge containers keep running
under the OLD profile until you approve that Plan on the Plans screen,
at which point the reconciler recreates them under the new one. A
profile change on a **native** Agent (no packaged runtime installed)
returns no `plan` field at all — there is nothing for it to restart.

### h. Processing class

1. Set an Agent's runtime `processingClass` to `cloud`, ask it to
   recall something you know is stored `local_only`.
2. Set it to `local_only`, ask again.

Expected: withheld in case 1, returned in case 2 — same refusal
wording either way memory is normally withheld by privacy rules.

**Deliveries are filtered too:** the same filter (`keystone/agentgateway/disclosure.go`,
`FilterDeliveries`) is applied by both readers of `GET /api/v1/agent/deliveries`,
the in-process client (`keystone/agentgateway/work.go`) and the HTTP route
(`keystone/api/handlers_agent.go`). Extra step: with the Agent set to `cloud`,
long-poll deliveries after a proposal outcome or a subscription match that
carries `local_only` content. Expected: the delivery still arrives with its id
and can be acked, but its payload is `{"withheld": true, "reason": ...}`.
Switch the Agent to `local_only` and the payload comes through in full.


### i. Hermes package (best effort — needs Docker, and network access to pull `nousresearch/hermes-agent`)

1. From the Console, install `agent-hermes` bound to an Agent, profile
   `platform-only`, pointed at a local model endpoint you already run.
2. Approve the resulting Plan.
3. Confirm the bridge's health shows `connected`, and send it a chat
   turn. Expected: the answer is Hermes's, not the in-process model's.
   Keystone logs `butler answered … loop=external` for it (never
   `loop=native` or `loop=runner` for an Agent whose runtime is Hermes),
   and the bridge's log shows the delivery being run and acknowledged.
   A turn Hermes takes longer than two minutes over answers "still
   working on it"; the task stays open and completes when Hermes
   reports back (#364).
4. Watch the bridge's logs for the MCP calls Hermes makes back through
   it, and confirm `disabled_toolsets` appears in its rendered config.
5. Change the profile to `sandboxed` (H6, #352): from the Agent's
   Runtime tab, change execution profile (or processing class) and
   save. Check the Plans screen before touching anything else.

Expected for step 5: a new Plan appears, titled something like
`restart <agent>'s runtime (agent-hermes#<instance>): execution
profile sandboxed, processing class <class>` — filed under your own
identity, exactly like a Console-driven install or reconfigure. The
running `hermes`/`agent-bridge` containers are untouched until you
approve it: Hermes freezes a session's tool set at start, so this is
"re-render the package's config, then restart", never a live change.
Approve the Plan, confirm the containers are recreated, and re-check
the bridge's logged config for the new profile's `disabled_toolsets`.

Known open points, honestly: the Hermes image is pinned to the
`v2026.9.21` index digest (package 0.1.1; an install made from 0.1.0
recorded a placeholder digest that 404s, and must be reconfigured with
the new `hermes_image_digest`); the `/v1/runs` body
(`input`, `instructions`, `conversation_history`) and event shape
(`{"event":"run.completed","output":…}`) follow Hermes's API server
source but are pinned only by unit tests (design-doc spike S3 was never
run); an answer that arrives after the two-minute wait completes the
task but is not yet shown in the conversation; the bridge's
`identity_header` is static, leaning on `max_concurrent_runs: 1`
instead of per-call identity; there is no egress allowlist yet; and an
external runtime's escalation deliveries are acknowledged as a no-op
rather than answered.

### j. Runtime inventory (H9, #355)

1. As Owner, open the Console's **Butlers** screen with at least two
   Agents that have runtimes: one healthy, one you can make unhealthy
   (e.g. revoke its credential as in scenario (b), or stop its Hermes
   container from scenario (i)).
2. Sign in as a Member instead and open the same screen.
3. Call the endpoint directly:
   ```sh
   curl -sS http://127.0.0.1:9451/api/v1/runtimes \
     -H "Authorization: Bearer <your admin token>"
   ```

Expected: (1) a **Runtimes** card appears above the roster filters,
one row per Agent with a runtime, showing type, health, heartbeat,
execution profile and processing class; the unhealthy Agent's row
sorts first; clicking any row opens that Agent's drawer on its
**Runtime** tab. (2) as a Member, the Runtimes card does not render at
all — it is Owner-only, the same gate as the Runtime tab it links to.
(3) the raw JSON is a list of `{"butler","agentClass","runtime":{...}}`
objects, one per live Agent, in the Agent API's own runtime-record
shape.

## 5. If something fails

- **Keystone's own logs**: if run under systemd, `journalctl -u
  keystone -o cat -f`; if run by hand as in step 4, its stderr in your
  terminal.
- **`-agent-heartbeat-ttl`** (default `90s`) controls how fast a quiet
  runtime is marked offline — lower it for a faster QA cycle:
  `-agent-heartbeat-ttl 10s`.
- **`-hostops-enabled`** (default off) must be set for scenario (g)'s
  `host.*` tools to exist at all; without it they are absent from
  every manifest, not merely refused.
- **Comparing against the old path**: `UseAgentGateway` in
  `keystone/cmd/keystone/main.go` is hardcoded `true` on `main`.
  Setting it `false` and rebuilding routes the native Agent back
  through the older closure wiring, kept one release as a
  fallback — a side-by-side comparison, not a runtime flag.
- **Which test maps to which area**: `keystone/agentapi` (contract
  types), `keystone/agentgateway` (the gateway, incl.
  `disclosure_test.go` for processing class), `keystone/api`'s
  `handlers_agent_test.go` (HTTP transport, in-process/HTTP identical
  checks), `keystone/automation` (event matching), `keystone/domestic`
  and `keystone/butler/tools_hostops_test.go` (host tools),
  `cli/cmd/agent-bridge` (the bridge binary).

## 6. Known limitations and follow-ups

- H8 — a split SSH-backed sandbox tier is not built (#354).
- H10 — the conformance suite has never been run against a real,
  packaged Hermes (#356).
- No egress allowlist on a Hermes container yet — it can reach any
  network address the node itself can.
- `host.upgrade_package` and `host.snapshot_dir` were deliberately left
  out of the H7 host tool family (#353) — mapping either to a
  human-approved Plan needs a Person→principal identity and a resource
  kind that don't exist yet.
- **Delivery filtering** is covered by `TestFilterDeliveryRedactsRestrictedPayloads`
  (`keystone/agentgateway/deliveryfilter_test.go`) and the byte-identical
  in-process versus HTTP delivery test in `keystone/api/handlers_agent_test.go`.
  Run those first if scenario (h)'s delivery step misbehaves.
- The broader phase tracking issues are #335–#345 (Phases 0–5) and
  #346–#356 (Phase 6, H1–H10); anything not explicitly called out above
  as landed should be assumed not done.
