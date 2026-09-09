# ADR-W030: Butler continuity, durable work, and scoped integrations

- **Status:** proposed — draft for discussion; no implementation or permission changes authorized by this record while proposed.
- **Relates to:** ADR-W004 (Plans), W005/W025 (packages and integrations), W006/W017 (Butler), W007 (Keystone), W009 (policy), W012 (secrets), W020 (identity), W024 (household automation), W026 (messaging), W027 (conversation UX), W028 (failure isolation).
- **Source:** household-owner design discussion, consolidated in [the capability and design brief](../butler-capability-and-design-brief.md).
- **Baseline:** main at `2e45b0db21ec644fc13c0cb10dc094e4c1408e12`.
- **Audience:** architecture review. Part A becomes binding on acceptance; Part C is the ordered, non-binding implementation plan and may be refined without weakening Part A. Acceptance must include the paired amendments listed in B1 in the same accepting revision, including W027 A11. Proposed status authorizes no implementation or permission change.

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

Equal adult standing includes creating, changing, and disabling shared household routines and correcting or forgetting household-shared memory without approval from the original author. Preserve author, last editor, version, and audit history, and notify the routine owner of another adult's behavioral change. Corrections and forgets of household-shared memory are audited and appear in the next configured learning digest for the other admitted adults; removal of a safety-relevant record also produces an immediate notice. Personal/private memory remains controllable by the person it describes and the configured homelab administrator; ordinary adult trust does not silently widen a private source. Safety-relevant records such as allergies show their source and scope rather than disappearing silently.

The current bot sets the execution boundary. Household cannot invoke Operator tools in the background, even for an operator. Handoff is explicit and continues context in the destination after identity and profile checks; it does not execute the pending operation automatically.

## A2. Shared history does not mean shared disclosure

Store multiple conversations in one logical history. Ordinary messages continue the active conversation; explicit references can resume another. Prefer clear recent matches, ask when ambiguous, and retain the conversation identity across handoffs.

Retrieval is constrained by authenticated person, profile, and audience before content reaches the model. Shared storage must not expose adult-private or operator information through Child or household-group sessions. Summaries, cached context, and learned procedures inherit the restrictions of their sources. Do not reuse a previously constructed privileged model context across a boundary change.

Keep named app-account mappings to one Wildstone identity. The model cannot assert acting identity. Shared app credentials retain the requesting person's attribution.

### A2.1 Identity admission and audience defaults

Maintain an explicit runtime link between an authenticated surface identity and a household member; API principals, chat bindings, and trusted-ingress identities are named link types, and equal names are not proof of identity. An authenticated administrator establishes or changes the link through a human-approved Plan in Console/CLI; its runtime projection supplies dispatch checks. Butler cannot grant itself manage authority. Keep original surface/member attribution immutable in history; do not reinterpret old authorship when a link changes.

New Butler admission and authority-increasing guardian/classification changes likewise require a human-approved Plan. An unlinked surface identity has no household-person access. Butler admission and disabled status are runtime membership controls, separate from LDAP account deletion or group membership. Check current membership and link generation at every retrieval and dispatch. Unlink or disable revokes derived grants and pauses affected tasks immediately; relinking requires fresh authorization. Household admission does not grant Operator access.

Offboarding starts with one audited administrator action that immediately disables Butler admission, revokes chat bindings/sessions and temporary grants, fences the member's connections, and prevents new retrieval, dispatch, ingestion, and proactive delivery. Desired-state cleanup follows through linked Plans; failed cleanup never restores access. A sole guardian cannot be removed while a Child admission remains active: assign another guardian or disable the Child admission first.

Trusted admitted adults retain shared household timeline access in direct messages, including other adults' household events. Explicitly private sources remain restricted. Preserve W026's conservative group/member filtering; a group audience does not inherit the sender's full direct-message access.

For the existing household timeline, registered legacy event types retain their household-shared audience under an explicit migration mapping; no blanket historical relabeling is required. That mapping also explicitly includes Keystone-owned automation lifecycle events and household-answer events from the existing chat/notification paths, which are not package-declared. Recognize trusted producer paths and event types together; a caller-supplied internal source name is not proof of origin. Extend package `publishes` declarations with audience policy: household-shared by default for declared household events, or a narrower validated audience. Persist the resolved policy/provenance at intake and preserve subsequent access revocations. Unknown event types, invalid audience declarations, imported personal sources, and missing derivative permission metadata fail closed. Derivatives of legacy events inherit the mapped audience. The timeline is not a general exemption from permission checks.

A future Child surface requires its own bot credentials, principal/role, endpoint, structurally limited gateway, and boundary tests. Guardian mappings and child classification are explicit admission records, never inferred from an LDAP group. Transport eligibility must be checked before enabling that future surface; no child deployment is authorized here.

## A3. Memory is owned data with derived retrieval indexes

