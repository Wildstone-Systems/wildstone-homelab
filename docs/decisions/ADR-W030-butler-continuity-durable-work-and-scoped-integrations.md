# ADR-W030: Butler continuity, durable work, and scoped integrations

- **Status:** proposed — draft for discussion; no implementation or permission changes authorized by this record while proposed.
- **Relates to:** ADR-W004 (Plans), W005/W025 (packages and integrations), W006/W017 (Butler), W007 (Keystone), W009 (policy), W012 (secrets), W020 (identity), W024 (household automation), W026 (messaging), W027 (conversation UX), W028 (failure isolation).
- **Source:** household-owner design discussion, consolidated in [the capability and design brief](../butler-capability-and-design-brief.md).
- **Baseline:** main at `a3362c9881cdf6d652b5e0cb9086597e36347d28`.
- **Audience:** architecture review. Part A becomes binding on acceptance; implementation details remain separate. Acceptance must include the paired amendments listed in B1 in the same accepting revision, including W027 A11. Proposed status authorizes no implementation or permission change.

## Intent and problem

One Butler should remember relevant context, complete work over time, and coordinate applications, while the bot through which a person speaks remains a firm authority boundary.

Wildstone already has a deterministic household engine, an event timeline, approved Automation resources, domestic executors, provider abstraction, audited conversations, chat bindings, and separate family/operator surfaces. W024 already incorporates the earlier agent architecture summary. This ADR extends that foundation rather than introducing another event bus or a replacement automation engine.

Current chat continuity is not a restart-safe task system. A package declaring an agent tool is not enough to supply and authorize its executor. Separate surface histories do not provide the desired experience of one assistant across explicit handoffs. Learned memory, tool requests, and cross-app work need lifecycle and authority contracts before implementation expands them.

The current catalog includes a Roundcube mail/calendar client package. Installing that client does not establish Butler's mailbox access, CalDAV retrieval, calendar writes, or ingestion permissions.

# Part A — Proposed decisions

## A1. One assistant, explicit profiles, explicit handoffs

Household, Operator, and a future Child profile share the assistant identity but expose different capabilities.

- Household offers scoped household knowledge and permitted app actions.
- Operator offers Household capabilities plus administrative tools. Host-state changes remain human-approved Plans; no approve capability or arbitrary host shell is added.
- Child offers guardian-approved knowledge and limited actions. Guardian review of child conversations is disclosed at setup; other adults do not automatically receive that access.

Homepage or app access does not grant Butler membership. Adults admitted to Household have equal shared-memory standing; personal privacy, operator authority, and guardian relationships remain separate concerns. Do not build a general adult role hierarchy for v1.

The current bot sets the execution boundary. Household cannot invoke Operator tools in the background, even for an operator. Handoff is explicit and continues context in the destination after identity and profile checks; it does not execute the pending operation automatically.

## A2. Shared history does not mean shared disclosure

Store multiple conversations in one logical history. Ordinary messages continue the active conversation; explicit references can resume another. Prefer clear recent matches, ask when ambiguous, and retain the conversation identity across handoffs.

Retrieval is constrained by authenticated person, profile, and audience before content reaches the model. Shared storage must not expose adult-private or operator information through Child or household-group sessions. Summaries, cached context, and learned procedures inherit the restrictions of their sources. Do not reuse a previously constructed privileged model context across a boundary change.

Keep named app-account mappings to one Wildstone identity. The model cannot assert acting identity. Shared app credentials retain the requesting person's attribution.

### A2.1 Identity admission and audience defaults

Maintain an explicit runtime link between an authenticated API/chat principal and a household member; equal names are not proof of identity. An authenticated administrator establishes or changes the link through a human-approved Plan in Console/CLI; its runtime projection supplies dispatch checks. Butler cannot grant itself manage authority. Keep original principal/member attribution immutable in history; do not reinterpret old authorship when a link changes.

