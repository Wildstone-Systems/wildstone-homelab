# Butler capability and design brief

This is non-normative background for [ADR-W030](decisions/ADR-W030-butler-continuity-durable-work-and-scoped-integrations.md). The ADR is the single proposed architectural contract. This brief explains the owner's intent, examples, and delivery priorities; it does not introduce additional permissions or requirements.

## Why this direction

The owner wants Butler to feel like one assistant wearing different tool belts. Household and Operator already have separate Telegram bots. Shared context should reduce repeated explanations while an explicit handoff keeps administrative work in the Operator surface. A future Child profile would use a smaller tool belt and its own admission and guardian controls.

The aspiration is the continuity and practical task handling associated with OpenClaw-, Hermes-, or Pi-like agents, integrated with Wildstone's existing foundations. It is not a decision to adopt any of those runtimes. Wildstone already owns Plans, policy, packages, deterministic household automation, and the event timeline. Butler adds dialogue, memory, bounded agent work, and authoring assistance.

See ADR sections A1–A2 for the identity, shared-history, and audience contracts, and A7–A8 for tools and approvals.

## What memory should feel like

If a household member says they dislike cilantro, Butler can remember that preference and offer it in an appropriate future meal discussion. A group message still describes that person's preference, not everyone's. An inferred preference should remain tentative and easy to correct.

RAG helps locate relevant saved material; structured records preserve exact facts, provenance, and access rules. A current calendar query answers what is scheduled now. These serve different purposes and do not require copying every application database.

The owner's preference is a daily or weekly learning digest, with weekly as an initial deployment choice. The digest makes learned information inspectable without requiring approval for every harmless preference. Sensitive or private sources retain their restrictions. See A3.

## Example: leaving home without breakfast logged

Home Assistant reports that a person left home. A user-authored Wildstone routine asks PantryOS whether breakfast was logged within the routine's configured window. A matching record can suppress the reminder. Missing records are not proof that the person did not eat; the routine can say “I don't see breakfast logged.” An unavailable PantryOS connection is a different outcome.

This illustrates why a shared integration contract matters: events, live queries, and typed actions let apps cooperate. The breakfast window, message wording, and missing-data response belong to the routine, not built-in product logic.

PantryOS is still in development. Its forthcoming API can be represented by a fixture while the adapter contract is tested. See A5–A6.

## Example: an upcoming appointment

A routine combines a CalDAV calendar entry with Home Assistant presence. The household member configures which appointments matter, when to remind, and how uncertain travel information is handled. There is no universal built-in departure calculation.

Read-only calendar access is the first integration step. Email monitoring and review-first appointment creation are an early follow-up because an assistant that identifies an appointment but cannot help put it on a calendar leaves useful work unfinished. Invites and RSVPs are separate actions.

The current mail/calendar client package does not itself give Butler mailbox credentials or calendar API access. Personal credential onboarding is a prerequisite, as described in A5.1.

## Example: a missing tool

A task needs a scoped search capability that is unavailable. Butler records what it needs and why, tied to the task. An administrator can review an existing-tool request or a development request. A read operation still needs a boundary: searching a connected recipe collection differs from searching arbitrary host files.

Temporary task-bound access is the owner's preferred default. Household's structural ceiling continues to exclude dangerous administrative tools. Tool requests are a way to improve capability without letting Butler grant itself power. See A7.

## Example: an interrupted task

Butler saves progress before and after each tool operation. If an application accepts a change but the response is lost, restarting should prompt verification, not an automatic repeat. The assistant can report partial completion and explain what remains uncertain.

A finished task and a delivered notification are different facts. If quiet hours or an outage prevent a message arriving, the work should not become unfinished again. See A4 and A10.

## Deployment preferences and tradeoffs

The owner's preferred quiet hours are 9 p.m.–7 a.m. in America/Chicago, following local daylight-saving changes. This is a configurable household/person preference, not a hardcoded timezone for every deployment. Messages outside a digest are welcome when relevant; most unassigned operational notices go to the configured homelab administrator without disclosing private content.

Both adult profiles may need local and cloud models. Local processing is attractive for inexpensive background ingestion and compaction; permitted cloud models can handle work that benefits from them. A remote Ollama endpoint may itself serve local or cloud models. Its responses do not establish how much account allowance remains, so cost and allowance enforcement are deferred. See A9.

