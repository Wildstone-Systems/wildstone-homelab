# Durable research accounting (S24)

Implements #497 under ADR-W047 and `native-agent-capability-contract-v1`.
This is dormant platform plumbing: no tool registration, provider dispatch
wiring, cache implementation, extraction, publication, or continuation route.
S25's separate dormant cache/reuse seam is documented in [SEARCH.md](SEARCH.md).

`Research` wraps one upstream attempt inside an existing Slice/Invoke tool
attempt. `Authorize` is required on every attempt, checkpoint and retained read;
it must resolve current trusted task identity, Agent/Person/surface authority,
provider policy and full egress provenance. Its limits are configuration, never
model arguments. `AuthorizeContinuation` separately authenticates explicit human
intent and approves the exact task/revision/totals; missing callbacks refuse.
No runtime method exposes continuation. S25/S26 supply the actual tool callers.
The canonical gateway maps the typed exhaustion error to the existing waiting
outcome only after confirming the stored research wait. Legacy Slice/Work.Wait
acknowledges that wait without opening another one or replacing its guidance.

Validate input, resolve policy, acquire any bounded upstream queue slot, and
check for an authorized retained result **before** calling `Attempt`. Its
callback performs exactly one upstream dispatch, with no internal retries;
every retry/fallback calls `Attempt` again. Callback errors must already use the
adapter's redacted classifications. Durable reservations bind the task to an
existing dispatched tool attempt and its current lease epoch. Do not invent
ledger attempts or treat a reservation ID as reusable dispatch permission.

Accounting policy:

- Default totals are ten searches and twenty fetches, with the provider policy's
  maxima of 100/200. SQLite transactions serialize admission and save the spend
  and attempt together before I/O.
- Failed DNS/transport/upstream attempts and cancellation after reservation count.
  Cancellation before reservation, preflight denial, verified cache hits and page
  reads do not. A crash or cancellation between commit and I/O conservatively
  spends the reservation; unknown reservations are never refunded or replayed.
- `ReadRetained` only reads already retained, currently authorized bytes. Cache
  misses return to the attempt path. All calls still use existing bounded
  Invoke/Slice tool and turn budgets; research counters do not replace them.
- Configuration changes never reset usage. Initial trusted configuration sets
  totals; subsequent increases cannot increase them implicitly. A detected
  decrease narrows totals, even below usage. An unchanged configuration preserves
  explicitly authorized extensions.
- Exhaustion atomically opens the existing `budget-exhausted` task wait, with
  usage and specific continuation guidance. No automatic wakeup is scheduled.
  Explicit continuation raises absolute totals with revision CAS, resolves only
  that research wait, and records the actor/totals without deleting any history.
  Resuming through other ledger paths never replenishes counters.
- Checkpoints retain up to 100 verified same-task step references and a closed
  next-action hint. Results, source provenance and receipts stay in the existing
  ledger; no query, page body, URL or filename is copied into wait/audit text.
  A resumed caller must reauthorize before reading checkpoint-linked results.

`FinishResearchAttempt` settles accounting only, even if cancellation has fenced
the task; it never publishes results. The existing tool ledger remains responsible
for result publication and rejects stale epochs. A failed accounting settlement
is returned to the caller, and the reservation remains spent.

Migration `0101_research_budgets.sql` is reserved for this slice. **Do not merge or
deploy the incomplete chain:** 0099 belongs to #475 / PR #518 and 0100 to #478 /
PR #523. The parent coordinates their merge before 0101 and verifies the complete
ordered upgrade. Local checks on an incomplete branch are component evidence,
not certification that the ordered migration chain is deployable.

These budgets constrain platform-mediated calls. They do not prevent the DDGS
local-network direct-call bypass risk accepted under #493. No live provider or
native deployment evidence is claimed; #509 owns that release gate.