New Butler admission and authority-increasing guardian/classification changes likewise require a human-approved Plan. An unlinked principal has no household-person access. Butler admission and disabled status are runtime membership controls, separate from LDAP account deletion or group membership. Check current membership and link generation at every retrieval and dispatch. Unlink or disable revokes derived grants and pauses affected tasks immediately; relinking requires fresh authorization. Household admission does not grant Operator access.

Trusted admitted adults retain shared household timeline access in direct messages, including other adults' household events. Explicitly private sources remain restricted. Preserve W026's conservative group/member filtering; a group audience does not inherit the sender's full direct-message access.

For the existing household timeline, registered legacy event types retain their household-shared audience under an explicit migration mapping; no blanket historical relabeling is required. That mapping also explicitly includes Keystone-owned automation lifecycle events and household-answer events from the existing chat/notification paths, which are not package-declared. Recognize trusted producer paths and event types together; a caller-supplied internal source name is not proof of origin. Extend package `publishes` declarations with audience policy: household-shared by default for declared household events, or a narrower validated audience. Persist the resolved policy/provenance at intake and preserve subsequent access revocations. Unknown event types, invalid audience declarations, imported personal sources, and missing derivative permission metadata fail closed. Derivatives of legacy events inherit the mapped audience. The timeline is not a general exemption from permission checks.

A future Child surface requires its own bot credentials, principal/role, endpoint, structurally limited gateway, and boundary tests. Guardian mappings and child classification are explicit admission records, never inferred from an LDAP group. Transport eligibility must be checked before enabling that future surface; no child deployment is authorized here.

## A3. Memory is owned data with derived retrieval indexes

Use one logical household memory service, structured records for exact facts and permissions, and hybrid meaning/keyword retrieval for selected text. Application records remain authoritative for current app state; live reads verify changing facts when needed. A vector database product is not selected here.

Butler may learn preferences and reusable procedures automatically. Distinguish explicit statements from tentative inferences; retain source, date, and corrections. Explicit preferences persist until corrected or time-bounded; unused inferences lose influence. Learned procedures confer no permissions and do not enable routines.

Offer configurable daily/weekly learning digests and inspect/correct/forget controls. A private source does not become household knowledge through extraction. Query permission does not imply bulk indexing permission.

Deletion removes retrieval access and solely dependent derivatives; forgetting prevents relearning from the same historical material. Original records survive compaction unless separately deleted. The restore quarantine in A10 is required; an independent deletion/revocation journal is a follow-up implementation option.

### A3.1 Memory acceptance boundaries

Use the shared redaction path before persistent memory storage, including exclusion of credentials and password-reset material. Source restrictions apply before extraction and persist on every derivative. When sources have different restrictions, a derivative must satisfy all of them; missing permission metadata fails closed.

Test poisoned inferences, malicious learned procedures, and attempted private-to-household extraction as distinct cases. An inference never becomes authorization; a learned procedure never bypasses current tools or approvals. Attribute a group statement such as “I dislike cilantro” to the authenticated sender, with its source audience recorded separately; it is not a preference of everyone in the group.

## A4. Tasks and routine runs share durable execution machinery

Use one execution foundation in Keystone for conversational tasks and Wildstone-owned routine runs. Retain routine definitions separately from each occurrence. Model-free steps remain model-free; agent steps receive bounded context and grants.

Recommended starting point: runtime-DB task/run rows, step/attempt rows, and transition history. Record owner, conversation, profile, objective, completion criteria, grants, deadline, next wakeup, results, and uncertainty. Exact schemas, claim/lease design, and migration are deferred to an implementation specification.

Replies, approvals, schedules, and unavailable models are saved waiting reasons. Cancellation stops new actions and attempts to interrupt in-flight work. Refinements preserve task history; separate objectives create separate tasks. Independent work may run together; incompatible shared changes pause for clarification rather than assigning one adult priority.

Resume relevant authorized work after restart. Do not blindly retry actions whose outcomes are unknown. Use destination verification or supported idempotency mechanisms; otherwise pause. Preserve partial successes and report remaining work. Completion requires evidence appropriate to the objective, not merely a model saying done. Notification delivery is separate from task completion.

