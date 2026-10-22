# Wildstone Home Lab — Agent Architecture Summary

**Background and still-valid foundations (events, integrations, handlers, execution safety, privacy gateway).** ADR-W033 and [`docs/agent-system-design.md`](agent-system-design.md) are the authority; where they cover a topic, this document defers to them.

- **Governed by:** [ADR-W033](decisions/ADR-W033-the-agent-design.md) (including amendments A4.1, B7.1, B9.1, E2.1, and ADR-W039's amendment to B6) where they overlap.

---

Wildstone provides persistent Butlers (Agents) that reason, remember, use tools, and complete work for the household, each reached through its own Connections. The home lab enforces identity, permissions, privacy, scheduling, and execution boundaries independently of the model.

In this document "Connection" means how a Person reaches an Agent (ADR-W033 B7); an adapter to an application such as Home Assistant or CalDAV is an "integration connection".

## 1 Executive Summary

What an Agent is — classes, tools, model chain, Connections, memory — is defined in design contract §5–§9 and ADR-W033 B1–B11.

An agent can work through a direct request using multiple tool calls, inspect results, adapt its plan, ask a necessary question, and continue until completion or a defined limit. It does not need to turn every request into an automation. Scheduled and event-triggered routines may invoke the same reasoning loop when their task requires analysis.

Application databases remain authoritative for domain state. The event layer records meaningful changes; the vector index supports semantic recall and is rebuildable.

Deterministic code owns authentication, authorization, scheduling, deduplication, state transitions, privacy enforcement, and action receipts. Models plan and select tools within those boundaries.

Sensitive values are stored locally in a protected secrets service. Models receive opaque references by default; authorized local code controls use and reveal.

## 2 Target Architecture

There are three first-class entry paths. They converge on durable task execution and the same permissioned tools, without forcing direct conversation through an event-rule pipeline.

| Entry path | Routing and execution |
|---|---|
| Direct conversation | Verify the sender's binding and the Connection, route the Connection to its Agent, create or resume a task, retrieve permitted context, and enter the agent loop. |
| Scheduled routine | Load the approved routine version (its own principal, ADR-W033 E6/E6.0), recheck current capability and policy, then execute deterministic steps and invoke the assigned Agent only for inference (E8). |
| Service event | Translate the source event through an adapter, store the canonical fact, run deterministic conditions, then take a permitted action or invoke the assigned Agent. |

### Shared runtime services

The local platform supplies identity and identity bindings, an Agent registry, conversation and task storage, memory and retrieval, a secrets vault and privacy gateway, a scheduler and handler engine, a tool gateway, and audit records. These are logical boundaries; they do not require one server, model copy, or database deployment per agent.

Every invocation carries trusted identifiers for the household, the initiating Person or approved routine version, the Agent, the conversation audience, the task, and the delivery Connection. These identifiers are assigned and validated by the runtime, not accepted as authority from model output or an external message.

The agent loop retrieves targeted context, proposes a tool operation, receives an independently authorized result, and continues. Authorized effects emit domain events. Event-triggered execution uses correlation and origin identifiers to prevent recursive feedback loops. Model requests pass through the privacy gateway before external processing.

## 3 Home Lab as Source of Truth and Event Backbone

The home lab captures meaningful activity in an append-only event history. This is conceptually similar to observability: Prometheus exposes measurements for Grafana, while the Wildstone event layer exposes domain facts for automations and agents. The difference is that these events describe user- and service-level state changes rather than only infrastructure metrics.

A canonical event is boring, structured, and replayable. The envelope is defined in ADR-W024 A2 (`household_events`):

| Field | Purpose |
|---|---|
| event_id | Publisher-supplied unique identifier for deduplication and tracing. |
| occurred_at | Timestamp for when the domain event occurred. |
| recorded_at | When Keystone stored it; stamped by Keystone. |
| subject | Member/device/household the event concerns. |
| source | The publishing principal; stamped from its token. |
| type | Canonical type such as person.arrived_home or pantry.meal_logged. |
| entity | Optional affected object such as calendar event, device, work item, or media session. |
| payload | Small structured details necessary to interpret the event. |
| correlation_id | Groups events produced by the same workflow or action. |
| schema_version | Allows event contracts to evolve safely. |

This event store serves several purposes simultaneously: deterministic automation triggers, an audit trail, replay/debugging, agent context retrieval, and a foundation for future analytics. Application databases remain authoritative for application state; the event layer is not the only database for every domain.

Event privacy. Keep raw credentials, private transcripts, and unnecessary sensitive content out of the append-only event ledger. Store identifiers and minimal outcome metadata. Memory deletion must not be defeated by an immutable duplicate in event payloads. Replay and dry-run modes disable real side effects.

## 4 Integration Model

Applications remain independent. They do not need to know about one another. Each integration translates whatever signal the application already exposes into the Wildstone canonical event contract.

### Preferred event extraction methods

1. Native webhooks or event streams — Best option. Subscribe to application-native state changes and translate them directly.

2. First-party domain events — For Wildstone-built software such as PantryOS, emit a domain event whenever a relevant transaction commits. This is a development standard.

3. API polling — Acceptable when a third-party application has no push mechanism. Keep polling isolated inside the adapter and emit events only for detected changes.

4. Database change observation — Possible when the database is under your control, but used carefully to avoid coupling to internal schemas.

Log parsing is not a contract and is not accepted (ADR-W024 A3).

### Why the canonical intake API matters

A new application only needs to learn one Wildstone contract.

Handlers subscribe to canonical events instead of learning every vendor API.

First-party apps can emit events natively, while community integrations can provide thin adapters.

Adapters can be versioned, tested, replaced, or disabled without changing downstream automations.

This creates a straightforward extension target for future third-party or community-built Wildstone-compatible apps.

Adapter authority. Validate source identity and schemas at intake. An event subject identifies who an event concerns; it does not confer that person's authority. Scope each integration connection and its action capabilities independently.

## 5 Handler Rule Engine

The “handler” is not the agent gateway. It is the deterministic decision layer between the event bus and any expensive or probabilistic action. Its job is to answer: given this event, should anything happen, and if so, what workflow should run?

Example: breakfast check when leaving home

Home Assistant emits person.left_home.

A handler subscribed to that event checks whether a breakfast/meal event was recorded for that member during the relevant morning window.

If breakfast exists, the handler terminates. No LLM call is made.

If breakfast is missing, the handler asks a context provider for recent breakfasts and any other narrowly relevant data.

Only then is the agent invoked to phrase an intelligent question such as whether today matched the recent pattern.

The user answer is interpreted and routed through an executor to update the calorie tracker and/or PantryOS.

Those updates generate new domain events and close the loop.

This is the key token-control mechanism: most events never reach the LLM. Deterministic checks cheaply eliminate irrelevant cases.

Handlers filter event-driven work; they do not gate ordinary conversation. For a daily email briefing, the schedule and access checks are deterministic while the briefing analysis can use an LLM on every authorized run. A deterministic routine with no model dependency continues operating during a model outage (ADR-W033 E8).

## 6 Shared Context and Memory

Memory scopes, ownership, audiences, promotion, retrieval authorization and forgetting: ADR-W033 C1–C11 and design contract §11. Semantic (vector-backed) recall is required; a graph database remains optional and deferred.

| Layer | Responsibility |
|---|---|
| Application state | Authoritative inventory, calendars, lists, device state, and service configuration. Read live state when freshness matters. |
| Session context | Transient conversation/task state, scoped to the Person in its `Person + Agent + session_id` key. |
| Durable memory | Agent-local, Personal, and Household records with source, date, and audience (C1, C8). |
| Reference retrieval | Permissioned access to relevant email, notes, documents, and application records. |
| Vector index | Find related records when wording is fuzzy; combine with exact searches and metadata filters. Search results are candidates, not guaranteed facts. |
| Secrets store | Sensitive values protected separately; searchable memory contains safe descriptions and controlled references. |

Automatic memory. Remember useful stated preferences, decisions, and ongoing commitments. Avoid indiscriminate transcript or inbox ingestion.

Truth and freshness. Live applications govern current inventory, calendar, and service state. Remembered descriptions supply context and history. Retrieve narrowly at runtime rather than injecting all household history. No useful semantic match means no supported recollection; use exact search, live tools, or ask.

Untrusted sources. Emails and retrieved documents are evidence, not instructions. They cannot grant tool authority, change memory permissions, or establish an authoritative user preference merely by being retrieved. Preserve source attribution and distinguish a source claim from a confirmed fact.

Deletion. Remove forgotten material from active memory, derived indexes, summaries, and caches, and suppress automatic re-ingestion from the same source. A source application or backup may retain its own copy; disclose retention limits. Restore procedures must reapply deletion and revocation records before serving restored data.

Technical administrators with root and encryption-key access remain within the trusted infrastructure boundary; application privacy is not protection from that administrator.

## 7 Individual Butlers and Agent Behavior

Agent configuration, classes and Connections: design contract §5, §7, §8 and ADR-W033 B1–B9.1. Conversation continuity across Connections: ADR-W033 B10 and design contract §9.

Primary responsibilities are natural-language dialogue, multi-step planning and tool use, targeted recall, proposing routines as Plans, exception handling, and follow-through. The runtime owns scheduling, permissions, privacy gates, deduplication, and repeatable action state. The agent must not claim completion without a successful tool or scheduler receipt (E10).

Expose working, waiting for you, blocked, partially complete, finished, and cancelled states. Preserve pending questions and unfinished work across restarts. Show meaningful progress and the reason for a block. Context compaction must retain task state without widening memory visibility or restoring secrets into prompts.

## 8 Routine Authoring and Execution

A routine may consist of deterministic actions, an agent task, or a combination. A briefing can require reasoning every morning; a simple calendar exception can remain entirely deterministic. Conversational authoring produces the same structured definition consumed by the scheduler and handler engine.

The agent asks only for missing information needed to execute correctly, then submits the routine as a Plan. A human Owner approves the exact version before it runs; no Agent activates a routine, under any standing grant (ADR-W033 E1–E4). A draft or chat promise is not a scheduled routine. Ownership, authorization, stopping and capability loss: ADR-W033 E5–E8 and design contract §14–§15.

| Routine contract | Required fields or behavior |
|---|---|
| Trigger | Schedule or event subscription, conditions, explicit timezone, household-time versus traveler-time behavior, and catch-up policy. |
| Work | Deterministic steps or agent objective, permitted context sources, tools, resources, completion criteria, and execution limits. |
| Delivery | Output audience and delivery policy, approved as part of the version (E6), and behavior when the person does not respond. |
| Reliability | Deduplication, retries, timeout handling, partial completion records, cancellation, and failure reporting. |

Example: “Whenever I get home, ask whether I got the mail.” Resolve the member, presence source, frequency, delay, destination, completion, optional task or Home Assistant update, and no-response behavior. Do not require a home-lab mutation when a routine only needs to ask a question.

A daily email briefing requires authorized mailbox reads and delivery to the intended person. Sending or deleting emails is a separate capability; reading a mailbox does not grant either.

## 9 Example Calendar Driven Household Automation

A calendar integration can emit or expose the fact that the children have no school on a particular Friday. A deterministic rule then suppresses the normal pickup/check-in automation for that date. The LLM may be used when the calendar entry is ambiguous or when the rule is first drafted, but the recurring skip logic is deterministic once defined.

## 10 Executors and Permissions

Executors expose typed actions through the tool gateway: notifications, application operations, approved Home Assistant actions, scoped infrastructure operations, and explicitly enabled agent delegation. The runtime checks authority independently before every action. Schemas, timeouts, resource scopes, and audit outcomes are required.

Effective authority: design contract §10 (interactive §10.1, routine §10.2) and ADR-W033 A0. A source event, model instruction, bot identity, or delegated agent cannot supply missing authority.

| Action class | Default |
|---|---|
| Read and prepare | Search permitted information, summarize, plan, and draft without repeated confirmation. |
| Reversible household work | Perform explicitly requested actions on authorized lists or household resources; report the result. |
| Routine creation | Submit a Plan; a human Owner approves the exact version (E2). |
| Consequential action | Approve concrete details for external messages, purchases, important deletion, or security changes. |
| Service operation | Owner-class tools, granted only to Owner Agents (B3). |
| Administrative change | Service-package rewrites, executable plugin installation, policy changes, and Wildstone code edits use a separate administrative change workflow. |

### Approval and enforcement

Approval binds the exact action, target, parameters, recipient, and approver. Material changes invalidate it. An unanswered approval does not authorize execution.

Operating a service is different from modifying its implementation. Permission to restart Jellyfin does not permit rewriting its Compose package, replacing its image, editing arbitrary files, or changing Wildstone policy. Enforce restrictions with scoped APIs, credentials, filesystem boundaries, and network controls. A prompt or shell-pattern denylist alone is insufficient containment.

Routine household workflows have no arbitrary host shell. Any optional exploratory execution belongs in an explicitly granted isolated workspace with restricted mounts, network access, and credentials. Installed MCP tools and plugins are subject to the same authorization and privacy boundaries as built-in tools.

## 11 Accepted Design State

Current design: ADR-W033 and design contract; implementation status: [`docs/audit/gap-report.md`](audit/gap-report.md).

## 12 Design Principles to Preserve

Persistent assistants. Make direct conversation, reasoning, tool use, and task completion useful without requiring a workflow definition for every request.

Shared knowledge with explicit visibility. People and the household own Personal and Household knowledge. Switching agents preserves access to permitted facts; changing audiences can change what may be disclosed.

Deterministic enforcement. Code owns authority, schedules, privacy, and action state. Reasoning-dependent tasks can pause during a model outage while independent deterministic routines continue.

Stable integration boundaries. Apps remain independent. Adapters normalize domain events and expose constrained actions through a common contract.

Retrieve with provenance. Use targeted, permission-filtered retrieval. Preserve source, date, and uncertainty. Application state remains authoritative.

No privilege expansion by indirection. A routine, delegation, remembered instruction, learned skill, or model switch cannot bypass an existing denial.

Evidence and recoverability. Persist task outcomes, verify completion, make side-effect-free replay possible, and resume without repeating completed actions.

Provider independence with privacy. Support local and cloud models while enforcing data policies independently. Changing provider requires a permitted data path, including fallback behavior.

## 13 Implementation Sequence and Audit Handoff

Implementation order: design contract §19. Semantic retrieval is required, but the database product is not prescribed. Do not introduce a separate deployment for every logical component unless isolation or operational needs justify it.

## 14 System Responsibilities

| Component | Responsibility |
|---|---|
| Wildstone platform | Identity, Connection routing, grants, memory, protected secrets, privacy, task persistence, scheduling, event history, execution, and audit. |
| Individual Butler | Purpose and personality, dialogue, plans, permitted retrieval, tool selection, routine proposals, exceptions, and clear progress or completion reporting. |
| Integrations | Translate native signals to canonical facts, expose scoped actions, and preserve application ownership of domain state. |

What each Person may do is set by their role (Owner or Member, ADR-W033 A1) and design contract §2–§3.

## 15 Sensitive Information and External Processing

Secret scopes, permissions, reveal surfaces and routine use: ADR-W033 D1–D7 and design contract §12.

### Local storage and controlled references

Sensitive information intentionally retained by a user is stored in an encrypted local secrets service, with protected key management and an explicit audience. General memory holds safe descriptions and references. Temporary verification codes and credentials discovered incidentally in email are excluded from automatic durable memory by default.

Use random opaque references, not hashes derived from passwords. A reference identifies a value but is not a bearer authorization. Bind temporary references to the request and check current identity, audience, target, and validity whenever resolving them. Reject fabricated, copied, expired, revoked, or unrelated references.

Use versus reveal. A tool may use a credential to access an approved service without permission to reveal its value. Inject credentials only into approved fields for approved destinations. Do not perform blanket replacement inside arbitrary model-generated URLs, emails, tool arguments, or group messages.

Protected display. Models may request a structured secret-display component. Trusted local code authorizes the destination and renders the value. Keep restored values out of ordinary transcripts, future model prompts, general logs, embeddings, and summaries.

Example: the household Wi-Fi password is recorded as a Household secret with an adult audience. An authorized Agent can help Mika recall it. A cloud model receives a temporary reference, and local code reveals the value only on a trusted reveal surface (D4). Jude or an unrelated conversation cannot resolve that reference.

### Privacy gateway and limitations

Inspect and transform data before every external processing boundary: model inference, embeddings, transcription, attachments, screenshots, tracing, and external tools. All model calls, including retries, summarization, and fallback, must use the gateway. Restrict browser, network, and executable tools so they cannot bypass the same data policy.

Explicitly classified values and known secrets receive deterministic protection. Pattern and contextual detection add defense for incidental sensitive information, but cannot guarantee detection of every password or sensitive meaning. Tokenization of a value does not anonymize all surrounding context. Support source-level restrictions such as keeping a mailbox local.

If inspection fails, the request stops. Unsupported or uninspectable content remains local or requires an explicit user decision through an authorized flow. Cloud fallback must satisfy the source policy; a local-only task must never fall back silently to cloud processing (B6).

Local models still use secret references by default to limit unnecessary exposure. Local inference alone does not make a workflow local: Telegram, Slack, cloud speech processing, or other services may already have received the content (D7). Secrets marked to remain inside the home must be captured and revealed in the local authenticated interface; external bots can direct the user there.

## 16 Operational Edge Cases and Defaults

### People and communication

Unknown senders, children, groups: an unknown or unbound account is refused before inference and gets no reply (ADR-W033 B9, B9.1); bindings come from authenticated enrolment (A2, B7.1); a child is a scheduled subject with no bindings (A8); Members cannot reach Owner Agents (B2); group messages reach an Agent only when it is addressed (B9.1). A claimed name, discovered bot username, or voice alone establishes nothing.

Delivery failures. Stop delivery through removed or misconfigured Connections. Never silently switch to another Agent's bot or another recipient (B11). Preserve pending delivery state and show the failure without disclosing private payloads.

### Routines and recovery

Uncertain effects and retries: ADR-W033 E10.

Restart and partial completion. Persist completed steps and pending work. Resume only unfinished work. Cancellation stops further actions where possible but cannot undo completed effects. Distinguish partial success from full completion.

Missed schedules. Set a catch-up policy per routine. Collapse missed briefings into one current briefing; skip stale physical actions by default. Store timezone explicitly, account for daylight-saving changes without accidental duplicate execution, and state whether travel changes the schedule.

Revoked authority: ADR-W033 F1 and design contract §17.1. Never substitute another person's credentials.

Conflicting automation. Coordinate writes to the same resource and detect recurring conflicts between agents, handlers, and application rules. Pause and surface conflicting routines rather than allowing repeated opposing commands.

Network and self-maintenance. Unattended Wi-Fi or network changes require a tested local recovery path because they may remove the agent's communication channel. Changes to the agent runtime or its host belong in controlled maintenance with recovery independent of the running agent.

Unavailable dependencies. Preserve unfinished work and report the failed model or integration. Continue unrelated deterministic routines. Apply finite retry, duration, step, and spending limits; exact values must be explicit configuration rather than unlimited defaults.

### Delegation and learning

Delegation. Carry the initiating person, task scope, allowed data, and bounded authority to each explicitly enabled delegate. The delegate may further restrict that authority but cannot widen it, bypass a denial, or combine grants to gain new privileges. Shared knowledge retrieval does not require delegation. Wildstone never substitutes another Agent automatically (B6, E8).

Loops. Track event origin and task correlation. Bound delegation depth, event recursion, tool steps, runtime, and cost. Stop recurring agent-to-agent or routine-to-routine loops and show a recoverable failure.

Learned procedures. Agents may save reusable procedures composed of already authorized tools as versioned definitions. Saving grants no authority; activating or replacing a routine follows the Plan and Owner-approval path (E11). Executable code, new plugins, credentials, wider grants, or platform modifications require administrative review.

### Agent lifecycle and incident controls

Clone, pause, retire, delete: ADR-W033 B1, E8, F3, F4 and design contract §17.2. A clone never duplicates bot credentials (B7.1), private transcripts, or running routines. Hiding an agent in the UI must not be confused with pausing it.

Incident response. Provide a household-wide stop (E6.1) and targeted Connection revocation. Record actors, targets, decisions, and action receipts without raw secrets. Recovery and backup procedures must preserve revocations, deletion policies, routine state, and key access controls.

## 17 Repository Audit Acceptance Criteria

Behavioral gates alongside design contract §18, which governs where they differ. An architecture description, UI screenshot, or passing unrelated CI job is not proof. Use synthetic secrets and test accounts; do not publish household credentials in evidence.

### AC01 Shared memory across agents

Giulia saves a gift idea through Butler A to an adults-only audience. Butler B retrieves it for Mika. A request from Jude and a household group including Jude do not expose the entry. Record both successful retrieval and denied disclosure.

### AC02 Routine proposal and approval

An Agent submits an email-briefing Plan using authorized reads and a specific delivery destination. It does not run until a human Owner approves that exact version. After approval, verify routine ID, next run, actual execution, and that it acquires no ungranted send or delete privilege.

### AC03 Person authority follows the request

A Member cannot reach an Owner Agent, even with malformed admission configuration. An Owner using an Owner Agent can perform a separately granted Owner-tool operation.

### AC04 Cloud assisted secret recall

Store a synthetic Wi-Fi secret locally, retrieve it through another authorized Butler, and inspect model-bound payloads for substitutes only. Reveal it through the approved local component. Reject expired references and attempts to inject it into a URL, tool destination, or group message.

### AC05 Crash after a completed action

Simulate a crash after an application accepts an action but before local completion is recorded. Recovery checks or deduplicates the effect and does not blindly repeat it; unsupported verification produces an uncertain state.

### AC06 Revocation and cached context

Queue work and retrieve private context, then revoke the person or Connection. Subsequent execution and disclosure fail, including cached prompts and active task continuation. Routines that deliver to the revoked Person pause for Owner resolution; other Household routines continue (F1).

### AC07 Untrusted email instructions

Supply an email that attempts to change permissions, poison authoritative memory, or export a secret. It remains attributed source content and cannot authorize those effects.

### AC08 Independent bot routing

Create two Butlers with different bot accounts. Direct replies, routine notifications, errors, and reconnect behavior use the correct identity. Switching Butlers preserves permitted shared knowledge without merging private conversations.

### AC09 Group and unknown-sender boundaries

An unknown account gets no reply. An unaddressed group message never reaches the Agent; an addressed one from an unbound account is silence; an Owner Agent answers only in direct messages (B9.1).

### AC10 Approval integrity

Approve a concrete consequential action or Plan, then change its target or parameters. Execution requires new approval of the new version.

### AC11 Memory correction and forgetting

Make concurrent conflicting memory writes, correct a fact, and forget a source-derived entry. Verify version handling, source attribution, index and cache invalidation, and suppression of re-ingestion. Document backup retention behavior.

### AC12 Local only processing

Disable the local model and attempt cloud fallback, embedding, transcription, attachment processing, and an external tool call with local-only content. Verify policy enforcement and stopping when inspection is unavailable.

### AC13 Scheduling and conflict handling

Exercise a missed briefing, stale physical action, daylight-saving boundary, and two routines targeting the same resource. Verify the configured catch-up policy and conflict handling without duplicate effects.

### AC14 Delegation and execution budgets

Delegate a bounded task and attempt privilege expansion or recursive handoffs. Verify the initiating identity is preserved, denials remain effective, and configured limits stop the loop.

### AC15 Lifecycle and partial work

Pause, cancel, clone, and delete an agent with active and scheduled work. Verify accurate partial status, that deterministic routine steps continue while inference steps block visibly (E8), no duplicated bot credentials or routines, and preservation of Personal and Household memory.

### AC16 Package integrity and learned skills

An agent learns a useful procedure using granted tools, while an attempted service-package rewrite or executable plugin install is routed to administrative review. Verify service-operation permission alone cannot modify the package.

## 18 Reference Products and Design Rationale

External references consulted in September 2026; product behavior may have changed, and none is evidence of Wildstone implementation. Hermes and OpenClaw as external runtimes: ADR-W039.

- Hermes Agent: named bots, persistent chats, profile-scoped memory, and an egress proxy using opaque credential references. [Bot Mode](https://hermes-agent.nousresearch.com/docs/user-guide/bot-mode) · [Memory](https://hermes-agent.nousresearch.com/docs/user-guide/features/memory) · [Security](https://hermes-agent.nousresearch.com/docs/user-guide/security) · [Egress Proxy](https://hermes-agent.nousresearch.com/docs/user-guide/egress/)
- OpenClaw: multi-agent routing to separate bot accounts; its trust model assumes one operator or a mutually trusting team, where Wildstone enforces authority per Person. [Multi Agent Routing](https://docs.openclaw.ai/concepts/multi-agent) · [Trust Model](https://docs.openclaw.ai/gateway/security/trust-model)
- Grok Bot: persistent named agents; its account-scoped computer shares files and logins across bots, where Wildstone grants integration connections individually. [Overview](https://docs.x.ai/grok-bot/overview)
- OWASP and Qdrant: prompt-only safeguards and vector metadata filters do not supply household authorization policy. [Sensitive Information Disclosure](https://genai.owasp.org/llmrisk/llm022025-sensitive-information-disclosure/) · [Excessive Agency](https://genai.owasp.org/llmrisk/llm062025-excessive-agency/) · [Qdrant Filtering](https://qdrant.tech/documentation/search/filtering/)
