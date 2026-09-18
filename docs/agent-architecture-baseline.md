# Wildstone Home Lab — Agent Architecture Summary

**Accepted architecture baseline and repository audit handoff.**
Revised 10 September 2026 · original summary 5 September 2026.

---

Wildstone provides persistent, individually addressable Butlers that reason, remember, use tools, and complete work for household members. Shared household knowledge provides continuity across agents. The home lab enforces identity, permissions, privacy, scheduling, and execution boundaries independently of the model.

Revision scope: The accepted defaults in this revision supersede conflicting language in the original summary. Sections 3, 4, 5, and 9 retain the event and integration foundation. Agent behavior, shared memory, authorization, routines, privacy, and audit criteria are now explicit. These are implementation targets; repository conformance has not yet been established.

## 1 Executive Summary

Each Butler is a persistent assistant with a stable identity, directive, selected model, dedicated chat connections, granted tools, and assigned routines. Users may create arbitrary Butlers. Household and operator are permission profiles; Telegram, Slack, Discord, web, and future voice interfaces are communication connections. They are separate concepts.

An agent can work through a direct request using multiple tool calls, inspect results, adapt its plan, ask a necessary question, and continue until completion or a defined limit. It does not need to turn every request into an automation. Scheduled and event-triggered routines may invoke the same reasoning loop when their task requires analysis.

Shared knowledge belongs to people and the household. Authorized Butlers use one shared memory service with explicit audiences; the agent that learned a fact does not own exclusive access to it.

Application databases remain authoritative for domain state. The event layer records meaningful changes; the vector index supports semantic recall and is rebuildable.

Deterministic code owns authentication, authorization, scheduling, deduplication, state transitions, privacy enforcement, and action receipts. Models plan and select tools within those boundaries.

Sensitive values are stored locally in a protected secrets service. Models receive opaque references by default; authorized local code controls use and reveal.

Both permission profiles can author and run routines. A routine can only perform actions allowed by its owner, agent, connection, and bounded authorization.

## 2 Target Architecture

There are three first-class entry paths. They converge on durable task execution and the same permissioned tools, without forcing direct conversation through an event-rule pipeline.

| Entry path | Routing and execution |
|---|---|
| Direct conversation | Verify the sender and destination, route the specific bot account to its Butler, create or resume a task, retrieve permitted context, and enter the agent loop. |
| Scheduled routine | Load the saved routine and owner grant, apply timing and current permissions, then execute deterministic steps or invoke its responsible Butler. |
| Service event | Translate the source event through an adapter, store the canonical fact, run deterministic conditions, then take a permitted action or invoke the assigned Butler. |

### Shared runtime services

The local platform supplies identity and account linking, a Butler registry, conversation and task storage, shared memory and retrieval, a secrets vault and privacy gateway, a scheduler and handler engine, a tool gateway, and audit records. These are logical boundaries; they do not require one server, model copy, or database deployment per agent.

Every invocation carries trusted identifiers for the household, initiating person or routine grant, Butler, conversation audience, task, and delivery connection. These identifiers are assigned and validated by the runtime, not accepted as authority from model output or an external message.

The agent loop retrieves targeted context, proposes a tool operation, receives an independently authorized result, and continues. Authorized effects emit domain events. Event-triggered execution uses correlation and origin identifiers to prevent recursive feedback loops. Model requests pass through the privacy gateway before external processing.

## 3 Home Lab as Source of Truth and Event Backbone

The home lab should capture meaningful activity in an append-only event history. This is conceptually similar to observability: Prometheus exposes measurements for Grafana, while the Wildstone event layer exposes domain facts for automations and agents. The difference is that these events describe user- and service-level state changes rather than only infrastructure metrics.

A canonical event should be boring, structured, and replayable. A practical MVP shape could include:

| Field | Purpose |
|---|---|
| event_id | Globally unique identifier for deduplication and tracing. |
| occurred_at | Timestamp for when the domain event occurred. |
| subject_id | Member/user/device/household identity associated with the event. |
| source | Service or adapter that emitted the event. |
| event_type | Canonical type such as person.arrived_home or pantry.meal_logged. |
| entity_id | Optional affected object such as calendar event, device, work item, or media session. |
| payload | Small structured details necessary to interpret the event. |
| correlation_id | Groups events produced by the same workflow or action. |
| schema_version | Allows event contracts to evolve safely. |