This does not claim exactly-once external execution. The W024 timeline remains authoritative for deterministic automation decisions and outcomes; task rows own agent steps, waits, attempts, and completion evidence. Link records by occurrence/action IDs and treat copied status as a projection, not a second authoritative result. Deterministic timeline replay must still work with an empty task table. Existing `automation.acted` records represent dispatch claims, not proof of successful external effects; migration must preserve that distinction.

Checkpoint each tool invocation before dispatch and after its result. Resume from the validated step ledger, reconstructing context with current permissions, rather than replaying a saved model/tool transcript. Persist model-call, tool-call, and active-work budgets across restarts. Exhausted budgets wait for review; they do not reset on retry. Waiting does not consume active-work budget, while absolute deadlines still expire.

Expose per-surface interactive iteration limits as validated runtime configuration in Console/API using the W017 A9 pattern. Initial configurable defaults are 16 rounds for Household and 24 for Operator, subject to implementation evaluation; count individual tool calls separately because one model round can request several. Snapshot limits onto the task. A turn reaching its slice limit checkpoints and waits to continue without discarding evidence or resetting aggregate task budgets. An authenticated “continue” resumes the unambiguous originating task; otherwise ask which task. Continuation grants another bounded slice only within the existing total budget, never action approval or extra authority. Exhausted total budgets require explicit review of a bounded extension.

Provider failures and stalls retain the same resumable ledger as restarts. Enforce a configurable provider-stall deadline, cancel/fence the abandoned attempt, and reject late results from superseded attempts. Retry or fall back automatically within remaining budgets before involving the person. If still blocked, send a deduplicated notice on the originating surface with a task-bound continue control/text path, respecting notification policy. Console distinguishes stalled/resumable work from terminal failure. Refresh stale observations on resume; do not repeat already-verified effects.

## A5. Common contracts, application-specific adapters, named connections

An integration declares events it reports, queries it answers, and actions it performs. Capabilities can be partial. Responses distinguish valid results, including empty lists, from unavailable/stale information and carry source and observation time.

Application-specific code belongs in adapters outside Keystone; native publishers need no additional publishing process. Support both packages integrating Wildstone-managed apps and connection-only packages for externally hosted apps. Connecting does not take ownership of the remote application. Adapters may run on a managed machine that can reach the app, not necessarily the app's host.

An adapter implementation supports multiple named connections, each with endpoint, credential references, identity mapping, and grants. Credentials follow the secret path and never enter model context.

Verify available capabilities against app version, configuration, and granted access. Discovery does not authorize newly available permissions. Outages mark capabilities unavailable and dependent work waiting.

Keep event intake, on-demand queries, and optional RAG indexing distinct. Preserve the canonical household timeline and SQLite bus decision in W024. No new broker or log-parsing contract is selected.

### A5.1 Adapter transport and personal credentials

Reuse the long-poll/ack pattern of the existing chat outbound queue, with a separate durable integration-work queue and dedicated claim/result endpoints; do not overload `chat_outbound` or chat's inbound message path. A waiting poll wakes when work arrives; queries have explicit deadlines and return unavailable on timeout. Keystone needs no new inbound adapter-controlled administration surface and makes no app-specific outbound calls. Adapter principals can claim/report only work assigned to their named connections; they gain no general query, act, or manage authority. Versioned request/result envelopes carry correlation IDs and validated capability/scope metadata. Introduce the closed `integration-adapter` role with `claim-integration-work` and `report-integration-result` verbs, connection-bound by Keystone. This does not expand `emit` or `converse`. The interactive query deadline defaults to 5 seconds end-to-end, including queue delay, configurable within the caller's remaining budget. Exact endpoint and lease schemas remain implementation work.

V1 adapters execute as package-managed containers on Wildstone nodes that can reach the external app. Resolve secret references through the existing node secret path into the adapter's scoped environment; secret values never travel through work/result queues. Connection-only describes ownership of the remote app, not an unmanaged adapter runtime. Running adapters on non-node machines requires a later credential/identity design.

