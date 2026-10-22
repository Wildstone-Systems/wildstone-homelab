# Slice 23: offboarding, boundary-stop, and soft deletion (#224)

Base: `main` after slice 22. The design contract is `docs/agent-system-design.md` §§17 and 18.8 and ADR-W033 F1–F6. Offboarding is distinct from Owner-to-Member demotion.

## Existing seams

- `OffboardMember` withdraws Person admission first, bumps identity generation, revokes links and chat bindings, pauses/transfers unfinished work, fences personal Connections, stops notices, and records per-store outcomes. `OffboardButlerMember` is a narrower runtime-only route; Console currently calls that narrower route.
- `Ledger.Authorize` and the runner already recheck Person/session/Connection authority before a slice, before model attempts and retries, before tools, and before accepting final answers. Existing tests exercise session withdrawal, but not each offboarding timing from this issue.
- `ChatOutboundAfter` is the last Keystone poll boundary; the adapter currently sends every returned body without a further authority check. A message polled before offboarding can therefore be sent afterward.
- `RetireButler` preserves an Agent's record and history and disables execution. Durable Person, Agent, Connection, and routine rows have no general soft-delete/purge workflow yet.

## Implementation sequence

1. Write negative tests for a queued delivery, a second model round, a retry, and prepared private context. Run them before changing the boundary code; reuse existing runner checks where tests show they suffice.
2. Make outbound delivery recheck recipient and provenance at poll and immediately before the adapter sends. Stop and record withheld delivery using only safe metadata, never body or subject. Offboarding must sweep queued work, but rechecks must remain correct if a sweep misses a row or an adapter already polled it.
3. Route the Owner-facing offboarding action through the full `OffboardMember` workflow with fresh Owner Console authentication. Keep Last-Owner protection and make directory projection converge after the local denial; external directory failure must not restore authority.
4. Record a resource-level disposition for associated tasks, memory, secrets, personal Connections, routines, and Agents. Household-owned material is retained; Personal memory and secrets are quarantined; in-flight shared work transfers in a paused state. The Owner sees identifiers and dispositions, never Personal secret values. The existing routine recipient hold resolves separately from offboarding.
5. Use the existing recoverable Wildstone routine archive state as the soft-delete path. A separate explicit purge requires a fresh Owner Console session, scrubs the routine document, revisions, reviewed version and Plan body, and leaves an audit tombstone and effect receipts. A purged routine cannot be restored or edited. No timer purges automatically. Retired Agents remain inspectable but cannot execute. The household decision for final purge is to erase payloads and retain minimal audit evidence.
6. Expose offboarding inventory in Console and distinguish immediate access loss from irreversible routine purge in the two flows.

## Verification

Use the issue's named negative cases as the acceptance matrix. Add store/API/adapter tests at the actual boundaries, including a message already polled when offboarding lands, a delivery failure report without payload or subject, a second provider call refused, a retry refused, and prompt payload inspection. Run focused tests first, then the Go, Console, and repository checks. Inspect the PR diff against all §§17/18.8 clauses before review.
