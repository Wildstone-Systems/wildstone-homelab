# Wildstone Butler: Capability and Design Brief

Status: supporting design brief for proposed [ADR-W030](decisions/ADR-W030-butler-continuity-durable-work-and-scoped-integrations.md); discussion material, not implementation authorization.

This brief consolidates the household owner's decisions from the design discussion. It distinguishes agreed product behavior, recommended implementation, configurable policies, examples, and unresolved architecture. Existing repository ADRs remain unchanged; conflicts identified here require explicit reconciliation in a future decision record.

## 1. Product intent

Butler is one household assistant that can understand requests, complete useful work across applications, maintain continuity, learn preferences and procedures, and follow up over time. Wildstone supplies durable task execution, identity, permissions, integration contracts, deterministic automation, and audit.

The assistant should feel like one entity wearing different hats. Model choice, communication channel, and tool set are implementation and access choices, not separate personalities.

The owner should describe desired outcomes in ordinary language. Butler supplies reasonable technical defaults and asks for clarification when ambiguity materially affects an outcome. It does not require users to design workflows themselves.

## 2. Repository baseline and architectural changes

The detailed baseline review used `main` at `17f441385e4385ff00249dc8a3f354df923d084d`. PR preparation subsequently checked main at `cfdf678736a57e18a1e88ef320c6409a8c424aad`, including changed-file history and current documentation. Newer work includes a Roundcube mail/calendar client package and Console/package improvements; it does not by itself implement Butler email ingestion or calendar tools. These are pinned review snapshots, not a claim that main cannot change.

Existing foundations include Keystone's SQLite runtime database, git-backed desired state, Plan approval and reconciliation, Wardstone's typed host operations, Butler's provider and tool interfaces, household events and Automations, domestic actions, Telegram/Matrix transports, and persistent chat conversation selection.

Relevant records:

- [W006: Butler operator loop](https://github.com/chickenrat17/wildstone-homelab/blob/17f441385e4385ff00249dc8a3f354df923d084d/docs/decisions/ADR-W006-butler-operator-loop.md)
- [W017: Butler productionization](https://github.com/chickenrat17/wildstone-homelab/blob/17f441385e4385ff00249dc8a3f354df923d084d/docs/decisions/ADR-W017-butler-production.md)
- [W024: Household events and automations](https://github.com/chickenrat17/wildstone-homelab/blob/17f441385e4385ff00249dc8a3f354df923d084d/docs/decisions/ADR-W024-household-events-and-automations.md)
- [W025: Catalog integration layer](https://github.com/chickenrat17/wildstone-homelab/blob/17f441385e4385ff00249dc8a3f354df923d084d/docs/decisions/ADR-W025-catalog-integration-layer.md)
- [W026: Butler messaging surfaces](https://github.com/chickenrat17/wildstone-homelab/blob/17f441385e4385ff00249dc8a3f354df923d084d/docs/decisions/ADR-W026-butler-messaging-surfaces.md)
- [W028: Capability blocking](https://github.com/chickenrat17/wildstone-homelab/blob/17f441385e4385ff00249dc8a3f354df923d084d/docs/decisions/ADR-W028-a-capability-waiting-is-not-a-node-failing.md)
- [W029: First-run state](https://github.com/chickenrat17/wildstone-homelab/blob/17f441385e4385ff00249dc8a3f354df923d084d/docs/decisions/ADR-W029-packages-seed-their-own-first-run-state.md)

This proposal deliberately evolves separate conversation histories into shared, permission-filtered continuity. The intended Operator capability set is Household plus administration; the earlier implementation had separate tool sets, so that union needs verification and deliberate implementation. Shared adult memory access, a Child profile, calendar approvals in household chat, and durable agent tasks are proposed behavior, not claims of existing implementation.

## 3. Identity, bots, and authority

| Access point | Intended capability |
|---|---|
| Household | Household knowledge, personal memory, and permitted household tools |
| Operator | Household capability plus administrative tools; infrastructure changes still require Plan approval |
| Child | A restricted set of knowledge and tools, granted by a guardian |

Homepage or application access does not grant Butler access. Admitted adults have equal standing for shared household memories; v1 does not introduce an elaborate configurable adult role hierarchy. Personal information may still be private. Operator and guardian status remain meaningful boundaries.

The current Telegram bot sets the tool boundary. Handoffs are explicit every time; Household never invokes Operator powers in the background, even for a person with Operator access. Context can transfer, but transfer neither grants authority nor automatically executes a pending action.

Each person has one Wildstone identity with explicitly linked application accounts. Similar names do not justify automatic identity merging. Shared application credentials retain attribution to the person requesting the action.

Child access permits limited actions such as adding shopping-list items, recording personal preferences, and marking the child's own chores complete. Broader changes require guardian approval. Guardians can review child conversations, with this visibility disclosed during setup; other adults do not automatically gain that access. Approval does not grant the Child bot a broader tool set.

Revocation blocks new access and further actions, invalidates relevant chat bindings, and flags affected routines for adoption or disabling. In-flight actions may be impossible to cancel; report their outcomes. Revocation preserves history under the retention policy and must override old task grants.

## 4. Conversations and memory

Use one logical household memory service and shared conversation storage, with multiple conversations. Ordinary chat continues the active conversation. Explicit requests can start or resume another; recent history helps resolve references, but ambiguity prompts clarification. Handoffs select the originating conversation.

Continuity is filtered by person, bot, and current audience. A conversation being stored centrally does not make it visible to every member or bot. Private adult or Operator material must not enter an unauthorized retrieval result, group reply, or Child session.

RAG retrieves relevant records; it does not replace exact state or authoritative application data. Recommended storage roles:

- Structured records for preferences, identities, permissions, dates, and task status.
- Meaning-based and keyword search for documents, conversations, and procedures.
- Live application reads for changing facts needed by a task.
- Rebuildable search indexes, with source references and freshness retained.

Information inherits source permissions. Being about a person does not automatically make information private; household coordination facts can be shared. Extracting a memory or summary cannot broaden its audience. A simple personal/household sharing experience should conceal the necessary underlying source and audience enforcement.

Butler automatically learns preferences and reusable procedures. Explicit statements and inferred preferences remain distinguishable. Inferences retain evidence and dates, are tentative, and lose influence without reinforcement. Explicit corrections take priority and prevent immediate relearning from the same superseded evidence. Safety-critical facts are not established by casual inference.

Learned procedures retain supporting evidence, recheck current conditions, contain no credentials, and grant no authority. They do not become active routines without approval.

Provide daily or weekly learning digests, proposed default weekly and configurable per person. Show new inferences, corrections, conflicts, and controls to forget or change sharing. Skip empty digests. Silence is not confirmation.

Onboarding selectively connects sources and collects useful household details. App installation does not imply permission to index all app content. Ingestion supports an initial import, incremental updates, deletion propagation, and periodic reconciliation. Background extraction and compaction prefer local processing; retain original records so summaries are not the sole evidence.

## 5. Durable task execution

Automatically create saved tasks for actions, monitoring, follow-up, and multi-step work. Simple questions need not become tracked tasks. State the objective and completion criteria briefly; clarify consequential ambiguity and proceed with independent authorized work where possible.

Recommended initial implementation: extend Keystone's runtime database with a task record, step/attempt records, and append-only task history. This recommendation does not require a new workflow platform or a task store in RAG.

The task records its owner, conversation, bot, objective, completion criteria, permissions, status, deadlines, and next wake time. Steps record intended operations, attempts, results, and evidence. Suggested states are queued, running, waiting, verifying, completed, failed, cancelled, and expired, with explicit waiting reasons.

Completion requires evidence appropriate to the request. Producing a recommendation can complete an investigation without applying a repair. For open-ended outputs, record the assessment honestly rather than claiming mechanical proof. The model cannot silently weaken completion criteria. Completion and delivery of its notification are separate states.

Save progress and resume automatically after restarts, subject to relevance and current authorization. An uncertain external action is checked before retrying; if the outcome cannot be established, pause rather than blindly repeat. Coordinate conflicting changes; preserve competing intentions for clarification, with no adult automatically outranking another. Independent work continues.

Refinements update a task with history; separate objectives create separate tasks. Cancellation stops new work and attempts to interrupt in-flight work. It does not implicitly reverse completed actions. Preserve partial successes, retry unfinished work when safe, and show what remains. A user can explicitly accept a reduced scope.

Use one durable execution foundation in Keystone for conversational tasks and Wildstone routine runs. Each recurring occurrence has its own saved run linked to a separate routine definition. Deterministic steps run without a model; a step needing reasoning invokes Butler with bounded context and permissions. Waiting for replies, approvals, models, handoffs, or scheduled times uses shared persistence mechanisms. Actions share outcome tracking, cancellation, and duplicate prevention. A breakfast workflow's event, lookup, question, and reply belong to one correlated run, not unrelated tasks.

Unification does not require every routine to invoke an agent, or conversational tasks to become recurring routines. Preserve native-app execution ownership and existing deterministic automation semantics rather than adding a competing scheduler or action path. The exact migration from current code remains engineering work.

## 6. Integrations and cross-app routines

Native app automations execute in the appropriate app. Cross-app routines execute in Wildstone. Butler authors, explains, tests, and changes both through supported interfaces.

### Integration contract and deployment

Use a common contract with an application-specific adapter implementation: what the app can report as events, answer through queries, and change through actions. Responses may be structured records or lists, not only yes/no answers. Distinguish valid empty results from unavailable or stale information, retaining source and observation time. An adapter can initially support only a subset of these capabilities.

Application-specific adapter logic runs outside Keystone, which understands the common contract. Reuse adapters across households and routines with deployment-specific configuration and account mappings. Applications able to emit canonical events directly need no additional publisher process.

Support both Wildstone-managed application packages with integration setup and connection-only packages for applications already running elsewhere. Connecting does not take ownership of an external application's installation. An adapter may run on a managed machine that can securely reach the application; co-location is not required.

Each adapter supports multiple named connections, with separate endpoints, credentials, and identity mappings. Events, queries, actions, and grants identify the connection, avoiding an assumption of one instance or account per application. Credentials reach the adapter through the approved secret path and are never exposed to the model.

The adapter declares supported capabilities; setup verifies those actually available under the application's version, configuration, and granted credentials. Butler sees only supported, verified, authorized capabilities. New permissions discovered after an upgrade remain disabled until approved. Temporary outages make capabilities unavailable rather than implying they never existed; dependent work can wait with an explanation.

Event intake, live queries, and RAG indexing are separate capabilities. Query permission does not authorize bulk indexing. Retain necessary events, query fresh data on demand, and index only selected content useful for recall. An integration remains useful without copying the application's database.

### Routine behavior

Apps publish canonical facts through package-owned integrations; Wildstone also performs targeted API queries. These methods complement each other. Prefer native events and APIs; polling belongs inside adapters. Do not introduce log parsing as a reliable contract without an explicit decision—W024 currently rejects it.

Reuse one publisher for facts such as presence rather than creating one publisher per consumer routine. Application state remains authoritative. No record found, stale data, and an unavailable source are distinct outcomes.

The routine platform supports triggers, schedules, conditions, context reads, actions, replies, deadlines, deduplication, stability periods, repeat limits, and fresh checks before delayed actions. Specific time windows, event eligibility, and reminder wording are routine configuration, not hardcoded product behavior.

Preview routines without side effects against suitable historical evidence; offer prospective trial mode when history is insufficient. Be explicit when live-only context prevents faithful historical replay. Approval is required before activation. Behavioral changes require approval; wording may vary within approved bounds. Authorized stopping is immediate, with version history retained.

External automations retain their native identity and owner. Check for overlap and direct edits before changing them. Do not silently migrate or duplicate execution between systems. If the preferred execution location lacks support, explain the gap and ask before substituting another.

Missing tools are visible capability gaps with the request workflow below. Butler may suggest integrations; installation goes through Operator and the existing approval process, while profile tool grants are administered in Console. It cannot independently install unfamiliar executable plugins. Incompatible updates pause dependent work; new permissions need approval. Show dependencies before removal and resume only relevant, authorized work.

### Profile tool controls and capability requests

Provide an admin-only Console surface for each Butler profile, grouping individually controlled tools by named connection. Explain each tool's behavior, whether it changes data, whether approval is required, availability, and dependent tasks/routines. Model changes do not change tool settings. Connection permissions and requesting-person access continue to constrain every call.

Profile boundaries are enforced outside the model. Household permits scoped household queries and explicitly allowed household actions, but excludes shell execution, arbitrary code, unrestricted filesystem search, credential access, and infrastructure administration. Forbidden tools are unavailable to enable in that profile. Temporary grants cannot cross the boundary; administrative work requires an explicit Operator handoff. Operator is not thereby granted unrestricted host execution: existing protocol and Plan constraints remain in force.

Disabling a tool removes it from offered tools and rejects further execution, including requests from existing sessions. Show affected work and pause dependent steps. In-flight operations may complete if they cannot be cancelled; report their result. An approved routine cannot bypass the switch or automatically reacquire access.

V1 includes durable capability requests linked to blocked tasks. Each records the objective, intended profile and connection, missing capability, reason, requested scope, and whether an existing tool can satisfy it. The admin can approve, narrow, or reject the request. Two request types are supported:

- Enable an existing tool within the profile's permitted boundary.
- Request development of a missing capability, with proposed behavior and acceptance checks.

Requests themselves grant no access. Approval to develop is separate from approval to install and enable. Automatic tool development is deferred; no agent modifies its own running tool set. Scope read tools as well as write tools: read-only access can disclose secrets or private information. A bounded Operator diagnostic search is different from arbitrary shell grep over the server.

One-off tool grants are temporary by default, bound to the task and its authorized identity/profile, connection, scope, and deadline. They expire on completion, cancellation, or the deadline; retries, restarts, model changes, and handoffs do not broaden or renew them. Permanent profile enablement is a separate admin choice.

Approved routines retain narrowly scoped grants for their declared tools, connections, and actions while enabled. These do not constitute general permanent tool grants for arbitrary conversational use. Profile restrictions, tool switches, connection availability, and revocation always take precedence. An existing grant remains subject to ordinary consequential-action approval; enabling a tool is not blanket approval of every invocation.

Once a requested capability becomes available, waiting work can resume only when still relevant and authorized. Requests cannot cause a silent handoff or grant the originating bot another profile's authority.

## 7. Approvals and input trust

Clear direct requests can authorize permitted low-impact household actions. Approved routines authorize their bounded actions. Proactive suggestions authorize nothing. Consequential actions require approval of the exact operation; tool enforcement lives outside the model.

Household proposals can be approved, edited, or dismissed in Telegram. Validate the approving account, bind approval to the proposal version, prevent duplicate execution, and refresh approval after material changes or expiry. Personal changes require the person's approval, shared household changes permit any authorized adult, and restricted child requests require a guardian. Infrastructure Plans retain Console/CLI approval.

Documents, emails, events, search results, memories, and procedures are untrusted content. They cannot grant permissions or override instructions. Following a document at a user's request still passes through normal tool checks.

## 8. Models and resource scheduling

Allow both local and cloud models for Household and Operator. Model choice is separate from identity and permissions. Named configurations identify endpoint, model, capabilities, and data destination; a remote Ollama endpoint alone does not identify where inference occurs.

Bots have defaults; routines and background jobs may select other models. Automatically escalate only within the owner's configured alternatives and fallback order. Preserve local-only restrictions. Bound attempts, record reasons, and report or pause when no allowed model works.

Defer cost estimation and account-allowance enforcement. Returned usage metadata may be recorded without claiming knowledge of shared provider limits.

Interactive work tries permitted fallbacks promptly. Background work retries within relevance limits. Deterministic rules, timers, and predefined responses work without a model. Configure concurrency per endpoint, reserve interactive capacity, prioritize time-sensitive work, and run indexing/compaction at lower priority.

## 9. Communication and control

Allow useful unsolicited observations outside digests. Explain why Butler reached out, avoid repetitive suggestions, and honor feedback. Use the appropriate bot. Suggestions do not authorize actions.

Quiet hours default to 9 p.m.–7 a.m. in America/Chicago, following daylight saving time, and are configurable per person. Direct replies remain immediate. Explicit timed requests and designated urgent categories can bypass quiet hours. No category is implicitly urgent; Butler cannot grant itself an exception. Combine queued updates and suppress stale ones.

Task updates go to the requester, personal messages stay private, and configured shared destinations receive household notices. The homelab admin is the fallback for unassigned notices, not a fallback entitlement to private information. Operator notices use Operator.

Informational notices need no reminders. Blocking questions receive one reminder, then wait; expire time-sensitive questions. Repeated escalation is configured explicitly. Silence is never approval. Bind replies by originating message or button; use a clear recent match for free text and clarify ambiguity.

Keep quick tasks concise. Report meaningful milestones, waiting, failures, approvals, and completion for longer work. Provide an active-work view and detailed history in Console without flooding Telegram.

## 10. Retention, recovery, and validation

Keep conversations and completed task summaries until deletion by default, with configurable cleanup. Detailed tool output may have shorter retention; exact periods remain implementation policy.

Forgetting removes active memory and prevents relearning from the same historical material. Deleting source content removes it from history/search and removes derivatives supported solely by it. Independently supported memories may remain. Enforce deletion across retrieval, summaries, caches, and indexes.

Back up memories, access settings, conversations, tasks, routines, integration mappings, and procedures. Rebuild search indexes as needed. After restore, reconcile external state before resuming actions and reapply deletions/revocations before retrieval or execution. The mechanism for keeping those records newer than a restored backup remains an open architecture requirement.

Activation checks cover useful task completion, bot boundaries, member privacy, unavailable apps/models, cancellation, restart recovery, duplicates, quiet hours, and revocation. Use fakes where appropriate followed by limited real integration trials. Permission/privacy failures block activation. No tests were run as part of this design consolidation.

## 11. Delivery scope

### Version one

Text interaction through existing Telegram bots and Console; explicit handoffs and shared permission-aware continuity; selected connected-source retrieval; learned preferences/procedures and digests; durable tasks and routine runs on a shared execution foundation; managed and connection-only integrations with named connections and verified capabilities; admin-controlled tools per profile; capability requests and scoped task/routine grants; cross-app routines; configurable model selection and fallbacks; control, audit, and recovery foundations.

Home Assistant and read-only selected CalDAV calendars provide an available integration path. PantryOS is still under development and will expose an API; define its contract and test with a fake until ready. CalDAV provider details remain to be established. IMAP is a separate email integration.

Child access behavior is agreed, but its implementation order within delivery still needs scheduling. Do not enable it before its knowledge and action boundaries pass validation.

### Early follow-up capabilities

- Direct receipt images, screenshots, and PDFs through dedicated extraction/vision processors, with structured results, source evidence, uncertainty handling, and verification. Do not assume the conversational model supports images.
- Read-only automatic monitoring of selected IMAP mailboxes/folders, plus review-first calendar creation/modification/cancellation. Show source and destination; default destination per person; check duplicates and changed appointments. Invitations and RSVPs require separate permissions. Initial email lookback remains configurable; starting at enablement was recommended but not separately confirmed.

### Later phase

Voice, preferably local speech-to-text and local speech generation, with Kokoro the preferred TTS engine. Telegram still transports audio. Voice uses the same conversation, task, identity, and bot boundaries. Preserve extension seams in v1 without building the pipeline.

### Deferred

Provider cost/account-limit enforcement, unrestricted shell or browser execution, self-installed executable plugins, automatic tool development, and autonomous multi-agent teams are not first-version requirements. Capability-development requests themselves are included in v1.

## 12. Illustrative workflows—not built-in routines

**Breakfast:** Home Assistant publishes departure; a Wildstone routine checks PantryOS within its configured time window, suppresses if breakfast is logged, or asks when appropriate. No log is not proof of no meal; unavailable data remains unknown. Replies can continue an authorized workflow.

**Appointment:** a routine reads CalDAV, schedules a configured check, verifies the appointment and presence, and sends an appropriate reminder. Lead time, appointment selection, location rules, and travel-time services are user-authored routine choices.

**Troubleshooting:** Operator investigates using fresh evidence, saves progress, survives a restart, and produces a proposed repair. Applying it follows existing Plan approval.

**Email to calendar:** a selected email yields an event proposal linked to its source. The person approves exact details before a calendar write. Reschedules and cancellations update the associated event only after review.

## 13. Remaining architectural work

These are platform decisions and engineering investigations, not a list of routine settings to ask the owner to configure:

1. Map this brief to as-built code and amend/supersede the precise existing ADR clauses for shared histories, Operator tool union, guardian visibility, and household chat approvals.
2. Specify task/step persistence, transactional claims, leases, cancellation, wakeups, and uncertain-outcome handling; define how routine executions link to agent tasks without duplicate schedulers or execution paths.
3. Define memory records, permission inheritance, cross-conversation retrieval, compaction evaluation, and local embedding/index selection. Confirm deployment and backup costs before choosing a database or model.
4. Specify package-owned query/action transports, schemas, credential delivery, identity mapping, change detection, compatibility, and executor registration for managed and connection-only deployments. Map connection-only packaging to W024's package lifecycle rules rather than silently contradicting them. Implement named connections, verified capability discovery, admin profile switches, durable capability requests, and task/routine grant enforcement. Define transactional revocation/disable checks at dispatch so cached model tool lists cannot authorize execution.
5. Define durable deletion and revocation enforcement across old-backup restoration. Do not promise post-backup knowledge can be recovered from the backup itself.
6. Define where declarative routine changes versus immediate runtime pauses/revocations live, preserving Plan mediation without weakening emergency stop behavior.
7. Establish a small implemented vertical slice, measurable acceptance cases, and a feasible v1 sequence. The broad capability scope is agreed; it is not yet an implementation estimate.

ADR-W030 proposes the architecture for review and lists the required amendments and open engineering decisions. Follow it with bounded implementation specifications after review; do not duplicate W024's foundational event-bus decision.

