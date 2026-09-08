# ADR-W030: Butler continuity, durable work, and scoped integrations

- **Status:** proposed — draft for discussion; no implementation or permission changes authorized by this record while proposed.
- **Relates to:** ADR-W004 (Plans), W005/W025 (packages and integrations), W006/W017 (Butler), W007 (Keystone), W009 (policy), W012 (secrets), W020 (identity), W024 (household automation), W026 (messaging), W027 (conversation UX), W028 (failure isolation).
- **Source:** household-owner design discussion, consolidated in [the capability and design brief](../butler-capability-and-design-brief.md).
- **Baseline:** main at `cfdf678736a57e18a1e88ef320c6409a8c424aad`.
- **Audience:** architecture review. Part A proposes product and authority contracts; Part B identifies amendments and unresolved engineering decisions. It is not a build specification.

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

## A3. Memory is owned data with derived retrieval indexes

Use one logical household memory service, structured records for exact facts and permissions, and hybrid meaning/keyword retrieval for selected text. Application records remain authoritative for current app state; live reads verify changing facts when needed. A vector database product is not selected here.

Butler may learn preferences and reusable procedures automatically. Distinguish explicit statements from tentative inferences; retain source, date, and corrections. Explicit preferences persist until corrected or time-bounded; unused inferences lose influence. Learned procedures confer no permissions and do not enable routines.

Offer configurable daily/weekly learning digests and inspect/correct/forget controls. A private source does not become household knowledge through extraction. Query permission does not imply bulk indexing permission.

Deletion removes retrieval access and solely dependent derivatives; forgetting prevents relearning from the same historical material. Original records survive compaction unless separately deleted. The restore-safe deletion/revocation mechanism remains a blocking engineering question in B2.

## A4. Tasks and routine runs share durable execution machinery

Use one execution foundation in Keystone for conversational tasks and Wildstone-owned routine runs. Retain routine definitions separately from each occurrence. Model-free steps remain model-free; agent steps receive bounded context and grants.

Recommended starting point: runtime-DB task/run rows, step/attempt rows, and transition history. Record owner, conversation, profile, objective, completion criteria, grants, deadline, next wakeup, results, and uncertainty. Exact schemas, claim/lease design, and migration are deferred to an implementation specification.

Replies, approvals, schedules, and unavailable models are saved waiting reasons. Cancellation stops new actions and attempts to interrupt in-flight work. Refinements preserve task history; separate objectives create separate tasks. Independent work may run together; incompatible shared changes pause for clarification rather than assigning one adult priority.

Resume relevant authorized work after restart. Do not blindly retry actions whose outcomes are unknown. Use destination verification or supported idempotency mechanisms; otherwise pause. Preserve partial successes and report remaining work. Completion requires evidence appropriate to the objective, not merely a model saying done. Notification delivery is separate from task completion.

This does not claim exactly-once external execution. W024's existing at-most-once dispatch and chat's separate delivery semantics must be reconciled explicitly in the implementation design.

## A5. Common contracts, application-specific adapters, named connections

An integration declares events it reports, queries it answers, and actions it performs. Capabilities can be partial. Responses distinguish valid results, including empty lists, from unavailable/stale information and carry source and observation time.

Application-specific code belongs in adapters outside Keystone; native publishers need no additional publishing process. Support both packages integrating Wildstone-managed apps and connection-only packages for externally hosted apps. Connecting does not take ownership of the remote application. Adapters may run on a managed machine that can reach the app, not necessarily the app's host.

An adapter implementation supports multiple named connections, each with endpoint, credential references, identity mapping, and grants. Credentials follow the secret path and never enter model context.

Verify available capabilities against app version, configuration, and granted access. Discovery does not authorize newly available permissions. Outages mark capabilities unavailable and dependent work waiting.

Keep event intake, on-demand queries, and optional RAG indexing distinct. Preserve the canonical household timeline and SQLite bus decision in W024. No new broker or log-parsing contract is selected.

## A6. Native automation where appropriate, Wildstone for cross-app coordination

Butler authors native routines through supported app interfaces when that app owns the workflow. Wildstone executes cross-app routines using incoming events and targeted queries. Track external routine IDs and edits; do not silently duplicate or migrate execution.

The platform supplies schedules/events, conditions, uncertainty handling, repeat controls, typed actions, replies, preview, trial mode, versioning, and audit. Breakfast and appointment examples in the brief are acceptance scenarios, not hardcoded product features. Timing, destinations, eligibility, and missing-data choices belong to user-authored routines.

Preview without side effects where historical evidence permits; disclose unavailable historical context. Activation and behavioral changes need approval. Authorized stopping must take effect promptly, with the exact Plan/runtime representation resolved in B2.

## A7. Admin-controlled tools and requests for missing capabilities

Provide an admin-only Console tool surface for each profile, grouped by named connection. Show tool purpose, data access, mutating behavior, approval requirements, availability, grants, and dependencies.

Disabling a tool both removes its advertised specification and rejects subsequent dispatch, including from saved sessions and routine runs. Cached tool lists are not authority. In-flight effects may be irreversible; report them.

Household excludes shell execution, arbitrary code, unrestricted filesystem search, credential access, and infrastructure administration. Toggles and temporary grants cannot cross that ceiling. A read tool also needs scope: searching connected recipes differs from searching every host file.