Use one logical household memory service, structured records for exact facts and permissions, and hybrid meaning/keyword retrieval for selected text. Application records remain authoritative for current app state; live reads verify changing facts when needed. A vector database product is not selected here.

Butler may learn preferences and reusable procedures automatically. Distinguish explicit statements from tentative inferences; retain source, date, and corrections. Explicit preferences persist until corrected or time-bounded; unused inferences lose influence. Learned procedures confer no permissions and do not enable routines.

Offer configurable daily/weekly learning digests and inspect/correct/forget controls. The digest includes another adult's corrections and forgets of household-shared memory; safety-relevant removals do not wait for the digest. A private source does not become household knowledge through extraction. Query permission does not imply bulk indexing permission.

Deletion removes retrieval access and solely dependent derivatives; forgetting prevents relearning from the same historical material. Original records survive compaction unless separately deleted. The restore quarantine in A10 is required; an independent deletion/revocation journal is a follow-up implementation option.

While admitted, a person can export their own conversations, personal memory, preference/procedure records, and task history in documented machine-readable form. The export includes provenance and permissions but no other person's private data, credentials, hidden Operator context, or shared data the requester cannot currently read. On offboarding, personal conversations and memory become inaccessible rather than becoming household-shared. They remain quarantined until the configured retention period expires or an authorized deletion request completes; without an explicit period, the existing manual-deletion default applies. Shared household facts remain with original attribution.

### A3.1 Memory acceptance boundaries

Use the shared redaction path before persistent memory storage, including exclusion of credentials and password-reset material. Source restrictions apply before extraction and persist on every derivative. When sources have different restrictions, a derivative must satisfy all of them; missing permission metadata fails closed.

Test poisoned inferences, malicious learned procedures, and attempted private-to-household extraction as distinct cases. An inference never becomes authorization; a learned procedure never bypasses current tools or approvals. Attribute a group statement such as “I dislike cilantro” to the authenticated sender, with its source audience recorded separately; it is not a preference of everyone in the group.

## A4. Tasks and routine runs share durable execution machinery

Use one execution foundation in Keystone for conversational tasks and Wildstone-owned routine runs. Retain routine definitions separately from each occurrence. Model-free steps remain model-free; agent steps receive bounded context and grants.

Recommended starting point: runtime-DB task/run rows, step/attempt rows, and transition history. Record owner, conversation, profile, objective, completion criteria, grants, deadline, next wakeup, results, and uncertainty. Exact schemas, claim/lease design, and migration are deferred to an implementation specification.

Replies, approvals, schedules, and unavailable models are saved waiting reasons. Cancellation stops new actions and attempts to interrupt in-flight work. Refinements preserve task history; separate objectives create separate tasks. Independent work may run together; incompatible shared changes pause for clarification rather than assigning one adult priority.

Offboarding moves unfinished personal tasks to `paused-offboarded`; they perform no retries, notices, or model calls. Shared tasks created by that member transfer operational ownership to the configured homelab administrator, preserve original authorship, and pause for grant/connection review before resuming. Task retention then follows the same configured offboarding policy as its conversation.

Resume relevant authorized work after restart. Do not blindly retry actions whose outcomes are unknown. Use destination verification or supported idempotency mechanisms; otherwise pause. Preserve partial successes and report remaining work. Completion requires evidence appropriate to the objective, not merely a model saying done. Notification delivery is separate from task completion.

This does not claim exactly-once external execution. The W024 timeline remains authoritative for deterministic automation decisions and outcomes; task rows own agent steps, waits, attempts, and completion evidence. Link records by occurrence/action IDs and treat copied status as a projection, not a second authoritative result. Deterministic timeline replay must still work with an empty task table. Existing `automation.acted` records represent dispatch claims, not proof of successful external effects; migration must preserve that distinction.

Checkpoint each tool invocation before dispatch and after its result. Resume from the validated step ledger, reconstructing context with current permissions, rather than replaying a saved model/tool transcript. Persist model-call, tool-call, and active-work budgets across restarts. Exhausted budgets wait for review; they do not reset on retry. Waiting does not consume active-work budget, while absolute deadlines still expire.

Expose per-surface interactive iteration limits as validated runtime configuration in Console/API using the W017 A9 pattern. Initial configurable defaults are 16 rounds for Household and 24 for Operator, subject to implementation evaluation; count individual tool calls separately because one model round can request several. Snapshot limits onto the task. A turn reaching its slice limit checkpoints and waits to continue without discarding evidence or resetting aggregate task budgets. An authenticated “continue” resumes the unambiguous originating task; otherwise ask which task. Continuation grants another bounded slice only within the existing total budget, never action approval or extra authority. Exhausted total budgets require explicit review of a bounded extension.

