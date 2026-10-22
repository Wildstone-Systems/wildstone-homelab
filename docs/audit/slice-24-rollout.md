# Slice 24 acceptance and rollout record (#225)

This records what was verified from the slice 24 worktree and what still
requires the household host. It is not a claim that every §18 gate passed.
The current audit dispositions in [gap-report.md](gap-report.md) remain the
source for unresolved product behavior.

## Code and scratch evidence

- Fresh SQLite migration chain: `TestMigrationChainAppliesFromEmpty` passed.
- Pre-epic migration chain: `TestMigrationChainAppliesFromPreEpicSnapshot`
  migrated a populated migration-0038 settings row, including non-default
  legacy values, through 0079. It checked surviving settings and integrity.
  The existing identity/audience realistic-snapshot tests passed after their
  deliberate-drop allowlist named the two retired columns.
- The local scratch Keystone was built from this worktree, started with its
  own data directory on `127.0.0.1:19441`, and returned `200` from authenticated
  `GET /api/v1/whoami`. It used no live household database, model or credential.
  Its process and data directory were removed after the check.
- `TestPartCAcceptance` passed against its isolated test database. The
  per-section §18 regressions live in Keystone API, runtime DB, automation,
  Butler and reconciliation tests; the tests cited by current finding rows
  point to the relevant subset.
- Console `npm run build` passed. The focused audit and ADR reconciliation
  tests passed. Full Linux Go testing of the bind-mounted Windows checkout
  passed the runtime DB and API packages, but the installer and package-lock
  suites compared checkout-specific shell/package bytes and failed in this
  CRLF worktree; remote Linux CI is the clean-checkout gate.

## §18 gate status

| Area | Repository/scratch evidence | Live-only or still-open check |
|---|---|
| 18.1 identity and access | Person, demotion, derived-authority and offboarding tests; see F-155 and F-158 | Confirm deployed Owner/Member configuration and recovery procedure read-only. Child classification is #284. |
| 18.2 service entitlements | Entitlement and portal tests in `keystone/runtimedb/entitlements_test.go` and `keystone/api/handlers_entitlements_test.go` | Direct URL denial and dormant assignment behavior against a scratch deployment with real ingress; no mutation of household service access. |
| 18.3 Agents and tools | Agent-class and authority tests in `keystone/runtimedb/agentclass_test.go` and `keystone/butlerstore/derived_authority_test.go` | Effective Console inventory is still open in #316. |
| 18.4 Connections and sessions | Multi-bot, admission, continuation and disclosure tests in `keystone/api/handlers_connections_test.go` and `keystone/api/handlers_chat_disclosure_test.go` | External platform transport behavior and delivery failure notices remain open in #314; live bot settings require read-only inspection. |
| 18.5 memory | Scope, disclosure and restore tests in `keystone/runtimedb/memoryscopes_test.go` and `memorydisclosure_test.go` | Semantic recall #311 and remaining retention/correction questions #317 are open. |
| 18.6 secrets and privacy | Secret authorization, routine-principal and destination-policy tests in `keystone/runtimedb/householdsecrets_test.go`, `routineprincipal_test.go` and `privacy_test.go` | Real model egress and content inspection need a scratch host with actual model endpoints; #317. |
| 18.7 Plans and routines | Exact-version review, execution, stop and capability recovery tests in `owner_review_queue_test.go` and `keystone/automation/routine_execution_test.go` | Clock schedules, delivery records and multi-step gaps remain #315. |
| 18.8 lifecycle | Demotion, offboarding, purge and `TestPartCAcceptance` | Verify deployed binary/version and household restore/backups read-only before rollout; no live mutation during acceptance. |

## Household rollout gate

The first rollout attempt treated `<ollama-host>` as the household host, but
[`runtime-access.md`](runtime-access.md) documents that address as the Ollama
endpoint. A later readiness pass found the Linux production and scratch hosts
as `<household-host>` and `<test-host>` over Tailscale (hostnames
redacted for publication). Both
accepted TCP connections on port 22, but neither accepted this workspace's
non-interactive SSH credential. No production snapshot, live binary version or
household §18 result is claimed. The next operator needs SSH authorization or
the established deployment workflow, then should:

1. Read the current service status and `go version -m` metadata of the
   installed Keystone binary. Capture a verified encrypted backup or
   filesystem snapshot of the runtime DB and preserve the current binary.
2. Copy a sanitized pre-epic production snapshot to an isolated scratch data
   directory. Apply the new binary's migrations there and run `PRAGMA
   integrity_check`, `PRAGMA foreign_key_check`, and read-only household
   summary queries. Do not run acceptance mutations on the live database.
3. Build the exact merged commit from a clean tree, verify `vcs.modified=false`,
   install the binary and restart `keystone.service` using the host's normal
   maintenance procedure. Migration 0079 only drops two unused settings;
   migration 0038 remains in applied history.
4. Check service health, the installed `vcs.revision`, migration 0079 presence,
   and the read-only §18 configuration checks. Exercise writes and external
   model/transport flows against a separate scratch instance on the host.

Rollback: stop the new service and restore the preserved prior binary if
startup fails. The old binary does not read the two dropped 0038 columns, but
the database migration is forward-only; never replace a migrated household
database with an older snapshot without restore-quarantine reconciliation.
Keep the encrypted backup for recovery, and record the actual artifact,
timestamp and outcome here after deployment.
