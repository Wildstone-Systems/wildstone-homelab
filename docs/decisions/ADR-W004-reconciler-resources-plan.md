# ADR-W004: Reconciler, resource schema, Statefile, plan artifact, external-resource pattern

- **Status:** accepted (locked decisions #2 + #3 of proposal §10, produced by Spike S2)
- **Relates to:** architecture proposal §3, implementation scope S2
- **Audience:** decision record AND implementation spec for Spike S2, written
  so implementation subagents can execute the work packages in Part C without
  the planning conversation. Part A/B are binding; if a work package
  contradicts them, stop and report.

## Conditional amendment from ADR-W030

This amendment becomes binding when ADR-W030 is accepted. Butler admission,
principal/member links, permanent profile tool enablement, standing routine
authority, and native-routine declarations are intentional desired-state
changes: represent them as typed resources and change them through Plans.
Their exact resource schemas land with their implementation slices.

Temporary task grants are bounded runtime control records rather than
Statefile resources. A disable, unlink, or revocation is an immediate runtime
denial and wins over older desired state. It also creates or links a Plan to
remove the stale declaration; until that Plan completes, Console reports
`revoked, declaration pending removal`. Reconciliation cannot lift the denial,
and re-enabling requires a fresh approved Plan. Existing risk classification
applies to the cleanup diff.

An adapter connection's primary and ordered eligible fallback nodes are
Plan-mediated desired state. Approval authorizes deterministic runtime
placement on any listed node without a new outage-time Plan. A single lease
epoch fences concurrent instances and late results. Adding a node is not
complete until its credential is provisioned; removing one immediately denies
new work and remains incomplete until local credential erasure and any required
remote revocation are verified.

---

# Part A — Decisions (binding)

### A1. Resource model (locked decision #2, schema half)

Two layers. **Capabilities** are the user-facing specs (out of S2 scope —
S3). **Resources** are the typed machine-layer records the Reconciler drives.
Every resource is:

```yaml
apiVersion: wildstone.dev/v1
kind: Container | Volume | Route | DnsRecord      # S2 ships these four
metadata:
  name: <string, unique per kind>
  owner: capability/<name> | system              # ownership chain; nothing orphaned
  node: <node id>                                 # which Wardstone applies it
spec: {kind-specific}
```

`status` (observedGeneration, conditions) lives ONLY in the runtime DB and is
**never written to the Statefile** — every Statefile commit is an intentional
spec change. The four S2 kinds' specs:

- **Container** — `image` (digest-pinned), `env` (map), `ports` (list of
  host:container), `mounts` (list of {source, target, readOnly}), `restart`.
  Applied via Wardstone EnsureContainer/StopContainer/RemoveContainer.
- **Volume** — `name`. Applied via Wardstone EnsureVolume.
- **Route** — `host`, `upstream` (`container/<name>:<port>` or `ip:port`),
  `auth` (`none` | `authelia_oidc`), `classification` (mandatory policy
  input, e.g. `family_public`). Applied by rendering a managed Caddy config
  fragment via Wardstone WriteManagedFile + a reload op (spike: reload is a
  ContainerLogs/Health no-op seam; real reload lands in E-series).
- **DnsRecord** — `zone`, `name`, `type` (A/CNAME/TXT), `content`, `proxied`
  (bool). Applied via the external-resource provider (Cloudflare), NOT
  Wardstone — see A5.

Kinds are a closed Go registry (one `ResourceKind` per kind); adding a kind is
a code change, mirroring the closed-op-surface philosophy of ADR-W003.

### A2. Statefile (locked decision #2, store half)

The desired state store is a **git repository** managed by Keystone:
`<DataDir>/statefile/`, one YAML file per resource at
`<kind>/<name>.yaml` (lowercased kind). Keystone commits every accepted
change with a structured message: `who` (user | butler | system), `what`
(plan id), `why` (summary). `status` is never committed.

- Git is driven by exec'ing the `git` binary behind a `gitRunner` seam
  (git is present in CI and on the VM; this avoids a heavy library dep and
  keeps the repo a real, user-cloneable git repo — the escape hatch). Tests
  init a real repo in `t.TempDir()`.
- An out-of-band human commit is treated like any proposed change on next
  reconcile: validate → plan → apply, and a commit that fails policy
  validation is reverted (spike: detect + refuse + log; auto-revert is a
  follow-up).
- Config identity: `user.name=wildstone-keystone`,
  `user.email=keystone@localhost`, set per-invocation (`-c`) so the spike
  never depends on global git config.

### A3. Plan artifact (locked decision #3)

One `Plan` object serves UX, safety, and AI-governance:

```
Plan{
  ID          string           # "plan-<n>" (monotonic; spike counter, DB seq later)
  Summary     string           # human one-liner
  Steps       []PlanStep
  OverallRisk RiskGrade        # max of step risks
}
PlanStep{
  Description string            # "restart Jellyfin (~2 min, no data affected)"
  Kind       string            # resource kind
  Name       string            # resource name
  Action     Action            # Create | Update | Delete | NoOp
  Op         *ResourceOp        # the typed operation(s) to apply this step
  Reverse    *ResourceOp        # reverse op for rollback where defined (nil = no auto-reverse)
  Risk       RiskGrade          # Low | Medium | High
  BlastRadius string            # what's affected: services touched, data safety
}
```

Rules:
- **No "just do it" write path.** Every intentional change produces a Plan.
  Low risk MAY auto-apply per policy; Medium/High require explicit approval
  (`Plan.Approve()` in-process for the spike; Console/CLI later).
- Risk grading (spike heuristic, pinned): data-affecting ops (Volume delete,
  mount change, Container delete with a bound volume) = **High**; service
  interruption without data risk (Container image change/restart, Route
  change) = **Medium**; additive/no-interruption (new Container, new
  DnsRecord, new Volume) = **Low**. `OverallRisk` = max over steps.
- Diff is **level-triggered**: `diff(desired, observed) → []PlanStep`.
  Ordering: creates/updates of dependencies before dependents
  (Volume → Container → Route → DnsRecord); deletes in reverse.

### A4. Reconcile loop

Level-triggered, per-kind controllers in one process (proposal §3.4):

```
observe (Wardstone + providers) → diff(spec, observed) → Plan
   → approve (auto if Low & policy allows, else wait) → apply step-by-step
   → health-gate each step (post-condition probe)
   → on gate failure: run step.Reverse (and prior steps' reverses), halt, raise incident
```

- **Bounded remediation**: apply retries with the agent's existing backoff
  posture; after N failures (spike N=3) stop and raise an incident rather
  than crash-loop.
- **Drift classes**: (a) benign (container restart counts) → ignore;
  (b) reconcilable (missing container) → repair via plan; (c) foreign (an
  object without the `dev.wildstone.owner` label / a resource Keystone
  doesn't own) → **never delete**, report + offer adoption. The agent's
  ownership guard (ADR-W003 A4.3) already enforces this at the op layer;
  the reconciler must not even generate delete ops for foreign objects.
- **Never auto-remediate data loss**: Volume/mount divergence always requires
  an approved plan, never auto-apply, regardless of policy.
- **Health gate**: each step has a post-condition. Container → Wardstone
  Health/ContainerStats shows running; Route → ProbeHttp/ProbeTcp;
  DnsRecord → provider read-back. On failure, the reconciler runs **only the
  failing step's `Reverse`** and then halts the plan; earlier steps that
  already passed their own gate stay applied. This is the level-triggered
  posture (proposal §3.4): a healthy, already-gated resource is not torn
  down for a later step's failure — the next reconcile converges the
  remainder. (Whole-plan atomic rollback is reserved for upgrade plans with
  explicit `onFailure: rollback`, §8 — not the general reconcile path.)
  Resolved during S2b, which surfaced the ambiguity against an earlier draft
  of this line.

### A5. External-resource controller pattern

Resources whose truth lives in a third-party control plane (Cloudflare DNS,
later Tailscale) implement a `Provider` interface instead of going through
Wardstone:

```
Provider interface {
  Observe(ctx, resource) (observed, exists bool, err error)
  Create(ctx, resource) error
  Update(ctx, resource) error
  Delete(ctx, resource) error
}
```

The reconcile loop treats providers identically to Wardstone-applied kinds —
same diff→plan→apply→health-gate. External control planes are the
proposal's flagged 2× overrun risk, so the spike prices in the hard parts up
front and the provider contract must handle them:

- **Drift**: `Observe` returns the live record; diff detects out-of-band
  changes.
- **Partial failure**: a Create that half-succeeds (record made, read-back
  fails) must be idempotent on retry (Create-or-Update by unique key:
  zone+name+type).
- **Rate limits**: the provider retries on HTTP 429 with backoff honoring
  `Retry-After`; after a bounded number of attempts it surfaces a typed
  `RateLimited` error the reconciler treats as retryable-later, not fatal.

---

# Part B — Codebase contract

### B1. Where S2 code lives (all under `keystone/`, which may take normal deps)

| Package | Contents |
|---|---|
| `keystone/resource` | `Resource` struct, the four kind specs, the closed kind registry, YAML (de)serialize, validation (classification mandatory on Route, digest-pin on Container image). |
| `keystone/statefile` | git-backed store: `Load()`, `Put(resource)`, `Delete(kind,name)`, `Commit(who,what,why)`, `List()`. `gitRunner` seam. |
| `keystone/plan` | `Plan`, `PlanStep`, `RiskGrade`, `Action`, risk grading, `Diff(desired, observed) *Plan`, ordering. |
| `keystone/reconcile` | the engine: `Reconciler` driving apply/health-gate/rollback over an `Applier` interface; drift classes; bounded retry. |
| `keystone/provider` | `Provider` interface (A5). |
| `keystone/provider/cloudflare` | Cloudflare DnsRecord provider + a fake CF API server for tests. |

The reconciler applies Wardstone-backed kinds through the existing
`gateway.Gateway.Dispatch` (ADR-W003) — wrap it in an `Applier` so tests
inject a fake. Reuse the S1/E1 test style: real gateway + real agent over
localhost mTLS, agent talking to the fake Docker; fake CF API server for DNS.

### B2. Build/verify contract (unchanged from ADR-W003 B2)

```sh
export GOTOOLCHAIN=go1.25.0
gofmt -l ./proto ./wardstone ./keystone ./cli ./testing/e2e   # empty
for m in proto wardstone keystone cli testing/e2e; do (cd "$m" && go vet ./... && go build ./... && go test ./...); done
(cd wardstone && go test -race -count=1 ./...) ; (cd keystone && go test -race -count=1 ./...)
```

New `keystone` deps allowed: `gopkg.in/yaml.v3` (already indirect via
testify). No proto changes expected in S2 — the reconciler uses the existing
op surface. If you need a new op arm, STOP and report (it likely belongs in a
W-package, not the spike).

### B3. Known traps (carried from ADR-W003 B3, still apply)

1. Generated oneof wrapper interfaces are unexported — build ops with the
   concrete `*wsv1.Operation_Xxx` wrappers.
2. `go build ./...` inside a module writes stub binaries — gitignore new
   mains by exact path.
3. `gofmt -w` before finishing; CI fails on unformatted code.
4. New module dirs go in `go.work` AND the four CI loops.
5. Shell cwd resets between Bash calls — cd with absolute paths.
6. Reconcile/apply tests over a real agent: reuse the same Gateway object on
   restart (same CA) or agents reject the new CA.

---

# Part C — Work packages (ordered; each verifies per B2, commits nothing)

Rules of engagement identical to ADR-W003 Part C: one package per change,
tests meeting B2 including the drift/partial-failure/rate-limit tests where
the package touches them, report back (files, decisions the spec didn't pin,
deviations, verification), STOP-and-report on any non-additive proto change or
new dependency beyond those named in B2.

### S2a — Data layer: resources + Statefile + plan types

- `keystone/resource`: the four kinds, closed registry, YAML round-trip,
  validation (Route needs classification; Container image must match the
  digest regex from ADR-W003 A4.2).
- `keystone/statefile`: git store with the `gitRunner` seam; `Put`/`Delete`/
  `Commit`/`List`/`Load`; structured commit messages; status never written.
- `keystone/plan`: `Plan`/`PlanStep`/`RiskGrade`/`Action`, the pinned risk
  heuristic (A3), `Diff(desired []Resource, observed []Resource) *Plan` with
  dependency ordering. No apply behavior yet.
- Tests: YAML round-trip + validation rejections; statefile Put→Commit→List
  round-trip and a structured-message assertion (real temp-dir git repo);
  Diff produces correct ordered steps + risk grades for add/update/delete/
  no-op cases including the data-loss=High cases.

### S2b — Reconcile engine over Wardstone (Container, Volume, Route)

- `keystone/reconcile`: `Reconciler` with an `Applier` interface (real impl
  wraps `gateway.Dispatch`); observe→diff→plan→approve→apply→health-gate→
  rollback; bounded retry (N=3) + incident on exhaustion; drift classes
  (never plan deletes for foreign/unowned objects).
- Route apply: render a managed Caddy fragment (WriteManagedFile) + a reload
  seam; health-gate via ProbeHttp against a test HTTP server.
- Tests (real gateway + real agent + fake Docker, temp-dir statefile):
  converge an empty node to {Volume, Container, Route}; re-reconcile is a
  no-op; kill the container out-of-band → drift detected → repair plan →
  reconverge; a High-risk (volume delete) plan is NOT auto-applied without
  approval; a health-gate failure triggers the step reverse and halts.

### S2c — Cloudflare DnsRecord external-resource controller

- `keystone/provider` (interface) + `keystone/provider/cloudflare`
  (implementation + a `httptest` fake CF API: list/create/update/delete
  records by zone, with injectable 429 + `Retry-After` and partial-failure
  modes).
- Wire DnsRecord into the reconcile engine via the Provider path.
- Tests: create one A record end-to-end; observe drift (record changed at
  CF) → update plan → reconverge; partial failure (create succeeds, first
  read-back 500s) → retry is idempotent (no duplicate record); 429 with
  `Retry-After` → provider backs off and eventually succeeds; the record's
  zone+name+type is the idempotency key.
- **Live-CF note**: all S2c tests run against the fake. The single
  live-Cloudflare record test (real token, real zone) is explicitly deferred
  to a manual run gated on the platform owner's scoped API token — document
  this in the package README and do NOT attempt a live call in CI.

### S2 exit

Produces this ADR (locked decisions #2 + #3). S2's "end to end on a fixture
VM" demo is the testing/e2e harness path (owner-run); the automated coverage
above is the CI-runnable equivalent. On completion, trim Part C to an
"implemented by" note.