This event store can serve several purposes simultaneously: deterministic automation triggers, an audit trail, replay/debugging, agent context retrieval, and a foundation for future analytics. However, application databases remain authoritative for application state; the event layer should not become the only database for every domain.

Event privacy. Keep raw credentials, private transcripts, and unnecessary sensitive content out of the append-only event ledger. Store identifiers and minimal outcome metadata. Memory deletion must not be defeated by an immutable duplicate in event payloads. Replay and dry-run modes disable real side effects.

## 4 Integration Model

The desired integration model is that applications remain independent. They do not need to know about one another. Each integration translates whatever signal the application already exposes into the Wildstone canonical event contract.

### Preferred event extraction methods

1. Native webhooks or event streams — Best option. Subscribe to application-native state changes and translate them directly.

2. First-party domain events — For Wildstone-built software such as PantryOS, emit a domain event whenever a relevant transaction commits. This should be a development standard.

3. API polling — Acceptable when a third-party application has no push mechanism. Keep polling isolated inside the adapter and emit events only for detected changes.

4. Database change observation — Possible when the database is under your control, but should be used carefully to avoid coupling to internal schemas.

5. Log parsing / side-effect observation — Last resort. Logs are not guaranteed domain contracts and are more brittle across versions.

### Why the canonical intake API matters

A new application only needs to learn one Wildstone contract.

Handlers subscribe to canonical events instead of learning every vendor API.

First-party apps can emit events natively, while community integrations can provide thin adapters.

Adapters can be versioned, tested, replaced, or disabled without changing downstream automations.

This creates a straightforward extension target for future third-party or community-built Wildstone-compatible apps.

Adapter authority. Validate source identity and schemas at intake. An event subject identifies who an event concerns; it does not confer that person's authority. Scope each service connection and its action capabilities independently. Home Assistant and IMAP are the initial connector priorities; PantryOS and calendar support extend the same contract.

## 5 Handler Rule Engine

The “handler” discussed is not the agent gateway. It is the deterministic decision layer between the event bus and any expensive or probabilistic action. Its job is to answer: given this event, should anything happen, and if so, what workflow should run?

Example: breakfast check when leaving home

Home Assistant emits person.left_home.

A handler subscribed to that event checks whether a breakfast/meal event was recorded for that member during the relevant morning window.

If breakfast exists, the handler terminates. No LLM call is made.

If breakfast is missing, the handler asks a context provider for recent breakfasts and any other narrowly relevant data.

Only then is the agent invoked to phrase an intelligent question such as whether today matched the recent pattern.

The user answer is interpreted and routed through an executor to update the calorie tracker and/or PantryOS.

Those updates generate new domain events and close the loop.

This is the key token-control mechanism: most events never reach the LLM. Deterministic checks cheaply eliminate irrelevant cases.

Handlers filter event-driven work; they do not gate ordinary conversation. For a daily email briefing, the schedule and access checks are deterministic while the briefing analysis can use an LLM on every authorized run. A deterministic routine with no model dependency should continue operating during a model outage.

## 6 Shared Context and Memory

Accepted change: semantic recall is required in the agent baseline. Vector-backed retrieval is no longer deferred. A graph database remains optional and deferred.

One shared memory service stores household knowledge and personal knowledge with explicit visibility. Every authorized Butler can retrieve the same permitted facts. Agent working state and conversation histories remain scoped to their tasks and participants; they are not a substitute for shared durable memory.

| Layer | Responsibility |
|---|---|
| Application state | Authoritative inventory, calendars, lists, device state, and service configuration. Read live state when freshness matters. |
| Conversation history | Source exchanges and unfinished context, limited to their participants and approved connections. |
| Durable memory | Useful stated preferences, decisions, commitments, and historical context, with source, date, and audience. |
| Reference retrieval | Permissioned access to relevant email, notes, documents, and application records. |
| Vector index | Find related records when wording is fuzzy; combine with exact searches and metadata filters. Search results are candidates, not guaranteed facts. |
| Secrets store | Sensitive values protected separately; searchable memory contains safe descriptions and controlled references. |

### Ownership and visibility

