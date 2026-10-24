# W050 slice 14: multi-node

> Draft issue body (expect to split into 14a join/placement/failover and 14b rolling host updates/leader handover). Plan: §6.2, §6.4, ADR-W050 §2.5, §2.8.

## Goal

A household adds machines from the Console. Apps marked "keep running" restart on another machine when one dies. Host updates roll one machine at a time.

## User-visible outcome

- "Add a machine" shows one installer command. At 2 nodes: "Partly protected" banner. At 3: "Any one machine can fail".
- "Keep this running if a machine fails" switch per package.
- "Update all machines, one at a time" Plan.

## Scope

- Join: Keystone gives the k3s join secret to the new Wardstone after mTLS join; `k3s-agent.yml` or `k3s-server.yml`.
- At 3 servers: migrate k3s datastore from SQLite to embedded etcd (`--cluster-init`) as a Plan **(inferred: k3s supports this; prove)**.
- Longhorn copies to 2; `failover` toleration tuning; soft anti-affinity for management.
- Rolling host updates: `CordonDrain` → `RunPlaybook` → wait Ready + Longhorn rebuilt → `Uncordon`; stop on first failure; servers first for k3s.
- Leader handover before its own drain; "no coordinator" state in Console.
- Backup Job placement re-proved with volumes on other nodes.
- External observer for host loss (W051 §4.7): document an off-site check; second node watches the first.

## Non-goals

`replicated`, `offload`; Talos.

## Dependencies

Slices 8, 10, 13; ADR-W051 accepted; owner-operated test cluster (#16, D5).

## Acceptance criteria (owner-run evidence records)

- [ ] Join a second and third node from the Console.
- [ ] Power off the node running Keystone: one Keystone comes back elsewhere; no second writer (W051 gate 5).
- [ ] Power off a node running a `failover` app: back within the stated time.
- [ ] Rolling OS update across 3 nodes with an app staying up.
- [ ] Leader handover during its own drain; Plan resumes.
- [ ] Power-loss chain (W051 gate 3) completes with readiness deadlines.

## Risks

Split-brain storage, quorum loss at 2 nodes, untestable in hosted CI.

## Size

**XL.** The cluster story itself; split into two issues.

## ADR amendment

Possibly W050 §2.8 wording for 2 nodes after measurements.