Butler can create durable requests linked to a blocked task:
1. Enable a supported but unavailable tool with proposed scope.
2. Develop a missing capability with behavior and acceptance criteria.

The admin may approve, narrow, or reject. Requests confer no authority. Development approval is separate from installation and enablement. Automatic tool development and self-modification are deferred.

One-off grants are task-bound and temporary by default, ending at completion, cancellation, or deadline. Permanent profile enablement is a separate choice. Approved routines hold scoped grants for declared tools/connections/actions while enabled; these are not general conversational grants. All grants remain subordinate to current profile ceilings, tool switches, person access, and revocation.

## A8. Tool access and action approval are separate

A clear request can authorize a permitted low-impact household action. Consequential operations require approval of the exact proposal. Household Telegram approvals are bound to the approving identity and proposal version, expire, and cannot execute twice from duplicate clicks. Personal changes require the person; shared changes allow an authorized adult; restricted child requests require a guardian.

Infrastructure approval remains in Console/CLI. Enabling a tool, receiving a document, or accepting a learned procedure does not approve an action. Retrieved content is untrusted input, including email instructions and event payloads.

## A9. Model choice does not change identity or authority

Both adult profiles can use local or cloud models. Configure named endpoints/models, processing location, permitted data, and fallback order. A remote Ollama endpoint is not proof of either local or cloud inference.

Allow automatic escalation within configured alternatives and bounded attempts. Local-only information cannot follow a cloud fallback. Background embeddings, extraction, and compaction prefer local processing where quality is adequate. Deterministic operations continue without models.

Use configurable endpoint concurrency and interactive capacity, with lower-priority ingestion/compaction. Defer cost tracking and account-limit enforcement: returned usage metadata does not establish remaining shared provider allowance.

## A10. Communication, retention, and recovery are explicit policies

Allow proactive messages, with deduplication, relevance checks, quiet hours, and appropriate bot routing. The owner's proposed quiet-hours default is 21:00–07:00 America/Chicago, configurable per person; urgent exceptions require explicit designation. These are configurable policies, not special-purpose routine logic.

Task updates go to the owner, private messages stay private, and unassigned notices default to the configured admin without granting access to private content. Replies correlate to the originating question; ambiguous or expired answers do not authorize stale actions. Silence is never approval.

Back up tasks, conversations, memories, grants, mappings, routines, procedures, and access settings. Reconcile restored actions against external state before resuming. Reapply later deletions/revocations before exposure; do not claim an old backup alone can contain later decisions. Conversation/task-summary retention defaults to manual deletion with configurable cleanup.

# Part B — Compatibility, review questions, and consequences

## B1. Proposed amendments; existing records remain unchanged in this PR

| Existing contract | Proposed change or preserved constraint |
|---|---|
| W026 A1: two surfaces, separate histories/tool sets | A1/A2 propose shared filtered continuity, Operator's Household-plus-admin capability, and a Child profile. Separate credentials, audience checks, and explicit handoffs remain. |
| W026 A5: no infrastructure approval over chat | Preserved. A8 adds exact household-action approvals only; no Plan approval through Telegram. |
| W024 A5/A6 and H6: narrow domestic execution and tool-free phrasing | Preserve the deterministic path and restricted Phraser. General durable agent steps need a distinct enforced grant/context boundary, not silent widening of H6. |
| W024 A10: integration lifecycle belongs to the service package | Managed apps retain that lifecycle. Connection-only packages add an explicit lifecycle for external apps; removing a connection must not remove the external app. |
| W006/W017: bounded provider loop and structural operator ceiling | Preserve no-approve/no-arbitrary-host-exec. Add runtime durability, discovery, grant checks, and request records outside the model. |
| W004/W007/W009: public API, Plans, and policy | Preserve public API parity and host-state Plan mediation. Decide declarative versus runtime grant/stop operations before implementation. |
| Scope §7 and vision | Record the delivery pull-forward and reconcile family cloud-processing statements; no implicit claim of local-only behavior. |

Do not weaken existing structural tests to fit the proposal. Acceptance requires explicit amendments and replacement/expanded boundary tests where behavior changes. The tree currently contains two ADR-W029 filenames; this proposal references them by title/path if needed and does not renumber unrelated history.

## B2. Open engineering questions for this discussion

- What transactional task/step model, leases, wakeups, and action identifiers fit current automation and chat delivery? How do we migrate without duplicate execution?
- How are source restrictions combined for multi-source summaries and cross-profile history, including same-person versus group retrieval? What is the fail-closed behavior when metadata is missing?
- Which adapter transport and schema versioning fit package contracts: HTTP, MCP behind the Wildstone abstraction, or both? How are credentials, permitted endpoints, and disabled connections enforced?
- Which grant changes are Plan-mediated desired state versus revocable runtime state? How can immediate disable/cancel coexist with existing invariants?
- How do deletion and revocation records survive restoring an older snapshot? Which recovery path blocks exposure until newer authority is established?
- Which local embedding/index and compaction approaches satisfy measured retrieval quality and resource limits? No vendor or database purchase is implied.
- What is the smallest complete implementation slice and its acceptance evidence? Do not treat the whole brief as one feature PR or a fixed delivery estimate.

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

The accompanying brief retains the full discussion scope. This ADR is the proposed architectural contract; unresolved decisions above remain visible for debate before acceptance.