| Scope | Example | Visibility |
|---|---|---|
| Personal | Preferred tea preparation | Authorized Butlers assisting that person; wider sharing only through a configured rule or explicit choice. |
| Household | Shared shopping plans | Butlers assisting members of the defined household audience. |
| Selected people | Christmas ideas for Jude | Designated adults; the subject of the memory is not automatically in its audience. |
| Private | Explicitly confidential note | Butlers assisting the specified person, through a suitable response destination. |

Default audience follows the source: a private conversation produces personal memory; household conversations and shared lists retain their existing audience. Users may configure broader sharing and say “keep this private” or “share this with these people.” Cross-agent access is enabled through the shared service; cross-person disclosure remains audience-controlled.

Example: Giulia tells one Butler that Jude wants a particular Minecraft toy and saves it to an adults-only gift list. A different Butler can retrieve it for Mika without receiving Giulia's full transcript. The gift list remains authoritative, and a request from Jude does not reveal the entry.

### Memory records and lifecycle

Each record needs a stable ID, household, subject, audience, source reference, contributor, creation and relevant event dates, version, status, and sensitivity or retention policy. Records can be current, superseded, disputed, expired, or deleted. The model may propose metadata; trusted code validates ownership and permitted audiences.

Automatic memory. Remember useful stated preferences, decisions, and ongoing commitments. Avoid indiscriminate transcript or inbox ingestion. Provide a visible memory activity log, editor, “remember this,” “forget this,” and controls for source scope, retention, and sharing.

Corrections and concurrent writes. Preserve attribution. A person can correct their own preference. Conflicting household claims remain attributed and unresolved until clarified. Use version checks to prevent two Butlers silently overwriting each other. Invalidate cached context after corrections or permission changes.

Truth and freshness. Live applications govern current inventory, calendar, and service state. Remembered descriptions supply context and history. Retrieve narrowly at runtime rather than injecting all household history. No useful semantic match means no supported recollection; use exact search, live tools, or ask.

Untrusted sources. Emails and retrieved documents are evidence, not instructions. They cannot grant tool authority, change memory permissions, or establish an authoritative user preference merely by being retrieved. Preserve source attribution and distinguish a source claim from a confirmed fact.

Deletion. Remove forgotten material from active memory, derived indexes, summaries, and caches, and suppress automatic re-ingestion from the same source. A source application or backup may retain its own copy; disclose retention limits. Restore procedures must reapply deletion and revocation records before serving restored data.

Retrieval authorization. Filter by current person, household, agent grant, and response audience before content reaches a model. Prevent stale prompts or cached context from retaining revoked access. Technical administrators with root and encryption-key access remain within the trusted infrastructure boundary; application privacy is not protection from that administrator.

## 7 Individual Butlers and Agent Behavior

Accepted change: the assistant is a first-class persistent capability. It can complete open-ended tasks and agentic routines, alongside deterministic household automation.

| Configuration | Required meaning |
|---|---|
| Identity and directive | Stable Butler ID, display name, purpose, instructions, owner or administrators, and lifecycle status. |
| Permission profile | Household or operator capability eligibility. Operator selection does not automatically grant every infrastructure tool. |
| Model | Selected provider and model, permitted fallback policy, privacy constraints, and execution budgets. |
| Chat connections | Dedicated bot identity per enabled external platform, with explicit account-to-Butler binding. Three Telegram-enabled Butlers require three Telegram bots. |
| People and groups | Paired users, allowed groups, permitted actions, and suitable delivery destinations. |
| Tools and connections | Explicit action grants, resource scopes, and credential references; no automatic inheritance of another agent's credentials. |
| Memory access | Grants into shared person and household knowledge; no agent-exclusive silo for ordinary durable facts. |
| Routines and work | Named routine ownership or participation, durable task state, cancellation controls, and evidence of execution. |

The same Butler may be reachable through web, Telegram, and Slack. Account linking requires authenticated enrollment; display names do not establish identity. Histories stay separate by default, while permitted shared memory maintains continuity. Users may deliberately link conversations.

Primary responsibilities are natural-language dialogue, multi-step planning and tool use, targeted recall, routine authoring and tuning, exception handling, and follow-through. The runtime owns scheduling, permissions, privacy gates, deduplication, and repeatable action state. The agent must not claim completion without a successful tool or scheduler receipt.