Provider failures and stalls retain the same resumable ledger as restarts. Enforce a configurable provider-stall deadline, cancel/fence the abandoned attempt, and reject late results from superseded attempts. Retry or fall back automatically within remaining budgets before involving the person. If still blocked, send a deduplicated notice on the originating surface with a task-bound continue control/text path, respecting notification policy. The P4 minimum sends only to the task owner's originating direct surface, reuses the existing outbound deduplication key, and holds non-urgent stalled-task notices during the household-timezone quiet hours; P15 adds per-person overrides, broader routing, relevance, urgent exceptions, and reply correlation. Console distinguishes stalled/resumable work from terminal failure. Refresh stale observations on resume; do not repeat already-verified effects.

## A5. Common contracts, application-specific adapters, named connections

An integration declares events it reports, queries it answers, and actions it performs. Capabilities can be partial. Responses distinguish valid results, including empty lists, from unavailable/stale information and carry source and observation time.

Application-specific code belongs in adapters outside Keystone; native publishers need no additional publishing process. Support both packages integrating Wildstone-managed apps and connection-only packages for externally hosted apps. Connecting does not take ownership of the remote application. Adapters may run on a managed machine that can reach the app, not necessarily the app's host.

An adapter implementation supports multiple named connections, each with endpoint, credential references, identity mapping, and grants. Credentials follow the secret path and never enter model context.

Each connection declares one primary adapter node and an ordered, explicitly approved set of eligible fallback nodes. The approving placement Plan deploys and starts the adapter container and provisions required connection secrets on every listed node before that node becomes eligible. One lease epoch permits only the current holder to claim work; the other running containers remain ready but inactive. Keystone may automatically move only that lease within the prepared set when the primary is unavailable. It never creates a container, selects an undeclared node, or copies credentials during an outage. Stale adapters/results from an earlier epoch are fenced. Automatic failback waits for a configurable healthy stabilization period to avoid flapping. When no eligible node is available, dependent work waits and Butler reports the outage.

Connection defaults follow ownership. An individual account starts personal, read-only, live-query-only, and local-processing-only until its owner explicitly widens indexing, audience, actions, or cloud use. A service explicitly connected as household infrastructure starts household-shared and read-only; selected RAG indexing is an explicit setup choice, and cloud routing still requires the data policy opt-in from A9. Discovered actions remain disabled until separately enabled and granted.

Verify available capabilities against app version, configuration, and granted access. Discovery does not authorize newly available permissions. Outages mark capabilities unavailable and dependent work waiting.

Keep event intake, on-demand queries, and optional RAG indexing distinct. Preserve the canonical household timeline and SQLite bus decision in W024. No new broker or log-parsing contract is selected.

### A5.1 Adapter transport and personal credentials

Reuse the long-poll/ack pattern of the existing chat outbound queue, with a separate durable integration-work queue and dedicated claim/result endpoints; do not overload `chat_outbound` or chat's inbound message path. A waiting poll wakes when work arrives; queries have explicit deadlines and return unavailable on timeout. Keystone needs no new inbound adapter-controlled administration surface and makes no app-specific outbound calls. Adapter principals can claim/report only work assigned to their named connections; they gain no general query, act, or manage authority. Versioned request/result envelopes carry correlation IDs and validated capability/scope metadata. Introduce the closed `integration-adapter` role with `claim-integration-work` and `report-integration-result` verbs, connection-bound by Keystone. This does not expand `emit` or `converse`. The interactive query deadline defaults to 5 seconds end-to-end, including queue delay, configurable within the caller's remaining budget. Exact endpoint and lease schemas remain implementation work.

V1 adapters execute as package-managed containers on Wildstone nodes that can reach the external app. Resolve secret references through the existing node secret path into the adapter's scoped environment; secret values never travel through work/result queues. Connection setup may fan one authenticated secret input directly over mTLS to the explicitly eligible Wardstone nodes without persisting it in Keystone. Adding a fallback is a Plan and is incomplete until the adapter container is running, its secret is provisioned, and readiness is verified. Removing one immediately fences its lease, stops and removes the adapter container so it cannot retain or rematerialize the credential, then verifies local secret erasure and, where the remote service permits separate credentials, revokes that node's credential. If container removal, erasure, or revocation cannot be proven, removal remains incomplete and the connection reports degraded. Connection-only describes ownership of the remote app, not an unmanaged adapter runtime. Running adapters on non-node machines requires a later credential/identity design.

The existing home-automation emitter is not a Home Assistant query/action adapter. Add that capability explicitly in an adapter with declared permissions, independently of whether Wildstone hosts Home Assistant.

Personal CalDAV/IMAP access requires member-scoped credential onboarding through an authenticated secret-input surface, following W020's claim/onboarding pattern and W012's secret handling. Store credentials through the node secret path; adapters receive scoped references, never model-visible values. Keep member, connection, requesting principal, and revocation linkage. Do not collect passwords through chat. Revocation invalidates future dispatch and ingestion. Where a provider cannot issue read-only credentials, enforce read-only adapter operations and disclose the credential's wider upstream power. Personal CalDAV rollout depends on this onboarding; fixtures can precede it.

