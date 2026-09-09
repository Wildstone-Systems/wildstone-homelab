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

Trusted adults can correct shared household memory and change or disable shared routines without asking the original author. Butler keeps the author/editor history and tells the owner when another adult changes routine behavior. A person and the configured homelab administrator control that person's private memory. Safety information such as an allergy keeps visible source/scope and an auditable removal.

Personal service connections start personal, read-only, live-query-only, and local-processing-only. Explicit household-service connections start household-shared and read-only. Indexing, cloud use, and actions are clear setup choices rather than side effects of connecting an account.

## Delivery sequence

1. Fix current-agent reliability on the existing surfaces: effective/self-capability inventory, configurable iteration slices, durable single-conversation progress, automatic bounded retry, stalled/resumable state, and a task-bound continue flow. Use current tools only and prove an eight-plus-read diagnosis and provider-failure resume without repeated verified work.
2. Add explicit identity linkage, permission-aware shared history, and the cross-bot handoff while preserving the first slice's attribution.
3. Add selected-source memory and a cross-app path using Home Assistant and read-only CalDAV, with credential onboarding. Exercise PantryOS against fixtures until its API is ready.
4. Add selected IMAP monitoring and review-first calendar writes, plus a dedicated receipt/screenshot/document intake pipeline shortly after v1. Evaluate extraction quality before relying on model image interpretation.
5. Add voice later. Local transcription is preferred; Kokoro is the owner's initial text-to-speech preference. Voice uses the same identities, conversations, and tool boundaries.
6. Consider the Child surface only after its separate transport, admission, guardian, and boundary checks are designed and verified.

After ADR acceptance, revise this sequence into implementation-sized work packages mapped to its sections and B1 amendments: identity/admission, task ledger, adapter queue/roles, credential onboarding, approval transport, recovery quarantine, and related boundaries. Each package should carry its applicable Part C acceptance evidence. That decomposition is follow-up work, not an implementation expansion in this PR.

These are sequencing preferences, not fixed delivery estimates. Automatic tool development, unrestricted execution, autonomous agent teams, and provider-account cost enforcement are deferred.

## Reading the proposal

[ADR-W030 Part B](decisions/ADR-W030-butler-continuity-durable-work-and-scoped-integrations.md#part-b--compatibility-review-questions-and-consequences) identifies existing contracts that must change together before acceptance. [The documentation index](README.md) links the current architecture and operating guides.

The draft remains open for debate. The main implementation work is reliable task recovery, permission-aware context, adapter/credential contracts, and safe restoration of durable runtime state.