Expose working, waiting for you, blocked, partially complete, finished, and cancelled states. Preserve pending questions and unfinished work across restarts. Show meaningful progress and the reason for a block. Context compaction must retain task state without widening memory visibility or restoring secrets into prompts.

## 8 Routine Authoring and Execution

Both household and operator Butlers can help users create and run routines. A routine may consist of deterministic actions, an agent task, or a combination. A briefing can require reasoning every morning; a simple calendar exception can remain entirely deterministic. Conversational authoring saves the same structured definition consumed by the scheduler and handler engine.

The agent asks only for missing information needed to execute correctly. When a request is sufficiently specified and within existing permissions, persist and activate it without redundant confirmation. Return the routine ID, next run time or trigger, responsible Butler, actions, and recipients. A draft or chat promise is not a scheduled routine.

| Routine contract | Required fields or behavior |
|---|---|
| Ownership | Stable ID and version, owner, responsible Butler, any participants, and a bounded authorization grant. |
| Trigger | Schedule or event subscription, conditions, explicit timezone, household-time versus traveler-time behavior, and catch-up policy. |
| Work | Deterministic steps or agent objective, permitted context sources, tools, resources, completion criteria, and execution limits. |
| Delivery | Specific Butler bot connection and destination, audience policy, and behavior when the person does not respond. |
| Reliability | Deduplication, retries, timeout handling, partial completion records, cancellation, and failure reporting. |
| Lifecycle | Enabled or paused status, current permissions checked at each run, revision history, and reassignment rules. |

Example: “Whenever I get home, ask whether I got the mail.” Resolve the member, presence source, frequency, delay, destination, completion, optional task or Home Assistant update, and no-response behavior. Do not require a home-lab mutation when a routine only needs to ask a question.

A daily email briefing requires authorized mailbox reads and delivery to the intended person. It is valid for either permission profile. Sending or deleting emails is a separate capability; reading a mailbox does not grant either.

## 9 Example Calendar Driven Household Automation

Another example was school pickup/check-in behavior. A calendar integration could emit or expose the fact that the children have no school on a particular Friday. A deterministic rule can then suppress the normal pickup/check-in automation for that date. The LLM may be used when the calendar entry is ambiguous or when the user is initially creating the rule, but the recurring skip logic should be deterministic once defined.

## 10 Executors and Permissions

Executors expose typed actions through the tool gateway: notifications, application operations, approved Home Assistant actions, scoped infrastructure operations, and explicitly enabled agent delegation. The runtime checks authority independently before every action. Schemas, timeouts, resource scopes, and audit outcomes are required.

Effective authority is the intersection of the initiating person's current authority, the Butler's grants, the service connection's scope, and the task or routine's bounded authorization. Channel and disclosure rules can further restrict an action. A routine runs under its owner grant; a source event, model instruction, bot identity, or delegated agent cannot supply missing authority.

| Action class | Accepted default |
|---|---|
| Read and prepare | Search permitted information, summarize, plan, and draft without repeated confirmation. |
| Reversible household work | Perform explicitly requested actions on authorized lists or household resources; report the result. |
| Routine creation | Activate a sufficiently specified routine within existing permissions; show schedule, actions, and recipients. |
| Consequential action | Approve concrete details for external messages, purchases, important deletion, or security changes unless a narrow standing authorization already covers them. |
| Service operation | Operator grants cover specified service operations. Standing authorization may permit a named service and tested recovery procedure. |
| Administrative change | Service-package rewrites, executable plugin installation, policy changes, and Wildstone code edits use a separate administrative change workflow. |

### Approval and enforcement

Approval binds the exact action, target, parameters, recipient, approver, and validity period. Material changes invalidate it. Unanswered or expired approvals do not authorize execution. The system can offer narrow standing permissions to avoid repeated prompts for the same bounded work; an agent cannot grant these permissions to itself.

Operating a service is different from modifying its implementation. Permission to restart Jellyfin does not permit rewriting its Compose package, replacing its image, editing arbitrary files, or changing Wildstone policy. Enforce restrictions with scoped APIs, credentials, filesystem boundaries, and network controls. A prompt or shell-pattern denylist alone is insufficient containment.

