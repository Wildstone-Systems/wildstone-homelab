# Native Agent capability contract v1

- **Status:** proposed with [ADR-W047](../decisions/ADR-W047-native-agent-research-and-workspaces.md); not implemented by this document.
- **Issue:** [#473](https://github.com/chickenrat17/wildstone-homelab/issues/473), parent [#471](https://github.com/chickenrat17/wildstone-homelab/issues/471).
- **Baseline:** `0727a5d57b90216896a931dd0209c2f3c33791fc`; original reconciliation `6516fd74c2fa63b734ea5756d7be39fb36c8a20a`.

This fixes interfaces for the implementation slices. Database table names,
locking mechanisms and UI labels are implementation choices. Changing these
wire semantics requires an explicit contract revision and compatibility review.
No endpoint, tool, schema field or database record below is claimed to exist today.

## 1. Existing envelope and authority

Keep `agentapi.InvokeRequest {handle, tool, args, idempotencyKey}` and
`InvokeResult {outcome, content, reason, waitReason, proposal, servedFromLedger}`.
New tools are individually registered platform capabilities in the existing
Manifest/Invoke path, not runtime-local functions or `do_household_action`
subcommands. `ToolSpec.InputSchema` describes strict JSON objects: unknown
properties, wrong types, overlong values and unsupported options are refused.
There is no model argument for Person ID, Agent ID, host path, credentials,
provider endpoint, authority, provenance labels, budget reset or task handle.
The existing invocation envelope supplies the handle.

All tools are `OptIn` in the registry and independently assignable/narrowable.
Their class is Member unless an Owner-only source/resource further narrows it.
Workspace defaults are explicitly seeded only on new native-Agent creation
after readiness. Existing Agents, broad bundles and runtime changes do not
acquire them. Search and export always require explicit assignment.

Read tools are nonmutating and remain eligible for QA-only sessions;
mutating tools are withheld and refused at dispatch in QA-only mode.
Read-only is not privacy permission: reads still check egress, provenance,
resource grants and surface ceilings. Workspace tools declare resource kind
`workspace`; the resolver checks a trusted, normalized resource reference
before execution. Named IDs are selectors, never bearer authority.

Gateway lookup must restore the full originating task/session provenance for
external-egress and workspace mutation decisions. The current call-scoped
disclosure turn alone is insufficient for source-taint inheritance on resume.
No trusted context means refusal, not a context-free success.

## 2. Types and identifiers

The following TypeScript is a language-neutral wire notation, not SDK code.
Optional properties may be omitted; JSON null is used only where specified.

```typescript
type Scope = "private" | "shared";
type Format = "md" | "txt" | "json" | "csv";
type ArtifactRef = { artifactId: string; revision: string };
type SourceRef = { sourceId: string; revision: string };
type Page<T> = { items: T[]; nextCursor?: string };
type FileInfo = ArtifactRef & {
  path: string; scope: Scope; format: Format; sizeBytes: number;
  updatedAt: string; taskRef?: string;
};
type SourceInfo = SourceRef & {
  url: string; title?: string; retrievedAt: string; sourceDate?: string;
  provider?: string; backend?: string; mediaType?: string;
  truncated: boolean;
};
type TextPage = {
  text: string; nextCursor?: string; truncated: boolean;
};
type Receipt = {
  receiptId: string; operation: string; artifact?: ArtifactRef;
};
type ToolBody<T> = { version: 1; data: T };
```

IDs are opaque server-issued strings, bounded to 128 UTF-8 bytes; cursors
to 512 bytes. Revisions identify immutable committed bytes plus provenance,
not a mutable filename or just an unqualified path hash. Timestamps are UTC
RFC3339. Paths obey W037's refusal rules (32 components, 4,096 UTF-8 bytes,
255 bytes/component); namespaces use stable Person/Agent IDs internally.
Public URLs are HTTP/S, at most 2,048 UTF-8 bytes, with no userinfo. Query
and search-filter strings are at most 1,024 bytes; implementations may impose
a smaller documented bound to fit a complete message.

`ToolBody<T>` is serialized as bounded JSON text in existing `content`, inside
the existing untrusted-content envelope wherever required. It is not a new
top-level `InvokeResult` field or an authority-bearing channel. Runtime/model
consumers treat it as data. Any machine decoder must use the existing envelope
format and reject malformed/truncated/redacted-invalid JSON; it must never
execute strings or infer authority from `receiptId`, URLs or file references.
Keystone checks authoritative receipts/objects itself for later operations.

