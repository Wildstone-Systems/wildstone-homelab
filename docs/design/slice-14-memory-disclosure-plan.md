# Slice 14: Person-aware memory retrieval and assembled context

Implements [#215](https://github.com/chickenrat17/wildstone-homelab/issues/215), part of [#201](https://github.com/chickenrat17/wildstone-homelab/issues/201). Based on main after slice 13 / PR #295. The epic's main-branch instruction supersedes the older branch named in the issue.

## Policy decision

Use existing Connection/provenance disclosure rules for one-to-one external chats, denying unknown or incompatible destinations. Compatible external disclosure remains supported. Group conversations remain refused in MVP. Session context stays separate from durable memory.

Full content requires matching, resolved reach, encryption, retention and surface classifications. Permission to list a neutral session candidate is insufficient to disclose its transcript. Unknown retention is unresolved for content disclosure even when two Connections both record it.

## Implementation

`Turn.MemoryDestination` recognizes only the exact built-in Console/Homepage constants at Connection reference zero. Other destinations carry their immutable Connection row ID. Group status comes from ingress. None of these fields is a tool argument.

`butlerstore.MemorySeams` binds a `MemoryReadContext` containing canonical Person and Agent IDs. Its fields are private to runtimedb. Missing or unresolved identity yields a denying context. At each read the SQL checks current Person admission, Agent access/lifecycle/class, record audience and group membership, inherited audience constraints, scope, per-Agent restrictions, and destination disclosure. Agent access cannot replace Person authorization. The existing Console Owner access-list exemption is carried explicitly from the trusted turn and requires a currently active Owner; it does not release another Person's audience, scope or Agent restriction.

Both `RetrieveMemory` and `SearchIndex` call `memoryReadClause`; there is one candidate authorization implementation. Filtering and promotion deduplication happen before LIMIT and before content is read. Slice 13's promotion/index fallback, forget, export and scope semantics remain intact. Older nil-context store queries remain internal inspection/maintenance APIs; model seams always supply a context, including on failed identity resolution. The test-only `testMemorySeams` helper retains existing isolated write/scope fixtures; new live tests use the public bound constructor.

The gateway independently refuses group recall/remember and suppresses group preambles, even if a caller accidentally supplies direct-session seams. This protects the real gateway below the existing HTTP group guard.

### Durable provenance: migration 0068

`memory_disclosure_origins` captures source Connection identities and their classifications in the memory insertion transaction. The source of a remembered conversation note is a **task ID**, allocated before first-turn execution. A continued task additionally contributes every inbound origin of its session history. Historical session IDs are recognized separately. Missing or unresolvable origins become explicit unknown constraints.

Existing records are backfilled without modifying their content, scope, owner or audience. The insertion trigger applies to every writer. At external read time, both the captured classifications and the source Connection's current classifications must match the current destination. Revoked or missing Connections deny disclosure. Changing both source and destination classifications cannot erase the original captured constraint.

A recursive lineage query includes both derivation edges and correction predecessors. Promotion retains its source edge; Agent transfer retains the same record and provenance. None of these operations releases disclosure restrictions. UNION makes traversal terminate even with a malformed cycle.

Trusted built-in destinations still apply Person, audience and Agent restrictions; only the external-origin compatibility constraint is absent there. Legacy/unclassified memory remains available on authorized trusted surfaces. Classifying a Connection later does not retroactively establish the unknown provenance of an existing record. Indexed documents without a memory row have no captured origins and are withheld externally until such provenance is supported.

### Cached recall and continuation

Verified recall results are re-dispatched through the current resource filter. Rechecking only tool permission could replay records after scope or audience revocation. Caching for other tools remains unchanged.

External transcript loading now checks full content compatibility for every origin in the same database read snapshot as the transcript. The candidate-list policy may still show neutral metadata, but that does not authorize loading content. Built-in history and the existing Person + Agent continuation boundaries remain separate from durable memory. This does not retroactively edit previously recorded conversational text after a memory is forgotten.

## TDD evidence and acceptance

The initial RED checkpoint reproduced private recall and preamble exposure through real group gateways and captured the leaked preamble in actual provider input. Normal HTTP ingress already refused groups; the regression did not claim a current HTTP exploit.

| Required case | Evidence |
|---|---|
| `TestSharedAgentReturnsNothingFromAnotherPersonsPersonalMemory` | Separate recall and actual provider-context assertions, with a positive owner control |
| `TestRecallDoesNotReachAGroupRoom` | Real gateway dispatch and independent preamble checks below ingress |
| `TestRestrictedHouseholdRecordIsExcludedForTheWrongAudience` | Both paths, including subsequent group-membership removal |
| `TestPerAgentRestrictionNeverWidensTheActivePersonCheck` | Household restriction toggles never authorize an excluded Person |
| `TestOneFilterGovernsRetrievalAndContextAssembly` | Both query paths use `memoryReadClause`; LIMIT 1 and policy-change checks exercise each caller |
| `TestAdmittedAgentUsesTheAdmittingPersonsPersonalMemory` | Positive cross-Agent Personal retrieval and provider-context checks |
| `TestDisclosurePolicyAppliesOnTopOfAudience` | Compatible external control; changed/unknown classifications, admission narrowing, lifecycle and access revocation |

Additional tests cover external provider input, resumed verified recall after restriction, missing destinations, disabled Persons, standalone indexed documents, correction/derivation/promotion/transfer provenance, all origins of continued sessions, and upgrading a populated 0067 database.

Eight independent bypass mutations were killed: preamble's bound context, recall's bound context, cached recall revalidation, gateway group recall, group preamble, correction predecessors, derivation ancestry and destination wiring. Each mutation was restored immediately after its test run.

## Validation

- Linux `go vet ./...` passed across proto, wardstone, keystone, cli and testing/e2e.
- Race checks completed across all five modules. The full API, runtimedb, butlerlive, butlerstore and personref suites passed after fixing the continuation fixtures and registering the new provenance columns in the Person-reference inventory.
- Final Console Owner compatibility fix: full butlerlive/butlerstore race suites and all affected memory/disclosure tests passed again; the new Owner regression was observed RED before the fix.
- Migration upgrade/backfill, repeat application, formatting and `git diff --check` passed.
- Eight bypass mutations were restored; no experimental mutations remain. Final diff reviewed. No Console product files changed.

## Scope boundaries

No semantic/vector retrieval changes, group-conversation enablement, unattended routine identity, provider fallback policy, or slice 16 `local_only` enforcement. No Console product changes are required. The original shared checkout and its unrelated local work are preserved.