Routine household workflows have no arbitrary host shell. Any optional exploratory execution belongs in an explicitly granted isolated workspace with restricted mounts, network access, and credentials. Installed MCP tools and plugins are subject to the same authorization and privacy boundaries as built-in tools.

## 11 Accepted Design State

The following baseline was accepted on September 10, 2026. Product defaults are starting points that may evolve through explicit design decisions. None of these entries establishes that the repository already implements the requirement.

| Area | Baseline | State |
|---|---|---|
| Agent creation | User-defined name, directive, permission profile, and model; expand with explicit connections, grants, and routines. | Accepted |
| Agent experience | Persistent multi-step assistant, dedicated bot identities, durable task state. | Accepted |
| Memory | Shared service with person and household audiences; semantic retrieval required. | Accepted |
| Secrets | Local protected values, model-visible substitutes, separately authorized use and reveal. | Accepted |
| Routines | Both profiles; deterministic scheduling plus optional recurring reasoning. | Accepted |
| Backbone | Canonical events, adapters, targeted context, typed executors, replay without effects. | Preserved |
| Graph database | Optional derived relationship index. | Deferred |
| Implementation choices | Concrete schemas, engine or provider selection, numeric budgets, retention periods, and deployment packaging. | Resolve during audit and planning |

Reported current behavior: users create a Butler with name, directive, household or operator surface, and model, then converse and request routines. Exact routine permissions and enforcement are unverified. Do not infer implementation from UI labels or from this architecture baseline.

## 12 Design Principles to Preserve

Persistent assistants. Make direct conversation, reasoning, tool use, and task completion useful without requiring a workflow definition for every request.

Shared knowledge with explicit visibility. People and the household own durable knowledge. Switching agents preserves access to permitted facts; changing audiences can change what may be disclosed.

Deterministic enforcement. Code owns authority, schedules, privacy, and action state. Reasoning-dependent tasks can pause during a model outage while independent deterministic routines continue.

Stable integration boundaries. Apps remain independent. Adapters normalize domain events and expose constrained actions through a common contract.

Retrieve with provenance. Use targeted, permission-filtered retrieval. Preserve source, date, and uncertainty. Application state remains authoritative.

No privilege expansion by indirection. A routine, delegation, remembered instruction, learned skill, or model switch cannot bypass an existing denial.

Evidence and recoverability. Persist task outcomes, verify completion, make side-effect-free replay possible, and resume without repeating completed actions.

Provider independence with privacy. Support local and cloud models while enforcing data policies independently. Changing provider requires a permitted data path, including fallback behavior.

## 13 Implementation Sequence and Audit Handoff

The next session should audit the repository against this baseline before proposing remediation. Identify the exact repository and commit, read its contribution and architecture instructions, inspect relevant code and migrations, and exercise runtime behavior where available. Distinguish observed behavior from inference and inaccessible deployment checks.

First, map existing Butler creation, bot routing, identity, tool grants, task persistence, routine scheduling, and memory paths to these requirements. Record implemented, partial, missing, or unverified status with code and runtime evidence.

Next, prioritize identity and authorization, shared-memory visibility, privacy boundaries, and durable execution before increasing tool reach. Preserve existing working integrations and contracts where they satisfy the baseline.

Then prove two independent Butlers with dedicated chat identities, shared permitted memory, and useful direct multi-step work. Test both allowed and denied paths with adult and child identities.

Prove an email briefing and the existing event-driven breakfast example. Initial adapter priorities remain Home Assistant and IMAP; use PantryOS and calendar when their real contracts are available. Label substitutes or fixtures explicitly.

Complete the acceptance scenarios in section 17. Produce a prioritized gap report and concrete implementation plan; surface decisions needed for any exceptions rather than silently weakening the baseline.

Choose concrete engines, schemas, numeric budgets, and retention periods after grounding in the repository. Semantic retrieval is required, but the database product is not prescribed. Do not introduce a separate deployment for every logical component unless isolation or operational needs justify it.

## 14 System Responsibilities

