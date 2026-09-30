# ADR-W047: Native Agent research, private workspaces, and artifact delivery

- **Status:** proposed; reconciled against the repository snapshot below; not implemented by this draft.
- **Date:** 2026-09-30
- **Project:** chickenrat17/wildstone-homelab
- **ADR identifier:** W047
- **Issue:** [#473](https://github.com/chickenrat17/wildstone-homelab/issues/473); tracker [#471](https://github.com/chickenrat17/wildstone-homelab/issues/471).
- **Binding v1 interface proposal:** [native-agent-capability-contract-v1](../design/native-agent-capability-contract-v1.md). These are new contracts to implement, not an inventory of shipped tools.
- **Decision owner:** Wildstone maintainer.
- **Scope:** native Agent research, persistent scoped files, user-facing artifacts, and explicit Filedrop export.
- **Provider sequence:** DDGS first; SearXNG and Tavily follow through adapter seams.

## 1. Context

The native Agent needs to retrieve current service documentation, perform research, write useful reports, and retain files across conversations and runtime restarts. Adding these capabilities must preserve Wildstone's separation between runtime reasoning and platform authority.

The native runtime's short-lived sidecar owns the reasoning loop. Keystone owns effective tool availability, identity, permissions, privacy, execution, task lifecycle, evidence, and audit. These capabilities extend that platform tool surface. They do not require a shell, arbitrary host filesystem access, authenticated browser sessions, or a different agent framework.

This draft records decisions accepted in the design discussion and incorporates a source review at commit `6516fd74c2fa63b734ea5756d7be39fb36c8a20a`. Section 13 separates existing machinery, required extensions, and explicit ADR amendments. Source inspection establishes compatibility and gaps; it does not certify deployed behavior.

## 2. Decision summary

| Area | Decision |
| --- | --- |
| Tool ownership | Keystone owns research, workspace, and export tools; the native runtime invokes them through its existing tool protocol. |
| Installation | Ship tool/adaptor support with Wildstone. DDGS service deployment is part of research setup, not an unconditional container on every installation. |
| Web activation | Offer setup during onboarding. Activate only after a provider is configured and tested and tools are enabled for selected Agents. |
| Workspace activation | Enabled for new native Agents, confined to authorized workspace scopes. Preserve existing Agent grants on upgrades. |
| Providers | DDGS first, default backend DuckDuckGo only. Additional backends are Owner-configured. No unrestricted automatic selection. |
| Deployment | One shared DDGS service per installation, reached through Keystone. It holds no Agent memory or workspace files. |
| Provider extension | Stable provider-neutral search/fetch tools, with seams for SearXNG and Tavily. |
| Workspace | Persistent configurable root on the Keystone host, private by Person and Agent unless explicitly shared. |
| Formats | Markdown, plain text, JSON, CSV. Files are data; workspace tools cannot execute them. |
| Delivery | Authenticated preview/download links; explicit copy/export to Filedrop Mine or Shared. |
| Retention | Durable artifacts remain until explicit deletion; trash expires under configured policy; terminal-task scratch expires separately. |

## 3. Platform and runtime boundary

The native runtime receives only the effective tool schemas for its current task. Every invocation is authorized again at execution time against the originating Person, Agent, task, Connection, disclosure policy, tool assignment, and limits. Cached manifests never preserve revoked authority.

Keystone performs or mediates network access and filesystem operations. The native sidecar gets neither a workspace mount nor provider credentials. Runtime restart or replacement does not change file ownership or erase work. Future runtimes may consume the same platform capabilities through the Agent API; native-runtime support is the initial acceptance target.

Shared DDGS execution does not merge Agent budgets or identities. Requests retain task attribution. DDGS is a replaceable worker, not a task coordinator or file store. Diagnostic logs must not persist private queries, page bodies, credentials, or workspace contents by default.

## 4. Installation, configuration, and controls

### 4.1 Installation and upgrades

- Research tool implementations and provider adapter contracts ship with Wildstone releases.
- Onboarding offers Web Research setup. Choosing DDGS proposes the managed, digest-pinned service through the existing Plan and Owner-approval flow, then tests the reconciled backend before activation. Onboarding is not a bypass around infrastructure approval.
- New native Agents receive bounded workspace capability by default. Storage readiness is a prerequisite to availability.
- Existing Agents retain their assignments and controls during upgrades. New tools appear as available to configure, without silently acquiring authority. Register these tools as `OptIn` so dynamic bundles and generic default seeding cannot acquire them. Explicitly seed the bounded workspace set only on new native-Agent creation after storage readiness; web and export remain separately assigned. This use of explicit seeding is a narrow amendment to ADR-W033 B5, not a change to existing bundle behavior.
- Removing a provider or losing storage marks dependent tools unavailable with an actionable reason. Removing a runtime or retiring an Agent does not delete artifacts.

### 4.2 Provider configuration

Proposed Console flow:

1. **Integrations → Web Research:** Owner creates a named provider connection, configures endpoint/credentials/backend as applicable, tests it, and sets limits.
2. **Agents → selected Agent:** Owner selects the household default connection or an explicit override and assigns research tools.
3. **Agents → selected Agent → Tools:** inspect and independently enable/disable research, workspace, and export tools through existing governance mechanisms.
4. **Agents → selected Agent → Files:** Owners browse and manage only files their current Person identity may access; administrative rights alone do not reveal another Person’s private content.
5. **Household portal → Assistant / My Agent Files:** Members and Owners open their own artifacts for admitted Member Agents, using household SSO and current resource authorization. Owner-Agent artifacts remain on the Console surface; the portal does not admit Owner Agents.

UI labels remain proposed. The split between Console administration and household artifact access is required by ADR-W032 and ADR-W033 A4. Reuse `TabTools.tsx`, tool assignments, controls, and connection ceilings rather than creating parallel authorization flags. Browsing an artifact is not permission to configure its Agent.

A search-provider configuration is not a human chat Connection. Add a typed, named search-provider registry with vaulted secret references, household default, and Agent selection; do not overload the current conversational Connection model. Integrate resource-changing endpoint/service edits with Plans and current Owner freshness requirements.

Each tool displays its effective state and reason: enabled, disabled, unassigned, provider missing, storage unavailable, or blocked by policy. A tool being shipped or registered is not equivalent to it being usable.

Only an authorized human administrator may change provider configuration or grant tools. A conversational request can use existing authority or request additional capability; it cannot grant itself tools, select arbitrary endpoints, or expand network destinations.

### 4.3 Provider seams

Keep Agent tools stable while providers implement normalized search and fetch/extraction results. Search and extraction may use different implementations. Capability discovery must state supported filters and formats; adapters may not silently discard required constraints.

Search returns title, canonical/source URL, snippet, selected provider/backend, retrieval timestamp, and available source-date metadata. Fetch returns final URL, media type, extracted text, retrieval timestamp, truncation information, and source reference. Provider errors distinguish unavailable, blocked/rate-limited, timeout, unsupported, and genuinely empty results.

DDGS uses a pinned Python service/container behind Keystone. DuckDuckGo is the only default backend. Owners may configure additional named backends and an explicit order. Fallback is permitted only within the configured policy, is recorded, and never invokes an unconfigured or paid provider silently. No automatic paid overages are enabled.

SearXNG and Tavily are follow-ons; v1 must define adapter seams and exercise them with contract fixtures, not implement those integrations prematurely. No guarantee of DDGS availability or upstream search quota is implied.

## 5. Web research tools and data boundary

Tool names are `web_search` and `web_fetch`; exact v1 inputs/results and registration rules are fixed in the companion interface contract.

V1 supports public HTTP/HTTPS retrieval of HTML, Markdown, and plain text. It excludes authenticated sessions, browser automation, JavaScript execution, PDF extraction, and arbitrary file downloads into the workspace. PDF and richer document extraction belong to dedicated later capabilities.

Public-web retrieval must reject loopback, private, link-local, metadata, and other non-public destinations, including after DNS resolution and redirects. Trusted internal provider endpoints are separately configured service connections; allowing Keystone to reach DDGS or SearXNG must not allow arbitrary private URLs in `web_fetch`. Connection establishment must preserve the validated destination, preventing DNS-rebinding bypasses.

Fetched content, search snippets, titles, and URLs are untrusted data. They cannot become instructions, tool grants, or execution authority. Researching a command never authorizes running it or modifying a service.

Outbound queries use only the information required for research. Source labels and disclosure restrictions must be enforced before network requests; a local inference route alone does not authorize disclosing local-only content through search. The platform must refuse unauthorized disclosure rather than relying solely on the Agent to sanitize queries. Existing `PrivacyPolicy.Check` separates model processing and delivery: its local-model verdict alone does not block a local runtime from sending local-only text to search. Add an external-egress check over task/session provenance, including workspace-source edges, before search queries and user-supplied URLs leave Keystone. A restricted source taints derived artifacts and later tasks; an allegedly sanitized model string does not remove that restriction. Absence of trusted provenance context fails closed on these tools. DDGS’s local container endpoint does not make its upstream search local processing.

### 5.1 Default limits

| Setting | Default |
| --- | --- |
| Search results | 5 per call |
| Research task budget | 10 searches and 20 page fetches |
| Request timeout | 30 seconds |
| Page download ceiling | 5 MiB |
| Extracted text ceiling | 50,000 characters per page, truncation disclosed |
| Search cache TTL | 15 minutes |
| DDGS request concurrency | 2 per installation |

Limits are configurable within platform policy. Searches, retries, and fetches consume a documented aggregate task budget; continuation preserves accumulated usage and evidence. When exhausted, save progress and offer continuation. An explicit continuation may extend the budget through platform policy, not a model-supplied counter reset.

Cache keys include provider connection, backend, query, and filters. Caches must preserve source timestamps and applicable privacy boundaries. Partition caches by the authorized Person and scope in addition to the request key. Recheck current provenance policy and authority on cache hits. A shared worker or household-default provider never implies a shared private-result cache.

### 5.2 Bounded tool transport

The existing gateways default to 4,000 bytes of result content, with redaction and untrusted envelopes. `do_household_action` accepts at most eight parameters, each rendered value at most 1,024 bytes. The 50,000-character extraction and 10 MiB file limits are storage/download ceilings, not single tool-message sizes.

Register individually typed research and workspace tools through the canonical platform gateway; do not tunnel large writes through the generic household action map or bypass its result safeguards. Return compact metadata, real source/artifact references, and opaque revision-bound cursors. Page text at a default of 2,500 UTF-8 bytes, keeping the entire result within the gateway content budget. Paginate result metadata and snippets too; five results may span pages. Retrieval of an already extracted page does not consume another upstream fetch, but remains bounded and authorized.

Workspace writes use bounded staged chunks and an explicit atomic commit against the expected base revision. A first implementation should keep chunk payloads at or below 1,024 UTF-8 bytes; larger structured payloads require separate reviewed bounds. Incomplete staging is scratch, consumes quota, and is never presented as a completed artifact. Backend export and file download stream bytes without routing them through the model. Do not globally enlarge every tool response to accommodate research.

Research must also respect the runtime’s execution limits. The older direct session default is eight tool round trips; the current native path takes its per-run turn limit from `runner.MaxSliceRounds` and spends its aggregate job budget through `Slice.Call`; a task allowing ten searches and twenty fetches is not a promise that they fit in one turn. Preserve runtime limits and checkpoint/continue bounded research slices through the work ledger. Tool-call ceilings, upstream budgets, and per-file size are distinct counters.

## 6. Persistent workspace and ownership

Default root: `/srv/wildstone/agent-workspaces`, configurable by the Owner on the Keystone host. Agent and Person partitions use stable IDs rather than display names. Tools resolve scopes from authenticated task identity; a model cannot select another Person by passing an ID or host path.

| Area | Purpose | Visibility |
| --- | --- | --- |
| Private task workspace | Research, drafts, and generated files | Originating Person through the associated Agent; other people excluded. |
| Shared Agent workspace | Explicitly shared procedures and artifacts | Authorized people and Agent sessions under explicit grants. |
| Task scratch | Intermediate research/extraction data | Same visibility as its task; eligible for cleanup after terminal state. |

Other Agents receive no access automatically. Cross-Agent sharing requires explicit grants. Access revocation is checked on listing, search, preview, download, mutation, and export, including resumed tasks. Search snippets and filenames are subject to the same isolation as file bodies.

Owner administration includes quotas, backup configuration, retirement, and lifecycle operations without automatically exposing private file contents through the UI. A host administrator retains underlying filesystem access. This design does not promise cryptographic privacy from host administrators.

Workspace files are distinct from:

- **Memory:** facts/preferences with their existing ownership and provenance.
- **Shared knowledge:** intentionally readable notes and procedures under the existing knowledge capability.
- **Task ledger:** authoritative execution state and effects.
- **Filedrop:** user file namespaces and their existing access contracts.

Saving a report does not automatically create memory, share it as knowledge, or replace task state. Workspace metadata maintains ownership, visibility, revision, lifecycle, and task provenance; the runtime database is authoritative for ownership, grants, privacy edges, task linkage, lifecycle, quota reservations, and publication receipts. Filesystem storage holds immutable revision bytes and bounded staging. Coordinate them through a recoverable staged-publication journal with content hashes; a filesystem rename and a SQLite transaction are not one atomic transaction. Reads expose only committed revisions, and startup recovery completes or rolls back incomplete publications without losing the prior revision.

Agent class and connection restrictions remain independent of the file’s Person audience. Owner-only source material and artifacts are inaccessible after demotion or through a Member surface, even if the Person remains their owner. An unattended routine/subscription has no active Person and cannot borrow its proposer’s or approver’s private workspace. V1 private-workspace operations require an authenticated interactive Person; future unattended shared work requires exact-version grants and an approved output audience under ADR-W033 E6 and ADR-W039.

## 7. File operations and safe presentation

The companion contract fixes individually registered `workspace_*` tools for list, read, search, mkdir, empty-directory removal, staged create/write, patch, move, trash, and restore. Permanent purge is a human control, not a routinely granted model tool. V1 must not grant arbitrary filesystem access.

Use the existing Member/Owner tool classes and resource-rule resolver. Add a workspace resource kind with explicit scope/revision checks; an Agent-level tool assignment alone is not a grant to every file. Research and workspace reads are nonmutating tools, so QA-only sessions can use them if otherwise authorized. Workspace create/edit/move/trash/restore and export are mutating and withheld/refused in QA-only mode. Use the domestic risk tier for reversible operations within the same authorized private scope; Shared disclosure requires explicit human intent plus source authority. Immediate purge requires the existing confirmation/freshness mechanisms appropriate to its scope. Provider installation and configuration remain Owner administration.

Agents may create, edit, and move files within authorized scope. Moving between visibility scopes is a disclosure operation requiring explicit sharing authority; it is not an ordinary path rename.

- Accept bounded relative paths only; refuse traversal, absolute paths, unsafe components, and symlink escapes.
- Publish complete revisions atomically and preserve revision history. Serialize revision comparison and publication under a per-artifact write lock coordinated with the metadata journal; checking a hash before an unguarded rename is insufficient.
- Require expected revisions for edits; refuse concurrent-change conflicts instead of overwriting them.
- Refuse silent replacement on create, move, and export collisions.
- Keep operations auditable without logging private file bodies.
- Treat `.md`, `.txt`, `.json`, and `.csv` as data. No shell, import, plugin loading, or executable workspace hook is granted.
- Markdown previews disable scripts, active HTML, and other executable content. Render JSON/CSV/text inertly; remote embeds must not silently disclose a viewer's identity or fetch arbitrary resources.
- `workspace_rmdir` removes only an authorized empty, non-root directory at its expected identity/generation revision; atomic emptiness checks and namespace serialization protect concurrent creation. Human directory removal uses a scoped path endpoint because directories are not artifacts. No recursive removal or deletion of contained files/history is granted.
- Deletion first moves files to recoverable trash. Immediate permanent deletion requires human confirmation; configured expiry is a separate lifecycle policy. This ADR explicitly amends ADR-W033 F6 only for workspace scratch and file-trash retention; indefinite recovery remains unchanged for People, Agents, Connections, routines, and other durable entities.

Partial failure must leave existing files intact or expose a recoverable state. Idempotent operations and task evidence prevent duplicate artifacts or exports on retry/resume. No completion claim may cite a file link invented by the model.

## 8. Lifecycle, quotas, and backup

| Setting | Default |
| --- | --- |
| Quota | 1 GiB per Agent, including scratch, revisions, and trash |
| Maximum file size | 10 MiB for v1 formats |
| Scratch retention | 7 days after entry into completed, failed or cancelled |
| Trash retention | 30 days, then automatic purge under configured policy |
| Artifact retention | Until explicitly deleted |
| Backup | Daily for artifacts and revision history; scratch excluded |

Pending, running, waiting and paused-offboarded tasks are protected from scratch cleanup; unknown states fail closed. Failed-task scratch and abandoned staging use the same seven-day terminal retention as completed/cancelled work. Cleanup rechecks task state, terminal timestamp and unresolved-publication recovery pins immediately before deletion. Quota exhaustion produces an actionable refusal and must not silently delete durable artifacts. Writes, revisions, exports, and concurrent operations must account for capacity consistently.

Retiring an Agent preserves files and authorized human access. Agent removal or package removal must not accidentally destroy the workspace. Permanent purge is a separate human-confirmed operation. The configured trash expiry authorizes scheduled purge of trashed data; it does not authorize early model-initiated purge.

Backup must include metadata needed to restore identity, permissions, revision history, and task linkage alongside durable bytes. Restore must preserve revocation and forgetting/offboarding rules rather than reintroduce access. Storage migration and unavailable mounts must fail visibly rather than create an empty replacement workspace. Register the durable workspace root in the existing BackupPlan/recovery mechanism and capture a coherent revision manifest alongside the runtime database snapshot. Include publication metadata and privacy/grant records, not just directory bytes. The current database `BackupTo` snapshot alone does not provide a coordinated filesystem backup. Restore under existing quarantine, reconcile the manifest and lifecycle/revocation state, then reopen access. Daily backup is a configured target using an authorized backup destination; installations without one display “backup not configured” rather than claim protection.

## 9. Research workflow and artifact delivery

Short questions receive sourced chat answers. Explicit report requests and substantial research produce a Markdown artifact plus a concise chat summary and authenticated preview/download link. Reports include source URLs, research date, findings, and uncertainties; distinguish observed service state from documentation guidance and inference.

The native Agent:

1. Determines the question and relevant service version, using authorized live tools when needed.
2. Searches and reads relevant sources, prioritizing matching official documentation.
3. Saves intermediate findings and source references in scoped task scratch.
4. Creates a durable report when appropriate.
5. Presents the principal findings with a platform-issued artifact link.
6. Exports to Filedrop only when requested and permitted.

Unfinished research can resume across conversation/runtime restart without replaying completed effects. Artifact persistence and user notification are separate outcomes: a delivery failure does not undo a saved report. A scoped file remains available through the authorized Console or household My Agent Files surface when Filedrop is not installed.

Authenticated links enforce current identity and access at open/download time. They are not public bearer links. Chat channels unable to open the authenticated surface still receive a useful summary and navigation guidance; future attachment transport must preserve the destination's disclosure rules.

## 10. Filedrop export

Add a dedicated artifact-export operation; name `artifact_export`. It accepts a scoped artifact reference/revision, a permitted destination, and a filename. Keystone transfers bytes directly; the export need not put bytes back through model context.

- **Mine:** resolves to the requesting Person from task identity. Export is allowed when requested and the Person and Agent possess applicable authority.
- **Shared:** requires an explicit request and a disclosure check. A request cannot override restrictions inherited from private source material.
- **Another Person's Mine:** unavailable through this tool.
- **Default:** copy/export; preserve the workspace original.
- **Requested move:** copy, verify destination publication, then remove the source through recoverable workspace deletion rules. Never claim a transactional cross-store rename.
- **Collision:** refuse or settle on a new filename through a documented policy; no silent overwrite.
- **Unavailable destination:** preserve the source and report the delivery failure.

Reuse ADR-W037’s current Person-to-Mine folder mapping, pinned-parent confinement, upload staging/exclusive-link publication, required-mount guards, and execution fences immediately before publication. Do not infer Mine from a changeable display name. Refuse unsupported filesystems visibly; do not replace the NFS atomic no-overwrite refusal with check-then-rename. Destination per-file limits also apply. Once copied to Shared, everyone with Filedrop access can change the exported copy; explain that existing collaborative-folder contract before disclosure. Workspace ACLs cannot impose a new read-only ACL on Shared. Workspace trash does not add trash/restore to existing Filedrop deletion.

This ADR extends ADR-W037 and the domestic Filedrop capability with generated-artifact publication. Existing Agent tools only list/find/rename/delete; a new export transfer seam is required. Export does not grant a tool to read existing Filedrop document contents. Existing Filedrop list/find/rename/delete semantics remain separately governed. Export visibility must be rechecked before publication, and its effect receipt records the actual settled filename and authorized link.

## 11. Consequences and deferred work

Benefits: current documentation access, durable user-facing work, runtime independence, scoped files, and useful research without shell authority. Costs: an additional managed provider service, persistent storage/metadata, lifecycle cleanup, privacy-aware network controls, and a larger Console/tool surface.

DDGS is best-effort and depends on upstream search behavior. Pin and evaluate its actual release and backend from the deployment network. The shared service and global concurrency limit may queue work; surface queued or unavailable states rather than leaving the Agent apparently thinking indefinitely.

Deferred: SearXNG/Tavily implementations, authenticated browsing, JavaScript execution, PDF/document extraction, PDF/Word/XLSX generation, arbitrary code execution, automatic memory ingestion of artifacts, and autonomous provider/tool configuration.

## 12. Acceptance evidence

The native runtime, not a Hermes-only demonstration, must pass:

1. Onboarding configures/tests DDGS; no web calls occur before activation. Default backend is DuckDuckGo only.
2. Tools tab reports effective availability and supports assignment/disable without Agent recreation. Revocation interrupts subsequent calls.
3. A current-documentation prompt actually searches/fetches and answers with source URLs, or records a specific failure classification.
4. A report is created with provenance and a real authenticated link, reopened in a later conversation, and retained across restart.
5. A second Person and an ungranted Agent cannot list, search, read, preview, mutate, or export private files.
6. Sharing private-derived content without disclosure authority is refused; changing the destination or naming another Person cannot bypass this.
7. Traversal, symlink escapes, private-network fetches, redirect/rebinding bypasses, and active-content previews are refused.
8. Concurrent edits preserve the prior file; quota and collision failures leave existing artifacts intact.
9. Research limits park work with saved progress; authorized continuation extends limits without erasing accumulated usage.
10. Scratch/staging cleanup expires completed/failed/cancelled work after seven days and protects pending/running/waiting/paused-offboarded or unknown states and unresolved recovery data; trash restore works; retirement preserves artifacts.
11. Filedrop Mine export succeeds, reports the actual destination, and does not grant content-read access. Shared export requires explicit permitted disclosure.
12. Restart, retry, destination failure, and cancellation do not duplicate exports or falsely report completion. Notification failure leaves the artifact usable.
13. Backup/restore preserves durable files and authorization metadata; revocation remains effective after restore.
14. A Member can open their report through household SSO without entering Console; an Owner cannot list another Person’s private files through administrative UI access.
15. Upgrading an Agent with broad service bundles does not grant new tools; new native Agents get the explicitly seeded workspace set, and QA-only execution refuses writes even on hand-crafted calls.
16. Large extraction and multipart report writes stay within message bounds; stale cursors, concurrent commits, interrupted staging, and restart recover without publishing partial files.
17. Local-only/trusted-only source data cannot leave through queries, URLs, Shared export, cache reuse, or resumed artifact reads; absent provenance context refuses the call.
18. Empty-directory removal works for Agents and humans with current scope/revision authority, refuses roots/nonempty or recreated targets, and is refused in QA-only mode. Files and revision history remain intact.

Use provider contract fixtures and native-runtime end-to-end tests, then a separately recorded live DDGS conformance run. Record pinned versions, effective settings, model, task/trace identifiers, observed behavior, and limitations. Live household deployment is not authorized by this draft alone.

## 13. Repository reconciliation result

**Reviewed snapshot:** [`6516fd74c2fa63b734ea5756d7be39fb36c8a20a`](https://github.com/chickenrat17/wildstone-homelab/commit/6516fd74c2fa63b734ea5756d7be39fb36c8a20a), retrieved from current GitHub code search and used explicitly for file reads on 2026-09-30. Review covered the controlling ADRs and runtime, gateway, grants, privacy, Files/knowledge, Console, and backup code. No runtime deployment or live DDGS conformance test was performed. ADR status labels are not treated as runtime evidence.

| Area | Current evidence | Reconciled result |
| --- | --- | --- |
| Runtime authority | ADR-W039/W040/W041; `keystone/agentapi/types.go`, `keystone/butlerlive/native.go`, `keystone/butler/runtime_tools.go`, `runtimes/native-agent/src/protocol.ts` | Compatible: add platform tools through the canonical Agent API; retain native sidecar isolation and execution fences. No new runtime filesystem API. |
| Per-Agent controls | ADR-W033 B4/B5; `keystone/runtimedb/toolassignments.go`, `keystone/butler/authority.go`, `console/src/views/butlers/TabTools.tsx` | Reuse current assignment/control machinery and tool inventory; extend it with readiness reasons and resource-scoped authority. |
| Defaults and upgrades | `keystone/api/handlers_butlers.go:243–280` seeds every available non-opt-in tool; W033 permits dynamic bundles | Register new tools opt-in; explicitly seed workspace defaults only for new native Agents. This prevents upgrades/bundles silently gaining new authority. Narrow W033 B5 amendment required. |
| Human surfaces | ADR-W032; ADR-W033 A4; `keystone/portal/scope.go` | Console is Owner-only and the portal admits only Member Agents. Add household My Agent Files; retain Console administration and Owner-Agent delivery there. No Member Console exception. |
| Payloads and slices | `keystone/butler/gateway.go` default 4,000 bytes; `household.go` 1,024-byte parameter values; `session.go` default eight iterations | Add paginated reads, staged chunk writes, and task checkpoints. Keep storage/download ceilings separate from model message and runtime limits. |
| Privacy | `keystone/runtimedb/privacy.go` (`PrivacyPolicy.Check`, `PrivacyTurn`, `CheckPrivacyDelivery`); `keystone/agentgateway/disclosure.go` | Extend provenance to workspace revisions and add explicit external egress policy. Existing local-model processing checks are insufficient for upstream search. |
| Work/retry | ADR-W030 and W041; runtime fences and existing task authority | Add research counters, checkpoint references, publication/export receipts, and durable idempotency tied to existing work. Persistence and notification remain separate. |
| File storage | ADR-W046; `keystone/knowledge/store.go`, `keystone/domestic/knowledge.go` | Shared knowledge is not a private workspace. Reuse patterns only; build a scoped store with stronger commit serialization, metadata, history, and recovery. |
| File confinement | ADR-W037; `keystone/portal/files_tree.go`, `files_mount.go` | Reuse or extract bounded path and pinned-descriptor helpers. Do not relax symlink, mount-loss, no-overwrite, or NFS rules. |
| Filedrop export | ADR-W037; `keystone/domestic/filedrop.go`, `keystone/api/handlers_portal_files.go` | Current Agent capability lacks upload/content transfer. Add generated-artifact export while preserving Mine mapping, Shared collaboration, destination fences, and existing deletion behavior. Explicit W037 extension required. |
| Lifecycle | ADR-W033 F1–F6 | Retirement preserves access-controlled files. Explicitly amend F6 only for configured workspace scratch/trash expiry, not automatic purge of durable entities. |
| Backup | ADR-W045; `keystone/runtimedb/recovery.go`, `keystone/recovery/recovery.go` | Extend existing backup/recovery with workspace bytes and a coherent metadata/revision manifest; retain restore quarantine. A host folder or SQLite snapshot alone is insufficient. |
| Existing web capability | `packages/agent-hermes/package.yaml`, `docs/design/hermes-capability-package-and-execution-profiles.md` | Hermes `outbound_web` is runtime-specific and defaults off. It does not implement the proposed native platform tools. Keep the tracks distinct. |

### Explicit contract amendments for publication

1. **ADR-W033 B5:** opt-in registration plus narrowly defined new-native workspace seeding. Per-Agent classes, bundles, and grants otherwise remain intact.
2. **ADR-W033 F6:** configured file-trash/scratch expiry is a limited retention exception; People, Agents, Connections, and routine deletion stays indefinitely recoverable until explicit purge.
3. **ADR-W037:** add an Agent generated-artifact export operation using existing publication and Mine/Shared rules. This does not introduce generic Agent reads of Filedrop documents, Shared ACLs, or Filedrop trash.
4. **ADR-W045:** include coherent workspace recovery data and quarantine checks in backup/recovery coverage.
5. **ADR-W039/W041 and W033 privacy contracts:** extend resource provenance/disclosure to artifact reads, external egress, and artifact delivery. Canonical runtime transport and authority ownership remain unchanged.

W032 and W046 need no scope reversal: the new surfaces preserve Owner-only Console and household-shared knowledge. Cross-reference this ADR from them when useful.

### Implementation details that remain to specify

The accepted product decisions are now reconciled. The companion v1 contract fixes tool names/shapes, identifiers, errors, cursor/write-session semantics, provider configuration, route families, and ownership/publication/backup boundaries. Focused implementation issues choose database table/migration layout, journal/locking implementation, storage-platform support, snapshot coordination mechanics, and channel-specific confirmation/link UI within those contracts. These are not claims that those interfaces exist today. Tests must exercise the new grant resolver rules, privacy edges, and native runtime integration rather than merely test isolated filesystem helpers.

The earlier [research issue #467](https://github.com/chickenrat17/wildstone-homelab/issues/467) is a related-work pointer; its current status and scope were not rechecked in this source review. Check it and other open issues before creating implementation tickets. Do not infer deployed capabilities from closed issue status.

### Evidence links

All paths above refer to the reviewed commit. Key source links:

- [Agent design / ADR-W033](https://github.com/chickenrat17/wildstone-homelab/blob/6516fd74c2fa63b734ea5756d7be39fb36c8a20a/docs/decisions/ADR-W033-the-agent-design.md)
- [Native runtime contract / ADR-W041](https://github.com/chickenrat17/wildstone-homelab/blob/6516fd74c2fa63b734ea5756d7be39fb36c8a20a/docs/decisions/ADR-W041-agent-runtime-contract.md)
- [Tool seeding](https://github.com/chickenrat17/wildstone-homelab/blob/6516fd74c2fa63b734ea5756d7be39fb36c8a20a/keystone/api/handlers_butlers.go#L243-L280)
- [Tool gateway](https://github.com/chickenrat17/wildstone-homelab/blob/6516fd74c2fa63b734ea5756d7be39fb36c8a20a/keystone/butler/gateway.go)
- [Privacy enforcement](https://github.com/chickenrat17/wildstone-homelab/blob/6516fd74c2fa63b734ea5756d7be39fb36c8a20a/keystone/runtimedb/privacy.go)
- [Filedrop contract / ADR-W037](https://github.com/chickenrat17/wildstone-homelab/blob/6516fd74c2fa63b734ea5756d7be39fb36c8a20a/docs/decisions/ADR-W037-shared-file-tree.md)
- [Knowledge contract / ADR-W046](https://github.com/chickenrat17/wildstone-homelab/blob/6516fd74c2fa63b734ea5756d7be39fb36c8a20a/docs/decisions/ADR-W046-markdown-knowledge.md)
- [Backup contract / ADR-W045](https://github.com/chickenrat17/wildstone-homelab/blob/6516fd74c2fa63b734ea5756d7be39fb36c8a20a/docs/decisions/ADR-W045-off-site-backup.md)

## 14. Follow-up sequence

1. Reconciliation completed at the recorded snapshot; refresh changed interfaces if the branch advances before publication.
2. Split this reconciled design into focused GitHub issues with dependencies, acceptance evidence, and explicit scope; check related tickets first.
3. Review W047 and its companion contract amendments in the #473 documentation PR. Code implementation and live deployment are later stages.

## 15. Publication refresh for #473

The original reconciliation remains pinned to `6516fd74c2fa63b734ea5756d7be39fb36c8a20a`.
Publication source reads were refreshed against current main
`0727a5d57b90216896a931dd0209c2f3c33791fc` on 2026-09-30. The controlling
W033/W037/W039/W041/W045 documents and the inspected gateway, authority,
Agent API, and default-seeding source retain the reviewed contracts.
The current main commit changes empty-chat-answer recovery (#470), not
these capability interfaces. Search indexing returned an older revision;
publication file reads used the explicit main commit rather than that index.

No W047 decision record appeared in the decision inventory; this PR assigns
W047. The proposal remains proposed until repository review accepts it.
Companion amendments are explicitly labelled proposals and become governing
only with this ADR's acceptance. None declares implementation complete.

Local verification checks Markdown structure, local links, TypeScript
contract examples, tool-name uniqueness, operation coverage, and unchanged
source/workflow scope. Full-repository checks require the authenticated CI
checkout; this editing workspace contains a pinned documentation/source
subset rather than a runnable clone. Record CI results in the PR.
