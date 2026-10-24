# Public documentation review — 2026-10-22

Baseline: `60df9dcbc1daa99d43f7692eb83f7d72f7070956` in
`Wildstone-Systems/wildstone-homelab`. Static source/documentation review only.

## Scope and provenance

The application management contract, glossary and baseline comparison originated
in old private-repository PR #609 (head
`f662b9474330765721d030cdab031188f023b492`). They were carried as documents,
without importing the old repository's commit history, and revalidated against
the public baseline. Current-source references use the public repository;
historical issue/PR references in older ADRs are not silently renumbered.

This review refreshes README and its install, vision, concepts, recovery,
threat-model and implementation-scope entry points, plus the documentation
index. It adds proposed ADR-W051 and an investigation/decomposition seed.
It does not implement guest provisioning, migration, a new package schema,
automatic remediation or an application adapter runtime.

## Findings and corrections

| Document | Stale claim or missing distinction | Correction and implementation evidence |
| --- | --- | --- |
| README | Blanket read/propose-only Agent description; weekly drill treated as universal proof; installer location and deployment ambiguity | Current authority caveat; Linux/systemd path versus proposed Proxmox topology; `scripts/install.sh`, `keystone/butler/owner_toolset.go`, `hostops_manifest_test.go`, `.github/workflows/exit-drill.yml` |
| install | Private-repository guidance foregrounded; control-plane versus combined install unclear; older blanket network-capability prohibition | Public download path; role-based and interactive modes; Linux guest/hypervisor distinction; Tailscale remains host preparation; `scripts/install.sh`, `packages/home-assistant/package.yaml` |
| vision | Two-butler roadmap and conditional W030 wording; unlimited proposal-only security claim; unqualified survivability proof | Current Agent/Person/Connection model, policy-bound processing, simple adoption and scoped recovery evidence; ADR-W033/W039/W049 and gateway implementation |
| concepts | Incomplete resource/credential-role sets; older two-surface account model; no-read-back confused with no sealed recovery | Registry as authoritative vocabulary; Person role versus Agent class versus credential role; sealed recovery and scoped tools; `keystone/resource/resource.go`, `runtimedb/principals.go`, `butler/owner_toolset.go`, `keystone/recovery` |
| recovery | Green fixture drill claimed to prove complete runbook; off-site checks described as absent; Statefile-only rebuild presented as full recovery | Failure-specific triage, scoped test evidence, off-site check/drill implementation and separately labeled legacy path; `keystone/backup/offsite.go`, `offsite_test.go`, `scripts/offsite-exit-drill.sh` |
| threat model | Every route described as one bearer chain; two mint exceptions asserted globally; no possible harm from closed operation surface implied | Selected-boundary review, session/transport distinctions, current Agent caveats, residual container/host risks; `keystone/api/api.go`, `consoleauth`, `principal`, Wardstone operations and host-tool tests |
| implementation scope | Original next actions, sizing and repository assumptions read as a current backlog | Historical framing and current precedence; W042/W043 voice implementation distinguished from original phasing; no retrospective re-scoping of accepted epics |
| docs index | Off-site engine marked unimplemented; no shared navigation for deployment/management proposals | Updated off-site code evidence and proposed-document navigation; status remains bounded by live validation |

The older catalog integration design note is not rewritten as an implementation
specification here. The new contract points readers to the existing domain
registry, `integrates`/`answers`/`agentTools`/`publishes` declarations and work
queue, so its historical proposals cannot be read as a reason to build a
second integration system.

## Validation and limits

- Read installer role selection, service enabling/restart behavior, bootstrap
  credential handling and release workflow references; did not install or reboot.
- Inspected the representative manifests linked in the [comparison](application-management-contract-v0.1-comparison.md), including both Home Assistant packages. Checked declared data, networking, identity and BackupPlan coverage; did not claim all upstream applications work.
- Read resource and credential-role registries, relevant Agent gateway and
  host-tool evidence, backup engine/check/drill code, and recovery runbooks.
- Checked changed Markdown relative file links, formatting and accidental
  private-repository operational links. Historical references remain historical.
- No Go, TypeScript, shell, package manifest or workflow behavior changes.
  Application builds/tests and live failure/restore experiments are outside
  this documentation change; CI results must be assessed separately.

## Remaining work

ADR-W051 and contract 0.1 need maintainer review and acceptance/decomposition.
Resource measurements, startup/readiness guarantees, storage backup coverage and application recovery conformance need
isolated experiments. See the [failure-domain review](failure-domain-recovery-review.md).
The historical implementation scope and older design notes are not current
release certification, and this review is not a full security audit.