The existing home-automation emitter is not a Home Assistant query/action adapter. Add that capability explicitly in an adapter with declared permissions, independently of whether Wildstone hosts Home Assistant.

Personal CalDAV/IMAP access requires member-scoped credential onboarding through an authenticated secret-input surface, following W020's claim/onboarding pattern and W012's secret handling. Store credentials through the node secret path; adapters receive scoped references, never model-visible values. Keep member, connection, requesting principal, and revocation linkage. Do not collect passwords through chat. Revocation invalidates future dispatch and ingestion. Where a provider cannot issue read-only credentials, enforce read-only adapter operations and disclose the credential's wider upstream power. Personal CalDAV rollout depends on this onboarding; fixtures can precede it.

## A6. Native automation where appropriate, Wildstone for cross-app coordination

Butler authors native routines through supported app interfaces when that app owns the workflow. Wildstone executes cross-app routines using incoming events and targeted queries. Track external routine IDs and edits; do not silently duplicate or migrate execution.

The platform supplies schedules/events, conditions, uncertainty handling, repeat controls, typed actions, replies, preview, trial mode, versioning, and audit. Breakfast and appointment examples in the brief are acceptance scenarios, not hardcoded product features. Timing, destinations, eligibility, and missing-data choices belong to user-authored routines.

Preview without side effects where historical evidence permits; disclose unavailable historical context. Native routine creation, behavioral edits, and deletion use Plan-mediated declarations that record the connection, external ID, ownership, and approved behavior. Removing a connection or declaration does not silently delete an external routine. Wildstone-owned routines retain approved Automation resources.

Local disable immediately blocks further Wildstone dispatch. It cannot by itself stop a routine running inside another application. A native stop requires a supported, explicitly preauthorized stop operation or a new Plan; show pending/failed remote stops honestly. A routine's activation approval can include a narrowly scoped stop operation, never an implicit delete or edit permission.

## A7. Admin-controlled tools and requests for missing capabilities

Provide an admin-only Console tool surface for each profile, grouped by named connection. Show tool purpose, data access, mutating behavior, approval requirements, availability, grants, and dependencies.

Disabling a tool both removes its advertised specification and rejects subsequent dispatch, including from saved sessions and routine runs. Cached tool lists are not authority. In-flight effects may be irreversible; report them.

Household excludes shell execution, arbitrary code, unrestricted filesystem search, credential access, and infrastructure administration. Toggles and temporary grants cannot cross that ceiling. A read tool also needs scope: searching connected recipes differs from searching every host file.

Butler can create durable requests linked to a blocked task:
1. Enable a supported but unavailable tool with proposed scope.
2. Develop a missing capability with behavior and acceptance criteria.

The admin may approve, narrow, or reject. Requests confer no authority. Development approval is separate from installation and enablement. Automatic tool development and self-modification are deferred.

One-off grants are task-bound and temporary by default, ending at completion, cancellation, or deadline. Permanent profile enablement is a separate choice. Approved routines hold scoped grants for declared tools/connections/actions while enabled; these are not general conversational grants. All grants remain subordinate to current profile ceilings, tool switches, person access, and revocation.

### A7.1 Structural enforcement and control-plane state

Compose Operator's household and administrative gateways explicitly. Runtime filtering only narrows each surface's structurally available capabilities; it cannot turn Household into Operator. Use one effective-authority resolver for dispatch, intersecting surface ceiling, authenticated membership, connection permission, enabled tools, grants, action approval, and current model verdict. Add structural tests rejecting direct handler execution that bypasses this resolver.

Extend the existing minimal tool manifest with purpose, input schema, data/action scope, mutating behavior, and approval requirements. A declaration alone still supplies no executor. Generate the offered capability inventory and supported resource-kind enumeration from the active gateway and kind registry, with validated descriptive metadata attached there rather than a second handwritten list. Pin coverage and exclusion in tests, including ConfigFile/SeedFile proposal support and QA-only restrictions. Provide a read-only self-capability tool showing only the caller profile's effective manifest and scoped unavailable-capability reasons, never hidden privileged tools or secrets. Consult it before filing a capability request.

