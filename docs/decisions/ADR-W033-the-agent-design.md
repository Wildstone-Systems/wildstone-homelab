# ADR-W033: The agent, designed

- **Status:** in progress — A1 and A2 accepted, Parts B, C and D open.
- **Relates to:** [the agent architecture baseline](../agent-architecture-baseline.md)
  (the accepted statement of intent, which this designs); ADR-W026 A3
  (one bot per surface — A2 amends it); ADR-W030 A3/A3.1 (owned memory
  and acceptance boundaries), A4 (durable work and budgets), A6 (draft
  then activate — A1 amends it), A7/A8 (the authority resolver);
  ADR-W031 A4 (per-Butler memory ownership), A7 (the bot token is the
  address); ADR-W012 A1/A3 (the node secret store).
- **Audience:** decision record. It is being written area by area, and
  an area that has not been worked through says so rather than
  guessing.

---

# Part 0 — Why this exists rather than a remediation plan

The baseline says what the agent system is for. It does not say how it
is built, and the repository was built against ADRs that predate it. An
audit of the six subsystems against the baseline
([#151](https://github.com/chickenrat17/wildstone-homelab/issues/151),
[`docs/audit/`](../audit/)) produced 118 findings: 62 missing, 33
partial, 11 implemented, 4 unverified since resolved.

Most of those findings turned out not to be defects with a fix. They
were consequences of design questions nobody had answered, and the same
question showed up in four or five findings at once. Scheduling the
findings would have meant answering those questions by accident, one
implementation at a time, in whatever order the audit happened to
surface them.

So this document designs the target, and the audit becomes the map of
what already matches. That is not the same as designing as though the
repository were empty. Several properties in it are hard to get and
were verified at runtime, and this design adopts them deliberately
rather than rediscovering them:

- permission filtering inside the **candidate** query, so a row the
  reader may not see never exists on their behalf even briefly
- completion refused without evidence, at three independent layers
  including a schema trigger
- no silent fallback to another model — named in the code, pinned by a
  test, observed refusing with the local route down
- the untrusted envelope: an injected household event was reported as
  an injection attempt rather than obeyed, on the live path
- memory confers no authority, observed unchanged across sixteen turns
- the coordinator lease actually gating durable work

What is decided here is binding. What is open is marked open, with the
issue where it is being worked through.

---

# Part A — What a Butler is, and who can reach it

Worked through in
[#178](https://github.com/chickenrat17/wildstone-homelab/issues/178).

### A1. A human approves a routine's creation, not its runs — *accepted 2026-09-11*

A person approves the **creation** of a routine. After that the routine
acts like any other job: it carries out the task it was given, on its
schedule or its trigger, without a human approving each run.

This settles the contradiction the audit found between ADR-W030 A6
("created as a draft and activated separately") and baseline §8
("persist and activate without redundant confirmation"). **ADR-W030 A6
is amended** for the run path.

It leaves one question open, and it is a narrower one: whether a Butler
may create *and activate* a routine without a human signature at all,
or only propose one for a person to approve. Baseline §10's narrow
standing grant is the shape that would allow it for routines of a named
kind — read-only, or notification-only, or one recipient. Open in
Part D.

*Findings settled or reframed by this:* F-154-11, and the framing of
F-154-01, F-154-03, F-154-17, F-154-20.

### A2. A bot token belongs to a Butler, and is never shared — *accepted 2026-09-11*

A **Butler** is the agent. A **bot token** is one channel it can be
reached through. A Butler may hold several — Sandy may be reachable on
Telegram *and* Discord — and a token belongs to exactly one Butler.
Alfred cannot be reached through Sandy's token; Alfred needs his own.

This rules out sub-addressing behind a shared bot (Telegram forum
topics, which ADR-W031 A7 left open as a deployment choice): it
multiplexes Butlers behind one token, which is the thing being ruled
out. **ADR-W026 A3 is amended** — a surface may carry more than one bot.
Its surface ceiling, that a household bot cannot reach the admin
surface, is independently right and survives.

The routing half already holds. `butler_addresses` supports many
addresses per Butler and refuses two Butlers one address (F-153-12), and
the address is recovered from the adapter's own credential rather than
from a request body (F-153-11, `implemented`). What does not hold is
that an address must name a *capability*, and capabilities are installed
one per surface — so a second Butler's Telegram address has nothing
behind it (F-153-01, F-153-02). **The work is in capability
installation and credential issuance, not in routing.**

*Findings settled or reframed by this:* F-153-01, F-153-02, F-153-03,
F-153-04, F-153-05, F-153-09, F-153-15.

### Open in Part A

- What a Butler owns as configuration, and what the Console shows per
  Butler rather than per deployment (F-153-15, F-155-19).
- **Person-authority-follows-the-request.** Required under any trust
  model: the §10 intersection has no person in it today (F-155-01,
  F-155-03), and AC03 was observed failing — a household member and an
  operator made the same request and both got a plan.
- How much of the adult-versus-adult defence to keep. The owner's
  position is that Console and chat access both mean admin and that
  children are the narrower class; several mechanisms defend instead
  against mutually-untrusting adults. Some of that is what makes a
  child scoping implementable later, so it is a decision rather than a
  deletion.

---

# Part B — What is remembered, and who can see it

**Open.** Worked through in
[#179](https://github.com/chickenrat17/wildstone-homelab/issues/179).

Covers: audiences (the schema has two values where the baseline needs
four), sharing across Butlers (**ADR-W031 A4 is superseded** and must be
amended — the amendment inverts the default rather than deleting the
isolation), whether capture is dictation or extraction (which decides
whether migration 0038 is finished or reverted), the single-household
assumption, and whether the memory preamble should exist in a group
conversation at all — narrowing `recall` does not close AC09, because
the preamble reaches the system prompt before the model reads anything.

---

# Part C — Secrets, and what leaves the house

**Open.** Worked through in
[#180](https://github.com/chickenrat17/wildstone-homelab/issues/180).

Covers: whether a household secret store is a second store or a second
class in the existing one (**ADR-W012 A1/A3 are implicated either way**),
opaque request-bound references, use-without-reveal, the protected
display path, and the privacy gateway — today a shape redactor at
twenty call sites stands in for it, and all five model call sites send
uninspected.

The `remember` credential refusal comes out here as a decision with a
replacement designed, not as a defect someone deletes.

---

# Part D — How work happens

**Open.** Worked through in
[#181](https://github.com/chickenrat17/wildstone-homelab/issues/181).

Covers: whether a task outlives a turn (which decides the budget reset,
the `uncertain` outcome and "partially complete" at once), the
remaining half of A1 — whether a Butler may activate a routine under a
narrow standing grant — delegation between Butlers, and agent
lifecycle: pause, clone, delete.

---

# Part E — What this changes in the older ADRs

Filled in as the parts land. Amendments already identified by the audit
and by A1/A2 above:

| ADR | Clause | Change |
|---|---|---|
| ADR-W030 | A6 | Draft-then-activate is amended for the run path (A1). |
| ADR-W030 | A4 | "A budget that resets on retry is not a budget" claims a property nothing holds: the live path opens a task per turn, so it resets per turn (F-158-06). Amend, or make it true. Part D. |
| ADR-W030 | A7/A8 | The authority resolver's query has no person and no connection; baseline §10 names four legs and the resolver was designed for two (F-155-01, F-155-02). Part A. |
| ADR-W031 | A4 | Per-Butler memory ownership is superseded by baseline §6/§7. Invert the default, keep the isolation as opt-in. Part B. |
| ADR-W031 | A7 | Sub-addressing is ruled out by A2. The binding half — the bot token is the address — is unaffected and holds. |
| ADR-W026 | A3 | One bot per surface is amended by A2; the surface ceiling survives. |
| ADR-W024 | A6 | Escalation may phrase one parameter and never decide; §8 admits an agent objective as routine work. Part D. |
| ADR-W024 | H7 | "There is no second execution path" is true of the engine and false of the repository. Part D. |
| ADR-W012 | A1/A3 | The node store cannot hold a household secret no node uses, and A3's "deliberately no GetSecret" conflicts with protected display. Part C. |
| ADR-W032 | A3 | "There is no narrower thing to be on this surface yet" is contradicted by baseline §16/§18. Part A. |