| Component | Responsibility |
|---|---|
| Wildstone platform | Identity, bot routing, grants, shared memory, protected secrets, privacy, task persistence, scheduling, event history, execution, and audit. |
| Individual Butler | Purpose and personality, dialogue, plans, permitted retrieval, tool selection, routine authoring, exceptions, and clear progress or completion reporting. |
| Integrations | Translate native signals to canonical facts, expose scoped actions, and preserve application ownership of domain state. |
| People | Choose assistants, authorize connections and capabilities, control memory audiences and retention, approve consequential work, and correct outcomes. |

Independent Butlers share knowledge through the household memory service. They remain individually addressable and permissioned. The deterministic backbone supports useful agent autonomy and protects the home lab from unintended changes.

## 15 Sensitive Information and External Processing

### Local storage and controlled references

Sensitive information intentionally retained by a user is stored in an encrypted local secrets service, with protected key management and an explicit audience. General memory holds safe descriptions and references. Temporary verification codes and credentials discovered incidentally in email are excluded from automatic durable memory by default. Users control intentional retention and permitted disclosure.

Use random opaque references, not hashes derived from passwords. A reference identifies a value but is not a bearer authorization. Bind temporary references to the request and check current identity, audience, target, and validity whenever resolving them. Reject fabricated, copied, expired, revoked, or unrelated references.

Use versus reveal. A tool may use a credential to access an approved service without permission to reveal its value. Inject credentials only into approved fields for approved destinations. Do not perform blanket replacement inside arbitrary model-generated URLs, emails, tool arguments, or group messages.

Protected display. Models may request a structured secret-display component. Trusted local code authorizes the destination and renders the value. Keep restored values out of ordinary transcripts, future model prompts, general logs, embeddings, and summaries.

Example: Giulia records the household Wi-Fi password through the authenticated local interface. Shared memory records its safe label and adult audience. Any authorized Butler can help Mika recall it. A cloud model receives a temporary reference, and local code reveals the value only in the approved interface. Jude or an unrelated conversation cannot resolve that reference.

### Privacy gateway and limitations

Inspect and transform data before every external processing boundary: model inference, embeddings, transcription, attachments, screenshots, tracing, and external tools. All model calls, including retries, summarization, and fallback, must use the gateway. Restrict browser, network, and executable tools so they cannot bypass the same data policy.

Explicitly classified values and known secrets receive deterministic protection. Pattern and contextual detection add defense for incidental sensitive information, but cannot guarantee detection of every password or sensitive meaning. Tokenization of a value does not anonymize all surrounding context. Support source-level restrictions such as keeping a mailbox local.

If inspection fails, the request stops. Unsupported or uninspectable content remains local or requires an explicit user decision through an authorized flow. Cloud fallback is opt-in and must satisfy the source policy; a local-only task must never fall back silently to cloud processing.

Local models still use secret references by default to limit unnecessary exposure. Local inference alone does not make a workflow local: Telegram, Slack, cloud speech processing, or other services may already have received the content. Secrets marked to remain inside the home must be captured and revealed in the local authenticated interface; external bots can direct the user there.

## 16 Operational Edge Cases and Defaults

### People and communication

Unknown senders. Require pairing or an explicit allowlist. Use stable platform identifiers and authenticated account linking. A discovered bot username, claimed name, or group membership alone does not grant private or operator access.

Children and guests. Apply the requesting person's authority even when the destination Butler has operator capabilities. For ambiguous speakers, permit only configured basic household actions; private information and sensitive actions require authenticated confirmation. Voice recognition alone does not establish authority.

Group audiences. Only retrieve information appropriate to all intended recipients. When membership cannot be verified, use explicitly group-shared information only. Check the destination again before sending if audience or membership has changed.

Delivery failures. Stop delivery through removed or misconfigured connections. Never silently switch to another Butler's bot identity or another recipient. Preserve pending delivery state and show the failure without disclosing private payloads.

### Routines and recovery

Uncertain effects and retries. Use deduplication keys where supported. After a timeout, inspect the external outcome before retrying. If confirmation is unavailable, record an uncertain result and avoid blindly repeating a consequential effect.

Restart and partial completion. Persist completed steps and pending work. Resume only unfinished work. Cancellation stops further actions where possible but cannot undo completed effects. Distinguish partial success from full completion.