For service configuration, use a ConfigFile Plan for configuration consumed from managed paths, and an authorized adapter settings action for app-owned settings. SeedFile remains write-once initialization; arbitrary edits inside application volumes are rejected. Infrastructure configuration stays Plan-mediated and excluded from household automation.

Add an Operator-only bounded container-log tool using the existing Wardstone `ContainerLogs` operation, not a new protocol arm. Preserve managed-container ownership checks, tail/byte limits, redaction and untrusted-result envelopes, and enforce current profile/node scope. Household never receives this tool.

Permanent profile enablement, identity links, and Butler admission are Plan-mediated standing authority increases, with runtime projections checked at use. Butler can propose only changes permitted by its own gateway and can otherwise file a request; it cannot approve them. Temporary bounded task grants remain audited runtime records approved by an authenticated human with manage authority. All disables/revocations are immediate runtime denials, including unlink, and override desired-state reconciliation: a previously approved declaration cannot silently re-enable revoked access. Re-enabling requires a fresh explicit Plan. Revoking a declared standing permission also creates or links a proposal to remove that declaration; Console shows “revoked, declaration pending removal” until the human-approved cleanup completes. Failed or rejected cleanup never lifts the runtime denial. Apply normal Plan risk classification to the actual diff rather than automatically assigning every cleanup Low risk. Host state and standing routine declarations remain Plan-mediated; routine grants remain subordinate to current denials.

Chat can show a Plan ID and a validated deep link to its Console approval page. The link carries no approval authority or bearer credential; Wildstone authentication and current Plan review are still required. No chat callback approves a Plan or grants administrative access.

## A8. Tool access and action approval are separate

A clear request can authorize a permitted low-impact household action. Consequential operations require approval of the exact proposal. Household Telegram approvals are bound to the approving identity and proposal version, expire, and cannot execute twice from duplicate clicks. Personal changes require the person; shared changes allow an authorized adult; restricted child requests require a guardian.

Infrastructure approval remains in Console/CLI. Enabling a tool, receiving a document, or accepting a learned procedure does not approve an action. Retrieved content is untrusted input, including email instructions and event payloads.

### A8.1 Approval transport

Do not interpret ordinary chat answers or the most recent pending question as approval. Each proposal carries a stable ID, revision, intended approver, exact action/scope, expiry, and single-use approval token. Bind callback handling to authenticated direct-message identity and consume approval atomically with dispatch authorization. Group messages cannot approve.

Telegram callbacks require an explicit adapter/API extension; the current free-text answer claim path is insufficient. A text fallback must name the exact proposal ID and revision and pass the same identity, expiry, and duplicate checks. Edits invalidate earlier tokens. Delivery acknowledgement is separate from human approval.

## A9. Model choice does not change identity or authority

Both adult profiles can use local or cloud models. Configure named endpoints/models, processing location, permitted data, and fallback order. A remote Ollama endpoint is not proof of either local or cloud inference.

Allow automatic escalation within configured alternatives and bounded attempts. Local-only information cannot follow a cloud fallback. Background embeddings, extraction, and compaction prefer local processing where quality is adequate. Deterministic operations continue without models.

Use configurable endpoint concurrency and interactive capacity, with lower-priority ingestion/compaction. Defer cost tracking and account-limit enforcement: returned usage metadata does not establish remaining shared provider allowance.

### A9.1 Data routing and model readiness

Cloud use is an explicit profile and data-policy opt-in for either adult profile; local-only sources cannot be sent there. Background processing prefers local models but may use cloud only when its own job policy and source permissions allow it. Future Child cloud routing requires an explicit guardian/administrator policy. This deliberately amends the vision's blanket family-local-only wording.

Every fallback model must pass its own capability/readiness assessment. A QA-only verdict cannot inherit the prior model's action authority. Recompute effective authority after model selection; pending writes wait for a permitted capable model or human intervention. Provider availability never widens permission.

