# W050 slice 2: Keystone backend seam + typed cluster ops

## Implementation review amendment — 2026-10-24

Docker-only extraction may proceed when scheduled, preserving Plans and ordering. Do not require a Kubernetes backend to emulate Docker result types. Keep typed cluster operation design provisional until the 1–3-service proof (#129) informs it. Review #179 and #180's authority, stale-operation, revision-binding and idempotency contracts before freezing wire operations. A Kubernetes Lease is coordination, not fencing.

This supersedes conflicting scope, ordering and acceptance language below. See [revised proof gates](../w050-migration-plan.md#implementation-review-and-proof-gates--2026-10-24). No runtime work is authorized by this amendment.


> Tracked in #158 (labels `W050`, `POST Alpha`, `agent:blocked`). This file is the source draft; the issue is the live copy.

## Goal

Separate "what should change" (diff, risk, approval) from "how to tell a backend" in Keystone, so a k3s translator can be added without touching the diff/approval code. Reserve the typed leader operations in the proto. No behaviour change.

## User-visible outcome

None.

## Scope

- `keystone/reconcile/reconcile.go`: introduce `Translator` (per backend). Move Docker-only logic into a Docker translator:
  - `ensureNodePrereqs` (Docker network, `:1775-1790`), `specHash` (`:1727`), `containerEnsureOp` (`:1872`), container health gate (`:1503-1700`);
  - `ingressaddr.go`, `hostaliases.go`, `hostport.go`, the `host/<port>` rendering (`keystone/resource/routeupstream.go:39`).
- `keystone/reconcile/applier.go:19`: `Apply(target, op)` where target is a node or "cluster".
- `keystone/reconcile/loop/loop.go:685-696`: a cluster group (empty until slice 4).
- `proto/wardstone/v1/wardstone.proto`: reserve field numbers 39+ with comments for `ApplyWorkload`, `DeleteWorkload`, `ObserveWorkload`, `WorkloadLogs`, `RestartWorkload`, `RunJob`, `ObserveJob`, `EnsureClusterSecret`, `EnsureClusterConfig`, `CordonDrain`, `Uncordon`, `RunPlaybook`. Write the messages as a design doc (`docs/design/w050-leader-ops.md`), not generated code yet.
- `keystone/api/handlers_secrets.go:328-331`, `handlers_personal_credentials.go:572-575`: replace Stop+Remove with one "restart workload" call on the translator.
- `keystone/internal/fakenode`: `FakeCluster` recorder.

## Non-goals

Any k3s client; any new wire arm in generated code.

## Dependencies

Slice 1.

## Acceptance criteria

- [ ] All existing reconcile, loop, backup and API tests pass unchanged (byte-identical Plans).
- [ ] New test: only the Docker translator emits `EnsureContainer`/`EnsureDockerNetwork`.
- [ ] Leader-op design doc reviewed: every op typed, none carries YAML, a command, or free key/value Ansible vars.
- [ ] e2e and exit drill pass.

## Risks

- Large file; risk of subtle ordering changes (secrets before prereqs, `secret.go` header). Keep the call order and add an ordering test.

## Size

**L.** `reconcile.go` is 2,308 lines with Docker assumptions in several places.

## ADR amendment

**ADR-W003:** list the reserved arms and add "no op carries manifests, YAML or commands".