Offboarding disables every personal connection and starts verified credential erasure/revocation on all eligible nodes. A household-shared connection whose credential belongs to the departing member also pauses: ownership may transfer to the configured administrator, but it cannot resume until a current authorized owner supplies or reauthorizes the credential. Connection and routine history retain the departing member's original attribution.

## A6. Native automation where appropriate, Wildstone for cross-app coordination

Butler authors native routines through supported app interfaces when that app owns the workflow. Wildstone executes cross-app routines using incoming events and targeted queries. Track external routine IDs and edits; do not silently duplicate or migrate execution.

The platform supplies schedules/events, conditions, uncertainty handling, repeat controls, typed actions, replies, preview, trial mode, versioning, and audit. Breakfast and appointment examples in the brief are acceptance scenarios, not hardcoded product features. Timing, destinations, eligibility, and missing-data choices belong to user-authored routines.

Preview without side effects where historical evidence permits; disclose unavailable historical context. Native routine creation, behavioral edits, and deletion use Plan-mediated declarations that record the connection, external ID, ownership, and approved behavior. Removing a connection or declaration does not silently delete an external routine. Wildstone-owned routines retain approved Automation resources.

Local disable immediately blocks further Wildstone dispatch. It cannot by itself stop a routine running inside another application. A native stop requires a supported, explicitly preauthorized stop operation or a new Plan; show pending/failed remote stops honestly. A routine's activation approval can include a narrowly scoped stop operation, never an implicit delete or edit permission.

Any trusted admitted adult may propose, change, disable, or request deletion of a shared household routine without the creator's permission. The normal Plan or exact-action approval boundary still applies; adult trust removes an extra owner veto, not the platform's review, version, and safety checks. Concurrent edits use the routine version and require a fresh preview when another change has landed.

Activation, behavioral changes, and deletion are reviewed in an authenticated household-routine page served through the household ingress, reached directly or through a chat deep link. It is not the admin Console. Authelia authenticates the adult; a dedicated `household-reviewer` service principal calls Keystone acting for the explicitly linked member. The member receives no Keystone principal or bearer token. Keystone rechecks the identity link generation, Butler admission, adult classification, Plan revision, and server-side household-routine classifier at decision time. The service principal holds only `review-household-routine-plan`, permitted when every Plan step is an approved household routine declaration/action at read or domestic tier; it excludes infrastructure, ConfigFile/SeedFile, connections or credentials, identity/admission, tool enablement/grants, and other standing authority. A mixed Plan is not split or partially approved. The URL carries no credential or approval token. Any trusted adult may approve this limited Plan; Operator/Admin retain their existing broader Console/CLI review authority.

An authenticated direct Household chat control may immediately disable a specific routine/version because it only removes effective authority. That runtime denial also creates or links the declaration-removal Plan from A7.1. Chat cannot activate, edit, delete, or approve a routine Plan.

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

Chat can show a Plan ID and a validated deep link to the appropriate authenticated review page. The link carries no approval authority or bearer credential; Authelia plus current Butler admission and identity linkage are still required for the household page, while Wildstone administrative authentication remains required for Console. No chat callback approves a Plan or grants administrative access. The household review service holds only the narrow acting-for authority described in A6 and exposes no general Plans or infrastructure administration.

## A8. Tool access and action approval are separate

A clear request can authorize a permitted low-impact household action. Consequential operations require approval of the exact proposal. Household Telegram approvals are bound to the approving identity and proposal version, expire, and cannot execute twice from duplicate clicks. Personal changes require the person; shared changes allow an authorized adult; restricted child requests require a guardian.

The default classification is criteria-based. A low-impact action is narrow, reversible, requested by the affected person, and has no financial, external-communication, access/security, physical-safety, destructive-data, or shared-schedule effect. An action is consequential when it spends or commits money, communicates externally as a person, changes access/security or a physical safety state, deletes non-reconstructable data, or changes a shared calendar/routine with effects on others. Infrastructure and permanent authority changes remain Plans in Console/CLI. Adapter metadata proposes a class, but platform policy validates it and an administrator may make it stricter, never silently weaker.

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

Offboarding records and deletion/retention deadlines are part of the authority state restored before exposure. A restored member, binding, grant, connection, or private-memory index remains quarantined when a later offboarding record may be missing. Export is a read governed by current permissions and never bypasses restore quarantine.

### A10.1 Undelivered notices and recovery evidence

Track delivery acknowledgement separately from a human response. A completed task whose notice expires remains completed, with the notice marked expired/undelivered and visible in Console; it must not become an unanswered approval wait. Waiting-for-human tasks remain waiting under their own deadline policy. No expired notice or silence approves work.