## A10. Communication, retention, and recovery are explicit policies

Allow proactive messages, with deduplication, relevance checks, quiet hours, and appropriate bot routing. Quiet hours use the configured household timezone and support per-person overrides; urgent exceptions require explicit designation. The owner's preferred hours are recorded as a deployment example in the brief. These are configurable policies, not special-purpose routine logic.

Task updates go to the owner, private messages stay private, and unassigned notices default to the configured admin without granting access to private content. Replies correlate to the originating question; ambiguous or expired answers do not authorize stale actions. Silence is never approval.

Back up tasks, conversations, memories, grants, mappings, routines, procedures, and access settings. Reconcile restored actions against external state before resuming. Restore into quarantine: no Butler retrieval, task dispatch, ingestion, or proactive delivery until an administrator reconciles authority and deletion state. Reapply available later deletions/revocations; an old backup cannot supply missing later decisions. Where reconciliation cannot establish current permission or deletion status, keep affected content unavailable and require fresh authorization and, where appropriate, reindex from current permitted sources. Administrative acknowledgement alone is not evidence that missing deletions were recovered. Conversation/task-summary retention defaults to manual deletion with configurable cleanup.

### A10.1 Undelivered notices and recovery evidence

Track delivery acknowledgement separately from a human response. A completed task whose notice expires remains completed, with the notice marked expired/undelivered and visible in Console; it must not become an unanswered approval wait. Waiting-for-human tasks remain waiting under their own deadline policy. No expired notice or silence approves work.

Durable Butler runtime state becomes a fourth recovery category alongside the existing recovery inputs; it is no longer wholly rebuildable. Amend W018 and the recovery guide to cover task ledgers, conversation/memory data, bindings, grants, and access controls. Replication lag is a recovery-point limit, not proof of zero loss. The recovery drill must restore an older snapshot containing subsequently revoked access/deleted content, demonstrate quarantine, and prove uncertain content and pending actions remain blocked until reconciliation.

# Part B — Compatibility, review questions, and consequences

## B1. Companion amendments included in this PR

| Existing contract | Proposed change or preserved constraint |
|---|---|
| W026 A1: two surfaces, separate histories/tool sets | A1/A2 propose shared filtered continuity, Operator's Household-plus-admin capability, and a Child profile. Add a separately secured third surface when Child ships and explicit gateway composition with narrowing-only runtime filters. Separate credentials, audience checks, and explicit handoffs remain. |
| W026 A5: no infrastructure approval over chat | Preserved. A8 adds exact household-action approvals only; no Plan approval through Telegram. |
| W024 A5/A6 and H6: narrow domestic execution and tool-free phrasing | Preserve the deterministic path and restricted Phraser. General durable agent steps need a distinct enforced grant/context boundary, not silent widening of H6. |
| W024 A10 / W025: integration lifecycle belongs to the service package | Managed apps retain that lifecycle. Connection-only packages add an explicit lifecycle for external apps; removing a connection must not remove the external app. |
| W006/W017: bounded provider loop and structural operator ceiling | Preserve no-approve/no-arbitrary-host-exec. Add runtime durability, discovery, grant checks, and request records outside the model. |
| W004/W007/W009: public API, Plans, and policy | Preserve public API parity and host-state Plan mediation. A6/A7.1 distinguish Plan-mediated standing authority/routines/host state from bounded temporary grants, immediate persistent runtime denials, and explicitly authorized stop operations. |
| Scope §7 and vision | Record the delivery pull-forward and explicit per-profile/data cloud opt-in in A9.1. |
| W012 / W020 identity and secrets | Add principal/member links, Butler admission/disable, guardian mappings, and member-scoped credential onboarding while preserving the write-only node secret path; do not merge identity stores by name. |
| W018 and docs/recovery.md | Add durable Butler runtime state, recovery-point limits, quarantine, and a restore/revocation exit drill. |
| W005 tool declarations / W024 A10 publications | Expand registry-backed tool metadata and publication audience policy, including explicit legacy timeline mapping. Declarations are neither executors nor grants. |
| W007 A2 closed roles | Add connection-bound integration-adapter claim/result verbs; preserve no general act/manage privilege. |
| W017 A9 / W006 bounded loop | Add per-surface runtime iteration limits, persisted budgets, provider-stall recovery and task-bound continuation. |
| W003 / W006 Operator reads | Reuse existing ContainerLogs wire operation and bounds; expand only the Operator gateway/reader with redacted, scoped logs. No new protocol arm is proposed. |
| W027 A11 conversation contract | Amend shared conversation continuity and permission-aware retrieval explicitly in the accepting revision. |