Missed schedules. Set a catch-up policy per routine. Collapse missed briefings into one current briefing; skip stale physical actions by default. Store timezone explicitly, account for daylight-saving changes without accidental duplicate execution, and state whether travel changes the schedule.

Revoked authority. Recheck current permissions before queued and scheduled actions. Pause affected routines when their owner or connection loses access. Never substitute another person's credentials. Invalidate cached private context and outstanding approvals.

Conflicting automation. Coordinate writes to the same resource and detect recurring conflicts between agents, handlers, and application rules. Pause and surface conflicting routines rather than allowing repeated opposing commands.

Network and self-maintenance. Unattended Wi-Fi or network changes require a tested local recovery path because they may remove the agent's communication channel. Changes to the agent runtime or its host belong in controlled maintenance with recovery independent of the running agent.

Unavailable dependencies. Preserve unfinished work and report the failed model or integration. Continue unrelated deterministic routines. Apply finite retry, duration, step, and spending limits; exact values must be explicit configuration rather than unlimited defaults.

### Delegation and learning

Delegation. Carry the initiating person, task scope, allowed data, and bounded authority to each explicitly enabled delegate. The delegate may further restrict that authority but cannot widen it, bypass a denial, or combine grants to gain new privileges. Shared knowledge retrieval does not require delegation.

Loops. Track event origin and task correlation. Bound delegation depth, event recursion, tool steps, runtime, and cost. Stop recurring agent-to-agent or routine-to-routine loops and show a recoverable failure.

Learned procedures. Agents may save reusable procedures composed of already authorized tools. Version them and test changes before replacing an active routine. Executable code, new plugins, credentials, wider grants, or platform modifications require administrative review.

### Agent lifecycle and incident controls

Clone. Copy appropriate configuration without silently duplicating bot credentials, private transcripts, or running routines. A clone gets a new agent identity and explicit grants into shared memory.

Pause and delete. Pause stops new runs and notifications; expose separate cancellation for active work. Deleting an agent preserves person- and household-owned memory and pauses its routines pending reassignment. Hiding an agent in the UI must not be confused with pausing it.

Incident response. Provide a household-wide stop control and targeted connection revocation. Record actors, targets, decisions, and action receipts without raw secrets. Recovery and backup procedures must preserve revocations, deletion policies, routine state, and key access controls.

## 17 Repository Audit Acceptance Criteria

Use these scenarios as behavioral gates. An architecture description, UI screenshot, or passing unrelated CI job is not proof. For each scenario record the commit, setup, expected and observed result, relevant code, and test or runtime evidence. Use synthetic secrets and test accounts; do not publish household credentials in evidence.

### AC01 Shared memory across agents

Giulia saves a gift idea through Butler A to an adults-only audience. Butler B retrieves it for Mika. A request from Jude and a household group including Jude do not expose the entry. Record both successful retrieval and denied disclosure.

### AC02 Routines on both profiles

A household Butler and an operator Butler each persist an email briefing using authorized reads and a specific delivery destination. Verify routine ID, next run, actual execution, and that neither acquires ungranted send or delete privileges.

### AC03 Person authority follows the request

A paired child messages an operator bot and requests an infrastructure change. The runtime denies it despite the bot's toolkit. An authorized operator can perform the separately granted operation.

### AC04 Cloud assisted secret recall

Store a synthetic Wi-Fi secret locally, retrieve it through another authorized Butler, and inspect model-bound payloads for substitutes only. Reveal it through the approved local component. Reject expired references and attempts to inject it into a URL, tool destination, or group message.

### AC05 Crash after a completed action

Simulate a crash after an application accepts an action but before local completion is recorded. Recovery checks or deduplicates the effect and does not blindly repeat it; unsupported verification produces an uncertain state.

### AC06 Revocation and cached context

Queue work and retrieve private context, then revoke the person or connection grant. Subsequent execution and disclosure fail, including cached prompts, active task continuation, and scheduled runs.

### AC07 Untrusted email instructions

Supply an email that attempts to change permissions, poison authoritative memory, or export a secret. It remains attributed source content and cannot authorize those effects.

### AC08 Independent bot routing

Create two Butlers with different bot accounts. Direct replies, routine notifications, errors, and reconnect behavior use the correct identity. Switching Butlers preserves permitted shared knowledge without merging private conversations.

