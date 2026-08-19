# ADR-W008: Reconcile service — plan execution, drift cadence, observed status

- **Status:** accepted
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

### A5. Events

The service emits: `reconcile.applied` (something changed and converged),
`reconcile.failed` (incident/rollback, with detail), `drift.repaired`
(low-risk drift auto-fixed), `drift.pending-approval`, and
`reconcile.skipped-offline`. Quiet ticks (empty diff, nothing pending)
emit nothing — the event stream is for changes, not heartbeats.

# Part B — Work packages

- **R1** — the loop package (this ADR's A1–A5), `PlansInState` accessor,
  cmd/keystone wiring (`-reconcile-interval`, default 30s), tests over a
  fake Applier.
- **R2+** (later): richer per-resource observe (close the S2b
  presence-vs-spec gap), event-driven ticks (plan approval pokes the
  loop instead of waiting for the interval), per-step reverse coverage
  audit, backoff/circuit breaker on repeated failures (ADR-W004 A4's
  bounded remediation at loop scope).
