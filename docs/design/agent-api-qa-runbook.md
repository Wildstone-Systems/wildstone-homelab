# Agent API and Hermes runtime: a QA runbook for this branch

This is a plain-language walkthrough for testing branch
`claude/issues-333-334-orchestration-dix2u1` before it merges. Every
command is meant to be copy-pasted one at a time, reading the
"Expected" line after each before moving on.

## 1. What this branch changes

Today, a Wildstone Agent (the Console still calls it a "Butler") always
thinks inside Keystone itself — the same process, no way to swap it
out. This branch adds a second path: Keystone now exposes a small,
well-defined API (`/api/v1/agent/...`) that a *runtime* — the thing
that actually does the reasoning — talks to instead of touching
Keystone's internals directly. The built-in ("native") Agent was moved
onto that same API internally, so nothing about it should look or
behave differently. On top of that path, this branch adds the pieces
needed to attach an external runtime — specifically Hermes, a
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

## 2. Before you start

**Branch and checkout.**

```sh
git fetch origin claude/issues-333-334-orchestration-dix2u1
git checkout claude/issues-333-334-orchestration-dix2u1
git log --oneline -1
```
Expected: the last line names a commit whose message starts with
`feat(packages): agent-hermes capability package`.

**Tool versions.** CI builds this branch with Go 1.25 and Node 22.
Check yours match closely enough:

```sh
go version
node --version
```
Expected: `go1.25.x` and `v22.x`. A different patch version is fine; a
different major version is not — install the right one before
continuing.

**Never use your real household database for this.** This branch adds
seven new database migrations (`0081` through `0087`, listed in
`keystone/runtimedb/migrations/`). The moment any `keystone` binary
built from this branch opens a data directory, those migrations run
automatically and only go forward — there is no "undo migration"
command. Do every check in this runbook against a **scratch**
Keystone: either a spare machine/VM installed fresh from
[`docs/install.md`](../install.md) (the ordinary path, just not your
real house), or the same host with a throwaway data directory and
different ports, shown in step 4 below. This mirrors the project's own
rule for testing risky changes: "no scratch test mutates the household
database" (`docs/design/slice-24-audit-rollout-plan.md`).

If you ever do end up pointing a build of this branch at your **real**
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
run) or only pre-existing warnings unrelated to this branch.

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
view) looks exactly like it did before this branch — the runtime
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

### h. Processing class

1. Set an Agent's runtime `processingClass` to `cloud`, ask it to
   recall something you know is stored `local_only`.
2. Set it to `local_only`, ask again.

Expected: withheld in case 1, returned in case 2 — same refusal
wording either way memory is normally withheld by privacy rules.

### i. Hermes package (best effort — needs Docker, and network access to pull `nousresearch/hermes-agent`)

1. From the Console, install `agent-hermes` bound to an Agent, profile
   `platform-only`, pointed at a local model endpoint you already run.
2. Approve the resulting Plan.
3. Confirm the bridge's health shows `connected`, and send it a chat
   turn.
4. Watch the bridge's logs for the MCP calls Hermes makes back through
   it, and confirm `disabled_toolsets` appears in its rendered config.
5. Change the profile to `sandboxed` and check whether a re-render
   Plan for the package appears.

Known open points, honestly: the pinned Hermes image digest is still a
placeholder pending real digest-pinning tooling; the `/v1/runs` body
shape is only pinned by a unit test, never verified against a live
Hermes (design-doc spike S3 was never run); the bridge's
`identity_header` is static, leaning on `max_concurrent_runs: 1`
instead of per-call identity; there is no egress allowlist yet; and an
external runtime's escalation deliveries are acknowledged as a no-op
rather than answered. Step 5 will likely show **nothing** re-renders —
that's H6 (issue #352), not yet built, not a regression.

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
  `keystone/cmd/keystone/main.go` is hardcoded `true` on this branch.
  Setting it `false` and rebuilding routes the native Agent back
  through the pre-branch closure wiring, kept one release as a
  fallback — a side-by-side comparison, not a runtime flag.
- **Which test maps to which area**: `keystone/agentapi` (contract
  types), `keystone/agentgateway` (the gateway, incl.
  `disclosure_test.go` for processing class), `keystone/api`'s
  `handlers_agent_test.go` (HTTP transport, in-process/HTTP identical
  checks), `keystone/automation` (event matching), `keystone/domestic`
  and `keystone/butler/tools_hostops_test.go` (host tools),
  `cli/cmd/agent-bridge` (the bridge binary).

## 6. Known limitations and follow-ups

- H6 — execution profile change does not yet re-render the bound
  Hermes package automatically (#352).
- H8 — a split SSH-backed sandbox tier is not built (#354).
- H9 — the Console has no Hermes runtime inventory view yet (#355).
- H10 — the conformance suite has never been run against a real,
  packaged Hermes (#356).
- No egress allowlist on a Hermes container yet — it can reach any
  network address the node itself can.
- `host.upgrade_package` and `host.snapshot_dir` were deliberately left
  out of the H7 host tool family (#353) — mapping either to a
  human-approved Plan needs a Person→principal identity and a resource
  kind that don't exist yet.
- Deliveries are not filtered by processing class the way manifests,
  invokes and recalls are (`keystone/agentgateway/disclosure.go`'s own
  doc comment explains the gap) — worth a follow-up issue if an Agent
  ever gets a subscription that could carry local-only content.
- The broader phase tracking issues are #335–#345 (Phases 0–5) and
  #346–#356 (Phase 6, H1–H10); anything not explicitly called out above
  as landed should be assumed not done.
