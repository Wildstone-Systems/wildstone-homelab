# ADR-W008: Reconcile service — plan execution, drift cadence, observed status

- **Status:** accepted; A1/A2 amended (accepted revision, bounded approval, per-Plan outcomes, #157)
- **Relates to:** ADR-W004 (engine semantics, unchanged), ADR-W007 (plan
  records, runtime DB, events), implementation scope E3
- **Audience:** decision record AND implementation spec for the E3
  service layer. The S2 engine (`keystone/reconcile`) stays the authority
  on how one pass works; this ADR decides how passes are driven.

---

# Part A — Decisions (binding)

### A1. Level-triggered loop over plan replay

The service (`keystone/reconcile/loop`) is level-triggered, per ADR-W004:
each tick loads the CURRENT desired state from the Statefile and lets the
engine diff it against observation. Plan records are consumed as
**intents, not scripts**: approval already committed the change to the
Statefile (ADR-W007 K3), and out-of-band acceptance did the same (A6), so
executing a plan means "converge to the Statefile now" — never replaying
stored steps against a world that may have moved. One tick can therefore
resolve several approved plans at once: they all describe the same target
tree.

### A2. Plan record lifecycle at the service

On each tick, every plan in `approved` moves to `applying`, one engine
pass runs per node, then all of them move together to `applied` (every
step applied + health-gated, or empty diff) or `failed` (incident,
rollback, or a disconnected node that the plan's resources need). The
failure detail goes in the event stream; the Statefile is NOT reverted on
failure — desired state remains desired (level-triggered: the next tick
retries), and abandoning a change is itself a new plan. A tick with no
approved plans is a **drift tick**: the same engine pass, but Medium/High
diffs are not auto-applied (the engine's default-deny approve) — the
drift is surfaced as an event (`drift.pending-approval`) and a proposed
plan record so a human can approve it through the normal API; Low-risk
drift repairs apply immediately (ADR-W004 A4 drift classes).
The service records at most ONE open drift proposal at a time (a
proposed plan with principal `system/drift`): re-proposing the same
drift every tick would flood the plan list.

### A2a. Amendment (2026-10-24, #157)

Amends A1 and A2, per ADR-W007 A8 and the
[publication and bounded-approval contract](../design/desired-state-publication-and-approval-contract.md):

- **A1:** "the CURRENT desired state" is the **accepted revision**, read
  as one immutable snapshot per tick; the tick reports which revision it
  used (`TickOutcome.Revision`). A repository with history but no
  accepted revision pauses the loop rather than converging to an empty
  tree. *(Implemented in 0a.)*
- **A2:** an approved Plan no longer authorizes every Medium/High step in
  the tick. A Medium/High step runs only when it matches an entry of a
  live approval envelope (same resource, action and desired digest, risk
  no higher than reviewed); anything else becomes a fresh pending
  preview. Low-risk drift repair keeps its own authority and is reported
  as drift repair. Each queued Plan gets its own outcome instead of one
  shared verdict. *(Pending: 0c.)*

### A3. Node grouping and connectivity

Desired resources group by `metadata.node`; the engine runs once per
group ("" holds the non-node-scoped kinds: DnsRecord via provider,
LdapGroup). A group whose node has no live agent session: with approved
plans pending → that's a plan failure (the operator approved a change
that cannot land; say so loudly); on a drift tick → skip quietly (agents
dial out and reconnect on their own schedule, ADR-W003; a homelab node
being briefly offline is not an incident).

### A4. Observed status

After each pass the service upserts `runtimedb.observed` per desired
resource: healthy unless the resource was implicated in a failed/reversed
step or the pass's incident, with the step error as detail. Coarse at R1
(the engine omits NoOp resources from step results, so an untouched
resource inherits "healthy"); refinement belongs to the engine's observe
surface (ADR-W004's known S2b limitation), not to this loop.

Two rules the alpha established, both about not lying to an operator who
is trying to work out whether their house is broken:

**A resource waiting for a human is not a resource that is broken.** When
a pass stops at pending approval, its resources record
`awaiting_approval` and say so; they are not reported absent. A restart
that manufactured a drift proposal once showed 27 of 45 resources red
while every container was up, and the reading fooled the person who wrote
it (F75).

**A status row outlives its resource only while the resource does.** Rows
are cleared when a delete applies; rows for resources nothing desires and
no plan is acting on are pruned, because a create that never succeeded
leaves nothing to delete and its row would sit red forever describing a
service that is gone (F92). Rows on a node the tick did not visit are
never pruned: they are the durable record the managed set is rebuilt from
at startup (F76), and losing them for an offline node strands whatever
they described.

### A5. Quarantining a step that cannot succeed

A plan applies in full or not at all (ADR-W004 A3), so a failed step
halts the pass and the steps behind it are never attempted. That is
right, and on its own it has no exit: the failed resource is still
desired, so the next tick rebuilds the same plan and halts the same way,
forever, while every other resource waits behind it. One forbidden mount
took out the control plane's ability to apply anything at all — including
the proposal that would have repaired it (F60).

So a step that fails **identically three times in a row** is set aside.
The pass still halts when it fails; the next plan is built without it.
The rest of the tree converges, and the quarantined resource reports what
it is stuck on.

The boundaries matter more than the mechanism:

- **Three, not one.** A slow registry or a node that has just reconnected
  must not earn a sentence.
- **Identical.** A different error, or a changed spec, starts the count
  again — this is a new situation, not the same failure repeating.
- **The action, not the resource.** A create that cannot succeed must
  never block a DELETE of the same resource. Removing it from desired
  state is how an operator gets a poisoned resource out, and it was the
  only way out of F60; quarantining that too would quarantine the escape
  hatch along with the fault.
- **Two ways out, both ordinary.** Fix the resource (the spec changes, so
  it is retried) or remove it (the record is forgotten). Neither requires
  knowing this mechanism exists.

A quarantined resource is NOT healthy and does not silently disappear:
it records why it was set aside, and `reconcile.blocked` is emitted.

### A6. Events

The service emits: `reconcile.applied` (something changed and converged),
`reconcile.failed` (incident/rollback, with detail), `reconcile.blocked`
(a resource set aside under A5, with the error that earned it),
`drift.repaired`
(low-risk drift auto-fixed), `drift.pending-approval`, and
`reconcile.skipped-offline`. Quiet ticks (empty diff, nothing pending)
emit nothing — the event stream is for changes, not heartbeats.

# Part B — Work packages

- **R1** (implemented) — the loop package (this ADR's A1–A4 and A6),
  `PlansInState` accessor, cmd/keystone wiring (`-reconcile-interval`,
  default 30s), tests over a fake Applier.
- **R2** (implemented):
  - **R2a** — event-driven ticks: `Loop.Poke` on plan approval
    (`api.Server.OnApproved`) and on accepted out-of-band edits; a poke
    bypasses backoff (fresh human intent).
  - **R2b** — failure backoff: consecutive failed ticks stretch the
    interval `base<<streak` capped at 32×, reset by any clean tick.
  - **R2c** — spec-aware container observe (protocol minor 5): the
    `dev.wildstone.spec-hash` label stamped at EnsureContainer, read
    back by the new `InspectContainer` op; the agent replaces on hash
    mismatch; a pre-minor-5 agent falls back to presence-only observe.
  - **R2d** — the reverse-coverage matrix pinned by test
    (`reconcile/reversecoverage_test.go`): Container Create reverses
    with Stop+Remove, Volume gate failure dispatches nothing beyond
    EnsureVolume, ever.
- **R4** (implemented) — A5's quarantine (`reconcile/blocked.go`), the
  `reconcile.blocked` event, `awaiting_approval` on observed rows, and
  the status-row prune. Tests hold the boundaries that matter: one
  failure does not quarantine, a quarantined create does not block the
  delete of the same resource, a fixed spec is retried, and an offline
  node keeps its rows.
- **R3+** (later): reverse coverage for the remaining kinds where one is
  definable, spec-aware observe for Volume (needs an inspect surface),
  per-node backoff instead of loop-global.
