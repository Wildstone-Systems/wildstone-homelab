# W050 slice 3: engine on one node

## Implementation review amendment — 2026-10-24

Before runtime implementation, review the authority contract in #180: separate installer/break-glass administration from scoped runtime reconciliation credentials; policies/RBAC and grant registration must not be writable by ordinary runtime credentials. Root on server nodes remains privileged; loading an admin kubeconfig only while elected does not revoke it on leadership loss. Admission is a guard against malformed workloads, not unrestricted administrator compromise. Start with add-ons needed by the 1–3 proof services; defer device plugins until a service needs them. Measure actual baseline resource use.

This supersedes conflicting scope, ordering and acceptance language below. See [revised proof gates](../w050-migration-plan.md#implementation-review-and-proof-gates--2026-10-24). No runtime work is authorized by this amendment.


> Tracked in #159 (labels `W050`, `POST Alpha`, `agent:blocked`). This file is the source draft; the issue is the live copy.

## Goal

A fresh Ubuntu/Debian machine runs k3s with the W050 add-ons, installed by signed local playbooks, with Wardstone enrolled and elected leader. No apps on k3s yet.

## User-visible outcome

- `install.sh --role keystone --edition k3s` (opt-in flag) installs the engine beside Docker (both run during coexistence). Keystone itself still runs as today (host service) in this slice.
- Node page shows: engine k3s version, "coordinator: this machine", Longhorn healthy.

## Scope

- `deploy/ansible/`: `prereqs.yml`, `k3s-server.yml`, `engine-addons.yml`, `wardstone.yml`; vendored, pinned manifests for Longhorn, kube-vip, NFS CSI, Intel GPU and generic device plugins.
- `playbooks.manifest` + ed25519 signature in `release.yml`; verification in `install.sh` and Wardstone.
- k3s flags per plan §6.1; secrets encryption on; Traefik/ServiceLB/local-storage off.
- Admission: PSA labels on namespaces; VAP set from plan §6.5, bound to `wildstone-apps` and `wildstone-host`; RBAC denying exec/attach.
- Wardstone: Lease election (`wildstone-leader`), admin kubeconfig loaded only by the holder; `HostFacts` reports edition and k3s version; `SystemdUnitStatus` allowlist adds `k3s`.
- Measure RAM/CPU/disk idle (feeds #129).

## Non-goals

Apps on k3s; Keystone in the cluster; multi-node; `RunPlaybook` over the wire (slice 8).

## Dependencies

Slices 0 and 2.

## Acceptance criteria

- [ ] QEMU e2e (`testing/e2e`): install → k3s Ready → Longhorn, kube-vip, admission present → Wardstone is leader.
- [ ] k3d test: a privileged pod, a `hostPath` pod, a `hostNetwork` pod and a tag-only image are refused in `wildstone-apps`; `kubectl exec` as a non-break-glass user is refused.
- [ ] A tampered playbook file fails verification before Ansible starts.
- [ ] Re-running the installer is idempotent.
- [ ] Measured idle RAM recorded in the PR.
- [ ] Docker backend e2e and exit drill unchanged.

## Risks

- RAM overhead on small machines (plan §9.1).
- `ansible-core` version drift across distros: pin the minimum and test both.

## Size

**L.** New playbooks, signing and admission policy, but no app paths.

## ADR amendment

**ADR-W014:** add the Ansible step. **ADR-W050 §2.2:** only server nodes may lead.