Durable Butler runtime state becomes a fourth recovery category alongside the existing recovery inputs; it is no longer wholly rebuildable. Amend W018 and the recovery guide to cover task ledgers, conversation/memory data, bindings, grants, and access controls. Replication lag is a recovery-point limit, not proof of zero loss. The recovery drill must restore an older snapshot containing subsequently revoked access/deleted content, demonstrate quarantine, and prove uncertain content and pending actions remain blocked until reconciliation.

## A11. One active coordinator, observable work

V1 runs one active Keystone/Butler coordinator and one authoritative runtime database. Do not add active-active writers, automatic leader election, or concurrent task schedulers. Recovery may prepare a standby copy, but promotion is explicit and fenced so the prior coordinator cannot continue dispatching. Adapter placement may fail over as described in A5 because adapters claim work from the active coordinator; when the coordinator is unavailable, Wildstone-owned cross-application work waits. Routines running natively in an application continue under that application's own availability.

Every Butler task produces a structured, append-only work trace that records what the system can actually observe:

- task/conversation/person/profile IDs; model endpoint and capability verdict; prompt, policy, tool-manifest, and configuration versions;
- context-source IDs, audience/permission labels, freshness, and retrieval scores without duplicating source content by default;
- proposed steps, tool calls, redacted/bounded inputs and results, attempt/lease epochs, timing, retries, fallback, stalls, waits, approvals, denials, and completion evidence;
- delivery state, user corrections, explicit feedback, cancellation, and final outcome.

Do not request or persist private chain-of-thought. A short rationale included in a user-visible proposal or answer may be stored as model-authored explanation, not as authoritative reasoning. Trace payloads pass the same redaction and permission checks as their sources; structural metadata may live with task history, while captured content inherits the strictest source retention/deletion policy. Temporary debug capture is explicit, scoped, visibly enabled, and automatically expires. Provider-reported token counts and latency may be recorded for capacity analysis but do not establish cost or remaining account allowance.

Console exposes the trace as a task timeline and supports human labels such as wrong answer, stale read, repeated work, inappropriate tool, permission refusal, excessive interruption, and useful completion. A reviewed incident can produce a sanitized regression fixture/evaluation with expected behavior. Prompt, model routing, tool metadata, policy, and evaluator versions are recorded so before/after results can be compared. Butler may recommend an improvement or file a capability request; it cannot rewrite its prompts, tools, permissions, policies, or evaluations automatically.

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
| W003 A1/A3 and W012 secret lifecycle | Add an idempotent, version-fenced typed connection-secret erasure operation and permit one connection credential on its explicitly approved primary/fallback nodes; values remain absent from Keystone/model/work queues. |
| W018 and docs/recovery.md | Add durable Butler runtime state, recovery-point limits, quarantine, and a restore/revocation exit drill. |
| W005 tool declarations / W024 A10 publications | Expand registry-backed tool metadata and publication audience policy, including explicit legacy timeline mapping. Declarations are neither executors nor grants. |
| W007 A2 closed roles | Add connection-bound integration-adapter claim/result verbs and a non-human household-reviewer service role limited to acting-for review of server-classified household-routine Plans; members receive no Keystone login or general act/manage/approve privilege. |
| W017 A9 / W006 bounded loop | Add per-surface runtime iteration limits, persisted budgets, provider-stall recovery and task-bound continuation. |
| W003 / W006 Operator reads | Reuse existing ContainerLogs wire operation and bounds; expand only the Operator gateway/reader with redacted, scoped logs. No new protocol arm is proposed. |
| W027 A11 conversation contract | Amend shared conversation continuity and permission-aware retrieval explicitly in the accepting revision. |
| W007/W017/W027 runtime and audit views | Keep one active coordinator in v1 and add permission-aware structured work traces, task timelines, feedback labels, and versioned regression evidence without chain-of-thought capture or self-modification. |

Each record named above now carries a conditional W030 amendment or an explicit preservation note. Those sections become binding only when W030 is accepted; until then the existing contracts remain authoritative. Do not weaken existing structural tests to fit the proposal. Implementation requires replacement/expanded boundary tests where behavior changes. The tree currently contains two ADR-W029 filenames; this proposal references them by title/path if needed and does not renumber unrelated history.

## B2. Implementation questions assigned to Part C

- P1/P3/P4 resolve transactional schemas, claim leases, wakeups, ledger/timeline correlation, and migration without duplicate external effects.
- P10 resolves versioned adapter endpoints, result schemas, cancellation, and bounded query latency under load.
- P6/P8/P16 resolve permission-label representation, derivative invalidation, and whether an independent deletion/revocation journal is required. Restore quarantine is required regardless of journal choice.
- P9 resolves local retrieval/index technology, compaction quality, and measured resource limits.