Search results, extracted text and workspace content are untrusted. Fit the
entire serialized body and envelope before returning it: default gateway
content budget is 4,000 bytes. Text-page target is 2,500 UTF-8 bytes, reduced
when JSON escaping/metadata would overflow. Do not rely on gateway truncation
to produce valid JSON. Even five search results may require pagination.
Listing/search uses at most 20 items/page, further reduced to fit the body.

## 3. Tool input/result map

The notation `X -> Y` names `args` and successful `ToolBody<Y>.data`.
Every call, including cursor pages and retries, is authorized again.

### Research (read tools)

```typescript
type SearchFilters = {
  region?: string; language?: string; recencyDays?: number;
};
type SearchArgs =
  | { query: string; filters?: SearchFilters; limit?: number }
  | { cursor: string };
type SearchHit = SourceInfo & { snippet: string };
type SearchData = Page<SearchHit> & {
  provider: string; backend: string; cacheHit: boolean;
  searchesUsed: number; searchesAllowed: number;
};
type FetchArgs = { url: string } | { cursor: string };
type FetchData = TextPage & {
  source: SourceInfo; fetchesUsed: number; fetchesAllowed: number;
};
```

| Tool | Input -> result | Rules |
| --- | --- | --- |
| `web_search` | `SearchArgs -> SearchData` | Resolve selected ready provider from Agent/default; limit defaults to 5 and is 1–5; positive integer recency supported or explicitly refused. Cursor branch cannot also supply query/filters/limit. |
| `web_fetch` | `FetchArgs -> FetchData` | First call retrieves/extracts public HTML/Markdown/plain text; cursor branch reads existing immutable extracted data with no upstream refetch. It cannot change URL. |

First-page search/fetch records actual URL/provider/backend/date and source
reference. Required filters may never be silently discarded. Search and
fetch/extraction are separate provider capabilities. Internal configured
provider transport is never an exception for arbitrary public-fetch URLs.
DNS resolution, connection pinning and every redirect must reject non-public
destinations; no cookies, browser/JS, PDF or authenticated sessions in v1.

### Workspace reads

```typescript
type ListArgs = { scope: Scope; path?: string; cursor?: string };
type ReadArgs = ArtifactRef & { cursor?: string };
type FindArgs = {
  scope: Scope; query: string; path?: string; cursor?: string;
};
type DirectoryRef = {
  scope: Scope; path: string; directoryRevision: string;
};
type Entry = { kind: "file"; file: FileInfo }
  | { kind: "directory"; directory: DirectoryRef };
type FindHit = { file: FileInfo; snippet: string };
```

| Tool | Input -> result | Rules |
| --- | --- | --- |
| `workspace_list` | `ListArgs -> Page<Entry>` | Empty/omitted path names scope root; page only current authorized committed files/directories, excluding trash and staging. |
| `workspace_read` | `ReadArgs -> {file: FileInfo; page: TextPage}` | Explicit committed revision, paginated UTF-8 text; recheck runtime processing privacy before disclosure. |
| `workspace_search` | `FindArgs -> Page<FindHit>` | Scoped filename/body text search; query cannot search another Person or bypass snippet privacy. No vector product required. |

### Workspace mutations

```typescript
type MkdirArgs = { scope: Scope; path: string };
type RmdirArgs = {
  scope: Scope; path: string; expectedDirectoryRevision: string;
};
type BeginArgs = {
  scope: Scope; path: string; expectedRevision: string | null;
  artifactId?: string;
};
type ChunkArgs = { writeId: string; index: number; text: string };
type CommitArgs = { writeId: string };
type PatchArgs = ArtifactRef & {
  oldText: string; newText: string;
};
type MoveArgs = ArtifactRef & { destinationPath: string };
type TrashArgs = ArtifactRef;
type RestoreArgs = ArtifactRef & { destinationPath?: string };
type WriteSession = {
  writeId: string; expectedRevision: string | null; nextIndex: number;
};
```

