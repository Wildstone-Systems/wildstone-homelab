# W050 slice 3: Engine bootstrap on one disposable node

> Tracked in #159. Source draft; the issue is the live execution copy. Post-alpha; owner scheduling of bounded work remains required. See the [execution gates](../w050-migration-plan.md#revised-execution-gates).

## Goal

Install a minimal signed k3s engine beside Docker for the 1–3-service proof.

## Scope

- Signed local prereqs/server/add-ons/Wardstone playbooks, pinned manifests, signature verification and idempotent installer; Keystone stays a host service.
- Install Longhorn and kube-vip for the initial proof; add NFS CSI only if a selected service needs it. Defer GPU/generic device plugins to #167 or an explicitly selected proof service.
- Use k3s secrets encryption and reviewed flags; separate bootstrap/recovery administration from scoped runtime reconciliation credentials.
- Lease election coordinates the runtime writer. Cancel work after leadership loss; retained server admin credentials are not revoked by election. Runtime credentials cannot modify admission/RBAC or grant registration.
- PSA/VAP enforce workload limits; exec/attach require break-glass. HostFacts reports engine/version/leader; measure idle RAM/CPU/disk.

## Dependencies

#157 and #158 complete; owner reviews #180 authority contract before runtime credential implementation (not the later fault-test results). Owner schedules a disposable target.

## Acceptance criteria

- [ ] QEMU install yields k3s Ready, required add-ons healthy and Wardstone leadership; repeat install is idempotent.
- [ ] Real API tests refuse unsafe pods and runtime attempts to alter admission/RBAC or mint host grants; no administrator-compromise containment claim.
- [ ] Tampered playbook fails before execution; unknown operations/parameters are refused.
- [ ] Record actual idle resource use and installed versions for #129; Docker e2e/exit drill unchanged.

## Non-goals

Apps, Keystone pod, device integration not needed by proof, full multi-node product and wire RunPlaybook.

## Size and decomposition

L; bootstrap, signing and authority checks.