Each record named above now carries a conditional W030 amendment or an explicit preservation note. Those sections become binding only when W030 is accepted; until then the existing contracts remain authoritative. Do not weaken existing structural tests to fit the proposal. Implementation requires replacement/expanded boundary tests where behavior changes. The tree currently contains two ADR-W029 filenames; this proposal references them by title/path if needed and does not renumber unrelated history.

## B2. Remaining implementation questions

- Transactional schemas, claim leases, wakeups, ledger/timeline correlation, and migration without duplicate external effects.
- Versioned adapter endpoints, result schemas, cancellation, and bounded query latency under load.
- Permission-label representation, derivative invalidation, and independent deletion/revocation journal design. Restore quarantine is required regardless of journal choice.
- Local retrieval/index technology, compaction quality, and measured resource limits.
- The smallest complete implementation slice and its acceptance evidence.

These details cannot relax Part A. The paired B1 amendments are present for review; implementation PRs must resolve their applicable schema and migration questions.

## B3. Alternatives and consequences

**Separate unrelated agents and memories:** simpler isolation, but rejected as the product direction because it loses continuity across the owner's explicit bot handoffs.

**One universal tool belt with model-selected privilege:** rejected; authority must remain outside the model and the Household bot must not silently become Operator.

**Copy every app database into RAG:** rejected; duplicates authoritative state, broadens ingestion, and hides freshness. Events, selected indexing, and live reads serve different purposes.

**Replace Wildstone with an external agent runtime:** not selected. Reuse existing policy, package, Plan, and event foundations first; an embedded/external runtime would require a separate compatibility decision.

**A new workflow broker or orchestration platform now:** deferred pending evidence that extending the runtime DB is inadequate.

The costs are durable-state complexity, permission-aware retrieval, app-specific adapter maintenance, and more recovery cases. Benefits are continuity, explainable cross-app work, reusable integration contracts, and a path to requesting capabilities without self-escalation.

# Part C — Delivery and acceptance direction

V1 targets text via existing bots/Console, shared filtered continuity, selected-source memory, durable work, scoped integrations/grants, capability requests, and configurable model routing. Home Assistant and read-only CalDAV are an initial cross-app path; PantryOS's forthcoming API can be tested with a fake until available. Child behavior is specified but must not be enabled until its own checks pass.

Follow-up work: dedicated receipt/screenshot/PDF intake, and selected read-only IMAP monitoring with review-first calendar writes. Calendar invitations/RSVPs remain separately controlled.

Voice is post-v1: local transcription preferred, Kokoro preferred for TTS, using the same identity and conversation interfaces. Cost enforcement, unrestricted execution, automatic tool development, and autonomous agent teams remain deferred.

Implementation PRs should prove:
- Same-person handoff continuity without cross-audience leakage or implicit elevated execution.
- Temporary grant expiry, tool disable, and identity revocation at dispatch, including paused/restarted work.
- A deterministic cross-app run with the model off, an unavailable source, duplicate events, and an uncertain action outcome.
- Review-bound household writes distinct from infrastructure Plan approval.
- Memory correction/deletion propagated to retrieval and derived content, including restoration behavior.
- Adapter behavior against fakes and a limited real deployment, not merely a declaration that lints.

The accompanying brief provides non-normative rationale, deployment examples, and delivery sequencing. This ADR alone defines the proposed architectural contract.