These details cannot relax Part A. The paired B1 amendments are present for review; implementation PRs must resolve their applicable schema and migration questions.

## B3. Alternatives and consequences

**Separate unrelated agents and memories:** simpler isolation, but rejected as the product direction because it loses continuity across the owner's explicit bot handoffs.

**One universal tool belt with model-selected privilege:** rejected; authority must remain outside the model and the Household bot must not silently become Operator.

**Copy every app database into RAG:** rejected; duplicates authoritative state, broadens ingestion, and hides freshness. Events, selected indexing, and live reads serve different purposes.

**Replace Wildstone with an external agent runtime:** not selected. Reuse existing policy, package, Plan, and event foundations first; an embedded/external runtime would require a separate compatibility decision.

**A new workflow broker or orchestration platform now:** deferred pending evidence that extending the runtime DB is inadequate.

The costs are durable-state complexity, permission-aware retrieval, app-specific adapter maintenance, and more recovery cases. Benefits are continuity, explainable cross-app work, reusable integration contracts, and a path to requesting capabilities without self-escalation.

# Part C — Ordered implementation plan

V1 targets text via existing bots/Console, shared filtered continuity, selected-source memory, durable work, scoped integrations/grants, capability requests, and configurable model routing. Home Assistant and read-only CalDAV are an initial cross-app path; PantryOS's forthcoming API can be tested with a fake until available. Child behavior is specified but must not be enabled until its own checks pass.

Each package below should normally land as one reviewable PR after this ADR is accepted. A package may be split when its migration and behavior cannot be reviewed safely together, but it must keep one acceptance boundary. Every package updates its public API types, Console/CLI consumers, recovery material, and tests when those surfaces change. New authority cannot ship ahead of its denial, revocation, audit, and recovery paths.

