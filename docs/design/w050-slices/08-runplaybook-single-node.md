# W050 slice 8: Signed host updates on one node

> Tracked in #164. Source draft; the issue is the live execution copy. Post-alpha; owner scheduling of bounded work remains required. See the [execution gates](../w050-migration-plan.md#revised-execution-gates).

## Goal

Execute typed OS/k3s/Wardstone update Plans with downtime stated before approval.

## Scope

- RunPlaybook uses signed allowlisted playbooks, fixed argv and typed per-playbook parameters.
- Implement bounded progress, reboot/reconnect/version reporting and High-risk Plan steps.
- Contribute OS/k3s/Wardstone evidence to #129. The #129 recommendation also needs #163 backup/replacement restore and #168's early migration proof; #164 cannot close it alone.

## Dependencies

#159 for host update implementation; use a running proof workload to verify post-reboot readiness. Owner schedules execution.

## Acceptance criteria

- [ ] OS update/reboot returns node and proof apps to readiness; versions/outage recorded.
- [ ] Pinned valid k3s patch succeeds; invalid release is refused before binary replacement.
- [ ] Unsigned/unknown playbook or invalid typed parameters are refused; Butler cannot approve/apply.
- [ ] Host-update evidence attached to #129; owner evaluates the complete proof separately.

## Non-goals

Drain/rolling multi-node productization and standalone acceptance of #129.

## Size and decomposition

M; one typed operation and fixed playbooks.
