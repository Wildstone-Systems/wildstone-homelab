# ADR-W009: Policy Engine — standing invariants as plan validation

- **Status:** accepted
- **Relates to:** architecture proposal §9.2, implementation scope E4,
  ADR-W006 A3 (the S4 spike rule this generalizes), ADR-W007 A6/K3
- **Audience:** decision record AND implementation spec for E4.

---

# Part A — Decisions (binding)

### A1. One engine, every write path

`keystone/policy` validates a proposed TARGET TREE (the full desired
state a change would produce) against the current tree, for a given
principal. The same engine runs on every path a change can enter by:

- **API plan proposal** (ADR-W007 K3): violations reject the proposal
  with 422 before a plan record is even created — a policy-invalid
  change never reaches the approval queue, whoever proposed it.
- **Out-of-band ingestion** (ADR-W007 A6): a hand-committed tree that
  fails policy is reverted exactly like a schema-invalid one — repo
  access does not outrank the standing invariants.
- **The Butler** (ADR-W006 A3): its spike-era single-rule
  `policyValidate` becomes a thin wrapper over this engine, preserving
  the S4 red-team property (a fully-obeyed injection still cannot
  produce a policy-invalid proposal).

Policy validates the tree, not the plan steps: the plan is derived from
the tree diff, so a valid target tree implies valid steps, and
tree-level rules can't be dodged by step ordering. Violations attach
only to resources the change TOUCHES (added/modified/deleted vs.
current): a pre-existing violation on an untouched resource must not
wedge every unrelated change — legacy violations are drift to surface,
not a reason to block a new volume. The laundering guards are
unaffected: they compare current vs. proposed of the same name, which is
by definition touched.

### A2. The rules (proposal §9.2 invariants, MVP mechanization)

Each rule has a stable ID (used by exemptions and violations):

| ID | Invariant | MVP mechanization |
|---|---|---|
| `classification-vocabulary` | §9.2-4 (with resource.Validate's owner/non-empty checks) | `Route.classification` must be one of the closed set adopted unchanged from the reference deployment: `family_public`, `family_private`, `admin_tailscale`, `internal_only`, `deprecated` |
| `public-requires-auth` | §9.2-2 | A route that is `family_public` in the target tree — or is currently `family_public` under the same name (the S4 downgrade-laundering guard, kept) — must not have `auth: none` |
| `admin-structural` | §9.2-1 | A route currently classified `admin_tailscale` may not be re-classified to `family_public` or `family_private`; admin surfaces never migrate onto family/public ingress by any single change |
| `secret-values-forbidden` | §9.2-3 (Statefile half) | Container env entries whose KEY is secret-shaped (password/secret/token/api-key) must carry a `secret/<name>` reference, not a literal; any env VALUE with a secret shape (long hex/base64, shapes shared with keystone/redact) is likewise rejected. The full secrets path (encrypted store, Wardstone materialization) is E8; this rule keeps the Statefile clean NOW so E8 has nothing to migrate |
| `butler-admin-scope` | §9.2 per-principal scoping | A `butler`-role principal may not create, modify, or delete a route that is (currently or proposedly) `admin_tailscale` — defense in depth on top of the RBAC verb table |

§9.2-5 (identity flows only through LLDAP/Authelia) is not
mechanizable until the identity kinds carry flow information — recorded
here as NOT yet enforced, revisit with E6's identity package.

### A3. Exemptions: explicit, logged, narrowly scoped

An exemption suppresses exactly one rule for exactly one (kind, name),
with a required human reason and approver — loaded from
`<DataDir>/policy-exemptions.yaml` at start (a root-owned file on the
host is itself an authenticated act). An exemption USED during
validation is always surfaced (event `policy.exempted` on the write
paths) — silent exemptions are how invariants rot. Unused or malformed
exemptions fail startup loudly rather than lurking.

### A4. Risk-tier vocabulary

`policy.Tier`: `low`, `medium`, `high`, and `domestic` — the approval
table says which tiers auto-apply (`low` only) and which require human
approval. `domestic` is defined and validated from day one (ADR-W001
foundation: the concierge tier where an acting-for principal's changes
get a distinct approval flow) but maps to require-approval and is
otherwise UNUSED until v2 — no behavior branches on it.

# Part B — Work packages

- **P1** — `keystone/policy`: Engine, rules table, exemptions loader,
  tier vocabulary; `redact` exports its value-shape detector for the
  secrets rule; table tests per rule incl. the laundering guards.
- **P2** — wiring: API proposal (422 + `policy.rejected` event),
  ingestion (policy failure → revert), Butler delegation,
  cmd/keystone exemptions loading.