| # | Work package | Repository-shaped change | Required evidence |
|---|---|---|---|
| **P1** | Durable work and trace schema | Add forward-only runtime-DB migrations and typed stores for tasks/runs, steps, attempts, transitions, waits, completion evidence, and append-only trace events. Link existing `butler_sessions`/turns without rewriting their attribution. Define versioned serialized envelopes and retention/permission fields. | Existing conversations migrate and remain readable; invalid state transitions and missing permission metadata fail closed; upgrade/interruption fixtures show no task can appear completed from a partial write. |
| **P2** | Effective capability inventory | `keystone/butler/manifest.go`, `reader.go` (`ContainerLogs`), `tools_read.go` (`get_container_logs`), gateway/household registration | `go test -race ./...` in `keystone/`; `keystone/butler/manifest_test.go` proves the household surface can neither discover nor dispatch an operator-only tool, that QA-only sessions are offered no proposable kinds or mutating tools, and that manifest and executor coverage cannot drift from the registered tool maps. | `TestSurfacesAreDisjoint` was replaced, not weakened: `describe_capabilities` is deliberately on both surfaces, so the test now asserts the boundary it was protecting — a shared tool must be non-mutating on both and read only the gateway it was called on — with a new test proving what the household manifest reports. `TestHouseholdUnwiredSeamsAreHonest` likewise now asserts that the one surviving tool on a bare deployment reports every other tool unavailable with a reason. |
| **P2** | Effective capability inventory | Generate the effective manifest from the active `Toolset`, resource-kind registry, profile ceiling, current model verdict, and existing executors. Add the read-only self-capability tool and Operator-only bounded container-log reader through the existing Wardstone operation. | Structural tests prove Household cannot discover or dispatch Operator-only tools; QA-only and unsupported resource kinds are excluded; manifest and executor coverage cannot drift. |
| **P3** | Durable conversational runner | Refactor the current in-memory `butler.Session` loop into a restart-safe task driver using P1. Checkpoint before and after every model/tool attempt, preserve verified results, enforce aggregate and per-slice budgets, require completion evidence, and keep one active Keystone coordinator. | An Operator diagnosis needing at least eight reads completes or resumes without repeating verified reads; process termination at each checkpoint resumes safely; a second coordinator cannot dispatch concurrently; `redteam_test.go`, `injection_test.go`, `hardening_test.go`, and `structural_test.go` pass unchanged, and the runner does not import `keystone/reconcile`. |
| **P4** | Retry, stall, and continuation | Add validated Household/Operator iteration settings, provider-stall deadlines, attempt fencing, bounded retry, task waits, cancellation, and task-bound continuation to the API and current chat/Console surfaces. Add the minimum direct-owner deduplication and household-timezone quiet-hours policy defined in A4. Refresh stale observations and recheck authority on resume. | Provider failure and restart preserve progress; late results are rejected; expired/disabled authority blocks continuation; notices deduplicate and wait through quiet hours; exhausted total budgets require review instead of resetting. |
| **P5** | Work inspection and improvement loop | Extend the Console's current per-turn redacted tool expansion into the permission-filtered task timeline from A11. Add feedback labels, sanitized regression-candidate export, version-pinned evaluator fixtures, and bounded expiring debug capture. | A failed/resumed task is explainable from request through delivery without secrets, unauthorized source content, or private chain-of-thought; one reviewed failure becomes a repeatable evaluation that detects the original behavior. |
| **P6** | Identity, admission, and shared continuity | Add explicit authenticated-surface-to-member links for API, chat, and trusted ingress, Butler admission/disable state, link generations, immutable attribution, conversation identities, permission-aware history retrieval, and explicit cross-bot handoff. Add the runtime denial half of offboarding and the future Child records without enabling a Child surface. | The same admitted adult can explicitly hand a recent conversation between Household and Operator while destination authority is recomputed; unlinked/disabled people and group audiences fail closed; no privileged context crosses the handoff. |
| **P7** | Tool controls, grants, and capability requests | Extend tool metadata and add profile/connection switches, task-bound temporary grants, routine grants, immediate denials, cleanup-Plan linkage, and durable supported-tool/development requests. Add the admin Console surface and effective-authority resolver used by every dispatch. | Disable/revocation wins over cached manifests and saved work; Household's structural ceiling cannot be widened; a missing read tool can be requested but not used until separately approved/enabled; grants expire at task end, cancellation, or deadline. |
| **P8** | Owned memory records | Add structured facts, preferences, tentative inferences, learned procedures, provenance, audience/permission inheritance, corrections, forgetting markers, deletion invalidation, learning-digest records, and personal export. Run extraction through the shared redaction boundary. | Poisoned input cannot create authority; private material cannot become shared through a derivative; corrected/forgotten facts disappear from eligible retrieval and do not reappear from the same source; other admitted adults see shared-memory corrections/forgets in the next digest and safety-relevant removals immediately; export excludes inaccessible material. |
| **P9** | Selected-source RAG and compaction | Choose the local hybrid keyword/vector implementation through measured fixtures, then add permission-first indexing/retrieval, source freshness, live-read verification hooks, compaction, reindexing, and local-first background job scheduling. | Retrieval quality/resource limits are recorded; permissions filter before model context; source deletion/revocation removes dependent index entries and summaries; changing app state is verified live rather than trusted from RAG. |
| **P10** | Connection and adapter contract | Add named connections, capability/version discovery, the closed integration-adapter role, durable integration-work queue, long-poll claim/ack/result endpoints, deadlines, cancellation, unavailable/stale/empty result types, and single lease epochs. Keep Keystone application-neutral. | A fake adapter proves events, queries, actions, timeout, cancellation, duplicate delivery, stale epoch rejection, and an empty valid answer; an adapter credential cannot claim another connection or reach general API authority. |
| **P11** | Placement, credentials, and adapter failover | Add primary/ordered fallback placement, health/stabilization state, explicitly prestarted eligible containers, pre-provisioned node-scoped secrets, personal IMAP/CalDAV credential onboarding, removal fencing, container removal, and verified erasure/revocation. | Failover moves only the lease among approved prepared nodes and never duplicates work or creates a container during an outage; no eligible node produces a wait; personal secrets never enter Keystone/model/queue payloads; a referenced secret cannot be erased and incomplete cleanup leaves the connection visibly degraded. |
| **P12** | First read integrations and cross-app run | Implement package-managed Home Assistant query/event support and read-only CalDAV against the adapter contract; exercise PantryOS against a versioned fake until its API is ready. Add departure/breakfast and calendar/presence fixture scenarios as user-authored routines, not built-in behavior; model-authored message text is optional. | The scenarios complete with the model disabled, handle unavailable and stale sources explicitly, deduplicate repeated events, and pause on uncertain effects. Limited real Home Assistant and CalDAV deployments pass the same contract tests as their fakes. |
| **P13** | Routine authoring and approvals | Add native-routine declarations/external IDs, Wildstone cross-app routine steps, previews/trial mode, versioned edits, immediate exact disable, the Authelia-authenticated household review page and narrow acting-for service route, household-routine Plan classification, and proposal-bound Telegram callbacks/text fallback. | An admitted adult can review only a pure household-routine Plan without receiving Console credentials; mixed/infrastructure/authority Plans and chat approval attempts fail; callbacks are identity/revision/expiry bound and single-use; native and Wildstone ownership stay distinct. |
| **P14** | Local/cloud model routing | Add named local/cloud routes, per-route readiness and concurrency, source/job processing policy, bounded automatic escalation, and lower-priority local-first background processing. Recompute effective authority after every selection. | Local-only information never reaches a cloud route; fallback never widens tools; an unready/QA-only fallback cannot inherit action authority; deterministic work continues with every model unavailable. |
| **P15** | Full proactive communication and delivery | Extend P4 with per-person quiet-hours overrides, relevance and urgent-exception policy, default-admin routing, question correlation, delivery acknowledgement, and undelivered-notice state across existing outbound surfaces. | Quiet hours, audience, and deduplication hold across restart; ambiguous or expired replies authorize nothing; an expired notice cannot become approval or reopen a completed task. |
| **P16** | Offboarding, restore, and v1 release gate | Finish cross-store offboarding for tasks, memory, connections, routines, bindings, grants, delivery, exports, and credentials. Extend encrypted recovery and quarantine reconciliation for all W030 state, including traces/evaluations and explicit fenced standby promotion. Run the combined Part C acceptance suite and update operating docs. | Immediate revocation holds across surfaces/nodes; personal work stays paused and private data quarantined while shared ownership transfers without changing authorship; restoring an older snapshot cannot revive deleted/revoked content or uncertain actions; the previous coordinator cannot dispatch after promotion. |

