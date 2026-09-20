# Slice 13: memory scopes implementation and review

Implements [#214](https://github.com/chickenrat17/wildstone-homelab/issues/214), part of [#201](https://github.com/chickenrat17/wildstone-homelab/issues/201). The contract is `docs/agent-system-design.md` sections 11, 17.2 and 18.5, with ADR-W033 taking precedence over the previous blanket Butler isolation rule in ADR-W031 A4.

## Settled decisions

After reviewing slice 12 ([PR #293](https://github.com/chickenrat17/wildstone-homelab/pull/293), merged as `e2b8de9`), the user selected:

1. Keep session context separate in `butler_sessions` / turns. Do not duplicate transcripts into durable memory or change first-turn session allocation.
2. Apply Personal and Household defaults retroactively. Retain genuinely Agent-local material and all original provenance.
3. Approve Personal-to-Household sharing in the authenticated household dashboard. A Member may approve their own memory; another Owner cannot approve it for them.

Implementation branches from and targets `main`, as corrected by epic #201. The original shared checkout and its uncommitted slice 12 work remain untouched; development uses an isolated worktree.

## Dependency review

Slice 12 supplies canonical Person and Agent keys on sessions. Its merged continuation path checks the intersection of all Connection origins and loads history in one read snapshot. The draft concern about considering only the most recent origin was resolved before merge.

First-turn execution still precedes session-ID allocation, and memory seams capture the Person and Agent rather than a durable session key. Keeping session context in the existing tables avoids introducing a competing identity or allocating a second conversation record. Slice 14 remains responsible for broader active-Person and output-destination context filtering; this slice proves its own explicit private-derivative negative case at both live memory seams.

## Schema and behavior

Migration `0067_memory_scopes.sql` adds three durable scope values. Together with the separate session schema, these represent all four design scopes.

| Scope | Resource boundary |
|---|---|
| Session | Existing Person + Agent + session identity and continuation authorization |
| Agent-local | Holding Agent, additionally constrained by Person ownership and source audiences |
| Personal | Owning Person, across permitted Agents |
| Household | Across Agents, subject to audience and per-Agent restrictions |

Retroactive mapping: Butler-stamped inference/procedure records remain Agent-local; Person-owned records without a household audience become Personal; remaining records become Household. Derivatives of Agent-local ancestors also remain local. Existing Butler stamps, source IDs, authors and owners are retained. Scope and ownership changes require new records, rather than in-place edits.

`memory_constraints` preserves every inherited source audience and Personal owner predicate. Retrieval, index-backed recall and export re-evaluate these predicates against current group membership. Search candidates consult the authoritative memory record instead of trusting copied index metadata. Pending household digests exclude Personal/Agent-local records and any inherited restricted audience, including entries queued before migration.

Remember defaults to Personal and may explicitly narrow to Agent-local. The model cannot choose the owner, audience, holding Agent, or Household scope. No autonomous tool can approve promotion.

Dashboard promotion creates a new Household record, a derivation link and an authenticated Person approval receipt. The original remains Personal. Approval releases only that Person's own private constraint, retaining group restrictions. Replays are refused. Forgetting the source invalidates the approved copy and its search chunks. Eligible household-wide sharing also enters the existing digest.

Correction reads and writes within one transaction so a concurrent transfer cannot recreate memory under the previous holding Agent.

Retirement optionally transfers all Agent-local records to a live destination in the same transaction. Source/author/owner/derivation provenance survives, and receipts record both Agents and the acting Owner. Personal and Household records remain reachable after retirement. Transfer preserves forgotten markers and cannot revive a suppressed record.

Per-Agent restrictions subtract Personal or Household access from the baseline; they never grant Person or audience authority. The Console exposes these controls and transfer selection. Both restriction directions and retirement require the existing fresh Owner authentication wrapper, with independent store-level role checks.

Forgetting writes the existing deletion journal atomically, suppresses source content for all descendant generations, and removes searchable copies. Restoring a pre-forget snapshot requires journal replay before quarantine can be released.

## Authorization layers

This slice changes section 10 layer 2, resource-specific memory authority, and preserves layer 3 audience restrictions. It does not replace layer 1 execution/admission decisions. Person identity comes from the authenticated/session caller; Agent-local access never grants access to another Person's private material. Console authority does not substitute for a Person's sharing consent.

## Implementation sequence and regression evidence

Behavioral tests were added before implementing cross-Agent Household/Personal access, authoritative index filtering, promotion, transfer and restriction routes. Additional red-to-green cases exposed missing deletion-journal writes, export bypass of inherited group restrictions, narrow-scope export/digest leaks and legacy local-derivative reclassification. Each was fixed before final validation.

The named #214 acceptance tests are present:

- `TestAgentLocalIsUnreadableByAnotherAgentByDefault`
- `TestHouseholdMemoryIsReadableAcrossAgents`
- `TestRetiredAgentsHouseholdMemoryRemainsReachable`
- `TestDerivedRecordNeverWidensBeyondStrictestSource`
- `TestAgentLocalDerivedFromPersonAIsExcludedForPersonB` — separate Recall and rendered-preamble assertions
- `TestPersonalToHouseholdPromotionRequiresTheOwningPerson`
- `TestAgentDeletionTransfersLocalMemoryWithProvenance`
- `TestForgetSurvivesBackupRestore` — actual snapshot, journal replay, multi-generation derivatives and index copies in every durable scope

Additional tests cover migration and foreign keys, immutable scopes, correction non-promotion, default Personal and explicit Agent-local live writes, broad stale index metadata, current group changes, export/digest denial, HTTP promotion ownership, real Console transfer/restriction routes and consequential authentication in both restriction directions.

Seven temporary mutations were applied at actual callers, run against the relevant regression, and reverted:

| Mutated caller | Observed failure |
|---|---|
| RetrieveMemory substitutes the all-Agent view | Agent-local cross-Agent denial |
| Preamble substitutes Person A for the active Person | Person B's rendered-context exclusion |
| SearchIndex drops authoritative memory predicates | Private recall exclusion despite broad copied index metadata |
| Dashboard handler substitutes the record owner for the authenticated Person | Another Person's HTTP promotion denial |
| Retirement handler ignores the requested transfer | Real route's destination-memory assertion |
| RetrieveMemory omits inherited constraints | Group-revocation exclusion |
| Correction reads the source before acquiring its transaction | Concurrent transfer must not recreate memory under the retired Agent |

These failed on behavior assertions, not compile errors or unrelated lower-layer refusals.

## Validation

- Windows affected-package tests and race tests passed for runtimedb, butlerstore, butler, API and Person-reference classification.
- The final correction/transfer change passed a fresh Linux `go vet` plus race run of memory, correction, digest, scope and related API regressions (runtimedb and API).
- Console `npm run build`, `render-check`, `contrast-check` and `handoff-check` passed. Vite reports the existing large-chunk advisory.
- Playwright exercised actual dashboard assets with fixture API responses: cancel sends no promotion request; accept sends one request and displays success; desktop/mobile layouts have no horizontal overflow or JavaScript errors. Screenshots were inspected.
- The live `ui-check` was unavailable because no operator credential is configured; fixture checks are not a production smoke test.
- Validation uses Go 1.25.14. Linux checks use a Git archive with LF checkout conversion and Caddy 2.8.4, matching CI's Caddy version. An initial Windows-mounted run found CRLF package-hash/script mismatches and Debian's obsolete Caddy; those environment problems were resolved without changing packages or scripts.
- Full Linux `gofmt -l`, `go vet ./...` and `go test -race ./...` passed in all five modules: proto, wardstone, keystone, cli and testing/e2e.
