# ADR-W007: Keystone core — public API, principals, runtime DB, Statefile ingestion

- **Status:** accepted; amended by ADR-W033; A4 amended (migration baseline, #231); A4/A6 amended (accepted revision and shared publication, #157)
- **Relates to:** architecture proposal §3/§6/§9.2, implementation scope E2
- **Governed by:** ADR-W033 where they overlap.
- **Audience:** decision record AND implementation spec for E2, written so
  implementation subagents can execute the work packages in Part C without
  the planning conversation. Parts A–B are binding; if a work package
  contradicts them, stop and report.

## Constraints from ADR-W030

- The closed role table adds `integration-adapter`, limited to
  `claim-integration-work` and `report-integration-result` verbs bound to its
  integration connections (an app adapter such as CalDAV, not an ADR-W033
  Connection). It receives no general read, act, propose, approve, or manage
  authority. Typed integration work uses its own durable queue and
  claim/result endpoints behind the same public API; it does not reuse chat
  tables or the chat inbound path.
- Plans are approved only by a human Owner, at the exact version, through the
  Owner Review Queue; no service role approves on anyone's behalf
  (ADR-W033 §E2, §E3).
- The runtime database is authoritative for task/step attempts, conversation
  turns, waiting reasons, immediate denials, identity-link projections, and
  delivery state. Task authority is derived from the request, not a stored
  grant (ADR-W033 §A0). Schema migrations preserve correlation to the
  household timeline without making runtime projections a second authority
  for deterministic outcomes. All Console, CLI, chat, and adapter behavior
  continues through the public API.
- V1 retains one active Keystone writer and task scheduler. A standby is a
  recovery target, not an active peer; promotion is explicit and fences the
  old coordinator before dispatch. The runtime DB adds append-only structured
  work trace events correlated to task, turn, tool attempt, policy/approval
  decision, and delivery. Trace access follows the underlying task/source
  permissions and the public API; there is no separate observability
  backdoor.

---

# Part A — Decisions (binding)

### A1. Two planes, two transports

Keystone serves two distinct planes and they never share a listener:

- **Agent plane** (exists, ADR-W003): the gRPC/mTLS gateway Wardstone agents
  dial out to. Unchanged by E2.
- **Public plane** (new): ONE API consumed by Console, `wild`, and the
  Butler — ground rule 3, no private UI-only or AI-only endpoints. HTTP +
  JSON over TLS (or a loopback/tailnet listener at MVP), stdlib `net/http`.

Why HTTP+JSON and not gRPC for the public plane: the Console is
TypeScript/React (grpc-web adds a proxy layer and a heavy toolchain for zero
MVP benefit), `curl` against the API is the escape hatch the design brief
promises, and the resource schema is already YAML/JSON-shaped Go structs
(ADR-W004) — protobuf would be a second, parallel schema for the same
objects. The agent plane keeps protobuf: that contract is
security-load-bearing and version-negotiated. The public API is versioned by
URL prefix (`/api/v1/`).

### A2. Principals, roles, acting identity

A **principal** is who calls the public API. Fields: `name`, `role`,
`disabled`, `actingFor` (see below). Three roles at MVP, checked
server-side per route (a closed table in code, mirroring the
closed-registry philosophy).

These are **API principal roles** — what a credential may call — not human
roles. A human is an Owner or a Member and nothing else (ADR-W033 §A1); a
principal role never makes anyone an Owner.

| Role | May |
|---|---|
| `admin` | everything, including principal/token management and plan approval. Held by an Owner's Console session and by host credentials. |
| `operator` | read everything; propose plans; the `approve` verb for machine operations. A machine credential: it never approves or rejects a Plan. |
| `butler` | read (redacted) everything; propose plans; NEVER approve |

Every Plan is approved only by a human Owner, at its exact version (ADR-W033
§E2): an `admin` credential, which is an Owner's Console session or the host
credential the installer and `wild plan approve` use. A routine Plan further
requires a fresh Owner Console session.

The Butler holding a first-class principal — not a special code path — is
what makes §6's "authority is not in the model" auditable: its writes are
plan proposals under the same RBAC table as everyone else's, and the S4
red-team invariant (butler package never imports the apply path) stays
grep-assertable.

**Acting identity (ADR-W001 foundation, schema-level only at MVP):** every
principal, session, and token record carries an `actingFor` string ("" =
acting as self). E2 stores and propagates it into audit records; no
behavior branches on it until v2 concierge mode.

### A3. Tokens and sessions

Opaque bearer tokens only. A token is 32 random bytes, presented as
`wst_<hex>`; the runtime DB stores `sha256(token)`, never the token —
minting is the only moment the secret exists in a response, and a DB read
can never leak a usable credential. Token records: hash, principal,
optional expiry, optional `actingFor` override, created/last-used/revoked
timestamps. A **session** is just a short-TTL token (Console login flow
lands with E10; the API-side primitive is identical). No JWTs: nothing to
mis-verify, revocation is a DB row, and the parse surface on the
unauthenticated path stays near zero.

Bootstrap: first start with an empty principals table mints an `admin`
API principal + bearer token and prints the token once to stdout (exactly
the join-token pattern of ADR-W003). It is a host/installer credential, not
a Person. The first human Owner is created by the installer, and lost web
access is recovered from the host CLI (`keystone owner create`,
`keystone owner reset-password`) (ADR-W033 §A3, §A7). Everything after that
goes through the API.

### A4. Runtime DB

SQLite via `modernc.org/sqlite` (pure Go — keeps the no-cgo build and the
darwin/arm64 cross-compile CI check). One database file
`<DataDir>/keystone.db`, WAL mode, `database/sql`.

Migrations: embedded `.sql` files (`embed.FS`), applied in filename order
inside a transaction each, tracked in `schema_migrations`. Forward-only; a
bad migration is fixed by a new migration.

**Baseline (2026-10, #231).** Migrations 0001 through 0104 (112 files) are
squashed into `migrations/0000_baseline.sql`: the schema and seed rows that
chain produced, verified identical to it (schema, rows, `integrity_check`,
`foreign_key_check`) when the baseline was cut. Replaying the chain into
every new database had become most of the test suite's cost, and the
intermediate states it walked through exist only on databases that have
already finished them. On open (`runtimedb/baseline.go`):

- a new database runs the baseline, then any later migration;
- a database that recorded every squashed migration adopts the baseline:
  its name is recorded as applied and the file never runs, so nothing in
  the schema or rows changes;
- a database that recorded only some of them is refused, naming the first
  missing one. It is opened once with a build from before the baseline,
  which finishes the chain, and then upgraded.

The baseline is a migration like any other: never edited, and a schema
change after it is a new migration. Forward-only still holds for everything
after it. A later re-baseline follows the same three rules.

E2 tables (owned by `keystone/runtimedb`):

- `principals`, `tokens` — A2/A3.
- `events` — the append-only operational event stream (feeds Console live
  view, ntfy, Butler — E3 consumes, E2 defines): seq, time, type, node,
  resource kind/name, message, JSON attrs.
- `plans` — plan records: id, created, principal, acting_for, summary,
  risk, state (proposed → approved → applying → applied | failed |
  rejected | superseded), full plan JSON, approval principal + time.
- `audit` — one row per public-API request that passed authn: time,
  principal, acting_for, method, path, status, remote addr. (The agent
  plane keeps its own dual audit trail, ADR-W003 W7; they answer different
  questions.)
- `statefile_head` — single row: the last Statefile commit hash Keystone
  itself wrote or accepted (A6).
- `observed` — latest observed status per resource (kind, name, node,
  healthy, detail, updated) — written by the reconciler (E3 wires it; E2
  owns the schema so the API can serve it).

Plan IDs become `plan-<runtimedb seq>` (retiring the S2 in-memory
counter), satisfying ADR-W004 A3's "DB seq later".

### A5. Middleware chain and redaction

Every public-plane request passes, in order: **authn** (bearer token →
principal) → **RBAC** (role × route table) → **rate limit** (token-bucket
per principal; unauthenticated requests share one strict bucket) →
**handler** → **audit append**. Denials are audited too.

**Redaction is a serialization property, not handler discipline.** The only
JSON encoder on the public plane lives in the API package and applies
redaction to every response body; handlers cannot opt out. S4's redaction
logic moves from `keystone/butler` into a shared `keystone/redact` package;
the Butler keeps calling it (its LLM-facing envelope needs redaction
independent of transport). Property-style tests fuzz resources/events/plans
with planted secret-shaped values and assert none survive any API
serialization path. What redacts: secret *values* (env values matching the
secret-shape heuristics, anything under a `secret/` ref). What must NOT
redact: `secret/<name>` references themselves — they are the plan/UX
vocabulary (ADR-W004).

### A6. Statefile: structured commits and out-of-band ingestion

The Statefile stays a real, user-editable git repo — that escape hatch is
product surface, so Keystone must be a well-behaved co-editor rather than
an owner:

- Every Keystone commit keeps the ADR-W004 structured trailer
  (`Who:`/`Plan:`/`Why:`); `Who` becomes `principal(actingFor)` when acting
  identity is set.
- Keystone records its own HEAD in `statefile_head` after every commit it
  makes or accepts.
- **Ingestion loop (E2 lands detect + validate + decide; E3's reconciler
  executes the resulting plan):** on each tick, if the repo HEAD ≠
  `statefile_head`, the delta is an out-of-band edit. Load + validate the
  tree. **Valid** → record the diff as an **auto-approved** plan (who =
  `system/oob`, summary = the foreign commit's subject + hash), emit an
  event, advance `statefile_head`. Auto-approved, not proposed — the edit
  is already the committed desired state, made by someone holding repo
  access (an authority senior to any API token); holding it "proposed"
  would leave HEAD ≠ marker and wedge every later tick, while the
  approved plan record gives the change the same audit and reconcile
  surface as an API change. A tree-neutral commit (docs, whitespace)
  advances the marker with an event and no plan. **Invalid** (parse,
  schema, or policy failure) → a new commit restoring the last accepted
  tree (restore-and-commit rather than `git revert`, so a run of foreign
  commits can never revert-conflict), with a structured trailer naming
  the failure; emit an event; advance `statefile_head` to the revert
  commit. First contact with a repo that has history but no marker
  validates and adopts the tree as baseline. The revert upgrade from
  S2's "detect + refuse + log" is deliberate: an invalid tree left in
  place would wedge every later reconcile; revert is itself a commit, so
  the human's work stays recoverable from git history.

### A7. Resource schema codegen

The Go structs in `keystone/resource` (ADR-W004) stay the single source of
truth. A small generator (`keystone/resource/gen`, `go generate`) walks the
kind registry via reflection and emits TypeScript definitions
(`console/src/generated/resources.ts`, committed like `proto/gen`) so the
Console cannot drift from the schema silently. CI regenerates and fails on
diff. No protoc, no OpenAPI toolchain at MVP; the generator is ~200 lines
we own.

### A8. Amendment (2026-10-24, #157): the accepted revision

Amends A4 (`statefile_head`) and A6. Adopts the
[desired-state publication and bounded-approval contract](../design/desired-state-publication-and-approval-contract.md),
with the maintainer's answers to its §8:

1. **The runtime DB selects the accepted revision.** `statefile_head` is
   the accepted revision: the one immutable commit that is desired state.
   The Statefile branch and working files are a human-editable mirror,
   never authority. An uncommitted edit, or a commit the ingestion loop
   has not yet checked, reaches neither a node nor a proposal.
2. **Out-of-band edits keep auto-approval** (A6), but only after the same
   schema and standing-policy checks as an API change, run against the
   **committed** tree, including on first adoption.
3. **An approved `update` does not cover a recomputed `create` or
   `delete`**, even when the final spec is the same.
4. **Each approved Plan gets its own outcome** (amends ADR-W008 A2).

Delivered in three parts, so each can be reviewed alone:

- **0a, immutable reads (implemented).** `statefile.Store.Load` and
  `Snapshot` read the accepted revision when an accepted-revision source
  is wired (production); ingestion validates `LoadAt(head)`, never the
  working files, and applies policy on first adoption; API approval and
  the household tracker writer wait (HTTP 409, the Plan stays proposed)
  while the branch carries an unaccepted commit or the working tree has
  uncommitted edits, since their `git add -A` commit would otherwise take
  those edits with it.
- **0c, bounded approval (implemented).** A Plan's reviewed steps are
  its approval entries (kind, name, action, digest); the reconciler runs
  a Medium/High step only when one matches the accepted content, and
  each Plan gets its own outcome. Rules 3 and 4; details in ADR-W008
  A2a, including why risk is recorded but not compared.
- **0b, one publisher (pending).** API approval, the tracker writer and
  ingestion share one `DesiredStatePublisher` with a durable journal and
  internal Git refs, so a crash between the Git and SQLite steps recovers
  to either the old or the new accepted revision.

Until 0b, the Git commit and the `statefile_head` update are still two
steps; the contract's §3 recovery table is not yet implemented.

# Part B — Invariants (binding)

1. **No unmediated write path** (ground rule 4): the public API's only
   mutating endpoints are plan proposal, plan approval, and
   principal/token management. There is no "write a resource" endpoint;
   even admin edits become proposed plans.
2. **One public API** (ground rule 3): Console, CLI, and Butler
   authenticate as principals against the same routes. CI greps that the
   butler package imports the API client, not gateway/reconcile internals.
3. **Secrets never serialize** (A5): enforced at the encoder, proven by
   property tests. Token secrets exist in exactly one response ever (mint).
4. **Statefile holds specs only** (ADR-W004): nothing in E2 writes status,
   plans, events, or audit into the Statefile repo; they live in the
   runtime DB.
5. **Audit is not optional**: any authenticated request that reaches RBAC
   produces an audit row, allowed or denied, before the response is final.

# Part C — Work packages (E2 execution plan)

- **K1 — `keystone/runtimedb`:** SQLite store, migrations framework, typed
  accessors for every A4 table, WAL + busy-timeout, `go test -race` clean.
- **K2 — `keystone/principal`:** principal/token model over runtimedb,
  mint/verify/revoke, role table, acting-identity plumbing, bootstrap
  admin path.
- **K3 — `keystone/api`:** public HTTP service: router, middleware chain
  (authn → RBAC → rate limit → audit), redacting encoder,
  `keystone/redact` extraction, endpoints: `GET /api/v1/whoami`,
  resources (list/get from Statefile), observed status, events (list +
  long-poll), plans (list/get/propose/approve/reject), principals/tokens
  (admin), `GET /api/v1/healthz` (unauthenticated, static). Property
  tests for redaction; table tests for RBAC.
- **K4 — Statefile ingestion:** `statefile.Head()`, foreign-commit
  detection, validate/decide/revert per A6, events + plan records via
  runtimedb; tests drive a real second git client editing the repo.
- **K5 — Codegen:** the A7 TS generator + CI drift check.
- **K6 — Wiring:** `cmd/keystone` serves both planes from one DataDir;
  an integration test boots gateway + API + statefile + runtimedb
  together (S1-style real-server test).

Sequencing: K1 → K2 → K3 strictly serial; K4 and K5 parallel after K1;
K6 last. When E2 completes, trim this Part C to an "implemented by" note
(the ADR-W003 precedent).