Shared adult household access avoids a complex adult role hierarchy. It does not erase private-source restrictions or the stronger tool boundary around Operator.

Trusted adults can correct shared household memory and change or disable shared routines without asking the original author. Butler keeps the author/editor history, tells the owner when another adult changes routine behavior, and includes another adult's shared-memory correction or forget action in the next learning digest. Removal of safety information such as an allergy is auditable and surfaced immediately; the record keeps visible source and scope. A person and the configured homelab administrator control that person's private memory.

Butler may draft and preview a shared routine in Telegram, then provide a direct link to an Authelia-authenticated household review page. Any admitted adult can approve that narrowly classified household-routine Plan there without receiving an admin Console credential or gaining access to infrastructure Plans. Telegram cannot approve activation or edits, but an authenticated adult can immediately disable an exact routine because that only removes future authority.

Personal service connections start personal, read-only, live-query-only, and local-processing-only. Explicit household-service connections start household-shared and read-only. Indexing, cloud use, and actions are clear setup choices rather than side effects of connecting an account.

For homes spread across several machines, a connection chooses a primary adapter node and approved fallbacks. Wildstone can fail over automatically only within that list. Credentials are placed on those nodes in advance and nowhere else; if all are unavailable, Butler waits and reports the outage. Removing a fallback is complete only after its local credential is erased and any node-specific remote credential is revoked.

When a member leaves, access, bindings, grants, connections, ingestion, and messages stop immediately. Personal unfinished work pauses and private history becomes inaccessible until its retention period or an authorized deletion; shared facts and routines remain, keep the original author, and transfer operational ownership to the homelab administrator. A person can export their own Butler data while still admitted. Any shared connection using that person's credential stays paused until a current owner reauthorizes it.

V1 uses one active Keystone/Butler coordinator. Cross-application work waits if it is unavailable, while routines hosted inside Home Assistant or another app follow that app's availability. A standby is for explicit recovery, not a second active scheduler.

Butler's work should be inspectable. Each task keeps a permission-filtered timeline of context sources and freshness, steps, tool attempts and redacted results, retries, waits, approvals, messages, and completion evidence, together with the prompt/model/tool/policy versions used. People can label problems and turn a reviewed, sanitized failure into a regression test. This captures observable behavior rather than private chain-of-thought, and Butler cannot change its own prompts, tools, policies, or tests.

## Delivery sequence

1. Fix current-agent reliability on the existing surfaces: effective/self-capability inventory, configurable iteration slices, durable single-conversation progress, automatic bounded retry, stalled/resumable state, a task-bound continue flow, and a structured Console work trace. Use current executors plus the self-capability read and bounded Operator log read defined by P2; add no new application write authority. Prove an eight-plus-read diagnosis and provider-failure resume without repeated verified work, and turn one sanitized failure into a version-pinned regression evaluation.
2. Add explicit identity linkage, permission-aware shared history, and the cross-bot handoff while preserving the first slice's attribution.
3. Add selected-source memory and a cross-app path using Home Assistant and read-only CalDAV, with credential onboarding. Exercise PantryOS against fixtures until its API is ready.
4. Add selected IMAP monitoring and review-first calendar writes, plus a dedicated receipt/screenshot/document intake pipeline shortly after v1. Evaluate extraction quality before relying on model image interpretation.
5. Add voice later. Local transcription is preferred; Kokoro is the owner's initial text-to-speech preference. Voice uses the same identities, conversations, and tool boundaries.
6. Consider the Child surface only after its separate transport, admission, guardian, and boundary checks are designed and verified.

ADR-W030 Part C turns this sequence into ordered implementation-sized work packages. Work starts only after acceptance; each package carries its own denial, revocation, audit, recovery, and acceptance evidence rather than treating the first successful demo as v1 completion.

These are sequencing preferences, not fixed delivery estimates. Automatic tool development, unrestricted execution, autonomous agent teams, and provider-account cost enforcement are deferred.

## Reading the proposal

[ADR-W030 Part B](decisions/ADR-W030-butler-continuity-durable-work-and-scoped-integrations.md#part-b--compatibility-review-questions-and-consequences) identifies existing contracts that must change together before acceptance. [The documentation index](README.md) links the current architecture and operating guides.

The draft remains open for debate. The main implementation work is reliable task recovery, permission-aware context, adapter/credential contracts, and safe restoration of durable runtime state.