P1 → P2 → P3 → P4 → P5 is the first reliability slice. It uses the current Operator and Household tools and adds no new application write authority. P6 starts only after that slice can preserve and explain work. P7 depends on P2/P3/P6; P8 depends on P6; P9 depends on P8; P10 depends on P1/P2/P6; P11 depends on P7/P10; P12 depends on P10/P11; P13 depends on P3/P6/P7/P12; P14 depends on P4/P8/P10; P15 depends on P3/P4/P6/P13; P16 follows every v1 package.

Each implementation PR updates its package here with an implemented-by link, the evidence run, and any accepted deviation. Later packages start from the accepted main branch and the recorded results, not from this proposal branch.

### Implementation status

| # | Landed | Implemented by | Evidence run | Accepted deviations |
|---|---|---|---|---|
| **P1** | Durable work and trace schema | `keystone/runtimedb/migrations/0017_butler_tasks.sql`, `keystone/runtimedb/butlertasks.go`, `butlersteps.go`, `butlertrace.go` | `go test -race ./...` in `keystone/`; `keystone/runtimedb/butlertasks_test.go` carries the three required proofs — a pre-0017 conversation reads back unchanged, illegal transitions and evidence/trace rows missing permission metadata are refused, and an interruption between evidence and completion leaves resumable running work rather than a completed task. | Completion is refused twice over: `TransitionButlerTask` cannot reach `completed` at all, and a SQLite trigger refuses the state change when no evidence row exists, so the invariant survives any writer. Trace, transition and evidence rows are immutable but deletable, because retention and offboarding (P16) must be able to remove them. |


The v1 release gate is P16, not the first successful cross-app demo. A partial deployment may dogfood P1–P5 on the existing surfaces, but memory, new connections, and new write paths remain disabled until their own package evidence and the applicable offboarding/recovery paths pass.

Follow-up work: dedicated receipt/screenshot/PDF intake, and selected read-only IMAP monitoring with review-first calendar writes. Calendar invitations/RSVPs remain separately controlled.

Voice is post-v1: local transcription preferred, Kokoro preferred for TTS, using the same identity and conversation interfaces. Cost enforcement, unrestricted execution, automatic tool development, and autonomous agent teams remain deferred.

Implementation PRs should prove:
- Same-person handoff continuity without cross-audience leakage or implicit elevated execution.
- Temporary grant expiry, tool disable, and identity revocation at dispatch, including paused/restarted work.
- A deterministic cross-app run with the model off, an unavailable source, duplicate events, and an uncertain action outcome.
- Review-bound household writes distinct from infrastructure Plan approval.
- An admitted adult can review a pure household-routine Plan through the Authelia-authenticated household page without receiving Console credentials, while mixed/infrastructure/authority Plans and all chat approval attempts are refused; exact chat disable takes effect immediately.
- Memory correction/deletion propagated to retrieval and derived content, including restoration behavior.
- Adapter behavior against fakes and a limited real deployment, not merely a declaration that lints.
- Adapter failover only among approved nodes, single-epoch fencing against duplicate work/late results, waiting with no eligible node, and verified credential erasure/revocation when a fallback is removed.
- Offboarding immediately denies access across surfaces and nodes, pauses personal work, transfers shared ownership without rewriting authorship, quarantines private data through retention/deletion, and survives restoration of an older snapshot.
- A trace can explain a resumed or failed task from request through retrievals, tool attempts, policy decisions, approvals, delivery, and evidence without exposing secrets, unauthorized content, or private chain-of-thought; a sanitized failure becomes a version-pinned regression evaluation.
- Starting a second coordinator cannot dispatch concurrently; explicit standby promotion fences the previous coordinator, and cross-app work waits safely during coordinator unavailability.

The accompanying brief provides non-normative rationale, deployment examples, and delivery sequencing. This ADR alone defines the proposed architectural contract.