| Tool | Input -> result | Rules |
| --- | --- | --- |
| `workspace_mkdir` | `MkdirArgs -> {receipt: Receipt; directory: DirectoryRef}` | Non-root path; existing directory may return verified idempotent receipt, never replace a file. |
| `workspace_rmdir` | `RmdirArgs -> Receipt` | Remove only the selected empty, non-root directory at the expected directory revision. Refuse nonempty targets; no recursive traversal, file deletion or trash purge. |
| `workspace_write_begin` | `BeginArgs -> WriteSession` | Create: null expectedRevision, no artifactId, path must be free. Edit: artifactId and current expectedRevision required, same scope/path. Derive allowed format from extension. |
| `workspace_write_chunk` | `ChunkArgs -> {writeId: string; nextIndex: number}` | Consecutive zero-based index, text at most 1,024 UTF-8 bytes/chunk. Same index+same bytes retry succeeds; different bytes conflict. |
| `workspace_write_commit` | `CommitArgs -> {receipt: Receipt; file: FileInfo}` | Recheck base revision, grants, privacy, fence and quota; publish complete immutable revision once. Commit empty text is permitted. |
| `workspace_patch` | `PatchArgs -> {receipt: Receipt; file: FileInfo}` | Replace exactly one occurrence of nonempty oldText against explicit revision; zero/multiple matches conflict. Combined old/new text at most 1,024 bytes. Larger/full edits use staged writes. |
| `workspace_move` | `MoveArgs -> {receipt: Receipt; file: FileInfo}` | Same visibility scope only, no replacement; preserve revision/history. Scope change is a human sharing operation. |
| `workspace_trash` | `TrashArgs -> Receipt` | File only; recoverable and quota-accounted. No recursive directory delete. |
| `workspace_restore` | `RestoreArgs -> {receipt: Receipt; file: FileInfo}` | Restore current trashed revision to original or allowed same-scope path; refuse collision. |

Write sessions bind trusted Person, Agent, scope, task, target and base
revision at creation. Changing those through later args is impossible.
Chunks live in quota-accounted task scratch; incomplete sessions never appear
as artifacts. Pending/running/waiting/paused-offboarded tasks retain staging; cleanup removes
eligible abandoned sessions seven days after entering completed, failed or
cancelled. Unknown states are protected, not assumed terminal. Resumed task handles may access
the same session only after current authority/provenance checks.

Mutation retries use existing Invoke idempotency keys (required, max 128
bytes) plus durable effect receipts. A key reused for different arguments is
`conflict`; keys scope to task and operation, not a global model-supplied
namespace. Chunk replay is additionally checked by stored index/hash.
An already committed write session returns its original receipt after
reauthorization; an expired/unavailable session returns `not_found`.

Directory revisions are opaque server-owned identity/generation tokens, not
path hashes. List and mkdir return them; removal requires the current token
so a removed/recreated directory cannot be deleted using a stale selector.
Authorize the current Person/Agent/scope and pin validated parent descriptors,
then serialize namespace mutations, compare the revision and use atomic
empty-directory removal at the execution fence. Concurrent create/recreate or
unsupported storage guarantees must refuse safely. Successful removal is
reversible by mkdir; it never deletes artifact revisions/history. Like mkdir,
rmdir is mutating, individually grantable, quota-safe and refused in QA-only
mode. Normal idempotency/receipt checks apply. Files still use trash/restore.

### Artifact export (mutating tool)

```typescript
type ExportArgs = ArtifactRef & {
  destination: "mine" | "shared"; filename: string;
  mode?: "copy" | "move";
};
type ExportData = {
  receipt: Receipt; destination: "mine" | "shared";
  filename: string; url: string;
  sourceTrashed: boolean;
};
```

`artifact_export`: `ExportArgs -> ExportData`. Filename is one safe leaf;
mode defaults to copy. Mine resolves from the requesting Person's existing
folder mapping. Another Person's Mine and arbitrary host paths are absent.
Shared requires trusted evidence of explicit requested disclosure plus source
authority; naming `shared` in a model argument is not that evidence. Use the
existing confirmation/task-intent mechanism when intent is not established.

Copy bytes directly through confined Filedrop publication with no overwrite,
destination-size/mount checks and execution fence. Requested move publishes
and verifies the copy, records the export receipt, then trashes the source.
If source trash fails after copy, record partial delivery and park/retry that
step; return the settled destination receipt with `sourceTrashed: false`,
and report that the move is incomplete. Never repeat publication or claim
cross-store atomic rename. Collision refuses rather than auto-overwriting.
No generic existing-Filedrop document read tool is introduced.

## 4. Cursors and errors