### AC09 Group and voice boundaries

Test an unverifiable group audience and an unrecognized speaker. Only explicitly shared or basic configured capabilities are available; private retrieval requires authenticated routing.

### AC10 Approval integrity

Approve a concrete consequential action, then change its target or parameters or let approval expire. Execution requires new authorization. A valid narrowly scoped standing grant avoids redundant confirmation.

### AC11 Memory correction and forgetting

Make concurrent conflicting memory writes, correct a fact, and forget a source-derived entry. Verify version handling, source attribution, index and cache invalidation, and suppression of re-ingestion. Document backup retention behavior.

### AC12 Local only processing

Disable the local model and attempt cloud fallback, embedding, transcription, attachment processing, and an external tool call with local-only content. Verify policy enforcement and stopping when inspection is unavailable.

### AC13 Scheduling and conflict handling

Exercise a missed briefing, stale physical action, daylight-saving boundary, and two routines targeting the same resource. Verify the configured catch-up policy and conflict handling without duplicate effects.

### AC14 Delegation and execution budgets

Delegate a bounded task and attempt privilege expansion or recursive handoffs. Verify the initiating identity is preserved, denials remain effective, and configured limits stop the loop.

### AC15 Lifecycle and partial work

Pause, cancel, clone, and delete an agent with active and scheduled work. Verify accurate partial status, no duplicated bot credentials or routines, and preservation of shared household memory.

### AC16 Package integrity and learned skills

An agent learns a useful procedure using granted tools, while an attempted service-package rewrite or executable plugin install is routed to administrative review. Verify service-operation permission alone cannot modify the package.

Audit output should map each criterion and relevant requirement to implemented, partial, missing, or unverified status; explain practical impact; rank remediation; and identify any design decision required. Inspect repository instructions before changing code. Deployment-only checks that cannot run must remain explicitly unverified.

## 18 Reference Products and Design Rationale

Official documentation reviewed September 10, 2026 informed the comparison below. Product behavior may evolve. Wildstone's accepted requirements take precedence over a reference product's defaults; these references are not evidence of Wildstone implementation.

Hermes Agent. Its named bots, persistent chats, tool selection, and routines support the desired assistant experience. Its built-in memory is profile-scoped and its documentation recommends an external provider for shared memory. Wildstone adopts shared household retrieval with audiences rather than a shared agent working directory. Its egress proxy also illustrates opaque credential references for outbound tool use, a narrower capability than inspecting every household data path.

[Hermes Bot Mode](https://hermes-agent.nousresearch.com/docs/user-guide/bot-mode)

[Hermes Persistent Memory](https://hermes-agent.nousresearch.com/docs/user-guide/features/memory)

[Hermes Security](https://hermes-agent.nousresearch.com/docs/user-guide/security)

[Hermes Egress Proxy](https://hermes-agent.nousresearch.com/docs/user-guide/egress/)

OpenClaw. Its multi-agent routing supports separate bot accounts. Its documented trust model assumes one operator or mutually trusting team, with delegated tool authority shared by people messaging a tool-enabled agent. Wildstone requires person-specific enforcement for adults, children, guests, and operators, even when they reach the same Butler.

[OpenClaw Multi Agent Routing](https://docs.openclaw.ai/concepts/multi-agent)

[OpenClaw Security Trust Model](https://docs.openclaw.ai/gateway/security/trust-model)

Grok Bot. Persistent named agents and end-to-end work provide a useful experience reference. Its documented account-scoped computer shares files, browser sessions, and app logins across bots. Wildstone shares knowledge through audience controls and grants service connections individually.

[Grok Bot Overview](https://docs.x.ai/grok-bot/overview)

Security and retrieval references. OWASP discusses sensitive information disclosure and excessive agency, including the limits of prompt-only safeguards. Qdrant documents metadata filtering for search; the application must enforce the appropriate filters. Neither a detector nor a vector database supplies the full household authorization policy.

[OWASP Sensitive Information Disclosure](https://genai.owasp.org/llmrisk/llm022025-sensitive-information-disclosure/)

[OWASP Excessive Agency](https://genai.owasp.org/llmrisk/llm062025-excessive-agency/)

[Qdrant Filtering](https://qdrant.tech/documentation/search/filtering/)