Cursors are opaque signed tokens or server-side handles bound to Person,
Agent, task/resource scope, normalized query, revision/snapshot and position.
They grant nothing; a changed query/ref with a cursor is `invalid_input`.
Revision/source cursors point to immutable retained bytes; listing cursors
use a consistent authorized snapshot or return `stale_cursor` after a change.
They expire when their source/session is cleaned up or after revocation.
Policy checks precede both cache/cursor hits and verified ledger-result reuse.
Public URLs are source evidence, not the cursor store.

Keep the current closed `Outcome` vocabulary. Errors put a stable code
prefix in bounded `reason`, e.g. `conflict: revision changed`; error `content`
may contain `{version: 1, error: {code, message}}` through the existing pipeline.
No new top-level wire field is necessary. The adapter normalizes these tool
errors to the listed outcomes rather than returning a successful-looking
text error. Existing Agent API authentication/handle errors remain unchanged.

| Code | Existing outcome | Meaning |
| --- | --- | --- |
| `invalid_input`, `unsupported` | `error` | Invalid schema/bounds or unavailable required format/filter. |
| `not_found`, `stale_cursor`, `conflict` | `error` | Authorized target absent/expired, snapshot changed, stale revision or collision. |
| `permission_denied`, `privacy_denied`, `context_missing` | `denied` | Authority/privacy/trusted context refused before effects. Hidden targets do not disclose existence. |
| `provider_unavailable`, `storage_unavailable`, `rate_limited`, `timeout` | `error` | Admitted operation could not reach a ready dependency or meet its deadline. |
| `quota_exceeded` | `error` | Reservation/file ceiling refused; preserve prior artifacts. |
| `budget_exhausted` | `waiting` | Park with saved progress; use existing budget wait reason, no silent reset. |
| `confirmation_required` | `waiting` | Existing human-confirmation wait; no source/destination effect yet. |
| `cancelled` | `error` | Interrupted attempt; preserve durable receipts/recoverability. |
| `internal_error` | `error` | Unexpected platform failure, redacted actionable detail. |

Empty search results are `ok` with empty items, not a provider failure.
Use the current applicable `WaitReason` enum values rather than adding a
second wait-state machine. Don't include protected filenames/queries/host
paths in reasons or operator/household audit fields that lack source access.

## 5. Provider configuration and adapter seams

```typescript
type ProviderKind = "ddgs" | "searxng" | "tavily";
type ResearchLimits = {
  searchesPerTask: number; fetchesPerTask: number;
  requestTimeoutSeconds: number; downloadBytes: number;
  extractedCharacters: number; cacheSeconds: number;
};
type SearchProvider = {
  id: string; name: string; kind: ProviderKind;
  endpointRef?: string; credentialRef?: string;
  backends: string[]; fallbackOrder: string[];
  limits: ResearchLimits;
  state: "unconfigured" | "untested" | "ready" | "unavailable";
  testedConfigRevision?: string;
};
type ProviderSelection = { providerId: string | null };
type AdapterCapabilities = {
  supportedFilters: ("region" | "language" | "recencyDays")[];
  formats: ("html" | "md" | "txt")[];
};
```

This is a typed registry, not conversational Connections. Provider selection
null inherits the household default; no default means web tools unavailable.
IDs refer to Owner-managed records. Credentials are write-only vault references;
endpoint refs name validated configuration/managed services, never model URLs.
Registry API representations omit secret values. A readiness test binds the
tested configuration revision; editing relevant settings invalidates readiness.
Removing a provider invalidates dependent selections without picking another
provider. V1 rejects unsupported provider kinds on create/test (their enum
names reserve the extension seam, not pretend they work).

DDGS backends default to `["duckduckgo"]`, fallbackOrder to `[]`. Both are
Owner-configured allowlists validated against the pinned service's supported
backends; no `auto` or implicit all-engine selection. Fallback is confined to
explicit entries and records actual backend/attempts. Paid-provider credentials
or overage changes require future provider implementation and review; no
implicit paid route. Resource-changing settings use normal Owner Plans.

The DDGS internal HTTP contract is `GET /health` (version/readiness only) and
`POST /v1/search` with query, supported filters, limit and one explicit backend.
The request is `{version: 1, query, filters?, limit, backend}`. Success is
`{version: 1, data: {items: [{url, title?, snippet, sourceDate?}], provider:
"ddgs", backend, retrievedAt}}`; failure is `{version: 1, error: {code,
message}}` with an appropriate non-success HTTP status. Health is
`{version: 1, ready: boolean}`. HTTP 200 with no hits is successful empty search.
Worker responses are byte-bounded; Keystone generates source IDs/revisions
after validation. Keystone, not the worker,
chooses fallback, enforces task budgets/privacy and retains evidence. Internal
service access uses the trusted homelab-network boundary accepted by the
maintainer in #493 (2026-09-30). Publish only on host loopback, with no public
Route. This is not Keystone-only caller isolation: reachable sibling containers
and local processes can call the unauthenticated worker directly, bypassing
Keystone permissions, task budgets and audit. Per-app/process isolation is a
known limitation (#191), not part of epic #471. Do not expose this service to
the public Internet. All Keystone-mediated authority checks remain required.

Adapters implement capability discovery, bounded cancellable search and
normalized source metadata/errors. Public fetch/extraction is a separate
Keystone implementation and can evolve independently. A fake adapter for each
reserved provider kind proves contract portability, not live support.

Default limits: five search hits/call; ten upstream search attempts and twenty
upstream fetch attempts/task; 30 seconds/request; 5 MiB decompressed/downloaded
page ceiling; 50,000 extracted characters/page; 15-minute cache; two active
DDGS requests/install. Every outbound attempt/retry reserves and spends one
applicable task unit before I/O; a DNS/transport/upstream failure still counts.
Policy denial before dispatch, verified cache hit and existing page read spend
no upstream unit, but remain subject to existing Invoke tool-call budgets.
Concurrent counters are atomic. Preserve counters on resume. Explicit human
continuation may raise allowed totals through policy; the model cannot do so.

Configured limits are positive integers with platform-defined maxima (including
the message bounds above); reject overflow, fractions and unbounded/zero values.
Configuration changes do not reset already spent task counters. Lowering a
limit below usage parks further outbound work; increasing it still requires
the authorized continuation/configuration policy, not a tool argument.

Cache partitions include Person/scope plus provider/backend/query/filters and
configuration revision. Cache TTL does not renew retrieval dates. Recheck
current grants and source taint before hits; no cross-Person cache disclosure.

## 6. Person-facing route families

These are additions to the existing public Keystone API, shared by Console,
CLI and portal clients. They are not a second runtime authority API. Route
registration must use existing identity, same-origin, admission, freshness
and surface checks. Members have no Console configuration access.

| Route family | Scope |
| --- | --- |
| `GET /api/v1/agents/{agent}/artifacts` | Scoped listing/search; portal only admitted Member Agents. |
| `GET /api/v1/agents/{agent}/artifacts/{artifact}/revisions` | Authorized retained revision metadata. |
| `GET /api/v1/agents/{agent}/artifacts/{artifact}/revisions/{revision}` | Inert preview metadata/text. |
| `GET /api/v1/agents/{agent}/artifacts/{artifact}/revisions/{revision}/download` | Authenticated attachment stream; nosniff. |
| `POST /api/v1/agents/{agent}/artifacts/{artifact}/{move,trash,restore,purge}` | Human commands with expected revision; purge confirmed. Brace notation names four concrete suffixes. |
| `POST /api/v1/agents/{agent}/workspace/directories/remove` | Human empty-directory removal with `RmdirArgs`; same authority, revision, fence and nonrecursive semantics as the tool. |
| `PUT /api/v1/agents/{agent}/artifacts/{artifact}/grants` | Human sharing; expected grant revision, preserve source audience. |
| `GET, POST /api/v1/research/providers` | Owner registry listing/create; POST configuration invokes normal Plan path when resource-changing. |
| `GET, PATCH, DELETE /api/v1/research/providers/{provider}` | Owner configuration lifecycle; expected config revision and applicable Plans. |
| `POST /api/v1/research/providers/{provider}/test` | Owner bounded test of stored config only. |
| `PUT /api/v1/research/default-provider` | Owner default selection, expected config revision. |
| `PUT /api/v1/agents/{agent}/research-provider` | Owner explicit override/null-inherit; not a tool grant. |

API Agent path selectors resolve the existing stable Agent record; no display
name-to-folder mapping. Artifact navigation links are canonical same-origin
UI links backed by these APIs. Open/download checks current identity/grants;
URLs themselves confer no access. Owner selection of an Agent in Console
does not reveal another Person's private files. Retired Agent reads still
require authorized Person access and cannot reactivate the runtime.

Human grant updates accept allowed Person/Agent selectors with expected grant
revision. Source owner authority, privacy and class/surface ceilings constrain
the proposed audience; a supplied Person selector cannot create ownership.
Human purge uses existing confirmation/freshness controls with an explicit
artifact/revision summary and current authorization. It deletes only the
approved retained workspace bytes/metadata under the documented policy,
not backup history or another person's resources.

## 7. Ownership and publication/recovery map

| Interface/state | Authority/owner | Required invariant |
| --- | --- | --- |
| Workspace root/readiness | Owner desired config; Keystone storage seam | Default `/srv/wildstone/agent-workspaces`; stable ID partitions; no sidecar mount; lost required mount refuses. |
| Artifact/grants/privacy/revisions/task linkage | Runtime database, Keystone resolver | Current Person + Agent scope; only committed revisions visible; inherited policy never widens through copies. |
| Immutable revision bytes | Confined Keystone filesystem store | Content hash checked; active operations pinned to validated descriptors; no overwrite publication. |
| Staging/publication journal | Runtime DB + storage coordinator | Durable intent/hash/base revision/quota reservation before publication; recover or roll back interrupted work. |
| Quota | Runtime DB reservations and reconciled storage accounting | 1 GiB per owning Agent includes revisions/staging/scratch/trash; shared readers do not duplicate charges; max file 10 MiB. |
| Tool authority | Existing Manifest/Invoke resolver, resource rule | Assignment != file grant; repeat checks on retries/cache/ledger hits; QA and surface ceilings remain. |
| Source/provenance | Task/session/source/artifact privacy graph | Server-owned edges persist before disclosure/effects; restrictions follow derived revisions and resumed work. |
| Export receipt | Work ledger + transfer coordinator | Record actual settled destination/hash/link; verify publication before source trash; uncertain outcome parks. |
| Backup manifest | Existing BackupPlan/recovery coordinator | Coherent committed DB metadata + revision-byte set; restore validation/quarantine before access. |

Publication transitions: `staging -> prepared -> bytes_published -> committed`.
Only committed revisions are current. Serialize compare/commit per artifact
and reserve quota before writes. Prepared journal records contain immutable
byte hash, old/new revision refs and effect identity. Publication must sync
bytes and journal/metadata durably according to filesystem guarantees.
Recovery completes verified bytes+metadata or rolls back pending metadata;
it never removes the prior committed revision to fix a failed new write.
No lock spans an unbounded model/network wait. Unsupported storage guarantees
fail visibly. Exact table layout and lock implementation belong to S02/S05.

Sharing creates a checked grant/derived revision scope with source edges;
ordinary move cannot change visibility. On retirement files persist. Scratch,
including abandoned staging, expires seven days after entry into completed,
failed or cancelled. Pending, running, waiting and paused-offboarded states
are protected; unknown states fail closed. Cleanup rechecks the state,
terminal timestamp, execution fence and unresolved-publication pins immediately
before deletion; it never discards data needed to recover uncertain effects.
File trash expires after 30 days under configured retention. Only this file
retention exception amends W033 F6. Permanent model purge is absent.

Backup manifest schema v1 names snapshot ID/time, runtime-DB snapshot hash,
workspace-config revision, committed artifact/revision IDs, byte-object hashes
and sizes, retained history/trash state, privacy/grant/lifecycle metadata
generation, and completion marker. Metadata may live in the referenced DB
snapshot, but the manifest must identify its consistent generation. Exclude
scratch/staging. Snapshot coordinator pins the committed revision set against
concurrent write/GC and captures a matching DB snapshot; directory copy plus
an unrelated SQLite snapshot is insufficient. No destination means backup
not configured. Daily protection is a configured target, not a default claim.

Restore validates manifest, hashes and metadata references in existing
quarantine, reconciles incomplete effects/quota/mount state, and reapplies
available post-snapshot revocation/offboarding evidence before reopening.
A restored old snapshot cannot prove no later revocations occurred. If that
evidence is absent, keep affected access quarantined for human recovery review
and explicit reauthorization; never silently re-enable stale grants. Retained
backup policy remains W045's; deleting a workspace file does not promise
erasure of historical backups. Restore must not re-ingest forgotten content
or replay notifications/exports merely because their receipts were recovered.

## 8. Handoff boundaries

S01 publishes these contracts only. Tracker #471 maps one-session slices
to persistence, authority, confined storage, publication, tool registration,
provider service/adapters, UI, lifecycle, export and recovery. Implementations
must include contract fixtures and adverse-path checks for their own boundary.
Record native-runtime traces separately from Hermes/OpenWebUI behavior.
Live DDGS availability/model conformance remains an explicit release gate.
